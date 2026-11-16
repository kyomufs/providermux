package main

import (
	"os"
	"path/filepath"
	"testing"

	"providermux/internal/config"
)

// TestDiagnosticCommandsReadAnUnstartableConfiguration.
//
// "mode server" before the token exists is a configuration the daemon refuses to
// start with. The commands that report on the daemon must still read it: that is
// exactly when the operator needs to see what is going on, and the repair has to
// be possible through the same tool.
func TestDiagnosticCommandsReadAnUnstartableConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	const body = `{"mode":"server","providers":{"p":{"type":"stub"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadConfigRead([]string{"--config", path})
	if err != nil {
		t.Fatalf("a diagnostic command refused an unstartable configuration: %v", err)
	}
	if cfg.Mode.Val != config.ModeServer {
		t.Fatalf("mode = %q, want %q", cfg.Mode.Val, config.ModeServer)
	}

	// The start-up path still refuses it, and that is the contract the skip
	// above must not weaken.
	f, err := parseFlags([]string{"--config", path})
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("the flags asked for an exit")
	}
	if _, err := loadConfigFor(f); err == nil {
		t.Fatal("the start-up loader accepted server mode without a token")
	}
}
