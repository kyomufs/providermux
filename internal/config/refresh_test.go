package config

import (
	"strings"
	"testing"
	"time"
)

func TestTheRefreshIntervalIsReadFromTheFile(t *testing.T) {
	cfg, err := LoadFrom(writeConfig(t, `{
		"rotation": {"controller": "http://127.0.0.1:9091", "group": "egress"},
		"retry": {"attempts": 2},
		"egress_refresh": {
			"enable": true,
			"interval": "6h",
			"sources": "/etc/sources.yaml",
			"out": "/var/lib/egress",
			"command": "proxprobe",
			"check": "tcp"
		}
	}`))
	if err != nil {
		t.Fatalf("loading the configuration: %v", err)
	}
	if !cfg.Refresh.Enabled() {
		t.Fatalf("the refresh is not enabled: %+v", cfg.Refresh)
	}
	if cfg.Refresh.Interval != 6*time.Hour {
		t.Errorf("interval is %s, want 6h", cfg.Refresh.Interval)
	}
	if cfg.Refresh.Out != "/var/lib/egress" {
		t.Errorf("out is %q", cfg.Refresh.Out)
	}
	if cfg.Refresh.Check != "tcp" {
		t.Errorf("check is %q, want tcp", cfg.Refresh.Check)
	}
}

func TestTheRefreshIsOffByDefault(t *testing.T) {
	cfg, err := LoadFrom(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("loading the configuration: %v", err)
	}
	if cfg.Refresh.Enabled() {
		t.Error("the refresh is on without being asked for")
	}
	if cfg.Refresh.Interval != DefaultRefreshInterval {
		t.Errorf("default interval is %s, want %s", cfg.Refresh.Interval, DefaultRefreshInterval)
	}
	if cfg.Refresh.Command != DefaultRefreshCommand {
		t.Errorf("default command is %q, want %q", cfg.Refresh.Command, DefaultRefreshCommand)
	}
}

func TestARefreshWithoutAnOutputDirectoryIsRefused(t *testing.T) {
	_, err := LoadFrom(writeConfig(t, `{
		"rotation": {"controller": "http://127.0.0.1:9091", "group": "egress"},
		"retry": {"attempts": 2},
		"egress_refresh": {"enable": true, "interval": "6h"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "egress_refresh.out") {
		t.Fatalf("error is %v, want a refusal that names the missing output directory", err)
	}
}

func TestARefreshWithoutRotationIsRefused(t *testing.T) {
	_, err := LoadFrom(writeConfig(t, `{
		"egress_refresh": {"enable": true, "interval": "6h", "out": "/var/lib/egress"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "needs rotation") {
		t.Fatalf("error is %v, want a refusal: a rebuilt profile has nowhere to be loaded", err)
	}
}

func TestAnUnreadableRefreshIntervalIsRefused(t *testing.T) {
	_, err := LoadFrom(writeConfig(t, `{
		"rotation": {"controller": "http://127.0.0.1:9091", "group": "egress"},
		"retry": {"attempts": 2},
		"egress_refresh": {"enable": true, "interval": "6 hours", "out": "/var/lib/egress"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "egress_refresh.interval") {
		t.Fatalf("error is %v, want a refusal that names the interval", err)
	}
}
