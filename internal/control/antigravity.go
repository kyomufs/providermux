package control

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"sync"
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
	AuthURL   string `json:"auth_url"`
	Verifier  string `json:"verifier"`
	State     string `json:"state"`
	Listening bool   `json:"listening"`
}

var (
	oauthMu       sync.Mutex
	activeOAuthCl func()
)

// handleAntigravityOAuthStart initializes PKCE flow for Google OAuth and starts callback listener on 127.0.0.1:51121.
func (h *Handler) handleAntigravityOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, "antigravity/oauth/start")
		return
	}

	var startReq struct {
		ProjectID string `json:"project_id"`
	}
	_ = decodeBody(r, &startReq)

	projectID := startReq.ProjectID
	if projectID == "" {
		if p := h.findAntigravityProvider(); p != nil {
			projectID = p.Project()
		}
	}

	flow, err := antigravity.StartOAuthFlow("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	listening := h.startOAuthCallbackListener(flow, projectID)

	writeJSON(w, http.StatusOK, oauthStartResponse{
		AuthURL:   flow.AuthURL,
		Verifier:  flow.Verifier,
		State:     flow.State,
		Listening: listening,
	})
}

func (h *Handler) startOAuthCallbackListener(flow *antigravity.OAuthFlow, projectID string) bool {
	oauthMu.Lock()
	if activeOAuthCl != nil {
		activeOAuthCl()
		activeOAuthCl = nil
	}
	oauthMu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:51121")
	if err != nil {
		h.warn("antigravity oauth: cannot bind 127.0.0.1:51121: %v (manual code entry will still work)", err)
		return false
	}

	mux := http.NewServeMux()
	server := &http.Server{Handler: mux}

	var closeOnce sync.Once
	cleanup := func() {
		closeOnce.Do(func() {
			go func() {
				time.Sleep(500 * time.Millisecond)
				_ = server.Close()
				_ = ln.Close()
			}()
		})
	}

	oauthMu.Lock()
	activeOAuthCl = func() {
		closeOnce.Do(func() {
			_ = server.Close()
			_ = ln.Close()
		})
	}
	oauthMu.Unlock()

	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		errParam := r.URL.Query().Get("error")
		if errParam != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `<!DOCTYPE html><html><body style="font-family:system-ui;background:#0f141c;color:#f87171;padding:40px;text-align:center;"><h2>Ошибка авторизации</h2><p>%s</p></body></html>`, html.EscapeString(errParam))
			cleanup()
			return
		}

		state := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")

		if state != flow.State {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<!DOCTYPE html><html><body style="font-family:system-ui;background:#0f141c;color:#f87171;padding:40px;text-align:center;"><h2>Ошибка: неверный параметр state</h2></body></html>`)
			cleanup()
			return
		}

		if code == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<!DOCTYPE html><html><body style="font-family:system-ui;background:#0f141c;color:#f87171;padding:40px;text-align:center;"><h2>Ошибка: код авторизации отсутствует</h2></body></html>`)
			cleanup()
			return
		}

		res, err := antigravity.ExchangeCode("", "", code, flow.Verifier, antigravity.OAuthRedirectURI, nil)
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `<!DOCTYPE html><html><body style="font-family:system-ui;background:#0f141c;color:#f87171;padding:40px;text-align:center;"><h2>Ошибка обмена токена</h2><p>%s</p></body></html>`, html.EscapeString(err.Error()))
			cleanup()
			return
		}

		keyVal := res.RefreshToken
		if projectID != "" {
			keyVal = res.RefreshToken + "|" + projectID
		}

		pID := provider.ID("antigravity")
		added := false
		if h.KeyRotation != nil {
			added = h.KeyRotation.AddKey(pID, keyVal)
			h.info("antigravity account added via oauth callback: %s (new: %t, email: %s)", keyFingerprint(keyVal), added, res.Email)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>providermux — Авторизация успешна</title>
  <style>
    body { font-family: system-ui, -apple-system, sans-serif; display: flex; align-items: center; justify-content: center; height: 100vh; margin: 0; background: #0f141c; color: #e2e8f0; }
    .card { background: #1a2230; padding: 32px 40px; border-radius: 16px; text-align: center; box-shadow: 0 8px 32px rgba(0,0,0,0.4); max-width: 420px; border: 1px solid #2d3748; }
    .icon { font-size: 48px; margin-bottom: 16px; }
    h1 { font-size: 20px; margin: 0 0 8px; color: #38bdf8; }
    p { color: #94a3b8; font-size: 14px; margin: 0 0 20px; line-height: 1.5; }
    .email { background: #242f42; padding: 6px 12px; border-radius: 8px; font-weight: 500; color: #f1f5f9; display: inline-block; margin-bottom: 16px; font-size: 13px; }
    .hint { font-size: 12px; color: #64748b; }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">✅</div>
    <h1>Авторизация успешна!</h1>
    <div class="email">%s</div>
    <p>Аккаунт успешно подключен к providermux.<br>Вы можете закрыть эту вкладку и вернуться в панель управления.</p>
    <div class="hint">Окно можно закрыть</div>
  </div>
  <script>setTimeout(() => { window.close(); }, 3000);</script>
</body>
</html>`, html.EscapeString(res.Email))

		cleanup()
	})

	go func() {
		_ = server.Serve(ln)
	}()

	// Auto-cancel listener after 5 minutes
	go func() {
		time.Sleep(5 * time.Minute)
		cleanup()
	}()

	return true
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
