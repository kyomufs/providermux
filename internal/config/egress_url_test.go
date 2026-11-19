package config

import (
	"strings"
	"testing"

	"providermux/internal/egress"
)

// TestEgressURLCanBeReplaced — the endpoint the gateway asks for its own
// address is a setting, because the endpoint lives outside this host.
func TestEgressURLCanBeReplaced(t *testing.T) {
	paths := writeConfig(t, `{"egress_url": "https://example.test/cdn-cgi/trace"}`)

	c, err := LoadFrom(paths)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.EgressURL.Val != "https://example.test/cdn-cgi/trace" {
		t.Fatalf("EgressURL = %q", c.EgressURL.Val)
	}
}

// TestEgressURLDefaultsToTheEchoDocument — an operator who says nothing gets a
// working observation, not an empty address.
func TestEgressURLDefaultsToTheEchoDocument(t *testing.T) {
	c, err := LoadFrom(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.EgressURL.Val != egress.DefaultURL {
		t.Fatalf("EgressURL = %q, want %q", c.EgressURL.Val, egress.DefaultURL)
	}
}

// TestEgressURLMustBeAnAddress — an observation endpoint that is not a URL is
// refused at load, where the operator can still see it.
func TestEgressURLMustBeAnAddress(t *testing.T) {
	// An empty value is not in this list: an empty string means "not set" for
	// every axis, and the default is the answer.
	for _, bad := range []string{"1.1.1.1/cdn-cgi/trace", "ftp://example.test/trace", "https://"} {
		_, err := LoadFrom(writeConfig(t, `{"egress_url": "`+bad+`"}`))
		if err == nil {
			t.Fatalf("egress_url %q was accepted", bad)
		}
		if !strings.Contains(err.Error(), "egress_url") {
			t.Fatalf("error for %q does not name the key: %v", bad, err)
		}
	}
}
