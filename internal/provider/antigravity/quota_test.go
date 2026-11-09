package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQuotaFetcherParsing(t *testing.T) {
	nestedJSON := `{
		"groups": [
			{
				"displayName": "GEMINI MODELS",
				"description": "Models within this group: Gemini Flash, Gemini Pro",
				"buckets": [
					{
						"bucketId": "gemini-5h",
						"displayName": "Five Hour Limit Remaining",
						"window": "5h",
						"resetTime": "2026-11-10T04:00:00Z",
						"remainingFraction": 0.7649
					},
					{
						"bucketId": "gemini-weekly",
						"displayName": "Weekly Limit Remaining",
						"window": "weekly",
						"resetTime": "2026-11-16T14:00:00Z",
						"remainingFraction": 0.4716
					}
				]
			},
			{
				"displayName": "CLAUDE AND GPT MODELS",
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
						"remainingFraction": 0.0073
					}
				]
			}
		]
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if len(summary.Groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(summary.Groups))
	}

	gemini := summary.Groups[0]
	if gemini.Group != "GEMINI MODELS" {
		t.Errorf("expected group 'GEMINI MODELS', got %q", gemini.Group)
	}
	if len(gemini.Windows) != 2 {
		t.Fatalf("expected 2 windows in gemini, got %d", len(gemini.Windows))
	}
	if gemini.Windows[0].RemainingFraction != 0.7649 {
		t.Errorf("expected 0.7649, got %f", gemini.Windows[0].RemainingFraction)
	}

	claudeGpt := summary.Groups[1]
	if claudeGpt.Group != "CLAUDE AND GPT MODELS" {
		t.Errorf("expected group 'CLAUDE AND GPT MODELS', got %q", claudeGpt.Group)
	}
	if len(claudeGpt.Windows) != 2 {
		t.Fatalf("expected 2 windows in claude/gpt, got %d", len(claudeGpt.Windows))
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
	if summary.Groups[0].Group != "GEMINI MODELS" {
		t.Errorf("expected GEMINI MODELS, got %s", summary.Groups[0].Group)
	}
	if len(summary.Groups[0].Windows) != 2 {
		t.Errorf("expected 2 windows, got %d", len(summary.Groups[0].Windows))
	}
}
