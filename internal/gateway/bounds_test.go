package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"providermux/internal/config"
	"providermux/internal/provider"
)

// TestBudgetsMatchTheConfigurationDefaults holds the gateway's fallbacks to the
// ones the operator sees in the configuration.
//
// Two copies of the same number exist because the gateway cannot import the
// configuration without the dependency pointing the wrong way. A test is what
// keeps them from drifting apart silently: a fallback that disagrees with the
// documented default is a budget nobody chose.
func TestBudgetsMatchTheConfigurationDefaults(t *testing.T) {
	if defaultFirstEventTimeout != config.DefaultFirstEventTimeout {
		t.Fatalf("first event fallback = %s, want %s",
			defaultFirstEventTimeout, config.DefaultFirstEventTimeout)
	}
	if defaultIdleTimeout != config.DefaultIdleTimeout {
		t.Fatalf("idle fallback = %s, want %s", defaultIdleTimeout, config.DefaultIdleTimeout)
	}
}

// TestSilentProviderIsCutOffAtTheBudget — a provider that accepts the connection
// and then says nothing is what a broken path looks like.
//
// Nothing fails in that state: the request is written, the socket stays open and
// the read never returns. Without a budget the client waits for as long as it is
// willing to, and the daemon waits with it. That is the whole failure being
// pinned here: the answer must arrive at the budget, as an error.
func TestSilentProviderIsCutOffAtTheBudget(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	h, p, st := newTestHandler(t, upstream)
	p.build = slowBuild(upstream.URL+"/chat/completions", 100*time.Millisecond, 100*time.Millisecond)

	start := time.Now()
	rec := postChat(t, h, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusGatewayTimeout, rec.Body)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the budget was not enforced: the turn took %s", elapsed)
	}
	if !strings.Contains(rec.Body.String(), `"code":"timeout"`) {
		t.Fatalf("the error is not a timeout: %s", rec.Body)
	}

	day := st.Snapshot().Days[0]
	if day.Aborted != 1 {
		t.Fatalf("Aborted = %d, want 1", day.Aborted)
	}
	if day.OK != 0 {
		t.Fatalf("OK = %d, want 0: a silent provider is not a success", day.OK)
	}
}

// TestTheHeadersWaitIsNotTheSilenceBudget — the two budgets of an attempt are
// not interchangeable.
//
// A provider that reasons declares a generous silence budget between events, and
// a model that thinks for minutes is healthy. The wait before the headers is a
// different question: nothing has been said yet, and a connection that has
// produced nothing has gone quiet for as long as its own stage allows. Reading
// the silence budget there is what leaves the client waiting for minutes on a
// connection that is already dead.
func TestTheHeadersWaitIsNotTheSilenceBudget(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	h, p, _ := newTestHandler(t, upstream)
	// A thinking model: minutes of silence once it has started, and a short wait
	// before it has.
	p.build = slowBuild(upstream.URL+"/chat/completions", 150*time.Millisecond, 30*time.Second)

	start := time.Now()
	rec := postChat(t, h, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusGatewayTimeout, rec.Body)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the wait for the headers followed the silence budget: the turn took %s", elapsed)
	}
}

// TestStreamSilenceBetweenEventsIsCutOff — the silence budget applies between
// events, not only before the first one.
//
// A stream that stops in the middle is worse than one that never starts: the
// status line is already sent, so the client cannot be told anything — the read
// simply ends, and the client asks again. Counting that as a success would hide
// exactly the failure the monitor exists to show.
func TestStreamSilenceBetweenEventsIsCutOff(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	h, p, st := newTestHandler(t, upstream)
	p.build = slowBuild(upstream.URL+"/chat/completions", 5*time.Second, 150*time.Millisecond)

	start := time.Now()
	rec := postChat(t, h, `{"model":"test/model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the headers were already sent", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"content":"hi"`) {
		t.Fatalf("the event before the silence did not reach the client: %q", got)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the silence budget was not enforced: the turn took %s", elapsed)
	}

	day := st.Snapshot().Days[0]
	if day.Aborted != 1 {
		t.Fatalf("Aborted = %d, want 1: a stream cut short is not a success", day.Aborted)
	}
	if day.OK != 0 {
		t.Fatalf("OK = %d, want 0", day.OK)
	}
}

// slowBuild points the stub at an address and gives the request the budgets the
// test works with.
func slowBuild(url string, firstEvent, idle time.Duration) func(provider.Request) (provider.Prepared, error) {
	return func(provider.Request) (provider.Prepared, error) {
		return provider.Prepared{
			URL:               url,
			Headers:           http.Header{"content-type": []string{"application/json"}},
			Body:              []byte(`{"model":"model"}`),
			FirstEventTimeout: firstEvent,
			IdleTimeout:       idle,
		}, nil
	}
}
