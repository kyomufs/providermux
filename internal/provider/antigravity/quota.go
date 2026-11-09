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
	Buckets []struct {
		WindowMinutes int     `json:"windowMinutes"`
		Remaining     float64 `json:"remainingFraction"`
		ResetTime     string  `json:"resetTime"`
		AllowedModels []any   `json:"allowedModels"`
	} `json:"buckets"`
	Models []struct {
		ID    string `json:"id"`
		Group string `json:"group"`
	} `json:"models"`
}

func accountFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
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

	bodyData := map[string]string{}
	if project != "" {
		bodyData["project"] = project
	}
	bodyJSON, _ := json.Marshal(bodyData)

	endpoint := strings.TrimRight(q.baseURL, "/") + "/v1internal:retrieveUserQuotaSummary"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", q.ua)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, fmt.Errorf("reading quota response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota endpoint returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
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
	}

	geminiWindows := []QuotaWindowView{}
	nonGeminiWindows := []QuotaWindowView{}

	// Upstream returns bucket windows in minutes (e.g. 300 = 5h, 10080 = 7d/weekly)
	// or separated by allowedModels / groups
	for _, b := range raw.Buckets {
		win := Window5Hour
		if b.WindowMinutes > 300 || b.WindowMinutes == 10080 {
			win = WindowWeekly
		}

		view := QuotaWindowView{
			Window:            win,
			RemainingFraction: b.Remaining,
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

	// Fallback: if groups weren't split by allowedModels, provide what we got
	if len(geminiWindows) == 0 && len(nonGeminiWindows) == 0 && len(raw.Buckets) > 0 {
		for _, b := range raw.Buckets {
			win := Window5Hour
			if b.WindowMinutes > 300 {
				win = WindowWeekly
			}
			geminiWindows = append(geminiWindows, QuotaWindowView{
				Window:            win,
				RemainingFraction: b.Remaining,
				ResetTime:         b.ResetTime,
			})
		}
	}

	if len(geminiWindows) > 0 {
		summary.Groups = append(summary.Groups, QuotaGroupView{
			Group:      "gemini",
			ModelCount: 3,
			Windows:    geminiWindows,
		})
	}
	if len(nonGeminiWindows) > 0 {
		summary.Groups = append(summary.Groups, QuotaGroupView{
			Group:      "non-gemini",
			ModelCount: 2,
			Windows:    nonGeminiWindows,
		})
	}

	return summary, nil
}
