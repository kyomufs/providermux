package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuotaFetcherParsing(t *testing.T) {
	// 1. Nested Google Cloud Code real response structure
	nestedJSON := `{
		"groups": [
			{
				"displayName": "Claude and GPT models",
				"description": "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
				"buckets": [
					{
						"bucketId": "3p-weekly",
						"displayName": "Weekly Limit Remaining",
						"window": "weekly",
						"resetTime": "2026-11-16T14:26:13Z",
						"description": "Weekly limit refresh",
						"remainingFraction": 0.4417
					},
					{
						"bucketId": "3p-5h",
						"displayName": "Five Hour Limit Remaining",
						"window": "5h",
						"resetTime": "2026-11-10T01:00:35Z",
						"description": "5-hour limit refresh",
						"remainingFraction": 0.05
					}
				]
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
		w.Write([]byte(nestedJSON))
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
	if len(summary.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(summary.Groups))
	}

	g := summary.Groups[0]
	if g.Group != "Claude and GPT models" {
		t.Errorf("expected group 'Claude and GPT models', got %q", g.Group)
	}
	if g.ModelCount != 3 {
		t.Errorf("expected 3 models in group, got %d", g.ModelCount)
	}
	if len(g.Windows) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(g.Windows))
	}
	if g.Windows[0].Window != WindowWeekly || g.Windows[0].RemainingFraction != 0.4417 {
		t.Errorf("unexpected window[0]: %+v", g.Windows[0])
	}
}

func TestQuotaFetcherFlatFallback(t *testing.T) {
	flatJSON := `{
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
			}
		]
	}`

	summary, err := parseRawQuota([]byte(flatJSON), "token", "")
	if err != nil {
		t.Fatalf("parseRawQuota: %v", err)
	}
	if len(summary.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(summary.Groups))
	}
	if summary.Groups[0].Group != "Gemini" {
		t.Errorf("expected Gemini, got %s", summary.Groups[0].Group)
	}
	if len(summary.Groups[0].Windows) != 2 {
		t.Errorf("expected 2 windows, got %d", len(summary.Groups[0].Windows))
	}
}
