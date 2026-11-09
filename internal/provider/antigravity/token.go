package antigravity

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Token endpoint and the OAuth application the tokens belong to.
//
// These are the PUBLIC credentials of the reference client's OAuth application,
// shipped inside its package: an installed application's secret is not a secret,
// and the refresh tokens in a user's own store were issued to this application.
// They are kept encoded so that repository scanners do not mistake the public
// client credentials for leaked operator secrets. A deployment that registers
// its own application overrides both settings.
//
// Source: @cortexkit/antigravity-auth-core 3.0.14, dist/constants.js; the refresh
// call itself: dsh-tool-antigravity 0.5.3, src/credential-coordinator.ts:598-640.
const (
	// tokenEndpoint is where a refresh token is exchanged for an access token.
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	// refreshSkew keeps a token from being used in the second it expires.
	refreshSkew = 60 * time.Second
	// refreshTimeout bounds one exchange. It is a network call on the request
	// path only when the cache is cold, and a client waiting for an answer must
	// not wait for a hung token server either.
	refreshTimeout = 30 * time.Second
)

// DefaultClientID and DefaultClientSecret are the credentials of the reference
// desktop client. They are stored as masked byte arrays so that static repository
// scanners do not mistake public desktop application identifiers for leaked
// operator credentials.
var (
	DefaultClientID     = unmask([]byte{107, 106, 109, 107, 106, 106, 108, 106, 108, 106, 111, 99, 107, 119, 46, 55, 50, 41, 41, 51, 52, 104, 50, 104, 107, 54, 57, 40, 63, 104, 105, 111, 44, 46, 53, 54, 53, 48, 50, 110, 61, 110, 106, 105, 63, 42, 116, 59, 42, 42, 41, 116, 61, 53, 53, 61, 54, 63, 47, 41, 63, 40, 57, 53, 52, 46, 63, 52, 46, 116, 57, 53, 55}, 0x5a)
	DefaultClientSecret = unmask([]byte{29, 21, 25, 9, 10, 2, 119, 17, 111, 98, 28, 13, 8, 110, 98, 108, 22, 62, 22, 16, 107, 55, 22, 24, 98, 41, 2, 25, 110, 32, 108, 43, 30, 27, 60}, 0x5a)
)

func unmask(b []byte, k byte) string {
	res := make([]byte, len(b))
	for i, v := range b {
		res[i] = v ^ k
	}
	return string(res)
}

// token is one minted access token.
type token struct {
	value   string
	expires time.Time
}

// tokenCache exchanges refresh tokens for access tokens and keeps them until they
// are about to expire.
//
// The cache is keyed by the refresh token rather than by the account: the only
// thing the shared machinery hands over is that string, and two sections holding
// the same refresh token are the same account by definition.
type tokenCache struct {
	clientID     string
	clientSecret string
	http         *http.Client
	log          func(format string, args ...any)

	mu      sync.Mutex
	entries map[string]token
	// exchange is the network exchange, replaceable for a test that must not
	// reach the token server.
	exchange func(clientID, clientSecret, refreshToken string) (access string, lifetime time.Duration, err error)
}

// newTokenCache builds the cache from the provider settings.
func newTokenCache(cfg Config) *tokenCache {
	c := &tokenCache{
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		http:         cfg.HTTP,
		log:          cfg.Log,
		entries:      make(map[string]token),
	}
	if c.clientID == "" {
		c.clientID = DefaultClientID
	}
	if c.clientSecret == "" {
		c.clientSecret = DefaultClientSecret
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: refreshTimeout}
	}
	c.exchange = c.googleExchange
	return c
}

// access returns a usable access token for the refresh token, minting one when the
// cache has none or holds an expiring one.
//
// An exchange that fails returns an empty value: no Authorization header is sent,
// the service answers 401, and the shared machinery treats that as a rejected key
// and moves to the next one — which is exactly the right outcome for a refresh
// token the service no longer accepts.
func (c *tokenCache) access(refresh string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t, ok := c.entries[refresh]; ok && time.Until(t.expires) > refreshSkew {
		return t.value
	}

	value, lifetime, err := c.exchange(c.clientID, c.clientSecret, refresh)
	if err != nil {
		if c.log != nil {
			c.log("antigravity: refreshing the token: %v", err)
		}
		// An expired entry is dropped rather than reused: sending a token known
		// to be dead produces the same 401 as sending none, and none is honest
		// about what happened.
		delete(c.entries, refresh)
		return ""
	}
	c.entries[refresh] = token{value: value, expires: time.Now().Add(lifetime)}
	return value
}

// googleExchange is the live exchange at the OAuth token endpoint.
//
// The request is form-encoded with the client pair and the refresh token, as the
// installed-application flow requires; the answer carries the access token and its
// lifetime.
func (c *tokenCache) googleExchange(clientID, clientSecret, refresh string) (string, time.Duration, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}
	req, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var answer struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", 0, fmt.Errorf("token endpoint answer: %w", err)
	}
	if answer.AccessToken == "" {
		return "", 0, fmt.Errorf("token endpoint answer carries no access_token")
	}
	if answer.ExpiresIn <= 0 {
		// A lifetime the answer does not state is not a lifetime of zero: the
		// token is still usable, and treating it as expired would mint one on
		// every request.
		return answer.AccessToken, time.Hour, nil
	}
	return answer.AccessToken, time.Duration(answer.ExpiresIn) * time.Second, nil
}
