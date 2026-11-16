package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"providermux/internal/config"
)

// THE END-TO-END CHECK.
//
// It assembles the daemon the way `up` does, but on the in-process stub provider
// and in XDG directories of its own. That is the whole path — the OpenAI
// surface, the provider dispatch, the refusal machinery, the counters and the
// control surface — with no key, no network and no writes outside the test's
// temporary directory.
//
// WHAT IT IS NOT. It does not start the pollers or the egress observer: both
// reach the network, and neither is needed to answer a request. The parts it
// exercises are the parts a client depends on.

const e2eConfig = `{
  "mode": "local",
  "listen": "127.0.0.1:0",
  "credential": "anonymous",
  "key_rotation": "off",
  "providers": {
    "stub": {
      "type": "stub",
      "settings": {"reply": "stub"}
    },
    "flaky": {
      "type": "stub",
      "settings": {
        "fail": 1,
        "fail_status": 429,
        "fail_text": "rate limit exceeded"
      }
    },
    "rotate": {
      "type": "stub",
      "credential": "account",
      "key_rotation": "on",
      "api_key": "first-key",
      "settings": {
        "keys": ["second-key"],
        "fail": 1,
        "fail_status": 403,
        "fail_text": "invalid key"
      }
    }
  }
}`

// e2eDaemon writes the configuration into isolated XDG directories and builds
// the daemon from it.
func e2eDaemon(t *testing.T) *httptest.Server {
	t.Helper()

	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	t.Setenv("HOME", root)

	// The machine's own axes must not leak into the fixture: the test is about
	// the file it writes, and an exported PROVIDERMUX_MODE would change the
	// daemon under it.
	for _, name := range []string{
		config.EnvMode, config.EnvListen, config.EnvToken, config.EnvTransport,
		config.EnvUpstream, config.EnvCredential, config.EnvRotation, config.EnvFamily,
		config.EnvFirstEventTimeout, config.EnvIdleTimeout,
		config.EnvReasoningIdleTimeout, config.EnvConfig, config.EnvState,
	} {
		t.Setenv(name, "")
	}

	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(e2eConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadFrom(paths)
	if err != nil {
		t.Fatalf("the fixture configuration does not load: %v", err)
	}

	d, err := build(cfg)
	if err != nil {
		t.Fatalf("the daemon does not assemble: %v", err)
	}

	srv := httptest.NewServer(d.handler)
	t.Cleanup(srv.Close)
	return srv
}

func e2ePost(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, raw
}

func e2eGet(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, raw
}

// TestEndToEndThroughTheStub — the whole client-visible path in one run.
func TestEndToEndThroughTheStub(t *testing.T) {
	srv := e2eDaemon(t)

	// 1. The catalog names every provider with a composite identifier.
	resp, raw := e2eGet(t, srv.URL+"/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models = %d: %s", resp.StatusCode, raw)
	}
	for _, want := range []string{"stub/stub-echo", "flaky/stub-echo", "rotate/stub-echo"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("the catalog lacks %q: %s", want, raw)
		}
	}

	// 2. An ordinary answer.
	resp, raw = e2ePost(t, srv.URL+"/v1/chat/completions",
		`{"model":"stub/stub-echo","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat = %d: %s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte(`"content":"stub"`)) {
		t.Fatalf("the answer is not the stub's: %s", raw)
	}

	// 3. A stream: the terminator has to arrive, or the client never closes.
	resp, raw = e2ePost(t, srv.URL+"/v1/chat/completions",
		`{"model":"stub/stub-echo","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("content-type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("stream content-type = %q", ct)
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	sawDone := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "data: [DONE]" {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("the stream has no terminator: %s", raw)
	}

	// 4. A refusal reaches the client as it is. The gateway neither repeats it
	// nor replaces it with another key: the client's own policy is the only one
	// that decides whether to ask again.
	resp, raw = e2ePost(t, srv.URL+"/p/flaky/v1/chat/completions",
		`{"model":"stub-echo","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("flaky = %d, want the provider's 429: %s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("rate limit exceeded")) {
		t.Fatalf("the refusal body was rewritten: %s", raw)
	}

	// 5. The key was rejected and there is another in the pool, but rotation is
	// not a reason to spend it behind the client's back. The 403 is the answer.
	resp, raw = e2ePost(t, srv.URL+"/p/rotate/v1/chat/completions",
		`{"model":"stub-echo","messages":[{"role":"user","content":"hello"}]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rotate = %d, want the provider's 403: %s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("invalid key")) {
		t.Fatalf("the refusal body was rewritten: %s", raw)
	}

	// 6. The control surface sees the pool, the usage and a live axis change.
	resp, raw = e2eGet(t, srv.URL+"/_control/keys")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("keys = %d: %s", resp.StatusCode, raw)
	}
	var keys struct {
		Providers []struct {
			Provider string   `json:"provider"`
			Keys     []string `json:"keys"`
			Used     bool     `json:"key_used"`
			Rotation bool     `json:"rotation"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("keys body: %v", err)
	}
	found := false
	for _, p := range keys.Providers {
		if p.Provider != "rotate" {
			continue
		}
		found = true
		if len(p.Keys) != 2 {
			t.Fatalf("rotate keys = %v, want two fingerprints", p.Keys)
		}
		if !p.Used || !p.Rotation {
			t.Fatalf("rotate = %+v, want a key in use with rotation wired", p)
		}
	}
	if !found {
		t.Fatalf("the rotate provider is missing from the keys response: %s", raw)
	}

	resp, raw = e2ePost(t, srv.URL+"/_control/options", `{"axis":"family","value":"V6"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("options = %d: %s", resp.StatusCode, raw)
	}
	var opt struct {
		Value   string `json:"value"`
		Current struct {
			Family string `json:"family"`
		} `json:"current"`
	}
	if err := json.Unmarshal(raw, &opt); err != nil {
		t.Fatalf("options body: %v", err)
	}
	if opt.Value != config.FamilyV6 || opt.Current.Family != config.FamilyV6 {
		t.Fatalf("the axis change was not canonical: %s", raw)
	}

	resp, raw = e2ePost(t, srv.URL+"/_control/options", `{"axis":"mode","value":"server"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("server mode without a token = %d, want 400: %s", resp.StatusCode, raw)
	}

	// 7. The counters tell the same story as the answers: two successes, two
	// refusals passed through, and not a single attempt more than the client
	// asked for.
	resp, raw = e2eGet(t, srv.URL+"/_control/status")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var status struct {
		Totals struct {
			OK      int `json:"ok"`
			Refused int `json:"refused"`
		} `json:"totals_today"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("status body: %v", err)
	}
	if status.Totals.OK != 2 {
		t.Fatalf("successful attempts = %d, want 2: %s", status.Totals.OK, raw)
	}
	if status.Totals.Refused != 2 {
		t.Fatalf("refused attempts = %d, want 2: %s", status.Totals.Refused, raw)
	}
}
