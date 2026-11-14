package gateway

import (
	"providermux/internal/capability"
	"providermux/internal/provider"
)

// Issuing credentials.
//
// WHY THIS IS HERE AND NOT IN THE PROVIDER. The provider assembles the HEADERS
// but does not decide which key to put in them: keys are issued by the shared
// rotation mechanism, and there is one of it for all key-based providers. All
// that is left to the provider is the shape — the header name and the prefix.
//
// THE CREDENTIAL AXIS DECIDES THE SOURCE, and the pool only supplies the value.
// That is the whole point of the axis: "anonymous" is not a statement about
// whether a key happens to be lying around, it is an instruction not to present
// one. A deployment that listed keys and then switched the credential to
// anonymous asked to reach the service without them — a pool left in the file is
// not consent to spend it.
//
// This used to work the other way: the axis gated rotation only, and any key
// present was sent regardless. The result was a mode called "anonymous" that
// quietly presented a key, which is a lie in the log and in the monitor, and a
// surprising bill for whoever put a key in the file and expected it to be
// unused.

// CredentialSource issues the credentials for a request.
type CredentialSource interface {
	// Next issues the next key.
	Next() (string, bool)
}

// credentialFor returns the key for a request.
func (h *Handler) credentialFor(p provider.Provider) string {
	return h.credentialForWithSelector(p, "")
}

// credentialForWithSelector returns the key for a request, respecting an explicit selector when given.
func (h *Handler) credentialForWithSelector(p provider.Provider, selector string) string {
	switch h.credentialKind(p) {
	case credentialAnonymous:
		return ""
	case credentialAccount:
		if selector != "" {
			if key, ok := h.findKey(p, selector); ok {
				return key
			}
		}
		// The pin is a decision about the first key, not about rotation: it
		// names the account the operator chose, and a refusal still moves on
		// through alternativeKey when the rotation axis allows it.
		if pin := h.pinnedKey(p); pin != "" {
			return pin
		}
		return h.nextKey(p)
	}

	// Not told which credential is in force: fall back to what the pool says.
	if selector != "" {
		if key, ok := h.findKey(p, selector); ok {
			return key
		}
	}
	if pin := h.pinnedKey(p); pin != "" {
		return pin
	}
	return h.nextKey(p)
}

// pinnedKey returns the key the provider is pinned to, if the pin resolves.
//
// A pin that names a key no longer in the pool is ignored rather than fatal:
// pools are edited from the panel, and a stale pin would otherwise stop every
// request until the file was fixed by hand. The log says it was ignored, so the
// stale entry can be seen and cleared.
func (h *Handler) pinnedKey(p provider.Provider) string {
	if h.Account == nil {
		return ""
	}
	pin := h.Account(p.ID())
	if pin == "" {
		return ""
	}
	key, ok := h.findKey(p, pin)
	if !ok {
		h.warn("pinned account %q is not in the %s pool; falling back to rotation", pin, string(p.ID()))
		return ""
	}
	return key
}

// catalogKey returns the account whose view of the catalog should be shown.
//
// The list is asked through a key because the service answers with what that
// account may use. The pin wins, since it is the operator's own choice; without
// one the first key is used, and the pool is only read, never rotated — listing
// models must not spend a turn of the rotation.
//
// Under anonymous the pool is not touched at all: the axis says not to present
// a key, and reading a catalog through one would be presenting it.
func (h *Handler) catalogKey(p provider.Provider) string {
	if kind := h.credentialKind(p); kind == credentialAnonymous {
		return ""
	}
	if key := h.pinnedKey(p); key != "" {
		return key
	}
	if h.KeyRotation == nil {
		return ""
	}
	kr := h.KeyRotation[string(p.ID())]
	if kr == nil {
		return ""
	}
	keys := kr.KeysSnapshot()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// findKey looks up a key by selector in the provider's pool.
func (h *Handler) findKey(p provider.Provider, selector string) (string, bool) {
	if h.KeyRotation == nil {
		return "", false
	}
	kr, ok := h.KeyRotation[string(p.ID())]
	if !ok || kr == nil {
		return "", false
	}
	return kr.Find(selector)
}

// The credential kinds, spelled as the configuration spells them.
//
// Duplicated as constants rather than imported: the gateway must not depend on
// the configuration package, or the axis would be read in two places at once.
// The values are part of the configuration's public surface, so a change there
// is a change here, and the test that compares them is what keeps them together.
const (
	credentialAnonymous = "anonymous"
	credentialAccount   = "account"
)

// credentialKind reports which credential is in force for the provider.
func (h *Handler) credentialKind(p provider.Provider) string {
	if h.Credential == nil {
		return ""
	}
	return h.Credential(p.ID())
}

// nextKey takes the next key from the provider's pool.
func (h *Handler) nextKey(p provider.Provider) string {
	if h.KeyRotation == nil {
		return ""
	}
	kr, ok := h.KeyRotation[string(p.ID())]
	if !ok || kr == nil {
		return ""
	}
	key, _ := kr.Next()
	return key
}

// issuesCredentials reports whether requests to the provider carry a key.
//
// It is the question the monitor asks, and it is answered by the same function
// that hands out the key — otherwise the two can disagree, and the monitor would
// claim a mode the request path does not follow.
func (h *Handler) issuesCredentials(p provider.Provider) bool {
	if h.credentialKind(p) == credentialAnonymous {
		return false
	}
	if h.KeyRotation == nil {
		return false
	}
	kr, ok := h.KeyRotation[string(p.ID())]
	return ok && kr != nil && kr.Len() > 0
}

// alternativeKey looks for another key of the same provider.
//
// It is needed on a retry: the current key has been rejected, and it is
// precisely a different one that is needed. If there is no other, the retry is
// pointless, and the caller must hand the refusal to the client.
//
// UNDER "anonymous" THERE IS NEVER AN ALTERNATIVE. The mechanism may hold keys
// and may have rotation switched on, but the credential in force says no key is
// presented, so there is nothing to replace — and reporting an alternative would
// make the retry loop reach for a key the mode forbids. This is where the axis
// and the rotation switch meet, and they are not the same question: the axis says
// whether a key is presented at all, the switch says whether a refused key is
// replaced.
//
// The answer also carries the rotation policy: a mechanism with rotation
// switched off never has an alternative. That is deliberate — "is there another
// key to try" is one question, and splitting it in two would leave the retry
// loop checking a setting the mechanism already knows about.
func (h *Handler) alternativeKey(p provider.Provider, current string) (string, bool) {
	if h.credentialKind(p) == credentialAnonymous {
		return "", false
	}
	if h.KeyRotation == nil {
		return "", false
	}
	kr, ok := h.KeyRotation[string(p.ID())]
	if !ok || kr == nil {
		return "", false
	}
	return kr.Take(current)
}

// retryFor returns the provider's retry policy, or nil when it has none.
//
// nil is not an error state. It says the provider did not ask for its refusals to
// be repeated, and the refusal then goes to the client as it arrived.
func (h *Handler) retryFor(p provider.Provider) *capability.Retry {
	if h.Retry == nil {
		return nil
	}
	policy, ok := h.Retry[string(p.ID())]
	if !ok {
		return nil
	}
	return policy
}

// KeyRotationRegistry is the management of rotation mechanisms.
//
// An interface rather than a concrete type: the control API and the interfaces
// must not depend on how key storage is arranged. It is enough for them to be
// able to ask and to change.
type KeyRotationRegistry interface {
	// Keys returns the provider's keys.
	Keys(id provider.ID) []string
	// KeyFingerprints returns the provider's key fingerprints.
	KeyFingerprints(id provider.ID) []string
	// HasRotation reports whether the mechanism is wired in for the provider.
	HasRotation(id provider.ID) bool
	// AddKey adds a key and reports whether it was new.
	AddKey(id provider.ID, key string) bool
	// RemoveFingerprint removes a key by fingerprint.
	RemoveFingerprint(id provider.ID, fingerprint string) bool
}
