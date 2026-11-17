package opencode

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"providermux/internal/provider"
)

// TestLaneRotatesOnlyAcrossAddresses — the identifier is replaced on a new
// address and nowhere else.
//
// The measurement behind the rule is in lane.go. What is asserted here is the
// rule itself: only a LIVE refusal on a DIFFERENT address retires an
// identifier; the same address, an expired refusal, and an unknown address all
// keep it. Rotating in those cases would send a second anonymous session from
// an address that is already spending its allowance, and buy nothing.
func TestLaneRotatesOnlyAcrossAddresses(t *testing.T) {
	const spent = "ses_0123456789ab0123456789abcd"

	cases := []struct {
		name    string
		address string
		spend   bool
		expire  bool
		want    bool
	}{
		{name: "no refusal at all", address: "198.51.100.7"},
		{name: "refused, same address", address: "198.51.100.7", spend: true},
		{name: "refused, other address", address: "203.0.113.9", spend: true, want: true},
		{name: "refused, window closed", address: "203.0.113.9", spend: true, expire: true},
		{name: "refused, address unknown now", address: "", spend: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLane()
			l.recordSend(spent, "198.51.100.7")
			if tc.spend {
				until := l.now().Add(time.Hour)
				if tc.expire {
					until = l.now().Add(-time.Second)
				}
				l.spend(spent, until)
			}

			got, replaced := l.freshSession(spent, tc.address)
			if replaced != tc.want {
				t.Fatalf("replaced = %v, want %v (got %q)", replaced, tc.want, got)
			}
			if !tc.want && got != spent {
				t.Fatalf("identifier changed without a reason: %q", got)
			}
			if tc.want {
				if got == spent || !isSessionID(got) {
					t.Fatalf("a replacement must be a new canonical identifier, got %q", got)
				}
			}
		})
	}
}

// TestLaneForgetsAClosedWindow — once the refusal expires, the entry stops
// standing in the way of the identifier it described.
func TestLaneForgetsAClosedWindow(t *testing.T) {
	const id = "ses_0123456789ab0123456789abcd"

	l := newTestLane()
	l.recordSend(id, "198.51.100.7")
	l.spend(id, l.now().Add(time.Minute))

	if _, pending := l.pending(id); !pending {
		t.Fatal("refusal must be pending inside its window")
	}

	l.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, pending := l.pending(id); pending {
		t.Fatal("refusal must be gone after its window")
	}
	if got, replaced := l.freshSession(id, "203.0.113.9"); replaced || got != id {
		t.Fatalf("an expired refusal must not retire the identifier, got %q", got)
	}
}

// TestRetryUntilReadsBothForms — the end of a refusal is taken from Retry-After,
// and from resetsAt when the answer carries only that.
func TestRetryUntilReadsBothForms(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	header := http.Header{}
	header.Set("Retry-After", "10066")
	until, ok := retryUntil(header, nil, now)
	if !ok || !until.Equal(now.Add(10066*time.Second)) {
		t.Fatalf("Retry-After: got %v %v", until, ok)
	}

	body := []byte(`{"type":"error","error":{"type":"FreeUsageLimitError","resetsAt":"2026-03-01T14:47:00Z"}}`)
	until, ok = retryUntil(http.Header{}, body, now)
	if !ok || !until.Equal(time.Date(2026, 3, 1, 14, 47, 0, 0, time.UTC)) {
		t.Fatalf("resetsAt: got %v %v", until, ok)
	}

	if _, ok := retryUntil(http.Header{}, []byte(`{"type":"error"}`), now); ok {
		t.Fatal("a refusal that names no end must not be recorded: it would hold the identifier forever")
	}
}

// TestSessionTravelsUnderANewNameAcrossAnAddress — the property the whole file
// exists for, asserted through Build and Classify rather than the ledger.
//
// A conversation refused on one address must leave with a different identifier
// once the gateway resolves another address, and must keep its identifier when
// the address does not change.
func TestSessionTravelsUnderANewNameAcrossAnAddress(t *testing.T) {
	p := newTestProvider(t)

	req := conversationRequest(t, "system", "first")
	req.Address = "198.51.100.7"

	first := buildSessionID(t, p, req)

	// The refusal of that request, as the service answers it.
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"10066"}},
		Body:       http.NoBody,
		Request:    &http.Request{Header: http.Header{"X-Opencode-Session-Id": []string{first}}},
	}
	body := []byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`)
	p.Classify(resp, body)

	if _, pending := p.lane.pending(first); !pending {
		t.Fatal("the refusal must be remembered against the identifier that carried it")
	}

	same := req
	if got := buildSessionID(t, p, same); got != first {
		t.Fatalf("same address must keep the identifier: %q -> %q", first, got)
	}

	moved := req
	moved.Address = "203.0.113.9"
	rotated := buildSessionID(t, p, moved)
	if rotated == first {
		t.Fatal("a new address must retire the refused identifier")
	}
	if !isSessionID(rotated) {
		t.Fatalf("the replacement is not a canonical identifier: %q", rotated)
	}

	// And the conversation keeps the replacement on that address.
	if gained := buildSessionID(t, p, moved); gained != rotated {
		t.Fatalf("the replacement must be stable: %q -> %q", rotated, gained)
	}
}

// TestNonQuotaAnswerDoesNotRetireTheIdentifier — a 400 that mentions a limit, or
// a 403 about the address, is not a spent allowance and must leave the
// identifier alone.
func TestNonQuotaAnswerDoesNotRetireTheIdentifier(t *testing.T) {
	p := newTestProvider(t)

	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError} {
		header := http.Header{"Retry-After": []string{"10066"}}
		resp := &http.Response{
			StatusCode: status,
			Header:     header,
			Body:       http.NoBody,
			Request:    &http.Request{Header: http.Header{"X-Opencode-Session-Id": []string{"ses_0123456789ab0123456789abcd"}}},
		}
		p.Classify(resp, []byte(`{"error":{"message":"free tier limit exhausted"}}`))
		if _, pending := p.lane.pending("ses_0123456789ab0123456789abcd"); pending {
			t.Fatalf("status %d must not mark the identifier as spent", status)
		}
	}
}

// newTestLane creates a ledger whose clock the test controls.
func newTestLane() *laneLedger {
	l := newLaneLedger(8)
	l.now = func() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }
	return l
}

// buildSessionID returns the identifier Build puts into the headers.
func buildSessionID(t *testing.T, p *Provider, req provider.Request) string {
	t.Helper()
	prep, err := p.Build(req)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	id := prep.Headers.Get("X-Opencode-Session-Id")
	if id == "" {
		t.Fatalf("no session identifier in %v", prep.Headers)
	}
	if strings.Contains(id, " ") {
		t.Fatalf("malformed identifier %q", id)
	}
	return id
}
