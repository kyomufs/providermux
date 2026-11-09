package antigravity

import "sync"

// skipThoughtSignature is the value the service accepts in place of a signature
// it issued itself.
//
// Gemini 3 signs every part it produces, and a function call replayed in the
// history without its signature is refused with 400 INVALID_ARGUMENT ("Function
// call is missing a thought_signature in functionCall parts"). Measured on
// 2026-11-09: a call answered by gemini-3.8-flash carried a 437-character
// signature beside the functionCall part. The OpenAI surface has no field for
// it, so the signature is lost on the way to the client and has to be replayed
// from here. When the exact signature is unknown — the process restarted, the
// client rewrote the history — this sentinel is what the vendor documents for
// that case.
const skipThoughtSignature = "skip_thought_signature_validator"

// signatureMemory is how many tool call signatures one process keeps.
const signatureMemory = 2048

// signatureStore keeps the signatures of the tool calls this provider emitted,
// keyed by the call identifier the client hands back in the history.
//
// The identifier is the service's own: it arrives in the functionCall part and
// is forwarded as the OpenAI tool call id, so the same string comes back with
// the tool result. A map rather than a per-conversation list because the client
// is free to trim the history, and a positional replay would then attach a
// signature to the wrong call.
//
// Bounded: signatures are several hundred bytes each and a long-lived process
// serves many conversations. Losing the oldest costs a fallback to the
// sentinel, not correctness.
type signatureStore struct {
	mu    sync.Mutex
	byID  map[string]string
	order []string
	limit int
}

func newSignatureStore(limit int) *signatureStore {
	return &signatureStore{byID: make(map[string]string), limit: limit}
}

// put records the signature of one call. An empty signature or identifier is
// ignored: there is nothing to replay and nothing to key it by.
func (s *signatureStore) put(id, signature string) {
	if id == "" || signature == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.byID[id]; !seen {
		s.order = append(s.order, id)
		if len(s.order) > s.limit {
			delete(s.byID, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.byID[id] = signature
}

// get returns the signature of one call, or an empty string.
func (s *signatureStore) get(id string) string {
	if id == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byID[id]
}

// signatureFor returns the value to put beside a replayed function call: the one
// the service issued when it is known, the sentinel otherwise.
func (s *signatureStore) signatureFor(id string) string {
	if sig := s.get(id); sig != "" {
		return sig
	}
	return skipThoughtSignature
}
