package config

import "testing"

// TestRotationNamesTheTransportItMoves — a deployment may split transports
// between providers, so the policy says which path the controller actually
// moves. Without the field a split deployment would rotate an exit that half
// the providers never travel.
func TestRotationNamesTheTransportItMoves(t *testing.T) {
	paths := writeConfig(t, `{
		"transport": "proxy",
		"retry": {"attempts": 2},
		"rotation": {
			"controller": "http://127.0.0.1:9091",
			"group": "egress",
			"transport": "direct"
		}
	}`)

	c, err := LoadFrom(paths)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Rotation.Transport != "direct" {
		t.Fatalf("Rotation.Transport = %q, want \"direct\"", c.Rotation.Transport)
	}
	if got := c.RotationTransport(); got != "direct" {
		t.Fatalf("RotationTransport() = %q, want the explicit \"direct\"", got)
	}
}

// TestRotationDefaultsToTheDeploymentTransport — the common case names only the
// controller: one transport, one selector behind it.
func TestRotationDefaultsToTheDeploymentTransport(t *testing.T) {
	paths := writeConfig(t, `{
		"transport": "proxy",
		"retry": {"attempts": 2},
		"rotation": {"controller": "http://127.0.0.1:9091", "group": "egress"}
	}`)

	c, err := LoadFrom(paths)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.Rotation.Transport != "" {
		t.Fatalf("Rotation.Transport = %q, want empty", c.Rotation.Transport)
	}
	if got := c.RotationTransport(); got != "proxy" {
		t.Fatalf("RotationTransport() = %q, want the deployment transport \"proxy\"", got)
	}
}

// TestAnUnknownRotationKeyIsRefused — the file is strict everywhere else, and a
// misspelt axis here would silently rotate nothing.
func TestAnUnknownRotationKeyIsRefused(t *testing.T) {
	paths := writeConfig(t, `{
		"retry": {"attempts": 2},
		"rotation": {"controller": "http://127.0.0.1:9091", "group": "egress", "transports": "direct"}
	}`)

	if _, err := LoadFrom(paths); err == nil {
		t.Fatal("an unknown rotation key was accepted")
	}
}
