package antigravity

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"providermux/internal/provider"
)

// Refusal classification.
//
// The refusal itself is passed to the client verbatim; what the provider has to
// answer here is one question — would another key help, or is the answer worth
// repeating.
//
// For this service a key is an ACCOUNT (a Google refresh token), which changes
// the sense of the question: "the capacity of this account is spent" and "this
// account is not permitted" are both answered by another account, so a 429 lands
// on the key. The Google error model is the source of the signals: every refusal
// is `{"error": {"code", "message", "status"}}` with a gRPC status name, and the
// HTTP status alone does not separate "burst" from "quota".

// quotaText are the text signals of an account whose capacity is spent.
var quotaText = regexp.MustCompile(`(?i)resource has been exhausted|quota|rate limit|capacity|too many requests|limit exceeded|exhausted`)

// authText are the text signals of a credential the service no longer accepts.
var authText = regexp.MustCompile(`(?i)unauthenticated|invalid authentication|invalid_grant|credentials|oauth|token has been expired|invalid token|login required`)

// permissionText are the text signals of an account that is not allowed here —
// as opposed to an address that is not served here.
var permissionText = regexp.MustCompile(`(?i)permission_denied|does not have permission|not authorized|forbidden|is not supported for|not eligible|ineligible`)

// addressText are the text signals of a refusal about where the request comes
// from. Another account does not fix it, another egress address might.
var addressText = regexp.MustCompile(`(?i)location is not supported|unsupported_country|not available in your country|user location|region is not supported|geo.?restrict`)

// Classify implements provider.Provider.
func (p *Provider) Classify(resp *http.Response, body []byte) provider.Failure {
	f := provider.Failure{
		Status:  resp.StatusCode,
		Body:    body,
		Headers: resp.Header,
	}
	f.RetryAfter = retryAfter(resp.Header)

	lower := strings.ToLower(string(body))
	status := errorStatus(body)

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		f.KeyRejected = true
	case http.StatusForbidden:
		// On this service a 403 is usually about the account: the OAuth client is
		// not allowed to call the service, the account has no project, the
		// consumer tier is closed to it. Another account is the only lever the
		// gateway has.
		f.KeyRejected = true
	case http.StatusTooManyRequests:
		// The capacity of the ACCOUNT is what runs out here, per model and per
		// day; the burst limiter and the quota arrive under the same status.
		f.KeyRejected = true
	}

	switch status {
	case "UNAUTHENTICATED", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION":
		// FAILED_PRECONDITION is the one that needs care: it covers both "this
		// account must accept terms" and "this request is malformed for this
		// model". Only the account reading is acted on, and another account is
		// the cheap answer; a malformed request is repeated by the client anyway.
		f.KeyRejected = true
	}

	if authText.MatchString(lower) || quotaText.MatchString(lower) {
		f.KeyRejected = true
	}

	// An address refusal is not a credential problem, whatever the status says:
	// rotating a pool of accounts over it only multiplies refused requests from
	// an address the service has already judged.
	if addressText.MatchString(lower) {
		f.KeyRejected = false
		f.Retryable = false
		f.AddressRejected = true
	}

	// The account is refused for a reason that is not the credential: the client
	// is not the reference application, the project is gone, the tier is closed.
	// Another key does not help and a repeat does not either.
	if permissionText.MatchString(lower) && !quotaText.MatchString(lower) && !authText.MatchString(lower) && resp.StatusCode != http.StatusTooManyRequests {
		f.Retryable = false
	}

	// Is a repeat worth it? A different question from the key one: the same
	// account and the same request sent again after the named delay.
	switch {
	case f.KeyRejected, f.AddressRejected:
		f.Retryable = false
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		f.Retryable = true
	}

	// The service is up but the edge could not reach the model: another model of
	// the same provider is a different backend, and repeating the request against
	// the same one is pointless.
	if status == "UNAVAILABLE" || status == "DEADLINE_EXCEEDED" || status == "INTERNAL" {
		f.UpstreamUnavailable = true
		f.Retryable = true
	}

	return f
}

// errorStatus reads the gRPC status name out of a refusal body.
func errorStatus(body []byte) string {
	var envelope struct {
		Error struct {
			Status string `json:"status"`
			Code   any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &envelope); err != nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(envelope.Error.Status))
}

// retryAfter reads the delay the service named.
//
// Both spellings occur: a number of seconds and an HTTP date.
func retryAfter(header http.Header) time.Duration {
	raw := strings.TrimSpace(header.Get("retry-after"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}
