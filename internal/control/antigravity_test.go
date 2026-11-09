package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"providermux/internal/provider"
	"providermux/internal/provider/antigravity"
)

func TestAntigravityControlHandlers(t *testing.T) {
	stubK := &stubKeys{
		keys: map[provider.ID][]string{
			"antigravity": {"refresh-token-1"},
		},
	}
	h := &Handler{
		KeyRotation: stubK,
	}

	mux := http.NewServeMux()
	h.Routes(mux)

	// 1. Test Quota endpoint with no provider registered
	req := httptest.NewRequest(http.MethodGet, "/_control/antigravity/quota", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("quota status code = %d, want 200", rec.Code)
	}

	var quotaResp AntigravityAccountsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &quotaResp); err != nil {
		t.Fatalf("parsing quota response: %v", err)
	}
	if quotaResp.ProviderID != "antigravity" {
		t.Errorf("expected provider_id 'antigravity', got %q", quotaResp.ProviderID)
	}

	// 2. Test OAuth Start endpoint
	startReq := httptest.NewRequest(http.MethodGet, "/_control/antigravity/oauth/start", nil)
	startRec := httptest.NewRecorder()
	mux.ServeHTTP(startRec, startReq)

	if startRec.Code != http.StatusOK {
		t.Fatalf("oauth start code = %d, want 200", startRec.Code)
	}
	var startResp oauthStartResponse
	if err := json.Unmarshal(startRec.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("parsing oauth start response: %v", err)
	}
	if !strings.HasPrefix(startResp.AuthURL, antigravity.OAuthAuthURL) {
		t.Errorf("expected auth URL to start with Google URL, got %q", startResp.AuthURL)
	}
	if startResp.Verifier == "" || startResp.State == "" {
		t.Errorf("expected non-empty verifier and state")
	}
}
