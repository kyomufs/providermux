package egress

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reloadRecorder is a controller that records what a refresh does to it.
type reloadRecorder struct {
	reloaded string
	selected []string
	now      string
}

func (r *reloadRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/proxies/egress", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"all": []string{"DIRECT", "node-a"},
			"now": r.now,
		})
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.reloaded = body.Path
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/proxies/egress/select", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeBuilder writes a profile where it is asked to and prints the summary line
// the real builder prints.
func fakeBuilder(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "builder.sh")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the builder: %v", err)
	}
	return path
}

func TestTheRefreshRebuildsAndReloadsTheProfile(t *testing.T) {
	recorder := &reloadRecorder{now: "node-a"}
	srv := recorder.server(t)
	rot, err := NewRotator(RotatorConfig{Controller: srv.URL, Group: "egress", ProbeCount: 2})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	out := t.TempDir()
	// $3 is the --mihomo directory; the builder writes its profile there and
	// prints the line that says how many nodes survived.
	builder := fakeBuilder(t, `mkdir -p "$3" && printf 'proxies:\n' > "$3/config.yaml" && echo "$3/config.yaml: 42 nodes + DIRECT"`)

	f, err := NewRefresher(RefreshConfig{Command: builder, Out: out, Rotator: rot})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}

	report, err := f.Run(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if report.Nodes != "42 nodes + DIRECT" {
		t.Errorf("nodes line is %q, want the builder's own summary", report.Nodes)
	}
	if report.Running {
		t.Error("a finished refresh still reports running")
	}
	if report.Error != "" {
		t.Errorf("error is %q, want empty", report.Error)
	}
	if report.Started.IsZero() || report.Finished.Before(report.Started) {
		t.Errorf("the report says it started at %s and finished at %s", report.Started, report.Finished)
	}
	if want := filepath.Join(out, profileFile); recorder.reloaded != want {
		t.Errorf("reloaded %q, want %q: a refresh that does not load its own output changes nothing", recorder.reloaded, want)
	}
	if _, err := os.Stat(filepath.Join(out, profileFile)); err != nil {
		t.Errorf("the builder's profile is not where the controller was pointed: %v", err)
	}
}

func TestAFailedBuildIsReportedAndDoesNotReload(t *testing.T) {
	recorder := &reloadRecorder{now: "DIRECT"}
	srv := recorder.server(t)
	rot, err := NewRotator(RotatorConfig{Controller: srv.URL, Group: "egress", ProbeCount: 2})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	builder := fakeBuilder(t, `echo "proxprobe: all sources failed" >&2; exit 1`)

	f, err := NewRefresher(RefreshConfig{Command: builder, Out: t.TempDir(), Rotator: rot})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}

	report, err := f.Run(context.Background())
	if err == nil {
		t.Fatal("a failed build reported success")
	}
	if !strings.Contains(report.Error, "all sources failed") {
		t.Errorf("error is %q, want the builder's own message", report.Error)
	}
	if recorder.reloaded != "" {
		t.Errorf("the controller was reloaded with %q after a failed build", recorder.reloaded)
	}
	if f.Status().Running {
		t.Error("status still reports running after the failure")
	}
}

func TestTheIntervalIsLive(t *testing.T) {
	rot, err := NewRotator(RotatorConfig{Controller: "http://127.0.0.1:1", Group: "egress", ProbeCount: 2})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	f, err := NewRefresher(RefreshConfig{Command: "proxprobe", Out: t.TempDir(), Rotator: rot})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}
	if every := f.Interval(); every != 0 {
		t.Errorf("interval is %s before it is set, want 0", every)
	}
	f.SetInterval(6 * time.Hour)
	if every := f.Interval(); every != 6*time.Hour {
		t.Errorf("interval is %s after it was set", every)
	}
}
