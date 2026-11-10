package control

import (
	"context"
	"net/http"
	"strings"
	"time"

	"providermux/internal/capability"
	"providermux/internal/provider"
	"providermux/internal/provider/antigravity"
)

// AntigravityAccountsResponse describes accounts and quota for Antigravity.
type AntigravityAccountsResponse struct {
	ProviderID string                            `json:"provider_id"`
	Configured bool                              `json:"configured"`
	Credential string                            `json:"credential"`
	Rotation   bool                              `json:"rotation"`
	KeyUsed    bool                              `json:"key_used"`
	Accounts   []antigravity.AccountQuotaSummary `json:"accounts"`
}

// handleAntigravityQuota fetches live quota for all registered Antigravity keys.
func (h *Handler) handleAntigravityQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "antigravity/quota")
		return
	}

	pID := provider.ID("antigravity")
	p := h.findAntigravityProvider()

	resp := AntigravityAccountsResponse{
		ProviderID: string(pID),
		Accounts:   []antigravity.AccountQuotaSummary{},
	}

	if p != nil {
		resp.Configured = true
		resp.Credential, resp.Rotation = h.credentialOf(pID)
		resp.KeyUsed = resp.Rotation || (resp.Credential == "account" && h.KeyRotation.HasRotation(pID))
	}

	if h.KeyRotation == nil || p == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	keys := h.KeyRotation.Keys(pID)
	fetcher := antigravity.NewQuotaFetcher(p)

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	for _, k := range keys {
		// Key can be formatted as refresh_token or refresh_token|project_id
		refreshToken := k
		project := ""
		if parts := strings.SplitN(k, "|", 2); len(parts) == 2 {
			refreshToken = parts[0]
			project = parts[1]
		}

		summary, err := fetcher.FetchQuota(ctx, refreshToken, project)
		if err != nil {
			resp.Accounts = append(resp.Accounts, antigravity.AccountQuotaSummary{
				AccountID:   keyFingerprint(k),
				Project:     project,
				Error:       err.Error(),
				LastChecked: time.Now().UTC(),
			})
			continue
		}
		resp.Accounts = append(resp.Accounts, *summary)
	}

	writeJSON(w, http.StatusOK, resp)
}

type oauthStartResponse struct {
	AuthURL  string `json:"auth_url"`
	Verifier string `json:"verifier"`
	State    string `json:"state"`
}

// handleAntigravityOAuthStart initializes PKCE flow for Google OAuth.
func (h *Handler) handleAntigravityOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, "antigravity/oauth/start")
		return
	}

	flow, err := antigravity.StartOAuthFlow("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, oauthStartResponse{
		AuthURL:  flow.AuthURL,
		Verifier: flow.Verifier,
		State:    flow.State,
	})
}

type oauthExchangeRequest struct {
	Code        string `json:"code"`
	Verifier    string `json:"verifier"`
	RedirectURI string `json:"redirect_uri,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
}

// handleAntigravityOAuthExchange exchanges auth code for refresh token and adds account.
func (h *Handler) handleAntigravityOAuthExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "antigravity/oauth/exchange")
		return
	}

	var req oauthExchangeRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Code == "" || req.Verifier == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code and verifier are required"})
		return
	}

	res, err := antigravity.ExchangeCode("", "", req.Code, req.Verifier, req.RedirectURI, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "exchange failed: " + err.Error()})
		return
	}

	if res.RefreshToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upstream gave no refresh token (offline consent required)"})
		return
	}

	keyVal := res.RefreshToken
	if req.ProjectID != "" {
		keyVal = res.RefreshToken + "|" + req.ProjectID
	}

	pID := provider.ID("antigravity")
	added := false
	if h.KeyRotation != nil {
		added = h.KeyRotation.AddKey(pID, keyVal)
		h.info("antigravity account added via oauth: %s (new: %t)", keyFingerprint(keyVal), added)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"added":       added,
		"email":       res.Email,
		"fingerprint": keyFingerprint(keyVal),
		"project_id":  req.ProjectID,
	})
}

func keyFingerprint(key string) string {
	return capability.Fingerprint(key)
}

func (h *Handler) findAntigravityProvider() *antigravity.Provider {
	if h.Providers == nil {
		return nil
	}
	p, err := h.Providers.Get(provider.ID("antigravity"))
	if err != nil {
		return nil
	}
	if agp, ok := p.(*antigravity.Provider); ok {
		return agp
	}
	return nil
}
