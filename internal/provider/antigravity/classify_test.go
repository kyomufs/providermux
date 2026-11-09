package antigravity

import (
	"net/http"
	"testing"

	"free-ai-router/internal/provider"
)

// TestTheVerdictOfARefusal — which lever the gateway should pull.
//
// The questions are separate: is the ACCOUNT the cause (another refresh token
// helps), is the ADDRESS the cause (only another uplink helps), and is a repeat
// worth the client's time.
func TestTheVerdictOfARefusal(t *testing.T) {
	p := newTestProvider(t)

	cases := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		key        bool
		retry      bool
		address    bool
		unavail    bool
	}{
		{
			name:   "the token is no longer accepted",
			status: http.StatusUnauthorized,
			body:   `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`,
			key:    true,
		},
		{
			name:   "the capacity of the account is spent",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`,
			key:    true,
		},
		{
			name:   "the account may not call the service",
			status: http.StatusForbidden,
			body:   `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`,
			key:    true,
		},
		{
			name:    "the address is not served here",
			status:  http.StatusForbidden,
			body:    `{"error":{"code":403,"message":"User location is not supported for the API use.","status":"PERMISSION_DENIED"}}`,
			address: true,
		},
		{
			name:    "the backend behind the edge is out",
			status:  http.StatusServiceUnavailable,
			body:    `{"error":{"code":503,"message":"The service is currently unavailable.","status":"UNAVAILABLE"}}`,
			retry:   true,
			unavail: true,
		},
		{
			name:       "a burst limit names its delay",
			status:     http.StatusServiceUnavailable,
			body:       `{"error":{"code":503,"message":"try later","status":"UNAVAILABLE"}}`,
			retryAfter: "7",
			retry:      true,
			unavail:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			if tc.retryAfter != "" {
				resp.Header.Set("retry-after", tc.retryAfter)
			}
			f := p.Classify(resp, []byte(tc.body))

			if f.KeyRejected != tc.key {
				t.Fatalf("KeyRejected = %v, want %v", f.KeyRejected, tc.key)
			}
			if f.Retryable != tc.retry {
				t.Fatalf("Retryable = %v, want %v", f.Retryable, tc.retry)
			}
			if f.AddressRejected != tc.address {
				t.Fatalf("AddressRejected = %v, want %v", f.AddressRejected, tc.address)
			}
			if f.UpstreamUnavailable != tc.unavail {
				t.Fatalf("UpstreamUnavailable = %v, want %v", f.UpstreamUnavailable, tc.unavail)
			}
			if tc.retryAfter != "" && f.RetryAfter.Seconds() != 7 {
				t.Fatalf("RetryAfter = %s, want 7s", f.RetryAfter)
			}
		})
	}
}

// TestARefusalIsHandedOverWhole — the gateway forwards the body verbatim, so the
// classification must not touch it.
func TestARefusalIsHandedOverWhole(t *testing.T) {
	p := newTestProvider(t)
	body := []byte(`{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`)

	f := p.Classify(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, body)
	if f.Status != http.StatusTooManyRequests {
		t.Fatalf("Status = %d", f.Status)
	}
	if string(f.Body) != string(body) {
		t.Fatalf("Body = %s, want the upstream body", f.Body)
	}
	if f.Headers == nil {
		t.Fatal("Headers = nil: the client would lose the headers of the refusal")
	}
}

// compile-time check that the provider is what the gateway expects.
var _ provider.Provider = (*Provider)(nil)
var _ provider.CredentialScheme = (*Provider)(nil)
var _ provider.Transcoder = (*Provider)(nil)
