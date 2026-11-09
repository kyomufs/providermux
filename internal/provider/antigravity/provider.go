// Package antigravity implements the Google Antigravity provider.
//
// WHAT IS SPECIFIC TO IT and therefore lives here:
//
//   - the credentials: an OAuth access token minted from a refresh token, so the
//     value the shared machinery calls "a key" is a refresh token and the access
//     token is derived from it here;
//   - the body: the service takes a request/response envelope of its own
//     (`v1internal`), not an OpenAI body, and the model name carries the thinking
//     tier in it;
//   - the answer: SSE events of the Gemini shape (`candidates[].content.parts`),
//     which are translated into OpenAI chunks by Transcode;
//   - the identity: the service recognises the client by its User-Agent, so the
//     string is part of the contract.
//
// WHAT IS NOT HERE and must not be: retry policy, key rotation, transport. That
// is common machinery — see internal/capability and internal/transport. A pool of
// refresh tokens is rotated by the same code that rotates any other provider's
// keys: the gateway does not know that these keys need minting.
//
// The source of the contract is dsh-tool-antigravity 0.5.3 and the client package
// it depends on (@cortexkit/antigravity-auth-core 3.0.14), read from source:
//
//	endpoints       src/llm-adapter.ts:76-78
//	body envelope   src/llm-adapter.ts (buildPayloadFromContents)
//	native table    dist/model-registry.js (AGY_NATIVE_MODEL_DEFINITIONS)
//	thinking        dist/transform/agy-generation.js, dist/transform/gemini.js
//	stream shape    src/llm-adapter.ts (findParts)
//	user agent      dist/fingerprint.js (buildAntigravityHarnessUserAgent)
//	oauth refresh   src/credential-coordinator.ts:8-9, 598-640
package antigravity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"providermux/internal/provider"
)

// DefaultBaseURL is the address of the model endpoints.
//
// The daily endpoint is the one the reference client uses; the production one
// answers the same routes under a different address and is the fallback of the
// same client. Overridable, because a deployment that mirrors the service names
// its own address.
const DefaultBaseURL = "https://daily-cloudcode-pa.googleapis.com"

// streamPath is the path of the streaming chat route.
//
// The stream form is the only one used here even for a client that asked for a
// single response: the service streams, and the gateway collects the events into
// one body when the client asked for one. A second, non-streaming path would be a
// second shape to keep in step for no gain.
const streamPath = "/v1internal:streamGenerateContent?alt=sse"

// DefaultUserAgent is the client identity the service recognises.
//
// It is not decoration: traffic without it is answered as an unknown client. The
// version and the change list are the parts the service reads, so they are copied
// verbatim from the client's own fingerprint.
//
// Source: @cortexkit/antigravity-auth-core 3.0.14, dist/fingerprint.js
// (AGY_CLI_VERSION 1.3.5, AGY_CLI_CHANGE_LIST 998863838).
const DefaultUserAgent = "antigravity/cli/1.3.5 (aidev_client; os_type=linux; arch=amd64; cl=998863838; auth_method=consumer)"

// ID is the provider identifier when the configuration names no other.
//
// The section's name is the route name, so a deployment with several accounts
// gives each its own section and the default is only the fallback.
const ID provider.ID = "antigravity"

// Config is the provider settings, parsed from the gateway configuration.
type Config struct {
	// ID is the route name of this section.
	ID provider.ID
	// BaseURL overrides the service address.
	BaseURL string
	// Project is the Google Cloud project the requests are filed under.
	//
	// Empty omits the field: the token carries its own project, and the reference
	// client omits it as well for the accounts that have none. The setting exists
	// for the case the service starts asking for it explicitly.
	Project string
	// UserAgent overrides the client identity.
	UserAgent string
	// ClientID and ClientSecret are the OAuth application the refresh tokens were
	// issued to.
	//
	// The defaults are the public application of the reference client, which is
	// what the tokens in its own store belong to. A deployment that registers its
	// own application sets both.
	ClientID     string
	ClientSecret string
	// HTTP is the client for token refreshes and nothing else: model requests go
	// over the gateway's transport, like every other provider's.
	HTTP *http.Client
	// Log receives the refresh warnings. May be nil.
	Log func(format string, args ...any)

	// FirstEventTimeout, IdleTimeout and ReasoningIdleTimeout are the silence
	// budgets carried into every prepared request.
	FirstEventTimeout    time.Duration
	IdleTimeout          time.Duration
	ReasoningIdleTimeout time.Duration
}

// Provider is the provider.Provider implementation for Antigravity.
type Provider struct {
	cfg     Config
	id      provider.ID
	baseURL string
	ua      string
	tokens  *tokenCache
}

// New creates a provider from configuration.
func New(cfg Config) (*Provider, error) {
	p := &Provider{cfg: cfg}
	p.id = cfg.ID
	if p.id == "" {
		p.id = ID
	}
	p.baseURL = strings.TrimRight(cfg.BaseURL, "/")
	if p.baseURL == "" {
		p.baseURL = DefaultBaseURL
	}
	p.ua = cfg.UserAgent
	if p.ua == "" {
		p.ua = DefaultUserAgent
	}
	p.tokens = newTokenCache(cfg)
	return p, nil
}

// ID implements provider.Provider.
func (p *Provider) ID() provider.ID { return p.id }

// Credential implements provider.CredentialScheme.
//
// The shared machinery issues "keys"; for this service a key is a REFRESH token,
// and the header it goes into is not a place a refresh token can go. So the value
// is exchanged for an access token here, at the moment of sending, and cached
// until shortly before it expires.
//
// An empty key returns an empty value and no header is sent: that is the
// anonymous mode, and the service answers it with a refusal of its own rather
// than with a request that pretends to be authenticated.
func (p *Provider) Credential(key string) string {
	if key == "" {
		return ""
	}
	return p.tokens.access(key)
}

// Build implements provider.Provider.
//
// The body is the service's own envelope. Two consequences are worth naming:
//
// The model name on the wire carries the effort, so the effort is resolved into
// the route rather than sent beside it (see catalog.go).
//
// The stream form is always requested. The gateway collects the events into one
// body for a client that asked for a single response, so the shape the client
// sees stays the one it requested.
func (p *Provider) Build(req provider.Request) (provider.Prepared, error) {
	def, ok := lookup(req.Model.ID)
	if !ok {
		return provider.Prepared{}, fmt.Errorf("antigravity: unknown model %q", req.Model.ID)
	}
	route := def.route(req.ReasoningEffort)

	contents := buildContents(req, def.claude)
	// The conversation identifier is the client's own when it sent one: the
	// service files the turns of one conversation under it, and a fresh value per
	// request would start a conversation at every turn.
	conversation := req.Session.Session
	if conversation == "" {
		conversation = mintID()
	}
	trajectory := mintID()

	request := map[string]any{
		"contents": contents,
		"labels": map[string]any{
			"last_step_index":          0,
			"model_enum":               route.enum,
			"trajectory_id":            trajectory,
			"used_claude":              def.claude,
			"used_claude_conservative": def.claude,
			"used_non_gemini_model":    def.claude || strings.HasPrefix(route.wire, "gpt-oss"),
		},
		"generationConfig": generationConfig(req, def, route),
		// The service files the conversation under this identifier and uses it
		// for its own cache routing, so it has to be stable across the turns of
		// one workspace. It is a signed 64-bit hash of the workspace URI as a
		// decimal string, which is what the reference client sends.
		"sessionId": signedSessionID(req.Session.Project),
	}
	if system := strings.TrimSpace(req.System); system != "" {
		request["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": system}},
		}
	}
	if tools := buildTools(req.Tools); tools != nil {
		request["tools"] = tools
	}

	body := map[string]any{
		"requestId":   fmt.Sprintf("agent/%s/%d/%s/1", conversation, time.Now().UnixMilli(), trajectory),
		"request":     request,
		"model":       route.wire,
		"userAgent":   "antigravity",
		"requestType": "agent",
	}
	if p.cfg.Project != "" {
		body["project"] = p.cfg.Project
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return provider.Prepared{}, fmt.Errorf("antigravity: encoding body: %w", err)
	}

	headers := http.Header{}
	headers.Set("content-type", "application/json")
	headers.Set("accept-encoding", "gzip")
	headers.Set("user-agent", p.ua)

	return provider.Prepared{
		URL:               p.baseURL + streamPath,
		Headers:           headers,
		Body:              raw,
		StreamsUpstream:   true,
		IdleTimeout:       p.idleBudget(def),
		FirstEventTimeout: p.cfg.FirstEventTimeout,
	}, nil
}

// idleBudget returns the silence budget for an answer of this model.
//
// Every route of this service thinks before it answers, and a high route thinks
// for minutes. The gateway-wide budget would cut an answer that is on its way, so
// the reasoning budget takes over whenever it is set.
func (p *Provider) idleBudget(modelDef) time.Duration {
	if p.cfg.ReasoningIdleTimeout > 0 {
		return p.cfg.ReasoningIdleTimeout
	}
	return p.cfg.IdleTimeout
}

// mintID returns a random identifier in the shape the service expects of a
// trajectory: an opaque token, not a UUID with punctuation.
func mintID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("trajectory-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
