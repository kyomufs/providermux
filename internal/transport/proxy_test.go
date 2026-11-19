package transport

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// connectProxy is an HTTP CONNECT proxy that records how many tunnels it was
// asked to open.
func connectProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var tunnels atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
			return
		}
		tunnels.Add(1)

		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			upstream.Close()
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			defer client.Close()
			defer upstream.Close()
			_, _ = io.Copy(upstream, client)
		}()
		_, _ = io.Copy(client, upstream)
		_ = upstream.Close()
	}))
	t.Cleanup(srv.Close)
	return srv, &tunnels
}

// TestProxyTunnelsTheRequest — the request reaches the target through the proxy,
// and the tunnel is reused rather than opened per request.
//
// Reuse is the reason the transport exists at all: an agent's turn is many
// requests, and a CONNECT per request would add a handshake to every one of
// them on top of the proxy's own work.
func TestProxyTunnelsTheRequest(t *testing.T) {
	proxy, tunnels := connectProxy(t)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain")
		_, _ = io.WriteString(w, "through the tunnel")
	}))
	defer target.Close()

	tr, err := NewProxy(map[string]any{"url": proxy.URL})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	defer tr.Stop()
	rt, err := tr.RoundTripper()
	if err != nil {
		t.Fatalf("RoundTripper: %v", err)
	}

	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodGet, target.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("request %d: reading the answer: %v", i, err)
		}
		if string(body) != "through the tunnel" {
			t.Fatalf("request %d: body = %q", i, body)
		}
	}

	if got := tunnels.Load(); got != 1 {
		t.Fatalf("tunnels opened = %d, want 1: the connection was not reused", got)
	}
}

// TestProxyRefusalIsAnError — a proxy that will not open the tunnel must fail
// the request rather than hang or fall back to a direct connection.
//
// A silent fallback would be the worst outcome: the request would leave from the
// host's own address, spend the quota this transport exists to avoid, and report
// success.
func TestProxyRefusalIsAnError(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer proxy.Close()

	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()

	tr, err := NewProxy(map[string]any{"url": proxy.URL})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	defer tr.Stop()
	rt, _ := tr.RoundTripper()

	req, _ := http.NewRequest(http.MethodGet, target.URL, nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("the refusal was not reported")
	} else if !strings.Contains(err.Error(), "CONNECT") {
		t.Fatalf("the error does not name the CONNECT: %v", err)
	}
}

// TestProxySettingsAreValidated — a transport that cannot work is refused at
// build time, where the message reaches the operator's terminal.
func TestProxySettingsAreValidated(t *testing.T) {
	cases := map[string]map[string]any{
		"no url":      {},
		"not a url":   {"url": "://nope"},
		"no host":     {"url": "http://"},
		"socks5 only": {"url": "socks5://127.0.0.1:1080"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProxy(cfg); err == nil {
				t.Fatal("the settings were accepted, want a refusal")
			}
		})
	}
}

// TestProxyIsRegistered: the entry point builds transports by identifier, so a
// transport that is not in the default registry cannot be selected from the
// configuration at all.
func TestProxyIsRegistered(t *testing.T) {
	r := NewRegistryWithDefaults()
	for _, info := range r.Infos() {
		if info.ID == Proxy {
			return
		}
	}
	t.Fatalf("transport %q is not in the default registry", Proxy)
}
