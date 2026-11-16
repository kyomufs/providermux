package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"providermux/internal/provider"
)

// TestStreamFlagFollowsTheClient — the shape of the answer is the client's.
//
// A client that sent stream: false and receives an event stream is being lied to
// about the content type of its own response, and every non-streaming parser
// fails on the body. The absent field means the protocol's default, which is a
// single response.
func TestStreamFlagFollowsTheClient(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"asked for a stream", `{"model":"test/model","messages":[],"stream":true}`, true},
		{"asked for a single response", `{"model":"test/model","messages":[],"stream":false}`, false},
		{"did not say", `{"model":"test/model","messages":[]}`, false},
		{"said something else", `{"model":"test/model","messages":[],"stream":"true"}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()

			h, p, _ := newTestHandler(t, upstream)

			var seen bool
			base := p.build
			p.build = func(req provider.Request) (provider.Prepared, error) {
				seen = req.Stream
				return base(req)
			}

			postChat(t, h, tc.body)

			if seen != tc.want {
				t.Fatalf("the provider was asked for stream = %t, want %t", seen, tc.want)
			}
		})
	}
}

// TestLocalProviderIsNotDialed — a provider that answers itself is not reached
// over the network.
//
// The stub builds an address that does not resolve precisely so that this is
// visible: if the local transport were not honoured, the request would fail with
// a name-resolution error instead of being answered.
func TestLocalProviderIsNotDialed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer upstream.Close()

	h, p, st := newTestHandler(t, upstream)
	p.build = func(provider.Request) (provider.Prepared, error) {
		return provider.Prepared{
			URL:     "http://stub.invalid/chat/completions",
			Headers: http.Header{"content-type": []string{"application/json"}},
			Body:    []byte(`{}`),
		}, nil
	}
	h.Providers.Register(localProvider{id: "local", inner: p})
	h.DefaultProvider = "local"

	rec := postChat(t, h, `{"model":"local/model","messages":[]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the local transport was not used", rec.Code)
	}
	if snap := st.Snapshot(); snap.Days[0].OK != 1 {
		t.Fatalf("the local answer was not counted: %+v", snap.Days[0])
	}
}

// localProvider is a provider that offers its own transport.
type localProvider struct {
	id    provider.ID
	inner provider.Provider
}

func (l localProvider) ID() provider.ID { return l.id }

func (l localProvider) Models(ctx context.Context) ([]provider.Model, error) {
	return l.inner.Models(ctx)
}

func (l localProvider) Build(req provider.Request) (provider.Prepared, error) {
	return l.inner.Build(req)
}

func (l localProvider) Classify(resp *http.Response, body []byte) provider.Failure {
	return l.inner.Classify(resp, body)
}

// RoundTripper implements provider.Local. It answers without a socket.
func (l localProvider) RoundTripper() http.RoundTripper { return localTransport{} }

type localTransport struct{}

func (localTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"content-type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n")),
	}, nil
}
