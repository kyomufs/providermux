package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"providermux/internal/capability"
	"providermux/internal/config"
	"providermux/internal/provider"
)

// UpdateOptionRequest defines an on-the-fly option modification request.
type UpdateOptionRequest struct {
	Axis     string `json:"axis"`
	Provider string `json:"provider,omitempty"`
	Value    string `json:"value"`
}

// CurrentOptions reflects the live configuration values in force.
type CurrentOptions struct {
	Mode                 string                    `json:"mode"`
	Credential           string                    `json:"credential"`
	KeyRotation          string                    `json:"key_rotation"`
	ModelFallback        string                    `json:"model_fallback"`
	Family               string                    `json:"family"`
	Transport            string                    `json:"transport"`
	FirstEventTimeout    string                    `json:"first_event_timeout"`
	IdleTimeout          string                    `json:"idle_timeout"`
	ReasoningIdleTimeout string                    `json:"reasoning_idle_timeout"`
	Providers            map[string]ProviderOption `json:"providers"`
}

// ProviderOption reflects live options for a specific provider.
type ProviderOption struct {
	Credential  string `json:"credential"`
	KeyRotation string `json:"key_rotation"`
	Transport   string `json:"transport"`
	// Account is the pinned key of this provider's pool (a fingerprint or a
	// position). Empty means the pool rotates on its own.
	Account string `json:"account"`
}

func (h *Handler) currentOptions() *CurrentOptions {
	if h.Config == nil {
		return nil
	}
	snap := h.Config.Snapshot()
	cur := &CurrentOptions{
		Mode:                 snap.Mode,
		Credential:           snap.Credential,
		KeyRotation:          snap.KeyRotation,
		ModelFallback:        snap.ModelFallback,
		Family:               snap.Family,
		Transport:            snap.Transport,
		FirstEventTimeout:    snap.FirstEventTimeout.String(),
		IdleTimeout:          snap.IdleTimeout.String(),
		ReasoningIdleTimeout: snap.ReasoningIdleTimeout.String(),
		Providers:            map[string]ProviderOption{},
	}
	if h.Providers != nil {
		for _, id := range h.Providers.IDs() {
			sID := string(id)
			pc := snap.Providers[sID]
			cur.Providers[sID] = ProviderOption{
				Credential:  pc.EffectiveCredential(snap.Credential),
				KeyRotation: pc.EffectiveRotation(snap.KeyRotation),
				Transport:   pc.EffectiveTransport(snap.Transport),
				Account:     pc.Account,
			}
		}
	}
	return cur
}

// handleOptionsUpdate processes on-the-fly changes to settings.
func (h *Handler) handleOptionsUpdate(w http.ResponseWriter, r *http.Request) {
	var req UpdateOptionRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}

	if req.Axis == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "axis is required"})
		return
	}
	// An empty value is clearing a setting, and only the axis that can be absent
	// allows it: a credential or a transport has no "none" state.
	if req.Value == "" && req.Axis != "account" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "value is required"})
		return
	}

	if h.Config == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration is not loaded"})
		return
	}

	var (
		apply   func(*config.Config)
		persist func() error
		status  int
		message string
		updated string
	)

	// The validation and the reading of the current entry happen under the same
	// lock as the write. Without it the panel could change a provider's
	// credential while a request was reading it, which is a data race the race
	// detector rightly reports.
	h.Config.Update(func() {
		if req.Provider != "" {
			pID := provider.ID(req.Provider)
			if h.Providers != nil {
				if _, err := h.Providers.Get(pID); err != nil {
					status, message = http.StatusBadRequest, fmt.Sprintf("unknown provider %q", req.Provider)
					return
				}
			}

			if h.Config.Providers == nil {
				h.Config.Providers = map[string]config.ProviderConfig{}
			}
			pc := h.Config.Providers[req.Provider]
			updated = req.Value

			switch req.Axis {
			case "credential":
				value, ok := canonical(config.CredentialValues(), req.Value)
				if !ok {
					status, message = http.StatusBadRequest, fmt.Sprintf("invalid credential value %q", req.Value)
					return
				}
				if config.CredentialUnsupported(config.ProviderType(req.Provider, pc), value) {
					status, message = http.StatusBadRequest, fmt.Sprintf("provider %q does not support credential %q", req.Provider, value)
					return
				}
				pc.Credential, updated = value, value
			case "key_rotation":
				value, ok := canonical(config.RotationValues(), req.Value)
				if !ok {
					status, message = http.StatusBadRequest, fmt.Sprintf("invalid key_rotation value %q", req.Value)
					return
				}
				pc.KeyRotation, updated = value, value
			case "transport":
				value := req.Value
				if len(h.Transports) > 0 {
					canonicalValue, ok := canonical(h.Transports, req.Value)
					if !ok {
						status, message = http.StatusBadRequest, fmt.Sprintf("invalid transport value %q", req.Value)
						return
					}
					value = canonicalValue
				}
				pc.Transport, updated = value, value
			case "account":
				// A pin names one key of this provider's pool: a fingerprint or a
				// pool position. An empty value clears it, and the pool rotates as
				// it did before. A value that is not in the pool is refused here
				// rather than at request time, where it would read as a stale pin.
				if req.Value != "" && !h.accountInPool(req.Provider, req.Value) {
					status, message = http.StatusBadRequest, fmt.Sprintf("unknown account %q for provider %q", req.Value, req.Provider)
					return
				}
				pc.Account = req.Value
			default:
				status, message = http.StatusBadRequest, fmt.Sprintf("unsupported provider axis %q", req.Axis)
				return
			}

			prov := req.Provider
			apply = func(c *config.Config) { c.Providers[prov] = pc }
			if path := h.Config.Paths.ConfigFile; path != "" {
				persist = func() error { return persistProviderOption(path, prov, req.Axis, updated) }
			}
			return
		}

		// Global axis update.
		switch req.Axis {
		case "mode":
			value, ok := canonical(config.ModeValues(), req.Value)
			if !ok {
				status, message = http.StatusBadRequest, fmt.Sprintf("invalid mode %q", req.Value)
				return
			}
			// A live switch to server mode is refused without a token: the
			// wrapper that guards both surfaces would otherwise start asking for
			// a secret that does not exist, and the daemon would be unreachable
			// through the very API that changed it.
			if value == config.ModeServer && strings.TrimSpace(h.Config.Token.Val) == "" {
				status, message = http.StatusBadRequest, "server mode requires a token: set it in the file or pass it in the environment"
				return
			}
			updated = value
			apply = func(c *config.Config) { c.Mode.Set(value, config.SourceFile) }
		case "credential":
			value, ok := canonical(config.CredentialValues(), req.Value)
			if !ok {
				status, message = http.StatusBadRequest, fmt.Sprintf("invalid credential %q", req.Value)
				return
			}
			updated = value
			apply = func(c *config.Config) { c.Credential.Set(value, config.SourceFile) }
		case "key_rotation":
			value, ok := canonical(config.RotationValues(), req.Value)
			if !ok {
				status, message = http.StatusBadRequest, fmt.Sprintf("invalid key_rotation %q", req.Value)
				return
			}
			updated = value
			apply = func(c *config.Config) { c.KeyRotation.Set(value, config.SourceFile) }
		case "model_fallback":
			value, ok := canonical(config.FallbackValues(), req.Value)
			if !ok {
				status, message = http.StatusBadRequest, fmt.Sprintf("invalid model_fallback %q", req.Value)
				return
			}
			updated = value
			apply = func(c *config.Config) { c.ModelFallback.Set(value, config.SourceFile) }
		case "family":
			value, ok := canonical(config.FamilyValues(), req.Value)
			if !ok {
				status, message = http.StatusBadRequest, fmt.Sprintf("invalid family %q", req.Value)
				return
			}
			updated = value
			apply = func(c *config.Config) { c.Family.Set(value, config.SourceFile) }
		case "transport":
			value := req.Value
			if len(h.Transports) > 0 {
				canonicalValue, ok := canonical(h.Transports, req.Value)
				if !ok {
					status, message = http.StatusBadRequest, fmt.Sprintf("invalid transport %q", req.Value)
					return
				}
				value = canonicalValue
			}
			updated = value
			apply = func(c *config.Config) { c.Transport.Set(value, config.SourceFile) }
		default:
			status, message = http.StatusBadRequest, fmt.Sprintf("unsupported global axis %q", req.Axis)
			return
		}

		if path := h.Config.Paths.ConfigFile; path != "" {
			persist = func() error { return persistGlobalOption(path, req.Axis, updated) }
		}
	})

	if message != "" {
		writeJSON(w, status, map[string]any{"error": message})
		return
	}

	// The file is written BEFORE the live value changes. A rejected write must
	// not leave the daemon running a setting the next start would not have, and
	// the operator is told the change did not happen.
	if persist != nil {
		if err := persist(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "the change was not saved: " + err.Error(),
			})
			return
		}
	}

	if apply != nil {
		h.Config.Update(func() { apply(h.Config) })
	}

	// Rotation is held by the mechanism as well as by the configuration. A live
	// change has to reach both, otherwise the axis says "on" while the mechanism
	// keeps the value it was built with.
	if req.Axis == "key_rotation" || req.Axis == "credential" {
		h.syncRotation()
	}

	h.info("axis %s updated to %s (provider %q)", req.Axis, updated, req.Provider)
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": true,
		"axis":    req.Axis,
		"value":   updated,
		"current": h.currentOptions(),
	})
}

// syncRotation brings the mechanisms' switches in line with the axes.
func (h *Handler) syncRotation() {
	setter, ok := h.KeyRotation.(interface {
		SetRotation(provider.ID, bool) bool
	})
	if !ok || h.Providers == nil || h.Config == nil {
		return
	}
	snap := h.Config.Snapshot()
	for _, id := range h.Providers.IDs() {
		pc := snap.Providers[string(id)]
		setter.SetRotation(id, pc.Rotates(snap.Credential, snap.KeyRotation))
	}
}

// canonical returns the allowed spelling of a value.
//
// The comparison is case-insensitive because an operator types "ON", but the
// stored value is the one the rest of the code compares against. Keeping the
// original spelling was the bug: "Account" was accepted and then failed every
// equality check that looked for "account".
func canonical(values []string, val string) (string, bool) {
	for _, item := range values {
		if strings.EqualFold(item, val) {
			return item, true
		}
	}
	return "", false
}

// readConfigDoc reads the raw configuration document.
//
// A document that does not parse is an ERROR, not an empty starting point. The
// file can hold settings that exist nowhere else, and overwriting it after a
// failed parse would silently discard every one of them.
func readConfigDoc(path string) (map[string]any, error) {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, fmt.Errorf("config: parsing %s: %w", path, err)
			}
		}
	case os.IsNotExist(err):
		// No file yet: the new one is created from nothing.
	default:
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	return doc, nil
}

func persistGlobalOption(path, key, value string) error {
	doc, err := readConfigDoc(path)
	if err != nil {
		return err
	}
	doc[key] = value
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return writeAtomic(path, out)
}

func persistProviderOption(path, prov, key, value string) error {
	doc, err := readConfigDoc(path)
	if err != nil {
		return err
	}
	provs, ok := doc["providers"].(map[string]any)
	if !ok || provs == nil {
		provs = map[string]any{}
		doc["providers"] = provs
	}
	pMap, ok := provs[prov].(map[string]any)
	if !ok || pMap == nil {
		pMap = map[string]any{}
		provs[prov] = pMap
	}
	// An empty value means the setting is absent, not that it is present and
	// empty: a written "" would be read back as a value by every reader that
	// does not special-case it.
	if value == "" {
		delete(pMap, key)
	} else {
		pMap[key] = value
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return writeAtomic(path, out)
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// The file holds provider keys, so it is owner-only: CreateTemp already uses
	// 0600, and the chmod makes that contract explicit rather than incidental.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Sync before rename: a rename that reaches the directory before the data
	// reaches the disk can leave an empty file after a power loss.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

// accountInPool reports whether value names a key of this provider's pool.
//
// A pin is written in the same three forms the gateway accepts for a selector —
// the key itself, its fingerprint, or a one-based position — so the operator can
// carry a value from the quota card or from the keys list without learning a
// fourth notation.
func (h *Handler) accountInPool(prov, value string) bool {
	if h.KeyRotation == nil {
		return false
	}
	id := provider.ID(prov)
	keys := h.KeyRotation.Keys(id)
	fingerprints := h.KeyRotation.KeyFingerprints(id)
	for i, key := range keys {
		if key == value {
			return true
		}
		if i < len(fingerprints) && fingerprintMatches(fingerprints[i], value) {
			return true
		}
		if strconv.Itoa(i+1) == value {
			return true
		}
	}
	return false
}

// fingerprintMatches accepts either the short fingerprint the counters print or
// a longer one built from the same digest, such as the account id on a quota
// card. Comparing a prefix keeps one identifier usable in both places instead of
// asking the operator to translate between two notations.
func fingerprintMatches(short, value string) bool {
	short = strings.ToLower(strings.TrimSpace(short))
	value = strings.ToLower(strings.TrimSpace(value))
	if short == "" || value == "" {
		return false
	}
	return short == value || strings.HasPrefix(value, short)
}

// catalogKey returns the key the panel should ask the catalog through.
//
// The catalog belongs to an account: a pinned account answers with its own
// models, and an unpinned provider is asked through the first key of its pool —
// the same rule the gateway uses, so the panel and /v1/models never disagree
// about what the provider offers.
func (h *Handler) catalogKey(id provider.ID) string {
	if h.KeyRotation == nil {
		return ""
	}
	keys := h.KeyRotation.Keys(id)
	if len(keys) == 0 {
		return ""
	}
	if h.Config != nil {
		if pin := strings.TrimSpace(h.Config.AccountFor(string(id))); pin != "" {
			// The pool understands all three selector forms — key, fingerprint
			// and one-based position. Using the same lookup keeps a position pin
			// from silently reading the first account's catalog.
			if finder, ok := h.KeyRotation.(interface {
				FindKey(provider.ID, string) (string, bool)
			}); ok {
				if key, found := finder.FindKey(id, pin); found {
					return key
				}
			}
			for _, key := range keys {
				if fingerprintMatches(capability.Fingerprint(key), pin) || key == pin {
					return key
				}
			}
		}
	}
	return keys[0]
}
