package gateway

import (
	"context"
	"errors"
	"sync"
	"time"
)

// The budgets of one attempt at the provider.
//
// WHY A WATCHDOG AND NOT A CLIENT TIMEOUT. Every timeout the standard library
// offers covers a stage of its own: the dial, the TLS handshake, the time a
// connection may sit idle in the pool. None of them covers the one case that
// matters here — a connection that is established and then goes quiet. A route
// that changed, an uplink that was switched, a VPN server that went away: no
// FIN, no RST, nothing to read and nothing that fails. A request waiting on such
// a connection waits for as long as anyone is willing to wait, and the client
// waits with it. That is what an operator reads as "the daemon has hung".
//
// The watchdog closes the hole: the attempt's context is cancelled the moment
// the provider has stayed silent longer than the budget, and the transport tears
// the connection down.
//
// TWO STAGES, because a stream has two kinds of silence. Before the headers
// there is no evidence of anything and the budget is short. Between events a
// model that reasons may hold a pause of minutes while still working, and a
// short budget there would tear a healthy stream.
const (
	// defaultFirstEventTimeout and defaultIdleTimeout are the fallbacks for a
	// provider that declares no budget of its own. The daemon passes the
	// configured values in; these stay equal to the configuration defaults, and
	// a test holds them together.
	defaultFirstEventTimeout = 30 * time.Second
	defaultIdleTimeout       = 120 * time.Second
)

// Causes explaining why an attempt was cut short.
var (
	// errFirstEvent — the headers did not arrive within the first stage.
	errFirstEvent = errors.New("the first event did not arrive within the budget")
	// errSilence — a response that had started stopped arriving for longer than
	// the silence budget.
	errSilence = errors.New("the response stopped arriving within the budget")
	// errNoAnswer — a request that does not stream got no answer within the
	// budget.
	errNoAnswer = errors.New("the answer did not arrive within the budget")
)

// watchdog enforces the budget of one attempt.
//
// It owns the attempt's context, because cancelling that context is the only way
// to make a read on a quiet connection return. The budget is re-armed before
// every read: the silence being bounded is the one between events, and an
// attempt that has already produced something is not the same as one that has
// produced nothing.
type watchdog struct {
	mu     sync.Mutex
	cancel context.CancelCauseFunc
	timer  *time.Timer
	cause  error
	// gen is the number of the budget currently running.
	//
	// A timer that has already elapsed cannot be stopped: Stop reports it, but
	// the callback is on its way, and a callback of an old budget cancelling an
	// attempt that has just received data would be a fault of its own. The
	// callback therefore carries the number of the budget it was armed for, and
	// only the current one counts.
	gen int
}

// newWatchdog opens an attempt and returns its context.
func newWatchdog(parent context.Context) (*watchdog, context.Context) {
	ctx, cancel := context.WithCancelCause(parent)
	return &watchdog{cancel: cancel}, ctx
}

// arm starts the budget, replacing any budget already running.
//
// A budget of zero disarms the watchdog: a stage the operator switched off does
// not exist. The first cause wins — the stage that was running when the budget
// elapsed is the one that explains the attempt.
func (w *watchdog) arm(budget time.Duration, cause error) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.gen++
	if budget <= 0 || w.cause != nil {
		return
	}
	gen := w.gen
	w.timer = time.AfterFunc(budget, func() { w.fire(gen, cause) })
}

// fire cancels the attempt with the given cause.
func (w *watchdog) fire(gen int, cause error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.gen || w.cause != nil {
		return
	}
	w.cause = cause
	w.cancel(cause)
}

// fired reports the cause of the cancellation, or nil when the watchdog was not
// the one who ended the attempt. It is what tells a provider that went quiet
// from a client that hung up.
func (w *watchdog) fired() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cause
}

// stop releases the attempt and its timer.
func (w *watchdog) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.cancel(nil)
}

// idleCloser is what a transport implements when it holds a pool of connections.
type idleCloser interface {
	CloseIdleConnections()
}
