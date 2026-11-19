package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"providermux/internal/egress"
	"providermux/internal/transport"
)

type warnLogger struct{ t *testing.T }

func (l warnLogger) Warn(format string, args ...any) { l.t.Logf(format, args...) }

// TestLiveQuotaRepeatRotatesTheExitAndComesHome runs the whole quota path
// against the real deployment: the real proxy controller, the real core behind
// it, and the real address observation through that core.
//
// Only the provider is faked, and only in the one place it cannot be faked
// honestly: a real 429 spends the operator's free allowance and arrives when it
// wants to. The refusal here is the exact body and type the free lane sends.
//
// What it proves, in order: the refusal is classified as quota, the exit is
// asked for another node, the new address is observed through the path the
// requests take, the request is repeated, the answer reaches the client, and the
// exit returns to its default afterwards.
//
//	PMUX_LIVE_CONTROLLER=http://127.0.0.1:9091 \
//	PMUX_LIVE_GROUP=egress \
//	PMUX_LIVE_PROXY=http://127.0.0.1:7891 \
//	go test ./internal/gateway -run Live -v
func TestLiveQuotaRepeatRotatesTheExitAndComesHome(t *testing.T) {
	controller := os.Getenv("PMUX_LIVE_CONTROLLER")
	if controller == "" {
		t.Skip("set PMUX_LIVE_CONTROLLER (and PMUX_LIVE_GROUP) to run against a real proxy")
	}
	group := os.Getenv("PMUX_LIVE_GROUP")
	if group == "" {
		t.Fatal("PMUX_LIVE_GROUP is required with PMUX_LIVE_CONTROLLER")
	}
	proxyURL := os.Getenv("PMUX_LIVE_PROXY")
	if proxyURL == "" {
		proxyURL = "http://127.0.0.1:7891"
	}

	rotator, err := egress.NewRotator(egress.RotatorConfig{
		Controller: controller,
		Group:      group,
		ProbeURL:   egress.DefaultURL,
		ProbeCount: 6,
	})
	if err != nil {
		t.Fatalf("rotator: %v", err)
	}

	// The address is observed through the proxy the requests leave from, which
	// is the point: the host's own address does not move when the node does, and
	// the repeat would then be refused for the address it just left.
	proxied, err := transport.NewProxy(map[string]any{"url": proxyURL})
	if err != nil {
		t.Fatalf("proxy transport: %v", err)
	}
	rt, err := proxied.RoundTripper()
	if err != nil {
		t.Fatalf("proxy round tripper: %v", err)
	}
	resolver := egress.New(egress.Config{
		Families:  []string{egress.FamilyV4},
		Transport: func(string) (http.RoundTripper, error) { return rt, nil },
		Timeout:   20 * time.Second,
		Log:       warnLogger{t},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := resolver.Start(ctx); err != nil {
		t.Fatalf("resolver: %v", err)
	}
	defer resolver.Stop()
	spent := resolver.Address()
	if spent == "" {
		t.Fatal("the proxy exit address was not observed")
	}

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
	h.Addresses = resolver
	h.Rotate = func(ctx context.Context) (string, error) {
		name, err := rotator.Rotate(ctx)
		if err != nil {
			return "", err
		}
		proxied.(interface{ CloseIdleConnections() }).CloseIdleConnections()
		return name, nil
	}
	settled := make(chan error, 1)
	h.Settle = func(ctx context.Context) error {
		err := rotator.Settle(ctx)
		settled <- err
		return err
	}

	rec := postChat(t, h, chatRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if hits.Load() < 2 {
		t.Fatalf("upstream hits = %d, want at least 2", hits.Load())
	}
	if next := resolver.Address(); next == spent {
		t.Fatalf("the exit address did not move: %s", spent)
	}
	select {
	case err := <-settled:
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the exit did not return to its default")
	}
}
