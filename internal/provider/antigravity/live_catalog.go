package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"providermux/internal/provider"
)

// The list of models is a property of the ACCOUNT, not of the service: a free
// account and a subscribed one are not offered the same routes. The provider
// therefore answers with two different things.
//
// The static table (catalog.go) is the CAPABILITY definition: which wire route a
// model takes, how many effort steps it has, what its context and output budget
// are. The live list is what this account may actually call. Neither replaces
// the other: an id that is live but has no table entry cannot be called — there
// is no route to build — and a table entry that is not live is not offered to
// the client. The two are intersected.
const (
	// liveCatalogPath is the route that answers with the models of one account.
	// The same route already serves the quota reader; here only the model keys
	// are read from it.
	liveCatalogPath = "/v1internal:fetchAvailableModels"
	// liveCatalogTTL is how long an answer is reused. The list changes when a
	// subscription changes, which is not a per-request event, while the panel
	// refreshes every twenty seconds — asking the service each time would spend
	// the account's traffic to draw the same table.
	liveCatalogTTL = 5 * time.Minute
	// liveCatalogMaxBytes bounds the answer. The reference client rejects more
	// than 512 model entries; the byte bound is what keeps a wrong address from
	// streaming into memory.
	liveCatalogMaxBytes = 512 << 10
)

// liveCatalog caches the ids per account.
//
// Keyed by the account fingerprint rather than by the key itself: the cache is
// long-lived, and a secret kept in a map for minutes is a secret that outlives
// its use.
type liveCatalog struct {
	mu      sync.Mutex
	entries map[string]liveCatalogEntry
}

type liveCatalogEntry struct {
	ids []string
	at  time.Time
}

func newLiveCatalog() *liveCatalog {
	return &liveCatalog{entries: make(map[string]liveCatalogEntry)}
}

func (c *liveCatalog) get(account string, now time.Time) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[account]
	if !ok || now.Sub(entry.at) > liveCatalogTTL {
		return nil, false
	}
	return entry.ids, true
}

func (c *liveCatalog) put(account string, ids []string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[account] = liveCatalogEntry{ids: ids, at: now}
}

// ModelsForAccount returns the catalog as one account sees it.
//
// A failed call falls back to the static table instead of an empty list: the
// panel is how the operator notices a broken account, and an empty panel would
// hide the provider rather than the fault.
func (p *Provider) ModelsForAccount(ctx context.Context, key string) ([]provider.Model, error) {
	// No key is not an error: it means "nobody asked on an account's behalf",
	// and the default list is the honest answer.
	if key == "" {
		return p.Models(ctx)
	}

	ids, err := p.liveModelIDs(ctx, key)
	if err != nil {
		p.logf("antigravity: live catalog for %s unavailable: %v", accountFingerprint(key), err)
		return p.Models(ctx)
	}

	live := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		live[strings.ToLower(id)] = struct{}{}
	}

	out := make([]provider.Model, 0, len(ids))
	known := make(map[string]bool, len(ids))
	for _, def := range modelTable() {
		if !def.isLive(live) {
			continue
		}
		out = append(out, def.providerModel())
		for _, wire := range def.wireNames() {
			known[strings.ToLower(wire)] = true
		}
		known[strings.ToLower(def.id)] = true
	}

	// An id the table does not know is not silently dropped: it is a model the
	// operator can be offered after the table learns its route, and saying so is
	// the only way that ever happens.
	for _, id := range ids {
		if !known[strings.ToLower(id)] {
			p.logf("antigravity: account %s sees model %q, which the table has no route for", accountFingerprint(key), id)
		}
	}

	return out, nil
}

// isLive reports whether the account was offered this model.
//
// The live ids are wire names, and a model with tiers appears under the tier
// routes as well ("gemini-4-argon-low"). Any of them means the model itself is
// available.
func (def modelDef) isLive(live map[string]struct{}) bool {
	if _, ok := live[strings.ToLower(def.id)]; ok {
		return true
	}
	for _, wire := range def.wireNames() {
		if _, ok := live[strings.ToLower(wire)]; ok {
			return true
		}
	}
	for id := range live {
		if strings.HasPrefix(id, strings.ToLower(def.id)+"-") {
			return true
		}
	}
	return false
}

// wireNames returns every route name this model can be asked for.
func (def modelDef) wireNames() []string {
	out := make([]string, 0, len(def.tiers))
	for _, route := range def.tiers {
		if route.wire != "" {
			out = append(out, route.wire)
		}
	}
	return out
}

// liveModelIDs reads the ids of the models this account may use.
func (p *Provider) liveModelIDs(ctx context.Context, key string) ([]string, error) {
	account := accountFingerprint(key)
	if ids, ok := p.live.get(account, time.Now()); ok {
		return ids, nil
	}

	access := p.tokens.access(key)
	if access == "" {
		return nil, fmt.Errorf("no access token for account %s", account)
	}

	ids, err := p.fetchLiveModelIDs(ctx, access)
	if err != nil {
		return nil, err
	}
	// The count is what tells an operator that the account answers with its own
	// list rather than with the table every account gets.
	p.logf("antigravity: account %s offers %d models", account, len(ids))
	p.live.put(account, ids, time.Now())
	return ids, nil
}

// fetchLiveModelIDs asks the service once.
func (p *Provider) fetchLiveModelIDs(ctx context.Context, access string) ([]string, error) {
	client := p.cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	// The project is sent when there is one and dropped when the service says it
	// does not belong to the account: an account created without a project
	// answers 403 to a body that names one. The quota reader does the same.
	body := map[string]string{}
	if project := p.cfg.Project; project != "" {
		body["project"] = project
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	endpoints := make([]string, 0, 2)
	if p.baseURL != "" {
		endpoints = append(endpoints, p.baseURL)
	}
	for _, base := range quotaEndpoints {
		if base != p.baseURL {
			endpoints = append(endpoints, base)
		}
	}

	var lastErr error
	for _, base := range endpoints {
		ids, err := p.readLiveModelIDs(ctx, client, base, access, encoded)
		if err == nil {
			return ids, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (p *Provider) readLiveModelIDs(ctx context.Context, client *http.Client, base, access string, body []byte) ([]string, error) {
	url := strings.TrimRight(base, "/") + liveCatalogPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setAntigravityHeaders(req, access, p.ua)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, liveCatalogMaxBytes))
	if err != nil {
		return nil, err
	}

	// A project the account does not own is refused, and the list itself is not
	// project-scoped: asking again without it is what the reference client does.
	if resp.StatusCode == http.StatusForbidden && len(body) > 2 {
		return p.readLiveModelIDs(ctx, client, base, access, []byte("{}"))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d: %s", url, resp.StatusCode, excerpt(payload, 200))
	}

	return parseLiveModelIDs(payload)
}

// parseLiveModelIDs reads the keys of the models object.
//
// The answer comes either wrapped in "response" or bare; the shape has been seen
// both ways, and a provider that only accepted one of them would go empty on a
// service change that is not about the models at all.
func parseLiveModelIDs(payload []byte) ([]string, error) {
	var envelope struct {
		Response *struct {
			Models map[string]json.RawMessage `json:"models"`
		} `json:"response"`
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decoding model list: %w", err)
	}
	models := envelope.Models
	if envelope.Response != nil && len(envelope.Response.Models) > 0 {
		models = envelope.Response.Models
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("model list is empty")
	}

	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	return ids, nil
}

// excerpt shortens a refusal for the log.
func excerpt(body []byte, limit int) string {
	text := strings.TrimSpace(string(body))
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// logf writes to the provider's log, if there is one.
func (p *Provider) logf(format string, args ...any) {
	if p.cfg.Log != nil {
		p.cfg.Log(format, args...)
	}
}
