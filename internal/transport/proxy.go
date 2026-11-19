package transport

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// proxy implements the transport that tunnels through a local HTTP proxy.
//
// WHY THIS EXISTS. The free lane counts its quota against the source address,
// and the only way to obtain a second budget is to leave from a different
// address. A local proxy that owns a pool of upstream nodes is that mechanism;
// the direct transport cannot reach any of them, because a transparent tunnel
// presents one exit and its rotation is outside the daemon's control.
//
// WHY THE CONNECT IS WRITTEN BY HAND. It would be shorter to set Proxy on an
// http.Transport, but that path loses the TLS client. Go's transport uses
// DialTLSContext only for non-proxied requests; behind a proxy it falls back to
// crypto/tls, and the provider answers that client with a 429 no matter how the
// headers are set (see the measurement in ClientTransport). The tunnel is
// therefore established by dial and the TLS handshake stays ours, so the
// request that reaches the provider is the same one the direct transport sends.
type proxy struct {
	rt *http.Transport
}

// ProxyConfig are the settings of the proxy transport.
type ProxyConfig struct {
	// URL is the proxy's address, e.g. http://127.0.0.1:7891.
	//
	// Only HTTP CONNECT is implemented. A mixed-port listener of mihomo speaks
	// it, and a SOCKS5 dialer would be a second mechanism with its own failure
	// modes for no gain: the exit is chosen by the proxy, not by the protocol
	// the daemon speaks to it.
	URL string
	// User and Password are the proxy's credentials, if it has any.
	User     string
	Password string
	// Family pins the address family of the connection TO THE PROXY.
	Family string
}

// NewProxy creates the proxy transport.
func NewProxy(cfg map[string]any) (Transport, error) {
	var parsed ProxyConfig
	if raw, ok := cfg["url"]; ok {
		if s, ok := raw.(string); ok {
			parsed.URL = strings.TrimSpace(s)
		}
	}
	if raw, ok := cfg["user"]; ok {
		if s, ok := raw.(string); ok {
			parsed.User = s
		}
	}
	if raw, ok := cfg["password"]; ok {
		if s, ok := raw.(string); ok {
			parsed.Password = s
		}
	}
	if raw, ok := cfg["family"]; ok {
		if s, ok := raw.(string); ok {
			parsed.Family = s
		}
	}
	u, err := url.Parse(parsed.URL)
	if err != nil {
		return nil, fmt.Errorf("transport proxy: invalid url %q: %w", parsed.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("transport proxy: url %q: only http and https proxies are supported", parsed.URL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("transport proxy: url %q has no host", parsed.URL)
	}
	if parsed.User == "" && u.User != nil {
		parsed.User = u.User.Username()
		parsed.Password, _ = u.User.Password()
	}
	return &proxy{rt: ClientTransport(tunnelDialer(u.Host, parsed.User, parsed.Password, parsed.Family))}, nil
}

// tunnelDialer returns a dialer that connects through the proxy at proxyAddr.
//
// The returned connection is the tunnel itself: whatever the caller writes
// afterwards goes to the target, and the TLS handshake that follows is the
// caller's own. That split is the point — the handshake must stay the one the
// provider accepts.
func tunnelDialer(proxyAddr, user, password, family string) Dialer {
	base := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAliveInterval}
	network := networkFor(family)

	var auth string
	if user != "" {
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	}

	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		dialNetwork := network
		if dialNetwork == "" {
			dialNetwork = "tcp"
		}
		conn, err := base.DialContext(ctx, dialNetwork, proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("proxy %s: %w", proxyAddr, err)
		}

		// The handshake and the first response share the connection, and the
		// response may be followed by the first bytes of the target's own
		// stream. A reader that is thrown away would swallow them, so the
		// reader is kept and the connection reports through it.
		deadline := time.Now().Add(tlsHandshakeTimeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = conn.SetDeadline(deadline)

		var request strings.Builder
		request.WriteString("CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n")
		if auth != "" {
			request.WriteString("Proxy-Authorization: " + auth + "\r\n")
		}
		request.WriteString("\r\n")
		if _, err := io.WriteString(conn, request.String()); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("proxy %s: sending CONNECT: %w", proxyAddr, err)
		}

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("proxy %s: reading CONNECT answer: %w", proxyAddr, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = conn.Close()
			return nil, fmt.Errorf("proxy %s: CONNECT %s: %s", proxyAddr, addr, resp.Status)
		}
		_ = conn.SetDeadline(time.Time{})

		if br.Buffered() == 0 {
			return conn, nil
		}
		return &bufferedConn{Conn: conn, r: br}, nil
	}
}

// bufferedConn keeps the bytes the CONNECT reader already took off the socket.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ID implements Transport.
func (p *proxy) ID() ID { return Proxy }

// RoundTripper implements Transport.
func (p *proxy) RoundTripper() (http.RoundTripper, error) { return p.rt, nil }

// Start implements Transport. The proxy is a separate process that the daemon
// does not own; whether it is up is answered by the first request, and refusing
// to start when it is momentarily down would make the gateway less available
// than the proxy it depends on.
func (p *proxy) Start(context.Context) error { return nil }

// Stop implements Transport.
func (p *proxy) Stop() error {
	p.rt.CloseIdleConnections()
	return nil
}

// CloseIdleConnections drops the tunnels and keeps the transport in service.
//
// A tunnel is pinned to the node that carried it: after the rotation the pool
// still holds tunnels through the address the rotation was meant to leave.
// Dropping them is what makes the next attempt actually come out of the new node.
func (p *proxy) CloseIdleConnections() { p.rt.CloseIdleConnections() }

// Info implements Transport.
func (p *proxy) Info() Info {
	return Info{
		ID:      Proxy,
		Summary: "tunnelled through a local HTTP proxy, so the exit address can be chosen",
		// The proxy is another process; a deployment that names this transport
		// does not work without it, and an interface should be able to say so.
		NeedsExternalProcess: true,
	}
}
