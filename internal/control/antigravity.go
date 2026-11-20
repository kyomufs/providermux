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
		// Whether the request path presents a key is about the pool and the
		// credential, not about rotation: a single key is still sent even when
		// there is nothing to rotate to.
		if h.KeyRotation != nil && resp.Credential != "anonymous" {
			resp.KeyUsed = len(h.KeyRotation.Keys(pID)) > 0
		}
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
		// The summary names the account by its token, but the panel has to tie
		// that name to a pool entry, and the pool is addressed by fingerprint.
		// One identifier per account, or the same Google account appears as two
		// different strings on neighbouring screens.
		summary.AccountID = keyFingerprint(k)
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
			writeOAuthPage(w, http.StatusBadRequest, oauthPage{
				Title:   "Вход не завершён",
				Message: "Google вернул ошибку авторизации. Начните вход заново из панели.",
				Detail:  errParam,
			})
			cleanup()
			return
		}

		state := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")

		if state != flow.State {
			writeOAuthPage(w, http.StatusBadRequest, oauthPage{
				Title:   "Ссылка устарела",
				Message: "Параметр state не совпал с текущим входом. Начните вход заново из панели.",
			})
			cleanup()
			return
		}

		if code == "" {
			writeOAuthPage(w, http.StatusBadRequest, oauthPage{
				Title:   "Код не пришёл",
				Message: "Google не вернул код авторизации. Попробуйте войти ещё раз.",
			})
			cleanup()
			return
		}

		res, err := antigravity.ExchangeCode("", "", code, flow.Verifier, antigravity.OAuthRedirectURI, nil)
		if err != nil {
			writeOAuthPage(w, http.StatusInternalServerError, oauthPage{
				Title:   "Не удалось получить токен",
				Message: "Google не принял код. Начните вход заново из панели.",
				Detail:  err.Error(),
			})
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

		writeOAuthPage(w, http.StatusOK, oauthPage{
			OK:      true,
			Title:   "Аккаунт подключён",
			Message: "Панель уже видит новый аккаунт и его квоты.",
			Email:   res.Email,
		})

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

type oauthPage struct {
	OK      bool
	Title   string
	Message string
	Detail  string
	Email   string
}

const oauthPageTemplate = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>providermux — {TITLE}</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: oklch(97.4% .008 85);
    --card: #fff;
    --ink: oklch(23% .012 80);
    --muted: oklch(50% .012 80);
    --line: oklch(90% .008 85);
    --ok: oklch(48% .09 190);
    --ok-soft: oklch(94% .03 190);
    --bad: oklch(52% .16 27);
    --bad-soft: oklch(95% .03 27);
    --shadow: 0 1px 2px oklch(20% .01 85 / .06), 0 24px 48px -24px oklch(20% .02 85 / .25);
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: oklch(19% .012 80);
      --card: oklch(23% .012 80);
      --ink: oklch(96% .006 85);
      --muted: oklch(70% .012 80);
      --line: oklch(31% .012 80);
      --ok: oklch(78% .1 185);
      --ok-soft: oklch(30% .04 190);
      --bad: oklch(72% .14 27);
      --bad-soft: oklch(32% .05 27);
      --shadow: 0 1px 2px oklch(0% 0 0 / .4), 0 24px 48px -24px oklch(0% 0 0 / .6);
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100svh; padding: 24px;
    display: grid; place-items: center;
    background: radial-gradient(120% 120% at 50% 0%, color-mix(in oklch, var(--ok-soft) 55%, var(--bg)) 0%, var(--bg) 55%);
    color: var(--ink);
    font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  .card {
    width: min(440px, 100%);
    padding: 40px 36px 32px;
    border: 1px solid var(--line);
    border-radius: 28px;
    background: var(--card);
    box-shadow: var(--shadow);
    text-align: center;
  }
  .mark {
    width: 64px; height: 64px; margin: 0 auto 20px;
    display: grid; place-items: center;
    border-radius: 22px;
    background: var(--ok-soft); color: var(--ok);
  }
  .mark.bad { background: var(--bad-soft); color: var(--bad); }
  .mark svg { width: 34px; height: 34px; }
  h1 { margin: 0 0 8px; font-size: 22px; line-height: 1.25; letter-spacing: -.02em; }
  p { margin: 0; color: var(--muted); font-size: 14px; line-height: 1.55; }
  .email {
    display: inline-flex; max-width: 100%;
    margin: 18px 0 2px; padding: 8px 14px;
    border-radius: 999px; background: var(--ok-soft); color: var(--ink);
    font-size: 13px; font-weight: 600; overflow-wrap: anywhere;
  }
  .detail {
    margin-top: 16px; padding: 12px 14px;
    border: 1px solid var(--line); border-radius: 14px;
    background: var(--bg); color: var(--muted);
    font: 12px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
    text-align: left; overflow-wrap: anywhere;
  }
  .actions { margin-top: 24px; display: flex; justify-content: center; gap: 10px; }
  button {
    appearance: none; border: 0; cursor: pointer;
    height: 40px; padding: 0 20px; border-radius: 999px;
    background: var(--ok); color: var(--bg);
    font: inherit; font-size: 13px; font-weight: 600;
  }
  button:hover { filter: brightness(1.06); }
  button:focus-visible { outline: 2px solid var(--ok); outline-offset: 3px; }
  .hint { margin-top: 14px; font-size: 12px; color: var(--muted); }
  @media (prefers-color-scheme: dark) { button { color: oklch(18% .01 80); } }
  @media (prefers-reduced-motion: no-preference) {
    .card { animation: rise .5s cubic-bezier(.2,.7,.3,1) both; }
    .mark svg path { stroke-dasharray: 44; stroke-dashoffset: 44; animation: draw .45s .15s cubic-bezier(.2,.7,.3,1) forwards; }
    @keyframes rise { from { opacity: 0; transform: translateY(10px) scale(.985); } }
    @keyframes draw { to { stroke-dashoffset: 0; } }
  }
</style>
</head>
<body>
  <main class="card">
    <div class="{CLASS}" aria-hidden="true">{MARK}</div>
    <h1>{TITLE}</h1>
    <p>{MESSAGE}</p>
    {EXTRA}
    <div class="actions"><button type="button" onclick="window.close()">Закрыть вкладку</button></div>
    <div class="hint">Вкладку можно закрыть — панель продолжит работать.</div>
  </main>
</body>
</html>`

func writeOAuthPage(w http.ResponseWriter, status int, p oauthPage) {
	mark := `<svg viewBox="0 0 64 64" fill="none" stroke="currentColor" stroke-width="5" stroke-linecap="round" stroke-linejoin="round"><path d="M18 33.5 28 43.5 46 23"/></svg>`
	class := "mark"
	if !p.OK {
		mark = `<svg viewBox="0 0 64 64" fill="none" stroke="currentColor" stroke-width="5" stroke-linecap="round"><path d="M21 21 43 43M43 21 21 43"/></svg>`
		class = "mark bad"
	}
	var extra strings.Builder
	if p.Email != "" {
		fmt.Fprintf(&extra, `<div class="email">%s</div>`, html.EscapeString(p.Email))
	}
	if p.Detail != "" {
		fmt.Fprintf(&extra, `<div class="detail">%s</div>`, html.EscapeString(p.Detail))
	}
	page := strings.NewReplacer(
		"{CLASS}", class,
		"{MARK}", mark,
		"{TITLE}", html.EscapeString(p.Title),
		"{MESSAGE}", html.EscapeString(p.Message),
		"{EXTRA}", extra.String(),
	).Replace(oauthPageTemplate)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, page)
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
