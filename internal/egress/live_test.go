package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveRotationRotatesARealController — the one part of the rotator that a
// fake cannot prove: that a real controller accepts the addresses this code
// builds and that the group actually moves.
//
// Gated on purpose. It needs a running proxy with a selector group, which is a
// deployment, not a test fixture; without the variables it skips, so the ordinary
// run stays network-free and deterministic.
//
//	PMUX_LIVE_CONTROLLER=http://127.0.0.1:9091 \
//	PMUX_LIVE_GROUP=egress go test ./internal/egress -run Live -v
func TestLiveRotationRotatesARealController(t *testing.T) {
	controller := os.Getenv("PMUX_LIVE_CONTROLLER")
	if controller == "" {
		t.Skip("set PMUX_LIVE_CONTROLLER (and PMUX_LIVE_GROUP) to run against a real proxy")
	}
	group := os.Getenv("PMUX_LIVE_GROUP")
	if group == "" {
		group = "egress"
	}

	rot, err := NewRotator(RotatorConfig{
		Controller: controller,
		Group:      group,
		Secret:     os.Getenv("PMUX_LIVE_SECRET"),
		ProbeCount: 6,
	})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}

	before := currentChoice(t, controller, group)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	got, err := rot.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got == before {
		t.Fatalf("the group stayed on %q", before)
	}
	if now := currentChoice(t, controller, group); now != got {
		t.Fatalf("the controller reports %q, the rotation returned %q", now, got)
	}
	t.Logf("rotated %q → %q", before, got)
}

func currentChoice(t *testing.T, controller, group string) string {
	t.Helper()
	resp, err := http.Get(controller + "/proxies/" + group)
	if err != nil {
		t.Fatalf("reading the group: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Now string `json:"now"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the group: %v", err)
	}
	return body.Now
}
