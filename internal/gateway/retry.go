package gateway

import (
	"context"
	"net/http"
	"time"

	"providermux/internal/provider"
)

// RetryPolicy is how many times one client request may reach the provider.
//
// It is the gateway's answer to a refusal the client cannot answer itself: the
// free lane's counter is spent per source address, the client cannot leave its
// address, and its own policy does not repeat the refusal
// (isRetryable(QuotaExceeded) is false in the reference client). One attempt is
// the historical behaviour and stays the default.
type RetryPolicy struct {
	// Attempts is the TOTAL number of upstream requests. 1 disables retrying.
	Attempts int
	// Delay is the pause before the second attempt; every further attempt waits
	// this much longer again.
	Delay time.Duration
}

// attempts is the number of attempts, never below one.
func (p RetryPolicy) attempts() int {
	if p.Attempts < 1 {
		return 1
	}
	return p.Attempts
}

// rotationRefreshTimeout bounds the address re-read that follows a rotation.
//
// The re-read is two HTTPS requests over the new exit, and it sits inside the
// client's request: a slow echo must not turn a repeat into a hang. A failure
// leaves the cached address in place, which costs a stale label and a session
// identifier that is reused where it was never spent — neither breaks the
// request.
const rotationRefreshTimeout = 5 * time.Second

// rotationTries bounds how many nodes one rotation walks through.
//
// A NODE IS NOT AN ADDRESS, and this is measured, not supposed: three different
// nodes of the same subscription were found to exit from one address
// (72.56.82.193 on 2026-11-19). Switching the node is therefore only a proposal
// — the address is the fact — and the honest loop asks for another node while
// the exit has not moved. The bound exists because every try costs the client a
// probe, and an exit that refuses to move at all must end in a verbatim refusal
// rather than in a hang.
const rotationTries = 3

// backoff waits before repeating a request that just failed, and reports
// whether the client is still there. A client that has gone away cancels the
// wait, and the loop returns through the ordinary failure path.
func (h *Handler) backoff(ctx context.Context, failed int) bool {
	wait := h.Retry.Delay * time.Duration(failed)
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// rotateAfterQuota asks for a different exit address and reports whether the
// request may be repeated.
//
// The decision is narrow on purpose. A quota refusal is the one the free lane
// keys to the source address, so it is the one a new exit answers. A key
// refusal, a malformed request, a content refusal and an upstream outage are
// all answered the same way from any address, and repeating them would only
// spend traffic.
//
// spent is the address the refusal came from. The rotation repeats while the
// observed address has not moved: a spent address is what the repeat must avoid,
// and the node name says nothing about it.
func (h *Handler) rotateAfterQuota(ctx context.Context, spent string, failure provider.Failure) bool {
	if h.Rotate == nil || !failure.QuotaRejected {
		return false
	}
	for try := 1; try <= rotationTries; try++ {
		name, err := h.Rotate(ctx)
		if err != nil {
			h.warn("egress: rotation after a quota refusal: %v", err)
			return false
		}

		// The next attempt leaves from an address nobody has measured yet. The
		// address is asked for NOW rather than at the next natural refresh: the
		// provider's lane bookkeeping and the request's own session identifier
		// are both keyed by it, and a stale value would describe the attempt
		// wrongly — and would make this loop blind to a node that shares the
		// address it must leave.
		if refresher, ok := h.Addresses.(AddressRefresher); ok {
			rctx, cancel := context.WithTimeout(ctx, rotationRefreshTimeout)
			refresher.Refresh(rctx)
			cancel()
		}

		next := h.address()
		switch {
		case next != "" && next == spent:
			h.warn("egress: node %q exits from the spent address %s; asking for another", name, next)
			continue
		case next == "":
			// The exit moved but the observation did not arrive — the trace
			// endpoint is the first thing a slow node fails at. Repeating is
			// still the better answer than the refusal: the node did change,
			// and the counter may well have moved with it.
			h.warn("egress: rotated to %q after a quota refusal; the new address was not observed; repeating the request", name)
			return true
		default:
			h.warn("egress: rotated to %q (%s → %s) after a quota refusal; repeating the request", name, spent, next)
			return true
		}
	}
	return false
}

// retryableRefusal reports whether the SAME request from the SAME exit may
// succeed if it is repeated.
//
// A quota is explicitly not one of them: its counter does not move, and the
// repeat that helps is the one from another address, which is a different
// decision (rotateAfterQuota). What is left is the provider being briefly
// unable to answer, which the client would retry itself if it retried at all.
func retryableRefusal(f provider.Failure) bool {
	if f.QuotaRejected {
		return false
	}
	switch f.Status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		529: // the non-standard "site is overloaded" the provider's edge uses
		return true
	}
	return false
}
