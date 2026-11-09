package antigravity

import (
	"testing"
	"time"
)

// TestTheAccessTokenIsMintedOnceAndReused — the exchange is a network call on the
// request path, and a token lives for an hour: minting one per request would add a
// round trip to every answer.
func TestTheAccessTokenIsMintedOnceAndReused(t *testing.T) {
	p := newTestProvider(t)

	exchanges := 0
	p.tokens.exchange = func(clientID, clientSecret, refresh string) (string, time.Duration, error) {
		exchanges++
		return "access-" + refresh, time.Hour, nil
	}

	if got := p.Credential("refresh-a"); got != "access-refresh-a" {
		t.Fatalf("Credential = %q", got)
	}
	if got := p.Credential("refresh-a"); got != "access-refresh-a" {
		t.Fatalf("Credential (second call) = %q", got)
	}
	if exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", exchanges)
	}

	// Another refresh token is another account, and its own entry.
	if got := p.Credential("refresh-b"); got != "access-refresh-b" {
		t.Fatalf("Credential (other key) = %q", got)
	}
	if exchanges != 2 {
		t.Fatalf("exchanges = %d, want 2", exchanges)
	}
}

// TestAnExpiringTokenIsMintedAgain — a token inside the skew window is replaced
// before it is used, not after the service refuses it.
func TestAnExpiringTokenIsMintedAgain(t *testing.T) {
	p := newTestProvider(t)

	exchanges := 0
	p.tokens.exchange = func(clientID, clientSecret, refresh string) (string, time.Duration, error) {
		exchanges++
		return "access", 10 * time.Second, nil
	}

	p.Credential("refresh")
	p.Credential("refresh")
	if exchanges != 2 {
		t.Fatalf("exchanges = %d, want a second one inside the skew", exchanges)
	}
}

// TestAFailedExchangeSendsNoToken — an anonymous request is honest about what
// happened; a request carrying a token known to be dead merely looks the same.
func TestAFailedExchangeSendsNoToken(t *testing.T) {
	p := newTestProvider(t)

	p.tokens.exchange = func(clientID, clientSecret, refresh string) (string, time.Duration, error) {
		return "", 0, errTestExchange
	}

	if got := p.Credential("refresh"); got != "" {
		t.Fatalf("Credential = %q, want an empty value", got)
	}
	if got := p.Credential(""); got != "" {
		t.Fatalf("Credential(\"\") = %q, want an empty value", got)
	}
}

// errTestExchange is the failure a test imposes on the exchange.
var errTestExchange = &exchangeError{}

type exchangeError struct{}

func (*exchangeError) Error() string { return "the token server refused" }
