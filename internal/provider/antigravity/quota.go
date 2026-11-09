package antigravity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// QuotaWindow represents a rate-limiting duration window.
type QuotaWindow string

const (
	Window5Hour  QuotaWindow = "5h"
	WindowWeekly QuotaWindow = "weekly"
)

// QuotaWindowView describes remaining fraction and next reset time for a window.
type QuotaWindowView struct {
	Window            QuotaWindow `json:"window"`
	RemainingFraction float64     `json:"remaining_fraction"`
	ResetTime         string      `json:"reset_time,omitempty"`
}

// QuotaGroupView describes the quota state for a model group ("gemini" vs "non-gemini").
type QuotaGroupView struct {
	Group      string            `json:"group"`
	ModelCount int               `json:"model_count"`
	Windows    []QuotaWindowView `json:"windows"`
}

// AccountQuotaSummary represents the normalized quota view for an account.
type AccountQuotaSummary struct {
	AccountID   string           `json:"account_id"`
	Email       string           `json:"email,omitempty"`
	Project     string           `json:"project,omitempty"`
	Groups      []QuotaGroupView `json:"groups"`
	Error       string           `json:"error,omitempty"`
	LastChecked time.Time        `json:"last_checked"`
}

// QuotaFetcher fetches and normalizes quota summaries from the Google Cloud Code Antigravity API.
type QuotaFetcher struct {
	tokens  *tokenCache
	baseURL string
	ua      string
	http    *http.Client
}

// NewQuotaFetcher creates a fetcher using the provider's token cache and settings.
func NewQuotaFetcher(p *Provider) *QuotaFetcher {
	return &QuotaFetcher{
		tokens:  p.tokens,
		baseURL: p.baseURL,
		ua:      p.ua,
		http:    p.cfg.HTTP,
	}
}

// rawQuotaResponse models the upstream JSON from /v1internal:retrieveUserQuotaSummary.
type rawQuotaResponse struct {
	Groups []struct {
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		Buckets     []struct {
			BucketID          string  `json:"bucketId"`
			DisplayName       string  `json:"displayName"`
			Window            string  `json:"window"`
			WindowMinutes     int     `json:"windowMinutes"`
			ResetTime         string  `json:"resetTime"`
			Description       string  `json:"description"`
			RemainingFraction float64 `json:"remainingFraction"`
			Disabled          bool    `json:"disabled"`
		} `json:"buckets"`
	} `json:"groups"`
	Buckets []struct {
		BucketID          string  `json:"bucketId"`
		DisplayName       string  `json:"displayName"`
		Window            string  `json:"window"`
		WindowMinutes     int     `json:"windowMinutes"`
		ResetTime         string  `json:"resetTime"`
		AllowedModels     []any   `json:"allowedModels"`
		RemainingFraction float64 `json:"remainingFraction"`
		Disabled          bool    `json:"disabled"`
	} `json:"buckets"`
}

func accountFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// Endpoints tried in order, matching @cortexkit/antigravity-auth-core.
var quotaEndpoints = []string{
	"https://cloudcode-pa.googleapis.com",
	"https://daily-cloudcode-pa.googleapis.com",
}

// FetchQuota retrieves the quota summary for a specific refresh token.
func (q *QuotaFetcher) FetchQuota(ctx context.Context, refreshToken, project string) (*AccountQuotaSummary, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("empty refresh token")
	}

	accessToken := q.tokens.access(refreshToken)
	if accessToken == "" {
		return nil, fmt.Errorf("failed to obtain access token from refresh token")
	}

	client := q.http
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	// Prepare endpoints to try
	endpoints := []string{}
	if q.baseURL != "" && q.baseURL != DefaultBaseURL {
		endpoints = append(endpoints, strings.TrimRight(q.baseURL, "/"))
	}
	endpoints = append(endpoints, quotaEndpoints...)

	var lastErr error
	var respBody []byte

	// Try each endpoint with project, with fallback to empty project on 403
	for _, base := range endpoints {
		url := base + "/v1internal:retrieveUserQuotaSummary"

		bodyData := map[string]string{}
		if project != "" {
			bodyData["project"] = project
		}
		bJSON, _ := json.Marshal(bodyData)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bJSON))
		if err != nil {
			lastErr = err
			continue
		}

		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Antigravity/1.18.3 Chrome/138.0.7204.235 Electron/37.3.1 Safari/537.36")
		req.Header.Set("X-Goog-Api-Client", "google-cloud-sdk vscode_cloudshelleditor/0.1")
		req.Header.Set("Client-Metadata", `{"ideType":"ANTIGRAVITY","platform":"MACOS","pluginType":"GEMINI"}`)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		// If 403 with project, retry with empty project
		if resp.StatusCode == http.StatusForbidden && project != "" {
			resp.Body.Close()
			reqEmpty, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
			reqEmpty.Header = req.Header.Clone()
			resp, err = client.Do(reqEmpty)
			if err != nil {
				lastErr = err
				continue
			}
		}

		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode == http.StatusOK {
			respBody = b
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("status %d from %s: %s", resp.StatusCode, base, strings.TrimSpace(string(b)))
	}

	if lastErr != nil && len(respBody) == 0 {
		return nil, fmt.Errorf("quota request failed: %w", lastErr)
	}

	email := fetchUserEmail(client, accessToken)
	summary, err := parseRawQuota(respBody, refreshToken, project)
	if err == nil && email != "" {
		summary.Email = email
	}
	return summary, err
}

func parseRawQuota(data []byte, refreshToken, project string) (*AccountQuotaSummary, error) {
	var raw rawQuotaResponse
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decoding quota response: %w", err)
	}

	summary := &AccountQuotaSummary{
		AccountID:   accountFingerprint(refreshToken),
		Project:     project,
		LastChecked: time.Now().UTC(),
		Groups:      []QuotaGroupView{},
	}

	// 1. First check nested groups (official Google Cloud Code API structure)
	if len(raw.Groups) > 0 {
		for _, g := range raw.Groups {
			windows := []QuotaWindowView{}
			for _, b := range g.Buckets {
				if b.Disabled {
					continue
				}
				win := Window5Hour
				winStr := strings.ToLower(b.Window + " " + b.BucketID)
				if strings.Contains(winStr, "week") || b.WindowMinutes > 300 {
					win = WindowWeekly
				}

				windows = append(windows, QuotaWindowView{
					Window:            win,
					RemainingFraction: b.RemainingFraction,
					ResetTime:         b.ResetTime,
				})
			}

			if len(windows) > 0 {
				modelCount := countModelsFromDescription(g.Description)
				if modelCount == 0 {
					modelCount = len(windows)
				}
				groupTitle := g.DisplayName
				if groupTitle == "" {
					groupTitle = "Google Cloud Code"
				}

				summary.Groups = append(summary.Groups, QuotaGroupView{
					Group:      groupTitle,
					ModelCount: modelCount,
					Windows:    windows,
				})
			}
		}
	}

	// 2. Fallback if top-level buckets were returned
	if len(summary.Groups) == 0 && len(raw.Buckets) > 0 {
		geminiWindows := []QuotaWindowView{}
		nonGeminiWindows := []QuotaWindowView{}

		for _, b := range raw.Buckets {
			if b.Disabled {
				continue
			}
			win := Window5Hour
			if b.WindowMinutes > 300 || strings.Contains(strings.ToLower(b.Window+" "+b.BucketID), "week") {
				win = WindowWeekly
			}

			view := QuotaWindowView{
				Window:            win,
				RemainingFraction: b.RemainingFraction,
				ResetTime:         b.ResetTime,
			}

			isGemini := true
			for _, m := range b.AllowedModels {
				mStr := fmt.Sprint(m)
				if strings.Contains(strings.ToLower(mStr), "claude") || strings.Contains(strings.ToLower(mStr), "gpt") {
					isGemini = false
					break
				}
			}

			if isGemini {
				geminiWindows = append(geminiWindows, view)
			} else {
				nonGeminiWindows = append(nonGeminiWindows, view)
			}
		}

		if len(geminiWindows) > 0 {
			summary.Groups = append(summary.Groups, QuotaGroupView{
				Group:      "Gemini",
				ModelCount: 3,
				Windows:    geminiWindows,
			})
		}
		if len(nonGeminiWindows) > 0 {
			summary.Groups = append(summary.Groups, QuotaGroupView{
				Group:      "Claude & GPT (3P)",
				ModelCount: 3,
				Windows:    nonGeminiWindows,
			})
		}
	}

	return summary, nil
}

func countModelsFromDescription(desc string) int {
	if desc == "" {
		return 0
	}
	parts := strings.SplitN(desc, ":", 2)
	target := desc
	if len(parts) == 2 {
		target = parts[1]
	}
	items := strings.Split(target, ",")
	count := 0
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			count++
		}
	}
	return count
}
