// Package gateway is the OpenAI surface the client sees.
//
// WHAT IS CENTRAL HERE. The gateway is a TRANSPARENT MIDDLEMAN. It does what
// the client would have done on a direct call, and adds no behaviour on top.
// Hence two rules to which the whole package is subject:
//
//  1. A REFUSAL IS PASSED THROUGH VERBATIM. The status, body and headers reach
//     the client byte for byte. The client parses the body itself: it looks
//     there for `error.type`, `error.code`, nested codes and markers, and
//     decides about a retry from them. Any repackaging would deprive it of
//     those signals. That includes `retry-after` and `retry-after-ms`: the
//     provider named the hold, and the gateway has no better number to offer.
//
//  2. THE GATEWAY DOES NOT INVENT RETRIES. The client has its own retry
//     policy, worked out in detail: an exponent with a ceiling, up to ten
//     times, with a limit on timeouts. A second policy on top of it would give
//     thirty requests where the client counted on ten. One client request is
//     therefore one provider request, always: no key switch, no repeat and no
//     move to another model happen behind the client's back.
package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"providermux/internal/capability"
	"providermux/internal/provider"
	"providermux/internal/state"
)

// Limits. The request body is bounded, because the client can send anything,
// and memory is not infinite.
const (
	maxBodyBytes = 4 << 20 // 4 MiB

	// refusalExcerpt is how much of a refusal body reaches the log.
	//
	// The counters cannot tell refusals apart: "the free tier is for the app
	// only", "abusive traffic from this IP" and "the address is out of quota" are
	// all a 403. Which one it was decides what fixes it — a client identity, a
	// different uplink, or waiting — and by the time anyone asks, the client's
	// own output is long gone.
	refusalExcerpt = 300
)

// Handler is the HTTP handler of the OpenAI surface.
type Handler struct {
	// Providers is the registry of the available providers.
	Providers *provider.Registry
	// Transport hands out the RoundTripper for requests. Used as the global fallback.
	Transport RoundTripperFactory
	// TransportFor returns the RoundTripperFactory for a specific provider.
	// When nil or when returning nil, Transport is used as the global fallback.
	TransportFor func(provider.ID) RoundTripperFactory
	// State is the counters.
	State *state.Store
	// Log is the log.
	Log *state.Logger

	// Addresses reports the egress address the traffic leaves from.
	//
	// The address is not read off the local interfaces: behind NAT they name an
	// address the provider never sees. It is asked of the outside instead.
	//
	// nil means unknown, and that is a legitimate state rather than a missing
	// setting: the address is a property of the network, and the daemon has to
	// keep working when it cannot be established. Unknown records no address
	// row, so the matrix stays honest instead of showing an invented one.
	Addresses AddressResolver

	// AddressesFor reports the egress address for one provider, when the
	// providers do not all leave by the same path.
	//
	// WHY. A transport is chosen per provider: one may go out through the
	// host's own routing while another is tunnelled through a proxy whose exit
	// can be chosen. The address that describes either path is not the same
	// value, and a lane keyed by the wrong address mislabels every refusal it
	// records. nil means Addresses speaks for all of them.
	AddressesFor func(id provider.ID) AddressResolver

	// DefaultProvider is the provider used when the client did not name one.
	// Empty means the first in the registry.
	DefaultProvider provider.ID

	// KeyRotation stores the key rotation mechanisms by provider. Empty means
	// anonymous operation: the provider will substitute its own anonymity
	// marker, and that is a normal mode, not a missing setting.
	KeyRotation map[string]*capability.KeyRotation

	// Credential reports which credential is in force for a provider:
	// "anonymous" or "account".
	//
	// The gateway does not read the configuration itself: the axis has a
	// per-provider override, and resolving it belongs to the package that owns
	// the axis. A function rather than a map, so that the answer is computed
	// from one place instead of being copied into a second one that can drift.
	//
	// nil means "not told", and then the credential is taken from the pool's
	// presence, which is the old behaviour: a provider with a mechanism gets a
	// key, one without gets the anonymity marker. That keeps the gateway usable
	// from a test that wires only a registry.
	Credential func(provider.ID) string

	// Account reports the key a provider is pinned to, or "" for none.
	//
	// A pin is not part of the credential axis: that axis says whether a key is
	// presented at all, while the pin says which one goes first when several
	// are in the pool. A function for the same reason as Credential — the answer
	// belongs to whoever owns the configuration.
	Account func(provider.ID) string

	// Client is the HTTP client. It is set by a field rather than created per
	// request: the client holds a connection pool, and losing it is not
	// acceptable.
	Client *http.Client

	// FirstEventTimeout is how long a stream may stay silent before its response
	// headers, and IdleTimeout how long a response may stay silent between
	// events. They are the FALLBACKS: a provider that declares a budget of its
	// own is obeyed, because the numbers belong to its contract — a model that
	// reasons holds pauses of minutes, and a shared budget would tear its stream.
	// Zero means the gateway's own default.
	//
	// They are not advice. A connection that goes quiet stops raising any error
	// at all, and without these budgets a request on it waits forever while the
	// client waits with it.
	FirstEventTimeout time.Duration
	IdleTimeout       time.Duration

	// Retry is how many times one client request may reach the provider, and
	// how long to wait between attempts.
	//
	// Zero attempts means one: a gateway that repeats by default would change
	// the client's traffic behind its back, and the reason to repeat is narrow
	// enough that it is asked for explicitly.
	Retry RetryPolicy

	// Rotate asks for a different exit address and reports the one chosen.
	//
	// nil means the exit cannot be changed, and then a quota refusal is passed
	// to the client exactly as it arrived. The gateway does not construct this
	// itself: the proxy controller is a separate process, and reaching it is a
	// property of the deployment, not of the request path.
	Rotate func(ctx context.Context) (string, error)

	// RotateFor reports whether the exit this provider leaves by is the one
	// Rotate moves.
	//
	// WHY. Rotating a selector group that stands behind a transport the provider
	// does not use moves an exit nobody travelled: the repeat would leave from
	// the same address as the refusal and spend a rotation for nothing. When this
	// says no, the refusal is passed to the client exactly as it arrived. nil
	// means every provider leaves by the movable exit.
	RotateFor func(id provider.ID) bool

	// Settle returns the exit to its default, and is called after an answer
	// that arrived on a rotated exit.
	//
	// WHY. A rotation answers one refusal; it is not a new home. Without this,
	// the node a refusal pushed the gateway to stays in use after the default
	// exit answers again — measured: a node kept for hours after the address it
	// replaced had recovered. nil means the exit has no default to return to.
	Settle func(ctx context.Context) error
}

// RoundTripperFactory hands out the transport for requests.
//
// An interface rather than a concrete type: the gateway does not need to know
// how many transports exist or how they are arranged.
type RoundTripperFactory interface {
	RoundTripper() (http.RoundTripper, error)
}

// Routes registers the routes on the given mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/models", h.handleModels)
	mux.HandleFunc("/v1/chat/completions", h.handleChat)
}

// ProviderRoute is the prefix for choosing the provider right in the path.
//
// Form: /p/<provider>/v1/chat/completions. It is needed when the client can
// address several routes: then every provider is visible as a separate
// endpoint, and switching between them can happen on the client's side.
const ProviderRoute = "/p/"

// routes registers both kinds of routes.
func (h *Handler) AllRoutes(mux *http.ServeMux) {
	h.Routes(mux)
	mux.HandleFunc(ProviderRoute, h.handleProviderRoute)
}

// resolveClient returns the HTTP client.
func (h *Handler) resolveClient() *http.Client {
	if h.Client != nil {
		return h.Client
	}
	return http.DefaultClient
}

// resolveProvider chooses the provider for a request.
//
// The order: an explicit name in the request body, then the name in the path,
// then the default provider. An explicit name in the body is respected, because
// the client may send a `model` of the form "provider/model" — that is a common
// shape, and it is better to understand it than to reject it.
func (h *Handler) resolveProvider(body map[string]any, pathID provider.ID) (provider.Provider, string, error) {
	if pathID != "" {
		p, err := h.Providers.Get(pathID)
		if err != nil {
			return nil, "", err
		}
		model, _ := body["model"].(string)
		return p, stripProviderPrefix(string(model), pathID), nil
	}

	rawModel, _ := body["model"].(string)
	if name, rest := splitProviderPrefix(rawModel); name != "" {
		// The first segment is a provider only when the registry knows it. A
		// model identifier may itself contain slashes (for instance
		// "poolside/laguna-s-2.1:free"), and treating every such name as a
		// provider prefix made the gateway reject a perfectly valid model with
		// "unknown provider".
		if p, err := h.Providers.Get(provider.ID(name)); err == nil {
			return p, rest, nil
		}
	}

	if h.DefaultProvider != "" {
		p, err := h.Providers.Get(h.DefaultProvider)
		if err != nil {
			return nil, "", err
		}
		return p, rawModel, nil
	}

	all := h.Providers.All()
	if len(all) == 0 {
		return nil, "", fmt.Errorf("no providers configured")
	}
	return all[0], rawModel, nil
}

// splitProviderPrefix parses the form "provider/model".
//
// The separator is looked for up to the first slash; a provider name cannot
// contain a slash, while model identifiers often do (for instance
// "poolside/laguna-s-2.1:free"), so the first part is checked against the
// registry by the caller.
func splitProviderPrefix(model string) (name, rest string) {
	i := strings.Index(model, "/")
	if i <= 0 || i == len(model)-1 {
		return "", ""
	}
	return model[:i], model[i+1:]
}

// stripProviderPrefix strips the provider prefix from the model name.
func stripProviderPrefix(model string, id provider.ID) string {
	prefix := string(id) + "/"
	if strings.HasPrefix(model, prefix) {
		return strings.TrimPrefix(model, prefix)
	}
	return model
}

// handleProviderRoute parses a path of the form /p/<provider>/v1/...
func (h *Handler) handleProviderRoute(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, ProviderRoute)
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		http.NotFound(w, r)
		return
	}
	id := provider.ID(rest[:slash])
	inner := rest[slash:]

	switch inner {
	case "/v1/models":
		h.handleModelsFor(w, r, id)
	case "/v1/chat/completions":
		h.handleChatFor(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// handleModels answers with the list of models of all providers.
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "not supported",
			"GET /v1/models: the method must be GET")
		return
	}
	h.serveModels(w, r, "")
}

// handleModelsFor answers with the list of models of one provider.
func (h *Handler) handleModelsFor(w http.ResponseWriter, r *http.Request, id provider.ID) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "not supported",
			"GET /v1/models: the method must be GET")
		return
	}
	if _, err := h.Providers.Get(id); err != nil {
		writeError(w, http.StatusNotFound, "provider not found", err.Error())
		return
	}
	h.serveModels(w, r, id)
}

// serveModels assembles the list of models.
//
// The format is the OpenAI list. The model identifier is composite:
// "provider/model", otherwise two models with the same name at different
// providers would be indistinguishable, and the client could not pick the one
// it needs.
//
// CAPACITIES ARE REPORTED, NOT WITHHELD. An entry carries what the provider
// declared about the model: the context window, the output ceiling, the
// modalities, whether it reasons. The daemon is the only party that knows these
// numbers, and a client that cannot read them has to invent them — and a
// hardcoded window is wrong in both directions: too large and the client
// overruns the context, gets a refusal it cannot explain, and has nothing to
// compact against; too small and it truncates a conversation that would have
// fit. A capacity the provider did not declare is omitted rather than zeroed,
// so a client can tell "not declared" from "declared as nothing".
func (h *Handler) serveModels(w http.ResponseWriter, r *http.Request, only provider.ID) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
		Name    string `json:"name,omitempty"`
		// ContextLength is the combined request and response window in tokens.
		ContextLength int `json:"context_length,omitempty"`
		// MaxCompletionTokens is the response ceiling in tokens.
		MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
		// InputModalities lists what the model accepts ("text", "image").
		InputModalities []string `json:"input_modalities,omitempty"`
		// Reasoning reports whether the model reasons.
		Reasoning bool `json:"reasoning,omitempty"`
		// ReasoningEfforts lists the effort words the model accepts, in the
		// provider's own spelling. A client that cannot read them has to offer
		// a ladder of its own, and a word the provider does not know is either
		// silently ignored or refused as a bad request.
		ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
		// Free repeats the provider's own statement that the model is free. It
		// is informational: what the gateway sends is decided by the provider
		// section, not by this field.
		Free bool `json:"free,omitempty"`
	}

	var entries []modelEntry
	for _, p := range h.Providers.All() {
		if only != "" && p.ID() != only {
			continue
		}
		models, err := provider.ModelsForKey(r.Context(), p, h.catalogKey(p))
		if err != nil {
			h.warn("provider %q: the catalog is unavailable: %v", p.ID(), err)
			continue
		}
		for _, m := range models {
			entry := modelEntry{
				ID:                  string(p.ID()) + "/" + m.ID,
				Object:              "model",
				Created:             modelsCreatedAt,
				OwnedBy:             string(p.ID()),
				Name:                m.Name,
				ContextLength:       m.Context,
				MaxCompletionTokens: m.MaxOutput,
				Reasoning:           m.Reasoning,
				ReasoningEfforts:    reasoningEfforts(m.ReasoningOptions),
				Free:                m.Free,
			}
			// Modalities are always reported, because the gateway itself has to
			// answer the question "may I attach an image here": a model that
			// did not declare vision takes text, and saying so is not a guess.
			if m.Vision {
				entry.InputModalities = []string{"text", "image"}
			} else {
				entry.InputModalities = []string{"text"}
			}
			entries = append(entries, entry)
		}
	}

	if entries == nil {
		entries = []modelEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   entries,
	})
}

// modelsCreatedAt is a constant creation marker for the model list. The list is
// static, so inventing a new time every time would be dishonest.
const modelsCreatedAt int64 = 1759718400

// reasoningEfforts reduces the declared forms of effort control to the words a
// client can pass back in "reasoning_effort".
//
// Only two forms name values: an effort ladder contributes its own words, a
// toggle contributes "off" and nothing else. The second half deserves a word:
// switching thinking on is not the same as asking for a specific amount of it,
// so a client that offers "high" where the model only has a switch is guessing
// at a name the provider may not accept. Order is preserved and duplicates are
// dropped, because the ladder is shown to a human in the order the provider
// declared it.
func reasoningEfforts(options []provider.ReasoningOption) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(values ...string) {
		for _, v := range values {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, option := range options {
		switch option.Kind {
		case provider.ReasoningToggle:
			add("off")
		case provider.ReasoningEffort:
			add(option.Values...)
		}
	}
	return out
}

// handleChat handles a request to a model.
func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	h.handleChatFor(w, r, "")
}

// handleChatFor handles a request, addressing it to a particular provider where
// necessary.
func (h *Handler) handleChatFor(w http.ResponseWriter, r *http.Request, pathID provider.ID) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "not supported",
			"POST /v1/chat/completions: the method must be POST")
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request", "reading the body: "+err.Error())
		return
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request", "parsing the body: "+err.Error())
		return
	}

	p, rawModelID, err := h.resolveProvider(body, pathID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "provider not found", err.Error())
		return
	}
	if rawModelID == "" {
		writeError(w, http.StatusBadRequest, "invalid request", "the model field is required")
		return
	}

	modelID, keySelector := extractKeySelector(r, rawModelID)

	model, err := h.resolveModel(r, p, modelID)
	if err != nil {
		// A name that begins with an unknown provider is reported as such, not
		// as a missing model: the client asked for a lane that does not exist.
		// A slash is not proof of a provider prefix — valid model identifiers
		// contain them — so this branch is reached only after the model failed
		// to resolve on the fallback provider.
		if name, _ := splitProviderPrefix(rawModelID); name != "" {
			if _, getErr := h.Providers.Get(provider.ID(name)); getErr != nil {
				writeError(w, http.StatusBadRequest, "provider not found",
					fmt.Sprintf("unknown provider %q in the model field", name))
				return
			}
		}
		writeError(w, http.StatusNotFound, "model not found", err.Error())
		return
	}

	req, err := h.buildRequest(r, body, p, model)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request", err.Error())
		return
	}

	h.forward(w, r, p, req, model, keySelector)
}

// extractKeySelector extracts an explicit key selector from headers, query string,
// or model suffix (e.g. "model@selector" or "model#selector").
func extractKeySelector(r *http.Request, rawModel string) (cleanModel, selector string) {
	cleanModel = rawModel
	if s := r.Header.Get("x-providermux-key"); s != "" {
		selector = strings.TrimSpace(s)
	} else if s := r.Header.Get("x-key"); s != "" {
		selector = strings.TrimSpace(s)
	} else if s := r.Header.Get("x-account"); s != "" {
		selector = strings.TrimSpace(s)
	} else if s := r.URL.Query().Get("key"); s != "" {
		selector = strings.TrimSpace(s)
	}
	if idx := strings.IndexAny(cleanModel, "@#"); idx > 0 {
		if selector == "" {
			selector = cleanModel[idx+1:]
		}
		cleanModel = cleanModel[:idx]
	}
	return cleanModel, selector
}

// resolveModel finds a model in the provider's catalog.
//
// A model that is not in the catalog is rejected: the catalog is what the
// provider has declared, and sending a request to an unknown model would mean
// getting a refusal later and a less understandable one.
func (h *Handler) resolveModel(r *http.Request, p provider.Provider, id string) (provider.Model, error) {
	models, err := provider.ModelsForKey(r.Context(), p, h.catalogKey(p))
	if err != nil {
		return provider.Model{}, fmt.Errorf("the provider catalog is unavailable: %w", err)
	}
	for _, m := range models {
		if m.ID == id {
			return m, nil
		}
	}
	return provider.Model{}, fmt.Errorf("provider %q does not declare model %q", p.ID(), id)
}

// buildRequest translates the client request into the provider's shape.
func (h *Handler) buildRequest(r *http.Request, body map[string]any, p provider.Provider, model provider.Model) (provider.Request, error) {
	messages, err := parseMessages(body["messages"])
	if err != nil {
		return provider.Request{}, err
	}
	tools, err := parseTools(body["tools"])
	if err != nil {
		return provider.Request{}, err
	}

	ids := identifiers(p, r)

	req := provider.Request{
		Model:    model,
		Messages: messages,
		System:   systemFrom(body),
		Tools:    tools,
		// The client's own answer: the shape of the response belongs to the
		// client that asked for it, and the gateway reproduces the direct call.
		//
		// Asking for a stream unconditionally would hand a client that sent
		// stream: false a body of event-stream chunks under a content type it
		// never requested — and every non-streaming parser in existence fails on
		// it. The absent field means the protocol's default, which is a single
		// response.
		Stream:  streamRequested(body),
		Session: ids,
	}

	if raw, ok := body["tool_choice"].(string); ok {
		req.ToolChoice = raw
	}
	if v, ok := body["max_tokens"].(float64); ok && v > 0 {
		req.MaxTokens = int(v)
	}
	// The newer spelling is what the v2 client sends. Both are read because the
	// gateway's surface is OpenAI-compatible, and a caller may use either.
	if v, ok := body["max_completion_tokens"].(float64); ok && v > 0 {
		req.MaxTokens = int(v)
	}
	if v, ok := body["temperature"].(float64); ok {
		req.Temperature = &v
	}
	if v, ok := body["reasoning_effort"].(string); ok {
		req.ReasoningEffort = v
	}

	return req, nil
}

// forward sends the request to the provider and passes the response to the
// client.
//
// A QUOTA REFUSAL IS ANSWERED BY A DIFFERENT EXIT, NOT BY ANOTHER KEY. That is
// the measured shape of the free lane: it counts by source address, the client
// cannot change its address, and the client's own policy does not repeat the
// refusal at all — the reference client classifies FreeUsageLimitError as
// QuotaExceeded and isRetryable returns false for it
// (packages/ai/src/schema/errors.ts). A turn that meets a spent address
// therefore ends, and nothing above this function can prevent it.
//
// The gateway is the one party that can both repeat the request and leave by
// another exit, so it does both, and only where that is the answer: a quota
// refusal with a rotation available. A key refusal, a malformed request, a
// content refusal, a spent account — none of those are repeated here, and the
// client receives them exactly as before.
//
// Everything the client could observe happens on the LAST attempt, so a repeat
// stays invisible to it except by the extra latency: a single response is not
// written until it is complete, and a stream is not opened until the provider's
// headers arrive. No repeat can duplicate a byte the client has already seen.
//
// ONE OUTCOME PER ATTEMPT is recorded, not one per client request. A refusal
// that was answered from another exit is still a refusal the provider issued,
// and precisely the volume of those tells the operator which address is burnt.
// Counting only the client's final answer would hide the failures this
// mechanism exists to absorb.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, p provider.Provider, req provider.Request, model provider.Model, keySelector ...string) {
	// The gateway substitutes the credentials: the client has no keys.
	selector := ""
	if len(keySelector) > 0 {
		selector = keySelector[0]
	}

	attempts := h.Retry.attempts()

	// Whether this request has already moved the exit. It is what makes the
	// return to the default exit a consequence of an answer that worked, not a
	// periodic housekeeping nobody asked for.
	rotated := false

	for attempt := 1; ; attempt++ {
		// The egress address is read per attempt, not per request: a rotation
		// between two attempts IS a change of this value, and the lanes a
		// provider keeps are keyed by it.
		address := h.addressFor(p.ID())
		// The provider sees the address for one decision only: whether an
		// identifier it minted still serves this request. A lane that counts by
		// address and by session cannot answer that without it.
		req.Address = address

		prepared, err := p.Build(req)
		if err != nil {
			h.warn("provider %q: assembling the request: %v", p.ID(), err)
			writeError(w, http.StatusInternalServerError, "gateway error", err.Error())
			h.recordAbort(model.ID, address, "")
			return
		}

		key := h.credentialForWithSelector(p, selector)
		h.applyCredential(p, prepared, key)

		// The budgets of the attempt. Both are re-armed as the attempt
		// progresses: the first bounds the wait for the response headers, the
		// second the silence between events.
		//
		// The two are read from their OWN fields of the prepared request. They
		// are not interchangeable: a provider that reasons declares a generous
		// silence budget between events, and using that number for the wait
		// before the headers would leave a request hanging on a connection that
		// has gone quiet for as long as the model is allowed to think.
		firstEvent := Timeout(prepared.FirstEventTimeout, h.FirstEventTimeout)
		idle := Timeout(prepared.IdleTimeout, h.IdleTimeout)

		wd, ctx := newWatchdog(r.Context())
		upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, prepared.URL, bytes.NewReader(prepared.Body))
		if err != nil {
			wd.stop()
			writeError(w, http.StatusInternalServerError, "gateway error", err.Error())
			h.recordAbort(model.ID, address, h.fingerprint(key))
			return
		}
		upstream.Header = prepared.Headers

		// The stage being bounded depends on the shape of the answer. A stream
		// sends its headers as soon as the provider starts answering, so a
		// silence before them is already evidence that something is wrong. A
		// single response sends nothing until it is complete, so the only thing
		// that can be bounded there is the answer itself.
		//
		// What counts is the shape ON THE WIRE, not the one the client asked
		// for: a provider may require the stream form regardless, and then the
		// headers do arrive at once.
		if req.Stream || prepared.StreamsUpstream {
			wd.arm(firstEvent, errFirstEvent)
		} else {
			wd.arm(idle, errNoAnswer)
		}

		resp, err := h.do(upstream, p, req.Model.ID)
		if err != nil {
			wd.stop()
			cause := wd.fired()

			if cause != nil {
				// The provider took the connection and then went quiet.
				// Nothing failed: the request was written into a path that no
				// longer exists, and no answer is coming. The path is suspect,
				// so the egress address is asked for again rather than waited
				// out — and a new attempt may well leave by another one.
				h.warn("provider %q: silent: %v", p.ID(), cause)
				h.refreshAddressFor(p.ID())
				if attempt < attempts && h.backoff(r.Context(), attempt) {
					continue
				}
				writeError(w, http.StatusGatewayTimeout, "timeout",
					"the provider did not answer: "+cause.Error())
				h.recordAbort(model.ID, address, h.fingerprint(key))
				return
			}

			// The provider is unreachable. This is NOT a provider refusal, and
			// presenting it as one is not allowed: the client must see a
			// transport failure. The same applies to the counters — a broken
			// path is not the provider refusing.
			h.warn("provider %q: connection: %v", p.ID(), err)
			if attempt < attempts && h.backoff(r.Context(), attempt) {
				continue
			}
			writeError(w, http.StatusBadGateway, "transport", "the provider is unreachable: "+err.Error())
			h.recordAbort(model.ID, address, h.fingerprint(key))
			return
		}

		// The status line is here: the wait for the headers is over in both
		// shapes, and what remains bounded is the silence between the pieces of
		// the answer.
		wd.arm(idle, errSilence)

		if resp.StatusCode < 400 {
			if req.Stream {
				h.passThroughStream(w, resp, prepared.ToolNames, wd, idle)
			} else {
				h.answerOnce(w, resp, prepared.ToolNames, wd, idle)
			}
			wd.stop()
			resp.Body.Close()
			if cause := wd.fired(); cause != nil {
				// The provider stopped speaking in the middle of the stream.
				// The client sees a truncated answer and asks again; counting
				// this as a success would hide it from the monitor exactly where
				// it costs most.
				h.warn("provider %q: stream cut short: %v", p.ID(), cause)
				h.recordAbort(model.ID, address, h.fingerprint(key))
				return
			}
			h.recordSuccess(model.ID, address, h.fingerprint(key))
			if rotated {
				h.settleAfterSuccess()
			}
			return
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		wd.stop()
		resp.Body.Close()
		if readErr != nil {
			// The provider refused, but the refusal itself did not arrive
			// intact. The client gets a transport failure, and the outcome is
			// recorded as an abort: no refusal reached anyone.
			h.warn("provider %q: reading the refusal: %v", p.ID(), readErr)
			if attempt < attempts && h.backoff(r.Context(), attempt) {
				continue
			}
			writeError(w, http.StatusBadGateway, "transport", "reading the response: "+readErr.Error())
			h.recordAbort(model.ID, address, h.fingerprint(key))
			return
		}

		failure := p.Classify(resp, body)

		// A verdict on the key is only meaningful when a key was sent.
		//
		// In anonymous mode the gateway substitutes an anonymity marker rather
		// than a credential, and a provider answers 401/403 to a bare request as
		// well. Reporting that as a rejected key is a lie in the log — and it
		// would spend the whole pool on a refusal that has nothing to do with
		// any credential.
		if key == "" {
			failure.KeyRejected = false
		}

		// The one refusal a repeat can answer: the allowance is spent on this
		// exit, and another exit has its own. The rotation is asked for before
		// the address is re-read, because the next attempt must leave from the
		// new node and not from the one whose counter is spent.
		//
		// The wait is here as well, and it is not decoration: the free lane also
		// counts a burst, measured on one exit that refused a dozen requests in
		// two minutes and answered the same address again a minute later. A new
		// exit alone does not answer that, and neither does a wait alone — the
		// exit whose day is spent stays spent.
		if attempt < attempts && h.rotationAllowed(p.ID()) && h.rotateAfterQuota(r.Context(), p.ID(), address, failure) && h.backoff(r.Context(), attempt) {
			rotated = true
			continue
		}

		// A refusal is about an address as much as it is about a key: the free
		// tier is limited per address, and the operator moves to another uplink
		// precisely because this one is spent. So the refusal is the one moment
		// where a stale answer costs something — the requests that follow would
		// be credited to the address they no longer leave from — and the address
		// is asked for again here instead of waiting out the TTL.
		h.refreshAddressFor(p.ID())

		// An overloaded provider or a busy edge is a refusal that a second
		// attempt from the same exit may survive. It is not the free lane's
		// quota, which does not move on its own and is handled above.
		if attempt < attempts && retryableRefusal(failure) && h.backoff(r.Context(), attempt) {
			continue
		}

		h.writeRefusal(w, resp, body, failure, p, model, address, key)
		return
	}
}

// excerpt renders at most n bytes of a body as a single log-safe line.
func excerpt(body []byte, n int) string {
	if len(body) > n {
		body = body[:n]
	}
	s := strings.TrimSpace(strings.Join(strings.Fields(string(body)), " "))
	if s == "" {
		return "(empty)"
	}
	return s
}

// writeRefusal passes the refusal to the client VERBATIM.
//
// This is where the package's central rule lives. Neither the status, nor the
// body, nor the headers change. The client looks in the body for the signals it
// decides by: `error.type`, `error.code`, nested codes, the upstream code marker
// in a rewritten message. Repackaging would deprive it of those signals, and the
// decision would diverge from the one the client would have made on a direct
// call.
//
// `retry-after` and `retry-after-ms` travel with the rest. They are the
// provider's own answer to "when may this be asked again", and the gateway is
// not the party that can improve on it: replacing the number would make the
// client wait a different time than the provider asked for, which is a
// divergence no proxy is entitled to introduce.
func (h *Handler) writeRefusal(w http.ResponseWriter, resp *http.Response, body []byte, failure provider.Failure, p provider.Provider, model provider.Model, address, key string) {
	for name, values := range resp.Header {
		lower := strings.ToLower(name)
		if hopByHop[lower] || lower == "content-length" {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	if w.Header().Get("content-type") == "" {
		w.Header().Set("content-type", "application/json")
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	h.recordRefusal(model.ID, address, h.fingerprint(key))
	h.warn("provider %q: refusal %d from %s, key rejected: %t: %s",
		p.ID(), failure.Status, address, failure.KeyRejected, excerpt(body, refusalExcerpt))
}

// hopByHop are the headers that belong to a single connection rather than to
// the response itself.
var hopByHop = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// do performs the request through the gateway's transport.
func (h *Handler) do(req *http.Request, p provider.Provider, model string) (*http.Response, error) {
	client := *h.resolveClient()

	// A provider that offers its own transport is answered by it. The order
	// matters: the provider's transport is more specific than the global one, and
	// asking the global transport to reach a stub's address would only produce a
	// name-resolution failure.
	if local, ok := p.(provider.Local); ok {
		if rt := local.RoundTripper(); rt != nil {
			client.Transport = rt
			resp, err := decode(client.Do(req))
			return translate(resp, err, p, model)
		}
	}

	var tf RoundTripperFactory
	if h.TransportFor != nil {
		tf = h.TransportFor(p.ID())
	}
	if tf == nil {
		tf = h.Transport
	}

	if tf != nil {
		if rt, err := tf.RoundTripper(); err == nil && rt != nil {
			client.Transport = rt
		}
	}
	resp, err := decode(client.Do(req))
	return translate(resp, err, p, model)
}

// translate hands a successful answer of a non-OpenAI service to its provider.
//
// WHY IT IS HERE AND NOT INSIDE THE PROVIDER. The provider knows its own dialect,
// but it does not own the response: the gateway passes the body through, watches
// the silence, collects a single answer out of a stream and hands a refusal to the
// client byte for byte. So the provider rewrites the body and everything else runs
// unchanged — one translation point, one real path.
//
// A REFUSAL IS NOT TRANSLATED. The status, body and headers belong to the client,
// which parses the service's own error there. Translation is for the answer that
// the client is about to read as its own.
func translate(resp *http.Response, err error, p provider.Provider, model string) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return resp, nil
	}
	t, ok := p.(provider.Transcoder)
	if !ok {
		return resp, nil
	}
	resp.Body = t.Transcode(resp.Body, model)
	// The bytes are the client's shape now, not the upstream's: the length and
	// the coding of the original no longer describe them, and a stale
	// content-length would cut the translated stream at the first event.
	resp.Header.Del("content-encoding")
	resp.Header.Del("content-length")
	resp.Header.Set("content-type", "text/event-stream")
	resp.ContentLength = -1
	return resp, nil
}

// decode undoes the compression the provider asked for.
//
// The provider sets Accept-Encoding itself, as the reference client does, and a
// Go transport decodes only what it negotiated itself: an explicit header
// switches that off. So a compressed body arrives as bytes nobody here can
// read, and the consequences are not cosmetic. A refusal is classified by the
// `error.type` inside its body, so a compressed one is classified blind — the
// key is blamed for the wrong reason, or nothing is blamed at all. The body is
// also the thing handed to the client VERBATIM, and it was being handed octets
// of gzip under `content-type: application/json` with the content-encoding
// header dropped: the client shows the client binary garbage instead of the
// message the provider wrote.
//
// The reference service compresses its JSON, refusals included, so this is the
// ordinary path and not an edge case. Every encoding the client offers is
// handled; the earlier gzip-only form was a visible difference from the client
// on the wire.
func decode(resp *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}

	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if encoding == "" || encoding == "identity" {
		return resp, nil
	}

	var reader io.Reader
	var decompressor io.Closer
	switch encoding {
	case "gzip":
		zr, zerr := gzip.NewReader(resp.Body)
		if zerr != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("content-encoding gzip: %w", zerr)
		}
		reader, decompressor = zr, zr
	case "deflate":
		zr, zerr := zlib.NewReader(resp.Body)
		if zerr != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("content-encoding deflate: %w", zerr)
		}
		reader, decompressor = zr, zr
	case "br":
		reader = brotli.NewReader(resp.Body)
	case "zstd":
		zr, zerr := zstd.NewReader(resp.Body)
		if zerr != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("content-encoding zstd: %w", zerr)
		}
		reader, decompressor = zr, zr.IOReadCloser()
	default:
		// A coding nobody asked for means a header this package did not write
		// reached the provider. Passing it through undecoded would hand the
		// client compressed octets labelled as plain text, which is worse than a
		// failure the caller can see.
		_ = resp.Body.Close()
		return nil, fmt.Errorf("unsupported content-encoding %q", encoding)
	}

	// The decompressed reader closes over the body, so the two are closed as
	// one: closing it alone would leave the connection to the provider open.
	resp.Body = &decodedBody{Reader: reader, decompressor: decompressor, body: resp.Body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return resp, nil
}

// decodedBody is a decompressed upstream body. Closing it releases both the
// reader that decompresses and the connection underneath.
type decodedBody struct {
	io.Reader
	decompressor io.Closer
	body         io.ReadCloser
}

func (b *decodedBody) Close() error {
	if b.decompressor != nil {
		_ = b.decompressor.Close()
	}
	return b.body.Close()
}

// passThroughStream passes the stream to the client.
//
// The watchdog is re-armed before every read, because the silence that matters
// is the one between two pieces of the answer. Without it a stream that stops
// arriving without closing reads as a healthy one: there is no error to return
// and the client waits for an end that will not come.
func (h *Handler) passThroughStream(w http.ResponseWriter, resp *http.Response, toolNames map[string]string, wd *watchdog, idle time.Duration) {
	if ct := resp.Header.Get("content-type"); ct != "" {
		w.Header().Set("content-type", ct)
	} else {
		w.Header().Set("content-type", "text/event-stream")
	}
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)

	// A rewritten stream is read by line rather than by buffer: an event has to
	// be whole before its tool name can be recognized, and a tool name split
	// across two reads would be passed through half-replaced.
	if len(toolNames) > 0 {
		reader := bufio.NewReader(resp.Body)
		for {
			wd.arm(idle, errSilence)
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := w.Write(rewriteEventLine(line, toolNames)); werr != nil {
					return
				}
				if canFlush {
					flusher.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}

	buf := make([]byte, 32<<10)
	for {
		wd.arm(idle, errSilence)
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			// The stream is handed out as it arrives: a delay until the full
			// response would make streaming pointless.
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// streamRequested reports whether the client asked for an event stream.
//
// Only a real boolean counts. Anything else — absent, null, a string — is the
// protocol's default, and the default is a single response: guessing at a truthy
// value here would send a client an answer whose shape it did not ask for.
func streamRequested(body map[string]any) bool {
	v, ok := body["stream"].(bool)
	return ok && v
}

// applyCredential substitutes the credentials into the assembled request.
//
// The provider assembles the headers but does NOT know which key was issued for
// this request: the keys are issued by the shared mechanism. That is why the
// header is set here — and the decision of where and in what shape it goes is the
// provider's, so the work itself is delegated.
func (h *Handler) applyCredential(p provider.Provider, prepared provider.Prepared, key string) {
	provider.ApplyCredential(p, prepared, key)
}

// warn writes a warning to the log, if there is one.
func (h *Handler) warn(format string, args ...any) {
	if h.Log != nil {
		h.Log.Warn(format, args...)
	}
}

// AddressResolver reports the address the daemon's traffic leaves from.
//
// An interface rather than a string field because resolving it costs a network
// round trip and the answer changes: the implementation caches it and refreshes
// it in the background.
type AddressResolver interface {
	// Address returns the egress address, or an empty string when it is not
	// known.
	Address() string
}

// AddressRefresher is a resolver that can be asked to re-resolve.
//
// Separate from AddressResolver rather than folded into it: a resolver that can
// only report what it already has is still a perfectly good resolver, and how
// often the answer is worth taking is not the gateway's decision to force.
type AddressRefresher interface {
	Refresh(ctx context.Context)
}

// address returns the egress address, or an empty string when unknowable.
// addressFor reports the egress address the provider's traffic leaves from.
//
// Per provider, because a deployment may send one provider out through the host
// and another through a tunnel: the two addresses are different answers to the
// same question, and the lanes a provider keeps are keyed by the right one.
func (h *Handler) addressFor(id provider.ID) string {
	r := h.addressesFor(id)
	if r == nil {
		return ""
	}
	return r.Address()
}

// addressesFor is the resolver that speaks for the provider's path.
func (h *Handler) addressesFor(id provider.ID) AddressResolver {
	if h.AddressesFor != nil {
		if r := h.AddressesFor(id); r != nil {
			return r
		}
	}
	return h.Addresses
}

// rotationAllowed reports whether Rotate moves the exit this provider uses.
func (h *Handler) rotationAllowed(id provider.ID) bool {
	if h.RotateFor == nil {
		return true
	}
	return h.RotateFor(id)
}

// refreshAddress asks for a fresh address WITHOUT waiting for it.
//
// Detached from the request on purpose. The client is owed the refusal, not a
// round trip to an echo endpoint, and the answer is not needed here either: the
// next request is the one that reads it, and that is the request a stale answer
// would have mislabelled.
func (h *Handler) refreshAddressFor(id provider.ID) {
	r := h.Addresses
	if h.AddressesFor != nil {
		if forProvider := h.AddressesFor(id); forProvider != nil {
			r = forProvider
		}
	}
	refresher, ok := r.(AddressRefresher)
	if !ok {
		return
	}
	go refresher.Refresh(context.Background())
}

// recordSuccess records a successful request.
func (h *Handler) recordSuccess(model, address, key string) {
	if h.State != nil {
		h.State.RecordSuccess(state.Outcome{Model: model, Address: address, Key: key})
	}
}

// recordRefusal records a refusal received from the provider.
func (h *Handler) recordRefusal(model, address, key string) {
	if h.State != nil {
		h.State.RecordRefusal(state.Outcome{Model: model, Address: address, Key: key})
	}
}

// recordAbort records a request that ended without an answer from the provider.
//
// Kept apart from a refusal on purpose. A client that gave up, an unreachable
// upstream or a failure inside the gateway are not the provider refusing
// anything, and the error column of the monitor is read as the provider's
// fault. Mixing them in would make the counters the client decides by — which
// are exactly the retry signals — report someone else's failure.
func (h *Handler) recordAbort(model, address, key string) {
	if h.State != nil {
		h.State.RecordAbort(state.Outcome{Model: model, Address: address, Key: key})
	}
}

// writeJSON answers with a JSON body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers with the OpenAI error envelope.
//
// The envelope is used ONLY for the gateway's own errors: an invalid request,
// an unreachable upstream, an unknown model. Provider refusals do not GO
// through it — they are passed on verbatim (see writeRefusal).
//
// type and code carry the same string: the client has one vocabulary to branch
// on, and a second, different one would force it to keep a mapping between
// them.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

// Timeout resolves the silence budget of one stage.
//
// The provider declares it itself: reasoning models hold pauses noticeably
// longer than ordinary ones, and a shared budget would tear their stream. A
// provider that declares nothing takes the configured value.
func Timeout(declared, fallback time.Duration) time.Duration {
	if declared > 0 {
		return declared
	}
	return fallback
}

// fingerprint returns a key fingerprint for the counters.
//
// The key itself never reaches the state: it is a secret, and a short
// fingerprint is enough to answer the question "what has been spent".
// fingerprint names a credential without writing it.
//
// It delegates to the capability package because the counters in the state file
// and the monitor that reads them compare these strings: two implementations
// that disagreed by a trimmed space would silently split one key into two.
func (h *Handler) fingerprint(key string) string {
	return capability.Fingerprint(key)
}
