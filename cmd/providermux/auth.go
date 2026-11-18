package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"providermux/internal/config"
)

// requireToken wraps a handler with a shared-secret check.
//
// WHY A WRAPPER AND NOT A CHECK INSIDE THE SURFACES. There are two surfaces, and
// both give access to the operator's quota: one spends it with requests, the
// other can change keys. A check placed inside one of them leaves the other
// open — and that is exactly what happened: the control surface was closed,
// while the models surface answered anyone.
//
// A wrapper around the whole router settles the question at once and does not
// require remembering the check when a third surface is added.
//
// In local mode the check is not performed at all: nobody but this machine can
// reach a loopback listener, and a token is not needed there.
//
// THE DECISION IS MADE PER REQUEST, not once when the handler is built. The
// control surface can switch the mode on a live daemon, and a wrapper that
// captured the old value would keep answering requests after the switch — or
// keep demanding a token that had just been added. The configuration is read
// through a snapshot so that the check does not race with the change itself.
func requireToken(next http.Handler, cfg *config.Config) http.Handler {
	if cfg == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server, token := cfg.Access()
		if !server {
			next.ServeHTTP(w, r)
			return
		}
		// THE PANEL SHELL IS THE ONE EXCEPTION. It is a static page with no data
		// and no secret, and it is the page that asks for the token. Closing it
		// made server mode a dead end: the browser received a JSON 401 instead
		// of HTML, so the token dialog could never appear and the only way in
		// was pasting ?token= into the address bar. Everything the shell talks
		// to — /v1/* and /_control/* — stays behind the check, and the shell
		// itself renders nothing until its first call comes back with 401.
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		// Server mode without a token is a configuration the daemon refuses to
		// start with, and the live switch refuses it too. Denying here is the
		// last line of defence: comparing against an empty secret would accept
		// an empty header.
		if strings.TrimSpace(token) == "" {
			writeAuthError(w)
			return
		}
		if !tokenMatches(r, token) {
			writeAuthError(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenMatches compares the presented token against the expected one.
//
// The comparison runs in constant time. A byte-by-byte exit with an early
// return would let the secret be guessed from the response time.
//
// The length does leak: values of different lengths cannot be compared in
// constant time without knowing the length in advance. But what leaks is the
// length, not the content.
func tokenMatches(r *http.Request, want string) bool {
	presented := bearerToken(r)
	if presented == "" {
		presented = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}

// bearerToken pulls the token out of the header.
//
// Both the "Bearer <token>" form and the bare value are accepted: different
// clients behave differently, and demanding strictly one form would break
// someone else's code for no reason.
func bearerToken(r *http.Request) string {
	v := r.Header.Get("authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return v[7:]
	}
	return v
}

// writeAuthError answers with an access refusal.
//
// The shape is the same as the other gateway errors: the client has one
// vocabulary for branching, and a second, different one would force it to keep
// a mapping.
func writeAuthError(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "a token is required: the gateway runs in server mode",
			"type":    "AuthError",
			"code":    "AuthError",
		},
	})
}
