package config

import (
	"strings"
	"testing"
	"time"
)

// TestRetryAndRotationComeFromTheFile — the two settings are read, and their
// durations are parsed rather than kept as strings.
func TestRetryAndRotationComeFromTheFile(t *testing.T) {
	paths := writeConfig(t, `{
		"retry": {"attempts": 4, "delay": "750ms"},
		"rotation": {
			"controller": "http://127.0.0.1:9091",
			"group": "egress",
			"secret": "s3cret",
			"probe_url": "https://1.1.1.1/cdn-cgi/trace",
			"probe_count": 3
		}
	}`)

	c, err := LoadFrom(paths)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Retry.Attempts != 4 {
		t.Fatalf("Retry.Attempts = %d, want 4", c.Retry.Attempts)
	}
	if c.Retry.Delay != 750*time.Millisecond {
		t.Fatalf("Retry.Delay = %v, want 750ms", c.Retry.Delay)
	}
	if !c.Rotation.Enabled() {
		t.Fatal("rotation is configured but reports itself disabled")
	}
	if c.Rotation.Group != "egress" || c.Rotation.ProbeCount != 3 {
		t.Fatalf("Rotation = %+v", c.Rotation)
	}
}

// TestRetryDefaultsToOneAttempt — the gateway repeats nothing unless asked.
//
// A repeat is traffic the client did not send and cannot account for, so the
// default stays at the historical one request per request.
func TestRetryDefaultsToOneAttempt(t *testing.T) {
	c, err := LoadFrom(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Retry.Attempts != 1 || c.Retry.Enabled() {
		t.Fatalf("Retry = %+v, want a single attempt", c.Retry)
	}
	if c.Rotation.Enabled() {
		t.Fatal("rotation must be off when no controller is named")
	}
}

// TestRotationNeedsTheWholeAddress: a half-configured rotation is refused at
// load, not discovered at the first refusal that needed it.
func TestRotationNeedsTheWholeAddress(t *testing.T) {
	cases := map[string]string{
		"controller without group": `{"retry":{"attempts":2},"rotation":{"controller":"http://127.0.0.1:9091"}}`,
		"group without controller": `{"retry":{"attempts":2},"rotation":{"group":"egress"}}`,
		"rotation without retry":   `{"rotation":{"controller":"http://127.0.0.1:9091","group":"egress"}}`,
		"not an http url":          `{"retry":{"attempts":2},"rotation":{"controller":"socks5://127.0.0.1:9091","group":"egress"}}`,
		"zero probe count":         `{"retry":{"attempts":2},"rotation":{"controller":"http://127.0.0.1:9091","group":"egress","probe_count":0}}`,
		"zero attempts":            `{"retry":{"attempts":0}}`,
		"unknown retry key":        `{"retry":{"attempts":2,"retries":3}}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFrom(writeConfig(t, body)); err == nil {
				t.Fatal("the configuration was accepted, want a refusal")
			} else if !strings.Contains(err.Error(), "config:") {
				t.Fatalf("the refusal does not name the layer: %v", err)
			}
		})
	}
}
