package opencode

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"providermux/internal/provider"
)

// Refusal classification.
//
// WHY IT IS NEEDED AT ALL, if the gateway passes the refusal to the client
// verbatim. For one question: is the refusal about the credentials, so that the
// NEXT request should leave with another key. The body, status and headers go to
// the client unchanged — it parses them itself, and it owns the retry decision.
//
// The source of the signals is packages/ai/src/provider-error.ts (opencode
// 2.0.30), the function classifyProviderFailure. The provider uses the same
// logic as the client, so that the gateway's decision does not diverge from the
// client's.

// quotaCodes are the codes that mean the quota is exhausted, rather than a burst
// of requests.
//
// In the client these are QUOTA_CODES; the match matters: if the gateway decides
// it is a key issue while the client sees a quota, they will disagree on whether
// to retry the request.
var quotaCodes = map[string]bool{
	"insufficient_quota":     true,
	"usage_not_included":     true,
	"billing_error":          true,
	"gousagelimiterror":      true,
	"freeusagelimiterror":    true,
	"creditlimitexceeded":    true,
	"freetiererror":          true,
	"blackusagelimiterror":   true,
	"gousagelimitexceeded":   true,
	"freeusagelimitexceeded": true,
	"insufficient_credit":    true,
	"quota_exceeded":         true,
	"quotaexceedederror":     true,
}

// authCodes are the codes that mean the credentials were refused.
var authCodes = map[string]bool{
	"authentication_error": true,
	"permission_error":     true,
	"permission_denied":    true,
	"api_key_invalid":      true,
	"invalid_api_key":      true,
	"invalidcredential":    true,
	"invalid_credential":   true,
	"autherror":            true,
	"unauthorized":         true,
}

// quotaText are the text signals of a quota. They are applied only to 429 and
// 402, where a burst of requests and quota exhaustion share one status.
var quotaText = regexp.MustCompile(`(?i)insufficient[-_\s]?(?:quota|balance)|quota[-_\s]?exceeded|budget exceeded|usage limit|limit exhausted|credit limit|free tier`)

// resetsAtPattern finds the reset instant the free lane puts into a quota
// refusal next to Retry-After. It is read as a fallback, for an answer that
// carries only one of the two.
var resetsAtPattern = regexp.MustCompile(`"resetsAt"\s*:\s*"([^"]+)"`)

// authText are the text signals of a key refusal.
var authText = regexp.MustCompile(`(?i)incorrect api key|invalid api key|api key is invalid|unauthenticated`)

// freeLaneRefusal are the text signals of a refusal that concerns the free lane
// itself rather than the account or the key.
//
// The wording is reproduced from live responses: the first is the attestation
// refusal a non-conforming request gets, the second is the address-level flag an
// operator sees after abusive traffic has been detected from their egress
// address. Neither can be answered by another key.
var freeLaneRefusal = regexp.MustCompile(`(?i)can only be used from within opencode|detected abusive traffic|free models aren't available from your current network`)

// upstreamOutage is the text signal of the provider's edge failing to reach the
// model host: "Upstream request failed: Endpoint is unavailable."
//
// It is matched by TEXT and not by status because the status is not fixed for
// it. Measured on 2026-10-20 within one hour: 503 when the edge itself
// answered, 403 on one free model and 429 on another. One condition, three
// statuses, so the status would split it into three different verdicts.
//
// It is a guard on the KEY verdict only: the gateway neither repeats nor
// switches models, so this exists to keep "key rejected" in the log honest. No
// key reaches a model host that the edge cannot reach.
//
// THE WRAPPER IS NOT THE SIGNAL. The edge prefixes its own failures with
// "Upstream request failed:" and then quotes the model host's answer, so the
// prefix alone matches EVERYTHING the model host rejects — including a request
// the host refused as malformed:
//
//	Upstream request failed: [InvalidParameter] Invalid request: invalid or
//	unsupported parameter value trace_id: <hex>
//
// So the prefix only counts together with the transport wording that follows it
// ("endpoint is unavailable", "connection", "timed out", "no healthy"). Anything
// else behind the prefix keeps its own verdict — a malformed request is reported
// to the client as itself.
var upstreamOutage = regexp.MustCompile(`(?i)upstream request failed[^"]*?(?:endpoint is unavailable|unavailable|connection (?:refused|reset|error)|econn\w*|timed out|timeout|no healthy|bad gateway|service unavailable)|endpoint is unavailable`)

// Classify implements provider.Provider.
//
// The decision about the key is made from the totality of signals, because
// services report the same thing in different ways: by status, by a code in the
// body, by type or by text. The provider must recognise a key refusal in all
// forms, otherwise the gateway will not switch the key where that would have
// helped.
func (p *Provider) Classify(resp *http.Response, body []byte) provider.Failure {
	f := provider.Failure{
		Status:  resp.StatusCode,
		Body:    body,
		Headers: resp.Header,
	}

	codes := extractCodes(body)
	lower := strings.ToLower(string(body))

	// A key refusal. 401 and 403 belong here: the service answers with them both
	// for a wrong key and for an exhausted access right.
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		f.KeyRejected = true
	case resp.StatusCode == http.StatusForbidden:
		// 403 can also be a regional ban or an attestation refusal. For the
		// key decision the difference does not matter: if the gateway has a
		// key, the next request may leave with another; if it does not, the
		// verdict only reaches the log.
		f.KeyRejected = true
	}
	for _, code := range codes {
		if authCodes[code] {
			f.KeyRejected = true
		}
		// Quota codes mean the limit sits on the credentials: switching the
		// key is the only way to continue.
		if quotaCodes[code] {
			f.KeyRejected = true
		}
	}
	if authText.MatchString(lower) {
		f.KeyRejected = true
	}
	if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusPaymentRequired) && quotaText.MatchString(lower) {
		f.KeyRejected = true
	}

	// A free-lane access refusal is NOT a credential problem, whatever the status
	// says.
	//
	// Both of these arrive as 403 and both are about the REQUEST, not the key: the
	// first says the request did not look like it came from the client (the
	// admission conditions are in contract.go), the second says the egress address
	// is flagged. Rotating a pool over them achieves nothing except a pool's worth
	// of additional refused requests from an address that is already suspect,
	// which is the worst possible thing to spend it on.
	if resp.StatusCode == http.StatusForbidden && freeLaneRefusal.MatchString(lower) {
		f.KeyRejected = false
		// Reported as its own judgement: it is the only verdict a different exit
		// address could change, and nothing acts on it while the only transport is
		// direct.
		f.AddressRejected = true
	}

	// An upstream outage is not a credential problem: the edge could not reach
	// the model host, and no key changes that. Applied last so that a 403
	// carrying this wording is not blamed on the key just above.
	if upstreamOutage.MatchString(lower) {
		f.KeyRejected = false
	}

	// The session half of the refusal is the one part a different address can
	// leave behind, so it is recorded here: this is the only point that knows the
	// answer was a quota one and until when it holds (lane.go).
	p.markLaneRefusal(resp, body, codes, lower)

	return f
}

// markLaneRefusal remembers a refused session identifier until the refusal
// lifts.
//
// The identifier is read from the request that left, not from anything the
// answer carries: the service does not echo it. No identifier means the lane
// sees an anonymous call, which is under no session counter to remember.
func (p *Provider) markLaneRefusal(resp *http.Response, body []byte, codes []string, lower string) {
	if p.lane == nil || resp == nil || resp.Request == nil {
		return
	}
	// Only a quota answer marks. A 400 that happens to contain the word limit,
	// or a 403 about the address, says nothing about the allowance.
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusPaymentRequired {
		return
	}
	quota := quotaText.MatchString(lower)
	for _, code := range codes {
		if quotaCodes[code] {
			quota = true
			break
		}
	}
	if !quota {
		return
	}
	until, ok := retryUntil(resp.Header, body, p.lane.now())
	if !ok {
		return
	}
	p.lane.spend(resp.Request.Header.Get("X-Opencode-Session-Id"), until)
}

// extractCodes pulls codes out of the body from the places different services
// put them.
//
// The parsing is deliberately shallow: the body is passed to the client
// VERBATIM, and the only task here is to collect marker strings without altering
// the response itself.
func extractCodes(body []byte) []string {
	var seen []string
	add := func(v any) {
		s, ok := v.(string)
		if !ok || s == "" {
			return
		}
		seen = append(seen, strings.ToLower(s))
	}

	var top map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &top); err != nil {
		return seen
	}

	add(top["code"])
	add(top["error_type"])
	add(top["type"])

	if err, ok := top["error"].(map[string]any); ok {
		add(err["code"])
		add(err["type"])
		add(err["status"])
		add(err["error_type"])
		// Some services put the cause in innererror.
		if inner, ok := err["innererror"].(map[string]any); ok {
			add(inner["code"])
		}
		// And in metadata.
		if md, ok := err["metadata"].(map[string]any); ok {
			add(md["error_type"])
		}
	}

	// A wrapper of the form {"type":"error","error":{...}} has already been
	// handled above. What is left is to extract the label from a rewritten
	// message: the service replaces the code with its own and leaves the
	// original in square brackets at the start of the text.
	if err, ok := top["error"].(map[string]any); ok {
		if msg, ok := err["message"].(string); ok {
			if label := gatewayCodeLabel(msg); label != "" {
				seen = append(seen, strings.ToLower(label))
			}
		}
	} else if msg, ok := top["message"].(string); ok {
		if label := gatewayCodeLabel(msg); label != "" {
			seen = append(seen, strings.ToLower(label))
		}
	}

	return seen
}

// gatewayCodeLabelMatch is the marker of the original code at the start of a
// rewritten message, for example "Gateway: [rate_limit_exceeded] ...".
var gatewayCodeLabelMatch = regexp.MustCompile(`^[^:\n]+: \[([A-Za-z0-9_.-]+)\]`)

// gatewayCodeLabel returns the code label, if there is one.
func gatewayCodeLabel(message string) string {
	if m := gatewayCodeLabelMatch.FindStringSubmatch(message); len(m) == 2 {
		return m[1]
	}
	return ""
}
