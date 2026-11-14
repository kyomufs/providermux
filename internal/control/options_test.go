package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"providermux/internal/config"
	"providermux/internal/provider"
)

type stubProviders struct {
	ids []provider.ID
}

func (s *stubProviders) IDs() []provider.ID {
	return s.ids
}

func (s *stubProviders) Has(id provider.ID) bool {
	for _, item := range s.ids {
		if item == id {
			return true
		}
	}
	return false
}

func TestOptionsHandlers(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := config.Default()
	cfg.Paths.ConfigFile = cfgPath
	cfg.Providers = map[string]config.ProviderConfig{
		"antigravity": {
			Credential:  "account",
			KeyRotation: "on",
			Transport:   "direct",
		},
		"opencode": {
			Credential:  "anonymous",
			KeyRotation: "off",
			Transport:   "direct",
		},
	}

	h := &Handler{
		Config:     cfg,
		Providers:  &provider.Registry{}, // empty registry
		Transports: []string{"direct", "warp"},
	}

	mux := http.NewServeMux()
	h.Routes(mux)

	// 1. GET /_control/options
	req := httptest.NewRequest(http.MethodGet, "/_control/options", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("options status code = %d, want 200", rec.Code)
	}

	var optResp OptionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &optResp); err != nil {
		t.Fatalf("unmarshal options response: %v", err)
	}

	if optResp.Current == nil {
		t.Fatal("expected current options to be non-nil")
	}
	if optResp.Current.Mode != "local" {
		t.Errorf("current mode = %q, want 'local'", optResp.Current.Mode)
	}

	// 2. POST /_control/options (update global axis)
	updateBody := `{"axis": "model_fallback", "value": "on"}`
	postReq := httptest.NewRequest(http.MethodPost, "/_control/options", bytes.NewReader([]byte(updateBody)))
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusOK {
		t.Fatalf("post status code = %d, want 200", postRec.Code)
	}

	if h.Config.ModelFallback.Val != "on" {
		t.Errorf("expected model_fallback = 'on', got %q", h.Config.ModelFallback.Val)
	}
}

// TestAntigravityRefusesAnonymousAtRuntime — the file loaded at startup is not
// the only way a credential gets set: the panel writes it through the options
// endpoint. The refusal has to live there too, or the panel can put antigravity
// on the anonymous lane the provider does not have.
func TestAntigravityRefusesAnonymousAtRuntime(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")
	cfg.Providers = map[string]config.ProviderConfig{
		"antigravity": {Type: "antigravity", Credential: "account"},
	}

	h := &Handler{Config: cfg, Providers: &provider.Registry{}, Transports: []string{"direct"}}
	mux := http.NewServeMux()
	h.Routes(mux)

	body := `{"axis": "credential", "provider": "antigravity", "value": "anonymous"}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/_control/options", bytes.NewReader([]byte(body))))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := h.Config.Providers["antigravity"].Credential; got != "account" {
		t.Fatalf("credential = %q, want it unchanged at %q", got, "account")
	}
}

func TestFingerprintMatches(t *testing.T) {
	short := "a7f236d6"
	cases := []struct {
		value string
		want  bool
	}{
		{"a7f236d6", true},
		{"a7f236d6b8c64d7b", true},
		{"A7F236D6B8C64D7B", true},
		{"b8c64d7b", false},
		{"", false},
	}
	for _, c := range cases {
		if got := fingerprintMatches(short, c.value); got != c.want {
			t.Errorf("fingerprintMatches(%q, %q) = %v, want %v", short, c.value, got, c.want)
		}
	}
}
