package antigravity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	OAuthAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	OAuthScopes      = "openid email https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email"
	OAuthRedirectURI = "http://localhost:51121/oauth-callback"
)

// OAuthFlow holds state for a PKCE OAuth login session.
type OAuthFlow struct {
	Verifier  string `json:"verifier"`
	Challenge string `json:"challenge"`
	State     string `json:"state"`
	AuthURL   string `json:"auth_url"`
}

// StartOAuthFlow generates a PKCE code verifier and challenge, and returns the Google auth URL.
func StartOAuthFlow(clientID string) (*OAuthFlow, error) {
	if clientID == "" {
		clientID = DefaultClientID
	}

	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("generating random verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	h := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(h[:])

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return nil, fmt.Errorf("generating random state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	params := url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {OAuthRedirectURI},
		"scope":                 {OAuthScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}

	authURL := OAuthAuthURL + "?" + params.Encode()
	return &OAuthFlow{
		Verifier:  verifier,
		Challenge: challenge,
		State:     state,
		AuthURL:   authURL,
	}, nil
}

// TokenExchangeResult carries the exchanged OAuth tokens.
type TokenExchangeResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Email        string `json:"email,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
}

// ExchangeCode exchanges an authorization code and PKCE verifier for tokens.
func ExchangeCode(clientID, clientSecret, code, verifier, redirectURI string, httpClient *http.Client) (*TokenExchangeResult, error) {
	if clientID == "" {
		clientID = DefaultClientID
	}
	if clientSecret == "" {
		clientSecret = DefaultClientSecret
	}
	if redirectURI == "" {
		redirectURI = OAuthRedirectURI
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}

	req, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchanging code: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var answer struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		IdToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	res := &TokenExchangeResult{
		AccessToken:  answer.AccessToken,
		RefreshToken: answer.RefreshToken,
		ExpiresIn:    answer.ExpiresIn,
	}

	// Try extracting email from userinfo endpoint
	if answer.AccessToken != "" {
		email := fetchUserEmail(httpClient, answer.AccessToken)
		res.Email = email
	}

	return res, nil
}

func fetchUserEmail(client *http.Client, accessToken string) string {
	req, err := http.NewRequest(http.MethodGet, "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var u struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err == nil {
		return u.Email
	}
	return ""
}
