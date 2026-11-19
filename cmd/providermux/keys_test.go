package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"providermux/internal/capability"
	"providermux/internal/config"
	"providermux/internal/provider"
)

// newRotation builds a mechanism from a list of keys.
func newRotation(t *testing.T, keys ...string) *capability.KeyRotation {
	t.Helper()
	raw := make([]any, 0, len(keys))
	for _, key := range keys {
		raw = append(raw, key)
	}
	built, err := capability.NewKeyRotation(map[string]any{"keys": raw, "rotate": true})
	if err != nil {
		t.Fatalf("build the rotation: %v", err)
	}
	kr, ok := built.(*capability.KeyRotation)
	if !ok {
		t.Fatalf("unexpected mechanism type %T", built)
	}
	return kr
}

// TestHasRotationMeansTheMechanismIsWired.
//
// The interface asks whether rotation is wired in, not whether the pool happens
// to hold a second key. A provider with one key still sends that key on every
// request; reporting "no rotation" made the control surface call the key unused.
func TestHasRotationMeansTheMechanismIsWired(t *testing.T) {
	reg := &keyRegistry{byProvider: map[provider.ID]*capability.KeyRotation{
		"one": newRotation(t, "only"),
	}}

	if !reg.HasRotation("one") {
		t.Fatal("a wired mechanism with one key reported no rotation")
	}
	if reg.HasRotation("absent") {
		t.Fatal("an absent mechanism reported rotation")
	}
}

// TestSetRotationReachesTheMechanism — a live axis change has to reach the
// object that makes the decision, not only the configuration field.
func TestSetRotationReachesTheMechanism(t *testing.T) {
	kr := newRotation(t, "a", "b")
	reg := &keyRegistry{byProvider: map[provider.ID]*capability.KeyRotation{"p": kr}}

	if !reg.SetRotation("p", false) {
		t.Fatal("SetRotation did not find the mechanism")
	}
	if kr.Rotate() {
		t.Fatal("the mechanism still rotates after the switch was turned off")
	}
	if reg.SetRotation("absent", true) {
		t.Fatal("SetRotation reported success for an absent mechanism")
	}
}

// TestCatalogKeyFollowsCredentialAndPin — the one-shot models command asks the
// same account the request path would use.
func TestCatalogKeyFollowsCredentialAndPin(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConfig{
		"p": {Type: "opencode"},
	}
	cfg.Credential.Set(config.CredentialAnonymous, config.SourceDefault)

	reg := &keyRegistry{byProvider: map[provider.ID]*capability.KeyRotation{
		"p": newRotation(t, "first", "second"),
	}}

	// Anonymous presents no key, so the catalog is not asked through one.
	if got := reg.CatalogKey(cfg, "p"); got != "" {
		t.Fatalf("catalog key under anonymous = %q, want empty", got)
	}

	cfg.Update(func() { cfg.Credential.Set(config.CredentialAccount, config.SourceFile) })
	if got := reg.CatalogKey(cfg, "p"); got != "first" {
		t.Fatalf("catalog key without a pin = %q, want the first key", got)
	}

	cfg.Update(func() {
		pc := cfg.Providers["p"]
		pc.Account = capability.Fingerprint("second")
		cfg.Providers["p"] = pc
	})
	if got := reg.CatalogKey(cfg, "p"); got != "second" {
		t.Fatalf("catalog key with a fingerprint pin = %q, want the pinned key", got)
	}

	cfg.Update(func() {
		pc := cfg.Providers["p"]
		pc.Account = "2"
		cfg.Providers["p"] = pc
	})
	if got := reg.CatalogKey(cfg, "p"); got != "second" {
		t.Fatalf("catalog key with a position pin = %q, want the second key", got)
	}
}

// --- the pool on disk ---------------------------------------------------------

// TestAddedKeyIsWrittenToTheConfiguration.
//
// The mechanism is memory-only by construction, so an account added from the
// panel was gone at the next restart — and on NixOS that restart is every switch
// and every reboot. The configuration is the daemon's only store, and its loader
// already reads settings.keys, so an added key belongs there.
func TestAddedKeyIsWrittenToTheConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeDoc(t, path, map[string]any{
		"providers": map[string]any{
			"antigravity": map[string]any{"type": "antigravity"},
		},
	})

	reg := &keyRegistry{
		byProvider: map[provider.ID]*capability.KeyRotation{"antigravity": newRotation(t, "from-env")},
		path:       path,
	}

	if !reg.AddKey("antigravity", "refresh-token") {
		t.Fatal("AddKey reported that the key was not added")
	}

	stored := storedKeys(t, path, "antigravity")
	if len(stored) != 1 || stored[0] != "refresh-token" {
		t.Fatalf("stored keys = %v, want exactly the added one", stored)
	}

	// The read side has to see it, or the write is decoration.
	loaded := keysFromConfig(config.ProviderConfig{Settings: map[string]any{"keys": toAnySlice(stored)}})
	if len(loaded) != 1 || loaded[0] != "refresh-token" {
		t.Fatalf("keys read back = %v, want the added key", loaded)
	}
}

// TestAddedKeyIsNotDuplicated.
//
// A second add of the same key changes nothing: the file accumulates duplicates
// otherwise, and every one of them spends a rotation turn.
func TestAddedKeyIsNotDuplicated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeDoc(t, path, map[string]any{
		"providers": map[string]any{
			"antigravity": map[string]any{
				"type":     "antigravity",
				"settings": map[string]any{"keys": []any{"refresh-token"}},
			},
		},
	})

	reg := &keyRegistry{
		byProvider: map[provider.ID]*capability.KeyRotation{"antigravity": newRotation(t, "refresh-token")},
		path:       path,
	}

	if reg.AddKey("antigravity", "refresh-token") {
		t.Fatal("a key already in the pool was reported as new")
	}
	if stored := storedKeys(t, path, "antigravity"); len(stored) != 1 {
		t.Fatalf("stored keys = %v, want the list unchanged", stored)
	}
}

// TestRemovedKeyLeavesTheConfiguration.
//
// Removal has the same obligation as adding: a key removed from the panel came
// back at the next restart while the file still held it.
//
// The selector is a fingerprint, and the file is filtered by comparing
// fingerprints rather than the strings themselves: what the operator copied out
// of the panel is the fingerprint.
func TestRemovedKeyLeavesTheConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeDoc(t, path, map[string]any{
		"providers": map[string]any{
			"antigravity": map[string]any{
				"type":     "antigravity",
				"settings": map[string]any{"keys": []any{"first-token", "second-token"}},
			},
		},
	})

	reg := &keyRegistry{
		byProvider: map[provider.ID]*capability.KeyRotation{"antigravity": newRotation(t, "first-token", "second-token")},
		path:       path,
	}

	if !reg.RemoveFingerprint("antigravity", capability.Fingerprint("first-token")) {
		t.Fatal("RemoveFingerprint did not find the key")
	}
	stored := storedKeys(t, path, "antigravity")
	if len(stored) != 1 || stored[0] != "second-token" {
		t.Fatalf("stored keys = %v, want the surviving key only", stored)
	}
}

// TestAddThatCannotBeWrittenIsNotReportedAsAdded.
//
// A key that exists only in memory is the defect this write was added for, so a
// failed write rolls the pool back: what the panel calls "added" has to survive a
// restart, and saying "added" about a key that would vanish at the next one is
// worse than refusing.
func TestAddThatCannotBeWrittenIsNotReportedAsAdded(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the directory has to be: every write below it fails.
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("prepare the blocker: %v", err)
	}

	reg := &keyRegistry{
		byProvider: map[provider.ID]*capability.KeyRotation{"antigravity": newRotation(t)},
		path:       filepath.Join(blocker, "config.json"),
	}

	if reg.AddKey("antigravity", "refresh-token") {
		t.Fatal("AddKey reported success although the key could not be written")
	}
	if keys := reg.Keys("antigravity"); len(keys) != 0 {
		t.Fatalf("keys after the failed write = %v, want the pool unchanged", keys)
	}
}

// writeDoc writes a configuration document.
func writeDoc(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write the document: %v", err)
	}
}

// storedKeys reads settings.keys back from the file.
func storedKeys(t *testing.T, path, id string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	providers, _ := doc["providers"].(map[string]any)
	section, _ := providers[id].(map[string]any)
	settings, _ := section["settings"].(map[string]any)
	rawKeys, _ := settings["keys"].([]any)

	out := make([]string, 0, len(rawKeys))
	for _, v := range rawKeys {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// toAnySlice is what the loader sees after a round trip through JSON.
func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
