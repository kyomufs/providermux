package control

import (
	"net/http"
	"strings"

	"providermux/internal/subscriptions"
)

// SubscriptionState is what the panel shows about the list the exit is built from.
type SubscriptionState struct {
	// Store is the file the list is kept in, so the operator knows where their
	// subscriptions live.
	Store string `json:"store"`
	// Dedupe is the duplicate rule the builder applies.
	Dedupe string `json:"dedupe"`
	// Items are the subscriptions.
	Items []subscriptions.Item `json:"items"`
	// Error is a store that could not be read, reported next to the list rather
	// than instead of it.
	Error string `json:"error,omitempty"`
}

// subscriptionBody is what the panel sends for one subscription.
type subscriptionBody struct {
	ID       string            `json:"id"`
	URL      string            `json:"url"`
	UA       string            `json:"ua"`
	Insecure bool              `json:"insecure"`
	Headers  map[string]string `json:"headers"`
}

// dedupeBody is what the panel sends when the rule changes.
type dedupeBody struct {
	Dedupe string `json:"dedupe"`
}

// handleSubscriptions serves the list: read it, add, replace or drop an entry.
func (h *Handler) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	if h.Subscriptions == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this deployment does not keep a subscription list: egress_refresh.subscriptions is not set",
		})
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.writeSubscriptions(w)
	case http.MethodPost, http.MethodPut:
		var body subscriptionBody
		if err := decodeBody(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read the request: " + err.Error()})
			return
		}
		item := subscriptions.Item{
			ID:       body.ID,
			URL:      body.URL,
			UA:       body.UA,
			Insecure: body.Insecure,
			Headers:  body.Headers,
		}
		var err error
		if r.Method == http.MethodPost {
			_, err = h.Subscriptions.Add(item)
			if err == nil {
				h.info("subscriptions: %q was added", item.URL)
			}
		} else {
			_, err = h.Subscriptions.Update(item)
			if err == nil {
				h.info("subscriptions: %q was changed", body.ID)
			}
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h.writeSubscriptions(w)
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a delete needs the subscription id"})
			return
		}
		if err := h.Subscriptions.Remove(id); err != nil {
			writeJSON(w, statusForSubscriptionError(err), map[string]string{"error": err.Error()})
			return
		}
		h.info("subscriptions: %q was removed", id)
		h.writeSubscriptions(w)
	default:
		methodNotAllowed(w, "GET, POST, PUT, DELETE")
	}
}

// handleSubscriptionMode changes the duplicate rule.
func (h *Handler) handleSubscriptionMode(w http.ResponseWriter, r *http.Request) {
	if h.Subscriptions == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "this deployment does not keep a subscription list: egress_refresh.subscriptions is not set",
		})
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, "PUT")
		return
	}
	var body dedupeBody
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot read the request: " + err.Error()})
		return
	}
	if err := h.Subscriptions.SetDedupe(body.Dedupe); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	h.info("subscriptions: duplicate rule is now %q", body.Dedupe)
	h.writeSubscriptions(w)
}

func (h *Handler) writeSubscriptions(w http.ResponseWriter) {
	file := h.Subscriptions.List()
	state := SubscriptionState{
		Store:  h.Subscriptions.Path(),
		Dedupe: file.Dedupe,
		Items:  file.Items,
	}
	if state.Items == nil {
		state.Items = []subscriptions.Item{}
	}
	writeJSON(w, http.StatusOK, state)
}

// statusForSubscriptionError keeps "no such entry" apart from "the file could
// not be written": the first is the operator's typo, the second is a disk.
func statusForSubscriptionError(err error) int {
	if strings.HasPrefix(err.Error(), "no subscription") {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}
