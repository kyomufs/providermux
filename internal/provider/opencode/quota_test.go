package opencode

import (
	"net/http"
	"testing"
)

// TestTheFreeAllowanceRefusalIsAboutTheAddress — the measured 429.
//
// The body carries no resetsAt and no Retry-After (measured repeatedly on
// 2026-11-19), and the same key answered 200 minutes later from a different
// exit. So the refusal is the address's exhausted allowance: repeating it from
// the same exit cannot help, another exit can, and the key is untouched.
func TestTheFreeAllowanceRefusalIsAboutTheAddress(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`)
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	resp.Header.Set("content-type", "application/json")

	failure := newTestProvider(t).Classify(resp, body)

	if !failure.QuotaRejected {
		t.Fatal("the spent allowance was not recognised as a quota refusal")
	}
	if failure.KeyRejected {
		t.Fatal("the allowance was blamed on the key: the same key works from another exit")
	}
	if failure.AddressRejected {
		t.Fatal("this is not the free-lane attestation refusal; its marker would move the client to another address for the wrong reason")
	}
	if string(failure.Body) != string(body) {
		t.Fatal("the refusal body was altered")
	}
}

// TestABillingCodeIsAboutTheKey — a quota the account itself exhausted is not
// answered by another address: every exit sees the same balance.
func TestABillingCodeIsAboutTheKey(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"QuotaExceededError","code":"insufficient_quota","message":"You exceeded your current quota."}}`)
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}

	failure := newTestProvider(t).Classify(resp, body)
	if !failure.KeyRejected {
		t.Fatal("an exhausted balance must be blamed on the key")
	}
	if !failure.QuotaRejected {
		t.Fatal("an exhausted balance is still a spent allowance")
	}
}
