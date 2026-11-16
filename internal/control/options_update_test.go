package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"providermux/internal/config"
	"providermux/internal/provider"
)

func postOption(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, Prefix+"options", bytes.NewReader([]byte(body))))
	return rec
}

// TestOptionsStoreTheCanonicalSpelling.
//
// A value is accepted case-insensitively because an operator types "ON", but
// what is stored has to be the spelling every comparison uses. Keeping "ON"
// made the axis report one value while the request path looked for "on".
func TestOptionsStoreTheCanonicalSpelling(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")
	cfg.Providers = map[string]config.ProviderConfig{
		"p": {Type: "opencode"},
	}

	reg := provider.NewRegistry()
	reg.Register(&stubProvider{id: "p"})

	h := &Handler{Config: cfg, Providers: reg, Transports: []string{"direct", "warp"}}
	mux := http.NewServeMux()
	h.Routes(mux)

	if rec := postOption(t, mux, `{"axis":"credential","value":"Account"}`); rec.Code != http.StatusOK {
		t.Fatalf("global credential = %d: %s", rec.Code, rec.Body.String())
	}
	if got := cfg.Credential.Val; got != config.CredentialAccount {
		t.Fatalf("credential = %q, want %q", got, config.CredentialAccount)
	}
	// The file carries provider keys: it is created owner-only.
	if info, err := os.Stat(cfg.Paths.ConfigFile); err != nil {
		t.Fatalf("the configuration was not saved: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("configuration mode = %o, want 600", perm)
	}

	if rec := postOption(t, mux, `{"axis":"transport","provider":"p","value":"WARP"}`); rec.Code != http.StatusOK {
		t.Fatalf("provider transport = %d: %s", rec.Code, rec.Body.String())
	}
	if got := cfg.Providers["p"].Transport; got != "warp" {
		t.Fatalf("transport = %q, want %q", got, "warp")
	}
}

// TestOptionsRefuseServerModeWithoutToken.
//
// A live switch to server mode without a secret would make the daemon
// unreachable through the API that changed it: the token check would start
// demanding a value nobody has.
func TestOptionsRefuseServerModeWithoutToken(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")

	h := &Handler{Config: cfg}
	mux := http.NewServeMux()
	h.Routes(mux)

	if rec := postOption(t, mux, `{"axis":"mode","value":"server"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("server mode without a token = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := cfg.Mode.Val; got != config.ModeLocal {
		t.Fatalf("mode = %q, want it unchanged at %q", got, config.ModeLocal)
	}

	cfg.Update(func() { cfg.Token.Set("secret", config.SourceFile) })
	if rec := postOption(t, mux, `{"axis":"mode","value":"server"}`); rec.Code != http.StatusOK {
		t.Fatalf("server mode with a token = %d: %s", rec.Code, rec.Body.String())
	}
	if got := cfg.Mode.Val; got != config.ModeServer {
		t.Fatalf("mode = %q, want %q", got, config.ModeServer)
	}
}

// TestOptionsRefuseToOverwriteAMalformedFile.
//
// A file that does not parse may hold settings that exist nowhere else.
// Persisting one axis after a failed parse used to rewrite the file with that
// single key, silently discarding the rest.
func TestOptionsRefuseToOverwriteAMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	const broken = `{"providers": {"p": {`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Paths.ConfigFile = path
	cfg.Family.Set(config.FamilyAuto, config.SourceDefault)

	h := &Handler{Config: cfg}
	mux := http.NewServeMux()
	h.Routes(mux)

	if rec := postOption(t, mux, `{"axis":"family","value":"v6"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("malformed file = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if got := cfg.Family.Val; got != config.FamilyAuto {
		t.Fatalf("family changed in memory to %q after a failed save", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != broken {
		t.Fatalf("the file was rewritten:\n%s", raw)
	}
}

// TestKeysReportASingleAccountKeyAsUsed.
//
// Rotation needs a second key; PRESENTING a key does not. With one key and the
// account credential the key is on the wire, and the control surface has to say
// so.
func TestKeysReportASingleAccountKeyAsUsed(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConfig{
		"p": {Type: "opencode", Credential: config.CredentialAccount, KeyRotation: config.RotationOn},
	}
	cfg.Credential.Set(config.CredentialAccount, config.SourceDefault)

	h := &Handler{
		Config: cfg,
		Providers: func() *provider.Registry {
			reg := provider.NewRegistry()
			reg.Register(&stubProvider{id: "p"})
			return reg
		}(),
		KeyRotation: &stubKeys{keys: map[provider.ID][]string{"p": {"only"}}},
	}

	resp := h.keysResponse()
	if len(resp.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(resp.Providers))
	}
	if !resp.Providers[0].Used {
		t.Fatal("a single account key was reported as unused")
	}
	if !resp.Providers[0].Rotation {
		t.Fatal("rotation was requested in the configuration and should be reported")
	}
}

// TestOptionsResponseCarriesTheStoredValue — the caller must see what was
// stored, not the spelling it happened to send.
func TestOptionsResponseCarriesTheStoredValue(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")

	h := &Handler{Config: cfg}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := postOption(t, mux, `{"axis":"model_fallback","value":"ON"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("model_fallback = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	if body.Value != config.FallbackOn {
		t.Fatalf("response value = %q, want %q", body.Value, config.FallbackOn)
	}
}
