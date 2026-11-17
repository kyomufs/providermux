package opencode

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The free lane counts a refused request twice: once against the address it
// left from and once against the session identifier it carried. The two
// counters have different windows, and that is what produces the behaviour seen
// on the reference client:
//
//	23:29:35 refused, reset in 12989000 ms  (3h36m)
//	23:36:06 refused, reset in 15674000 ms  (4h21m)
//	23:42:35 refused, reset in 13042000 ms  (3h37m)
//	23:45:34 accepted
//
// Three refusals in thirteen minutes, each naming a different reset instant, and
// then an accepted request on the SAME session id long before any of those
// instants arrived. A single bucket cannot do that: the id was not what was
// spent on the third refusal, the address was. And the second refusal — the same
// id, refused again within a minute with a LATER reset — is the session counter
// running separately from the address one.
//
// So the identifier is worth rotating exactly when the address under it changes:
// the session counter belongs to the address it was spent on. Reusing the id on
// a fresh address re-sends a spent session into a lane that remembers it;
// rotating it there costs a cache affinity and nothing else.
//
// Rotating on the SAME address is not done. A fresh identifier on an address
// whose allowance is gone is a second anonymous session from one address — the
// shape the lane reads as a burst, and `we've detected abusive traffic from this
// IP address` is what that produces.
const (
	// defaultLaneLedgerSize bounds the number of conversations remembered. One
	// entry is an address and an instant; the ceiling keeps a day of agent work
	// from growing it without limit.
	defaultLaneLedgerSize = 4096
)

// laneEntry is what is remembered about one session identifier.
type laneEntry struct {
	// address is the egress address its last request left from, empty when the
	// gateway could not resolve one.
	address string
	// until is when the refusal it is under expires; zero when it is not under
	// one.
	until time.Time
}

// laneLedger remembers, per session identifier, the address it was last sent
// from and until when it is refused.
type laneLedger struct {
	mu    sync.Mutex
	limit int
	order []string
	entry map[string]laneEntry
	// now is a field rather than time.Now so the tests can move the clock.
	now func() time.Time
}

// newLaneLedger creates a ledger holding at most limit identifiers.
func newLaneLedger(limit int) *laneLedger {
	if limit <= 0 {
		limit = defaultLaneLedgerSize
	}
	return &laneLedger{
		limit: limit,
		entry: make(map[string]laneEntry, limit),
		now:   time.Now,
	}
}

// recordSend remembers which address an identifier just left from.
func (l *laneLedger) recordSend(id, address string) {
	if id == "" || address == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.update(id, func(e laneEntry) laneEntry {
		e.address = address
		return e
	})
}

// spend puts an identifier under a refusal until the given instant.
func (l *laneLedger) spend(id string, until time.Time) {
	if id == "" || until.IsZero() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.update(id, func(e laneEntry) laneEntry {
		if until.After(e.until) {
			e.until = until
		}
		return e
	})
}

// freshSession returns the identifier to send and whether it was replaced.
//
// It replaces the identifier only when the conversation is under a live refusal
// AND this request leaves from a different address than the one that refusal was
// issued on. Everything else keeps the identifier: an expired refusal is gone,
// and the same address cannot be helped by a new name.
func (l *laneLedger) freshSession(id, address string) (string, bool) {
	if id == "" || address == "" {
		return id, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entry[id]
	if !ok {
		return id, false
	}
	if !l.now().Before(e.until) {
		// The window closed on its own; nothing is left to avoid.
		e.until = time.Time{}
		l.entry[id] = e
		return id, false
	}
	if e.address == "" || e.address == address {
		return id, false
	}
	return newSessionID(), true
}

// pending reports whether an identifier is under a live refusal, and the
// address that refusal was issued on.
func (l *laneLedger) pending(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entry[id]
	if !ok || !l.now().Before(e.until) {
		return "", false
	}
	return e.address, true
}

// update applies change to the entry of id, adding it at the ceiling's cost.
func (l *laneLedger) update(id string, change func(laneEntry) laneEntry) {
	e, ok := l.entry[id]
	if !ok {
		// A replaced identifier is dropped rather than kept: the conversation
		// no longer sends it, and what is remembered about it is only useful to
		// another conversation that happens to reuse the value.
		for len(l.order) >= l.limit {
			delete(l.entry, l.order[0])
			l.order = l.order[1:]
		}
		l.order = append(l.order, id)
	}
	l.entry[id] = change(e)
}

// retryUntil reads when a refusal lifts: Retry-After in seconds, and failing
// that the resetsAt instant the free lane puts into the body.
//
// Neither present means the refusal names no end. That is left unmarked on
// purpose: an identifier held forever would never be usable again, and the
// window in every measured refusal is bounded.
func retryUntil(header http.Header, body []byte, now time.Time) (time.Time, bool) {
	if v := header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return now.Add(time.Duration(secs) * time.Second), true
		}
	}
	m := resetsAtPattern.FindSubmatch(body)
	if m == nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, string(m[1]))
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}
