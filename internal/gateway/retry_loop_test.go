package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"providermux/internal/provider"
)

// quotaBody is the refusal the free lane answers with when the address's
// allowance is spent. It carries no Retry-After and no reset time: the answer is
// another address, not a wait.
const quotaBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`

// quotaProvider is a stand-in that classifies 429 the way the opencode provider
// does — as a spent allowance rather than a rejected key.
func quotaProvider(t *testing.T, upstream *httptest.Server) *stubProvider {
	t.Helper()
	return &stubProvider{
		id:     "test",
		models: []provider.Model{{ID: "model", Context: 1000, MaxOutput: 100, Free: true}},
		build: func(provider.Request) (provider.Prepared, error) {
			return provider.Prepared{
				URL:     upstream.URL + "/chat/completions",
				Headers: http.Header{"content-type": []string{"application/json"}},
				Body:    []byte(`{"model":"model"}`),
			}, nil
		},
		classif: func(resp *http.Response, body []byte) provider.Failure {
			f := provider.Failure{Status: resp.StatusCode, Body: body, Headers: resp.Header}
			if resp.StatusCode == http.StatusTooManyRequests {
				f.QuotaRejected = true
			}
			if resp.StatusCode == http.StatusUnauthorized {
				f.KeyRejected = true
			}
			return f
		},
	}
}

// streamHandler answers with a complete one-event stream.
func streamHandler(w http.ResponseWriter) {
	w.Header().Set("content-type", "text/event-stream")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

const chatRequest = `{"model":"test/model","messages":[{"role":"user","content":"hello"}],"stream":true}`

// switchingAddress is an address that changes when the exit does.
//
// The gateway reads the address twice per rotation: once to label the refusal and
// once to decide whether the new exit is really another one. A constant would
// model a proxy whose nodes all share an address — which is a case of its own.
type switchingAddress struct {
	mu   sync.Mutex
	addr string
}

func (s *switchingAddress) Address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Refresh is what a real resolver does: re-read the address after the exit moved.
// Here the move is made by the Rotate stub, so there is nothing to do.
func (s *switchingAddress) Refresh(context.Context) {}

func (s *switchingAddress) set(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addr = addr
}

// TestATransportFailureIsRepeatedFromTheSameExit — a connection that dies before
// the provider answers is the one failure a repeat answers unconditionally:
// nothing was spent, and nothing reached the client.
func TestATransportFailureIsRepeatedFromTheSameExit(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server cannot hijack")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		streamHandler(w)
	}))
	defer upstream.Close()

	h, _, _ := newTestHandler(t, upstream)
	h.Retry = RetryPolicy{Attempts: 2, Delay: time.Millisecond}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2", got)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("the client did not get the answer: %q", rec.Body.String())
	}
}

// TestAQuotaRefusalIsRepeatedFromAnotherExit — the refusal that motivated the
// policy: the allowance is spent per address, so the gateway leaves by another
// one and repeats the same request.
func TestAQuotaRefusalIsRepeatedFromAnotherExit(t *testing.T) {
	var hits, rotations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, quotaBody)
			return
		}
		streamHandler(w)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	resetStub := quotaProvider(t, upstream)
	p.classif = resetStub.classif
	h.Retry = RetryPolicy{Attempts: 2, Delay: time.Millisecond}
	addr := &switchingAddress{addr: "203.0.113.7"}
	h.Addresses = addr
	h.Rotate = func(context.Context) (string, error) {
		rotations.Add(1)
		addr.set("198.51.100.9")
		return "some-other-node", nil
	}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2", got)
	}
	if got := rotations.Load(); got != 1 {
		t.Fatalf("rotations = %d, want 1", got)
	}
}

// TestAQuotaRefusalWithoutRotationReachesTheClient — a deployment that cannot
// change its exit must not repeat the request: the counter does not move, and a
// repeat only spends traffic. The client sees the refusal exactly as issued.
func TestAQuotaRefusalWithoutRotationReachesTheClient(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaBody)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	h.Addresses = fixedAddress("203.0.113.7")

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1: the refusal was repeated without a rotation", got)
	}
	if rec.Body.String() != quotaBody {
		t.Fatalf("the refusal was repackaged: %q", rec.Body.String())
	}
}

// TestOneAttemptIsTheDefault — without an explicit policy one client request is
// one provider request.
func TestOneAttemptIsTheDefault(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hijacker, _ := w.(http.Hijacker)
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()

	h, _, _ := newTestHandler(t, upstream)

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// TestTheQuotaRepeatWaitsForTheExit — a new exit is not the whole answer.
//
// Measured: one address refused a dozen requests in two minutes and answered
// the same address again a minute later, so the free lane also counts a burst.
// The repeat therefore waits before it leaves, and the wait is the client's: it
// ends the moment the client does, without spending a second provider request.
func TestTheQuotaRepeatWaitsForTheExit(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaBody)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 2, Delay: 30 * time.Second}
	addr := &switchingAddress{addr: "203.0.113.7"}
	h.Addresses = addr
	h.Rotate = func(context.Context) (string, error) {
		addr.set("198.51.100.9")
		return "some-other-node", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatRequest))
	h.handleChat(httptest.NewRecorder(), req.WithContext(ctx))

	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1: the repeat did not wait for the exit", got)
	}
}

// TestAQuotaOnTheLastAttemptIsPassedThrough — the budget is spent on the last
// attempt, and then the refusal the client gets is the provider's own, unchanged.
func TestAQuotaOnTheLastAttemptIsPassedThrough(t *testing.T) {
	var hits, rotations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaBody)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 2, Delay: time.Millisecond}
	addr := &switchingAddress{addr: "203.0.113.7"}
	h.Addresses = addr
	h.Rotate = func(context.Context) (string, error) {
		rotations.Add(1)
		addr.set("198.51.100.9")
		return "node", nil
	}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2", got)
	}
	if got := rotations.Load(); got != 1 {
		t.Fatalf("rotations = %d, want 1: the last attempt must not rotate", got)
	}
	if rec.Body.String() != quotaBody {
		t.Fatalf("the refusal was repackaged: %q", rec.Body.String())
	}
}

// TestAKeyRefusalIsNotRepeated — a rejected key is answered by another key, not
// by another address, and no policy in this file does that. Repeating it here
// would spend the whole pool on one bad credential.
func TestAKeyRefusalIsNotRepeated(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"AuthenticationError","message":"bad key"}}`)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	h.Addresses = fixedAddress("203.0.113.7")
	h.Rotate = func(context.Context) (string, error) {
		t.Error("a key refusal must not rotate the exit")
		return "", nil
	}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// TestANodeSharingTheSpentAddressIsNotAnAnswer — MEASURED, not imagined.
//
// Three consecutive nodes of one subscription were found to exit from
// 72.56.82.193 (2026-11-19). Selecting a node therefore proves nothing about the
// address, and a repeat from the same address with a new node name is a repeat
// that cannot succeed: the gateway asks for another node while the exit has not
// moved, and once the budget is spent the refusal reaches the client unchanged.
func TestANodeSharingTheSpentAddressIsNotAnAnswer(t *testing.T) {
	var hits, rotations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaBody)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	h.Addresses = fixedAddress("72.56.82.193")
	h.Rotate = func(context.Context) (string, error) {
		rotations.Add(1)
		return "another-name", nil
	}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rotations.Load(); got != rotationTries {
		t.Fatalf("rotations = %d, want the bounded %d", got, rotationTries)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1: the same address was not asked again", got)
	}
	if rec.Body.String() != quotaBody {
		t.Fatalf("the refusal was repackaged: %q", rec.Body.String())
	}
}

// TestTheExitReturnsToItsDefaultAfterAnAnswer — a rotation answers one refusal.
// Once the answer has arrived, the exit goes back to its default: leaving it on
// the node a refusal pushed it to is what kept a spent exit in use.
func TestTheExitReturnsToItsDefaultAfterAnAnswer(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, quotaBody)
			return
		}
		streamHandler(w)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 2, Delay: time.Millisecond}
	addr := &switchingAddress{addr: "203.0.113.7"}
	h.Addresses = addr
	h.Rotate = func(context.Context) (string, error) {
		addr.set("198.51.100.9")
		return "some-other-node", nil
	}
	settled := make(chan struct{}, 1)
	h.Settle = func(context.Context) error {
		settled <- struct{}{}
		return nil
	}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-settled:
	case <-time.After(2 * time.Second):
		t.Fatal("the exit did not return to its default after the answer")
	}
}

// TestTheExitStaysWhereItWasWithoutARotation — the return is a consequence of a
// rotation, not housekeeping: an answer from the default exit must not move it.
func TestTheExitStaysWhereItWasWithoutARotation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamHandler(w)
	}))
	defer upstream.Close()

	h, _, _ := newTestHandler(t, upstream)
	settled := make(chan struct{}, 1)
	h.Settle = func(context.Context) error {
		settled <- struct{}{}
		return nil
	}

	if rec := postChat(t, h, chatRequest); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	select {
	case <-settled:
		t.Fatal("the exit returned to its default without a rotation")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestAProviderOutsideTheRotatedTransportKeepsTheRefusal — a deployment may send
// one provider through the tunnel and another through the host's own routing. The
// selector stands behind one of them, so a refusal from the other one cannot be
// answered by moving it: the refusal goes to the client as issued, and no
// rotation is spent on a request that would leave from the same address again.
func TestAProviderOutsideTheRotatedTransportKeepsTheRefusal(t *testing.T) {
	var hits, rotations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaBody)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	h.Addresses = fixedAddress("203.0.113.7")
	h.Rotate = func(context.Context) (string, error) {
		rotations.Add(1)
		return "some-other-node", nil
	}
	h.RotateFor = func(provider.ID) bool { return false }

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1: the refusal was repeated for a provider the rotation does not move", got)
	}
	if got := rotations.Load(); got != 0 {
		t.Fatalf("rotations = %d, want 0", got)
	}
	if rec.Body.String() != quotaBody {
		t.Fatalf("the refusal was repackaged: %q", rec.Body.String())
	}
}

// TestTheAddressFollowsTheProviderPath — the address that decides whether a
// rotation worked is the address the provider's own traffic leaves from. A
// resolver built on the deployment's transport describes a different path, and
// comparing the two would either miss a real move or invent one.
func TestTheAddressFollowsTheProviderPath(t *testing.T) {
	var hits, rotations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, quotaBody)
			return
		}
		streamHandler(w)
	}))
	defer upstream.Close()

	h, p, _ := newTestHandler(t, upstream)
	p.classif = quotaProvider(t, upstream).classif
	h.Retry = RetryPolicy{Attempts: 2, Delay: time.Millisecond}

	// The deployment's own path never moves; the provider's path does.
	h.Addresses = fixedAddress("203.0.113.7")
	providerPath := &switchingAddress{addr: "203.0.113.7"}
	h.AddressesFor = func(provider.ID) AddressResolver { return providerPath }
	h.Rotate = func(context.Context) (string, error) {
		rotations.Add(1)
		providerPath.set("198.51.100.9")
		return "some-other-node", nil
	}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits = %d, want 2: the moved provider path was not seen", got)
	}
	if got := rotations.Load(); got != 1 {
		t.Fatalf("rotations = %d, want 1", got)
	}
}
