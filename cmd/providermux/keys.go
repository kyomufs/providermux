package main

import (
	"os"
	"strings"
	"sync"

	"providermux/internal/capability"
	"providermux/internal/config"
	"providermux/internal/gateway"
	"providermux/internal/provider"
	"providermux/internal/state"
)

// keyRegistry holds the key rotation mechanisms per provider.
//
// A SEPARATE MECHANISM PER PROVIDER, not one shared. The implementation is
// identical, but the state is separate: having mixed the keys, the gateway would
// send one service's key in requests to another, and the usage counters would be
// attributed to the wrong one.
//
// The mechanism is attached only where keys exist at all. A provider without
// keys works anonymously, and that is a NORMAL mode, not a missing setting:
// services with a free tier are built exactly that way.
type keyRegistry struct {
	byProvider map[provider.ID]*capability.KeyRotation
	order      []provider.ID

	// path — the configuration file the pool is written back to. Empty means
	// there is no file to write: the CLI that only lists models builds a
	// registry for one read and never changes it.
	path string
	// log — where a write that could not be made is reported. Nil in the CLI,
	// where the error goes to the caller instead.
	log *state.Logger
	// mu serialises the read-modify-write of path. Two adds at once would both
	// read the old list and the second would drop the first.
	mu sync.Mutex
}

// newKeyRegistry assembles the mechanisms from the configuration.
//
// A provider's keys come from its section: first the value in the file, then the
// variable named alongside it. The fingerprints for reference are computed right
// here, so that the key itself does not travel further through the code.
func newKeyRegistry(cfg *config.Config, providers *provider.Registry, log *state.Logger) *keyRegistry {
	r := &keyRegistry{
		byProvider: make(map[provider.ID]*capability.KeyRotation),
		path:       cfg.Paths.ConfigFile,
		log:        log,
	}

	for _, p := range providers.All() {
		id := p.ID()
		pc, ok := cfg.Providers[string(id)]
		if !ok {
			continue
		}
		r.order = append(r.order, id)

		keys := dedupeKeys(id, keysFromConfig(pc))
		if len(keys) == 0 {
			// No keys: the mechanism is not created, and the provider
			// will run anonymously.
			continue
		}

		built, err := capability.NewKeyRotation(map[string]any{
			"keys": keys,
			// The rotation axis reaches the mechanism here, and only here: the
			// mechanism is where the answer is used, so this is the one place
			// that has to know it.
			"rotate": cfg.RotatesFor(string(id)),
		})
		if err != nil {
			continue // the settings shape has already been checked by the catalog
		}
		kr, ok := built.(*capability.KeyRotation)
		if !ok {
			continue
		}
		r.byProvider[id] = kr
	}
	return r
}

// keysFromConfig extracts a provider's list of keys.
func keysFromConfig(pc config.ProviderConfig) []any {
	// The primary source is the section fields. The list may also live in the
	// settings: that is more convenient when there are many keys and they are
	// kept separately.
	var raw []any
	if k := pc.ProviderKey(); k != "" {
		raw = append(raw, k)
	}
	if extra, ok := pc.Settings["keys"].([]any); ok {
		raw = append(raw, extra...)
	}
	if extra, ok := pc.Settings["keys"].([]string); ok {
		for _, k := range extra {
			raw = append(raw, k)
		}
	}
	return raw
}

// accountIdentity names the account a key belongs to.
//
// For most providers the key is its own identity. An Antigravity key may carry a
// project after a bar — `refresh_token|project_id` — but the project is a request
// parameter, not a second account: the same Google account connected twice
// differs only in it. Treating the whole string as identity is what produced the
// duplicate chip: the token from the environment had no project, the OAuth
// exchange appended one, and exact-string comparison saw two keys. The provider
// itself takes the project from its configuration, so collapsing the pair
// changes nothing on the wire.
func accountIdentity(id provider.ID, key string) string {
	key = strings.TrimSpace(key)
	if id != provider.ID("antigravity") {
		return key
	}
	if i := strings.IndexByte(key, '|'); i >= 0 {
		return key[:i]
	}
	return key
}

// dedupeKeys keeps one entry per account, first occurrence wins.
//
// The pool is built from two sources — the section field and the settings list —
// and the same account can arrive through both, or through the OAuth flow after
// it was already present. Within one build the second copy is dropped, so the
// panel does not show the same account twice and rotation does not spend a turn
// on it.
func dedupeKeys(id provider.ID, raw []any) []any {
	out := make([]any, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		identity := accountIdentity(id, s)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		out = append(out, s)
	}
	return out
}

// rotationMap hands out the map for the request surface.
func (r *keyRegistry) rotationMap() map[string]*capability.KeyRotation {
	out := make(map[string]*capability.KeyRotation, len(r.byProvider))
	for id, kr := range r.byProvider {
		out[string(id)] = kr
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstProvider returns the default provider.
//
// The first in configuration order: when the client did not name a provider, it
// is more reasonable to take a predictable one than a random one.
func (r *keyRegistry) firstProvider() provider.ID {
	if len(r.order) == 0 {
		return ""
	}
	return r.order[0]
}

// Keys implements gateway.KeyRotationRegistry.
func (r *keyRegistry) Keys(id provider.ID) []string {
	kr, ok := r.byProvider[id]
	if !ok {
		return nil
	}
	return kr.KeysSnapshot()
}

// KeyFingerprints implements gateway.KeyRotationRegistry.
//
// FINGERPRINTS are handed out, not keys: the reference is needed to learn how
// many keys are in use, not to see the secret. A fingerprint is short — enough
// to tell keys apart and not enough to reconstruct one.
func (r *keyRegistry) KeyFingerprints(id provider.ID) []string {
	kr, ok := r.byProvider[id]
	if !ok {
		return nil
	}
	keys := kr.KeysSnapshot()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fingerprint(k))
	}
	return out
}

// HasRotation implements gateway.KeyRotationRegistry.
//
// It answers whether the MECHANISM is wired in, not whether it currently has a
// second key: the two are different questions. A provider with one key still
// presents that key on every request, and reporting "no rotation" here used to
// make the control surface say the key was unused — a lie about the wire.
func (r *keyRegistry) HasRotation(id provider.ID) bool {
	_, ok := r.byProvider[id]
	return ok
}

// SetRotation implements the live-update side of rotation management.
//
// The mechanism keeps its own switch because Next must not repeat the policy
// check. A change made from the panel therefore has to reach the mechanism, or
// the axis would say "on" while every request kept leaving on the same key
// until the daemon was restarted.
func (r *keyRegistry) SetRotation(id provider.ID, rotate bool) bool {
	kr, ok := r.byProvider[id]
	if !ok {
		return false
	}
	kr.SetRotation(rotate)
	return true
}

// FindKey resolves a pool selector — key, fingerprint or one-based position.
//
// It mirrors the gateway's own lookup so that the panel asks the catalog
// through the account the request path would actually use.
func (r *keyRegistry) FindKey(id provider.ID, selector string) (string, bool) {
	kr, ok := r.byProvider[id]
	if !ok {
		return "", false
	}
	return kr.Find(selector)
}

// CatalogKey returns the key the catalog should be asked through.
//
// The same rule as the gateway and the panel: under the anonymous credential no
// key is presented at all; otherwise the pin wins, and without one the first
// key of the pool is used. Listing models must not spend a rotation turn, so the
// pool is only read.
func (r *keyRegistry) CatalogKey(cfg *config.Config, id provider.ID) string {
	if cfg == nil || cfg.CredentialFor(string(id)) == config.CredentialAnonymous {
		return ""
	}
	kr, ok := r.byProvider[id]
	if !ok {
		return ""
	}
	keys := kr.KeysSnapshot()
	if len(keys) == 0 {
		return ""
	}
	if pin := strings.TrimSpace(cfg.AccountFor(string(id))); pin != "" {
		if key, ok := kr.Find(pin); ok {
			return key
		}
	}
	return keys[0]
}

// AddKey implements gateway.KeyRotationRegistry.
//
// THE POOL IS WRITTEN TO THE CONFIGURATION, not kept in memory alone. An account
// added from the panel used to live only in the mechanism, so the next restart —
// a reboot, a `systemctl restart`, a NixOS switch that restarts the unit —
// rebuilt the pool from the file and the account was gone while the panel had
// reported it added. The file is the daemon's only store, and its loader already
// reads settings.keys, so the write and the read use one format.
//
// A key that cannot be written is taken back out: "added" has to mean "survives a
// restart", and leaving it in memory would recreate the very defect.
func (r *keyRegistry) AddKey(id provider.ID, key string) bool {
	kr, ok := r.byProvider[id]
	if !ok {
		return false
	}
	// The same account under a different project is not a second key. Letting it
	// in would leave the panel showing the account twice, and the operator has no
	// way to tell the copies apart — the reference is identical.
	identity := accountIdentity(id, key)
	for _, existing := range kr.KeysSnapshot() {
		if accountIdentity(id, existing) == identity {
			return false
		}
	}
	if !kr.Add(key) {
		return false
	}
	if r.path == "" {
		return true
	}
	if err := r.persistAdded(id, key); err != nil {
		kr.Remove(key)
		r.warnf("key for %q kept in memory only, not written to %s: %v", id, r.path, err)
		return false
	}
	return true
}

// RemoveFingerprint implements gateway.KeyRotationRegistry.
//
// Removal goes by fingerprint: the remover is not obliged to know the secret —
// what the reference showed is enough for it. The file is edited by the same
// rule, or the key would return at the next start.
func (r *keyRegistry) RemoveFingerprint(id provider.ID, fp string) bool {
	kr, ok := r.byProvider[id]
	if !ok {
		return false
	}
	for _, k := range kr.KeysSnapshot() {
		if fingerprint(k) != fp {
			continue
		}
		if !kr.Remove(k) {
			return false
		}
		if r.path == "" {
			return true
		}
		if err := r.persistRemoved(id, fp); err != nil {
			// Put back: the pool and the file must not disagree, or the key
			// returns at the next restart and the panel says it was removed.
			kr.Add(k)
			r.warnf("key %s for %q removed in memory only, %s unchanged: %v", fp, id, r.path, err)
			return false
		}
		return true
	}
	return false
}

// persistAdded appends a key to providers.<id>.settings.keys.
//
// The list, never the single `key` field: that field holds the one primary key,
// while a pool is what `keys` describes. Only the key just added is written — a
// value that came from the environment is not copied into the file, so a secret
// the operator keeps outside the configuration stays outside it.
func (r *keyRegistry) persistAdded(id provider.ID, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return editConfigDoc(r.path, func(doc map[string]any) error {
		settings := providerSettings(doc, string(id))
		list, _ := settings["keys"].([]any)
		for _, v := range list {
			if s, ok := v.(string); ok && strings.TrimSpace(s) == strings.TrimSpace(key) {
				return nil
			}
		}
		settings["keys"] = append(list, key)
		return nil
	})
}

// persistRemoved drops the keys with this fingerprint from the same list.
//
// By fingerprint rather than by equality: the operator hands back what the panel
// showed, and the panel shows fingerprints.
func (r *keyRegistry) persistRemoved(id provider.ID, fp string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return editConfigDoc(r.path, func(doc map[string]any) error {
		settings := providerSettings(doc, string(id))
		list, _ := settings["keys"].([]any)
		kept := make([]any, 0, len(list))
		for _, v := range list {
			if s, ok := v.(string); ok && fingerprint(s) == fp {
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == 0 {
			delete(settings, "keys")
			return nil
		}
		settings["keys"] = kept
		return nil
	})
}

// providerSettings returns the settings map of a provider section, creating the
// path on the way. It edits the document in place, so the caller has only to
// fill in the field it came for.
func providerSettings(doc map[string]any, id string) map[string]any {
	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		doc["providers"] = providers
	}
	section, _ := providers[id].(map[string]any)
	if section == nil {
		section = map[string]any{}
		providers[id] = section
	}
	settings, _ := section["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
		section["settings"] = settings
	}
	return settings
}

// warnf reports a failed write where the daemon's log is available.
func (r *keyRegistry) warnf(format string, args ...any) {
	if r.log != nil {
		r.log.Warn(format, args...)
	}
}

// fingerprint returns a short fingerprint of the key.
//
// It delegates to the capability package: the fingerprint printed here has to be
// the same string the counters and the monitor use, or an operator could not tell
// which key a refusal belongs to.
func fingerprint(key string) string {
	return capability.Fingerprint(key)
}

// Build-time check: the registry is obliged to satisfy the interface the control
// surface expects.
var _ gateway.KeyRotationRegistry = (*keyRegistry)(nil)

// expandKeyPlaceholders replaces `${NAME}` inside `settings.keys` with the value
// of the environment variable NAME.
//
// WHY. A declaration system writes the key list as NAMES, not values: the
// rendered configuration is world-readable, and the keys are handed to the
// daemon through the environment. A substitution in the declaration system
// would leave the dotted name in the file — an interpolation nobody reads back
// looks exactly like working configuration — so the expansion belongs HERE,
// where the key is about to be spent.
//
// Absent or empty variables are dropped with the placeholder: an empty string in
// the pool is a key that gets refused on every request. The daemon still refuses
// to start under the account credential with no keys at all, which is the exit
// that reports the real problem.
func expandKeyPlaceholders(cfg *config.Config) {
	for name, pc := range cfg.Providers {
		raw, ok := pc.Settings["keys"]
		if !ok {
			continue
		}
		var items []any
		switch v := raw.(type) {
		case []any:
			items = v
		case []string:
			for _, s := range v {
				items = append(items, s)
			}
		default:
			continue
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				continue
			}
			if value, isRef := envRef(s); isRef {
				if value == "" {
					continue
				}
				s = value
			}
			out = append(out, s)
		}
		pc.Settings["keys"] = out
		cfg.Providers[name] = pc
	}
}

// envRef resolves a whole-string `${NAME}` reference.
//
// The whole string must be the reference: a key that merely contains a `${`
// is a key, and rewriting part of it would silently send something else.
func envRef(s string) (string, bool) {
	if len(s) < 4 || s[0] != '$' || s[1] != '{' || s[len(s)-1] != '}' {
		return "", false
	}
	name := s[2 : len(s)-1]
	for i, r := range name {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return "", false
		}
	}
	return os.Getenv(name), true
}
