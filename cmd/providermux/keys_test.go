package main

import (
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
