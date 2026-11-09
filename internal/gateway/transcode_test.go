package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"providermux/internal/provider"
	"providermux/internal/state"
)

// translatingProvider is a provider whose service answers in a dialect of its own.
//
// The translation here is a stand-in for a real one: the upstream sends
// `event={"text":...}` and the provider turns it into OpenAI chunks. What is being
// tested is the seam, not the translation of any particular service.
type translatingProvider struct {
	*stubProvider
	// translated counts the calls, so a test can assert that a refusal was not
	// offered to the translation at all.
	translated int
}

func (p *translatingProvider) Transcode(upstream io.ReadCloser, model string) io.ReadCloser {
	p.translated++
	raw, _ := io.ReadAll(upstream)

	var out bytes.Buffer
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "event=") {
			continue
		}
		text := strings.TrimPrefix(strings.TrimSpace(line), "event=")
		out.WriteString(`data: {"id":"cmpl","object":"chat.completion.chunk","model":"` + model +
			`","choices":[{"index":0,"delta":{"content":` + strconv.Quote(text) + `},"finish_reason":null}]}` + "\n\n")
	}
	out.WriteString(`data: {"id":"cmpl","object":"chat.completion.chunk","model":"` + model +
		`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	out.WriteString("data: [DONE]\n\n")
	return io.NopCloser(bytes.NewReader(out.Bytes()))
}

// newTranslatingHandler assembles a handler over a provider that translates.
func newTranslatingHandler(t *testing.T, upstream *httptest.Server) (*Handler, *translatingProvider) {
	t.Helper()

	p := &translatingProvider{stubProvider: &stubProvider{
		id:     "test",
		models: []provider.Model{{ID: "model", Context: 1000, MaxOutput: 100, Free: true}},
		build: func(provider.Request) (provider.Prepared, error) {
			return provider.Prepared{
				URL:             upstream.URL + "/generate",
				Headers:         http.Header{"content-type": []string{"application/json"}},
				Body:            []byte(`{"model":"model"}`),
				StreamsUpstream: true,
			}, nil
		},
	}}

	reg := provider.NewRegistry()
	reg.Register(p)

	st, err := state.Open(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		Providers: reg,
		Transport: fixedTransport{rt: upstream.Client().Transport},
		State:     st,
	}
	return h, p
}

// TestANonOpenAIServiceIsTranslatedForAStreamingClient — the client reads the
// chunks its own protocol defines, not the service's events.
func TestANonOpenAIServiceIsTranslatedForAStreamingClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event=hello\n\n")
	}))
	defer upstream.Close()

	h, p := newTranslatingHandler(t, upstream)

	rec := postChat(t, h, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"x"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q, want the translated stream", got)
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello"`) {
		t.Fatalf("the answer was not translated: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("the translated stream has no done marker: %s", rec.Body.String())
	}
	if p.translated != 1 {
		t.Fatalf("translated = %d, want 1", p.translated)
	}
}

// TestATranslatedAnswerIsCollectedForAClientThatAskedForOne — the client asked for
// a single response and gets one, even though the service only streams.
func TestATranslatedAnswerIsCollectedForAClientThatAskedForOne(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event=ok\n\n")
	}))
	defer upstream.Close()

	h, _ := newTranslatingHandler(t, upstream)

	rec := postChat(t, h, `{"model":"test/model","messages":[{"role":"user","content":"x"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q, want one response", got)
	}
	if !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("the answer was not collected: %s", rec.Body.String())
	}
}

// TestARefusalIsNeverTranslated — the status, body and headers of a refusal belong
// to the client: it parses the service's own error there, and a translation would
// take that away from it.
func TestARefusalIsNeverTranslated(t *testing.T) {
	const refusal = `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, refusal)
	}))
	defer upstream.Close()

	h, p := newTranslatingHandler(t, upstream)

	rec := postChat(t, h, `{"model":"test/model","messages":[{"role":"user","content":"x"}]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
	if rec.Body.String() != refusal {
		t.Fatalf("body = %s, want the refusal verbatim", rec.Body.String())
	}
	if p.translated != 0 {
		t.Fatalf("translated = %d: a refusal was offered to the translation", p.translated)
	}
}
