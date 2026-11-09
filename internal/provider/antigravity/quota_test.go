package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuotaFetcherParsing(t *testing.T) {
	rawJSON := `{
		"buckets": [
			{
				"windowMinutes": 300,
				"remainingFraction": 0.85,
				"resetTime": "2026-11-10T04:00:00Z",
				"allowedModels": ["gemini-2.5-pro", "gemini-2.5-flash"]
			},
			{
				"windowMinutes": 10080,
				"remainingFraction": 0.95,
				"resetTime": "2026-11-17T00:00:00Z",
				"allowedModels": ["gemini-2.5-pro"]
			},
			{
				"windowMinutes": 300,
				"remainingFraction": 0.40,
				"resetTime": "2026-11-10T02:30:00Z",
				"allowedModels": ["claude-3-7-sonnet", "gpt-4o"]
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(rawJSON))
	}))
	defer server.Close()

	p, err := New(Config{
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.tokens.entries["my-refresh-token"] = token{
		value:   "test-access-token",
		expires: time.Now().Add(time.Hour),
	}

	fetcher := NewQuotaFetcher(p)
	summary, err := fetcher.FetchQuota(context.Background(), "my-refresh-token", "my-project")
	if err != nil {
		t.Fatalf("FetchQuota: %v", err)
	}

	if summary.AccountID == "" {
		t.Error("expected non-empty AccountID")
	}
	if summary.Project != "my-project" {
		t.Errorf("expected project 'my-project', got %q", summary.Project)
	}
	if len(summary.Groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(summary.Groups))
	}

	gemini := summary.Groups[0]
	if gemini.Group != "gemini" {
		t.Errorf("expected group 'gemini', got %q", gemini.Group)
	}
	if len(gemini.Windows) != 2 {
		t.Errorf("expected 2 gemini windows, got %d", len(gemini.Windows))
	}
	if gemini.Windows[0].RemainingFraction != 0.85 {
		t.Errorf("expected 0.85, got %f", gemini.Windows[0].RemainingFraction)
	}

	nonGemini := summary.Groups[1]
	if nonGemini.Group != "non-gemini" {
		t.Errorf("expected group 'non-gemini', got %q", nonGemini.Group)
	}
	if len(nonGemini.Windows) != 1 {
		t.Errorf("expected 1 non-gemini window, got %d", len(nonGemini.Windows))
	}
	if nonGemini.Windows[0].RemainingFraction != 0.40 {
		t.Errorf("expected 0.40, got %f", nonGemini.Windows[0].RemainingFraction)
	}
}
