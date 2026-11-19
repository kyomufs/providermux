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

	"providermux/internal/subscriptions"
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

	f, err := NewRefresher(RefreshConfig{Command: builder, Out: out, Rotator: rot, Sources: store(t)})
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

	f, err := NewRefresher(RefreshConfig{Command: builder, Out: t.TempDir(), Rotator: rot, Sources: store(t)})
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
	f, err := NewRefresher(RefreshConfig{Command: "proxprobe", Out: t.TempDir(), Rotator: rot, Sources: store(t)})
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

// store is a subscription list with one entry, as the panel would leave it.
func store(t *testing.T) *subscriptions.Store {
	t.Helper()
	s, err := subscriptions.Open(filepath.Join(t.TempDir(), "subs.json"))
	if err != nil {
		t.Fatalf("opening the subscription store: %v", err)
	}
	if _, err := s.Add(subscriptions.Item{ID: "main", URL: "https://sub.example.test/one", UA: "clash-verge/2.0"}); err != nil {
		t.Fatalf("adding a subscription: %v", err)
	}
	return s
}

func TestTheToolIsHandedTheListThePanelEdits(t *testing.T) {
	recorder := &reloadRecorder{now: "DIRECT"}
	srv := recorder.server(t)
	rot, err := NewRotator(RotatorConfig{Controller: srv.URL, Group: "egress", ProbeCount: 2})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	// The builder records its arguments and the subscription file it was given,
	// then produces a profile so the rest of the refresh can finish.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	copyFile := filepath.Join(dir, "sources.yaml")
	builder := fakeBuilder(t, `printf '%s\n' "$@" > `+argsFile+`; cp "$7" `+copyFile+`; mkdir -p "$3" && printf 'proxies:\n' > "$3/config.yaml" && echo "$3/config.yaml: 7 nodes"`)

	sources := store(t)
	if err := sources.SetDedupe(subscriptions.ModeNone); err != nil {
		t.Fatalf("SetDedupe: %v", err)
	}
	f, err := NewRefresher(RefreshConfig{Command: builder, Out: t.TempDir(), Rotator: rot, Sources: sources})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("reading the builder's arguments: %v", err)
	}
	// The mode is the operator's answer to "why so few nodes", so it has to reach
	// the tool rather than sit in the store.
	if !strings.Contains(string(args), "--dedupe\n"+subscriptions.ModeNone) {
		t.Errorf("the builder was not told the dedupe mode:\n%s", args)
	}
	rendered, err := os.ReadFile(copyFile)
	if err != nil {
		t.Fatalf("the builder was not handed a subscription file: %v", err)
	}
	if !strings.Contains(string(rendered), `url: "https://sub.example.test/one"`) {
		t.Errorf("the builder was handed:\n%s", rendered)
	}
}

// A subscription hands out records that differ only in the name, so the panel
// groups the nodes by the source that produced them. The profile cannot say
// that, and the builder's report does not print it, so the manifest next to the
// profile is the only place the answer exists.
func TestTheNodesKeepTheSourceThatProducedThem(t *testing.T) {
	recorder := &reloadRecorder{now: "DIRECT"}
	srv := recorder.server(t)
	rot, err := NewRotator(RotatorConfig{Controller: srv.URL, Group: "egress", ProbeCount: 2})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	out := t.TempDir()
	manifest := `{"generated":"2026-11-20T00:00:00Z","nodes":[
		{"name":"node-a","sources":["first"]},
		{"name":"node-b","sources":["first","second"]}]}`
	builder := fakeBuilder(t, `mkdir -p "$3" && printf 'proxies:\n' > "$3/config.yaml" && cat > "$3/nodes.json" <<'JSON'
`+manifest+`
JSON
echo "$3/config.yaml: 2 nodes + DIRECT"`)

	f, err := NewRefresher(RefreshConfig{Command: builder, Out: out, Rotator: rot, Sources: store(t)})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	sources := f.NodeSources()
	if got := strings.Join(sources["node-a"], ","); got != "first" {
		t.Errorf("node-a came from %q, want first", got)
	}
	if got := strings.Join(sources["node-b"], ","); got != "first,second" {
		t.Errorf("node-b came from %q, want both sources", got)
	}
}

// A manifest left over from yesterday's profile would label today's nodes with
// the wrong subscription: the names are rebuilt on every run. A missing file
// therefore clears the map instead of keeping the previous answer.
func TestAManifestThatDoesNotMatchClearsTheProvenance(t *testing.T) {
	recorder := &reloadRecorder{now: "DIRECT"}
	srv := recorder.server(t)
	rot, err := NewRotator(RotatorConfig{Controller: srv.URL, Group: "egress", ProbeCount: 2})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	out := t.TempDir()
	withManifest := fakeBuilder(t, `mkdir -p "$3" && printf 'proxies:\n' > "$3/config.yaml" && printf '{"nodes":[{"name":"node-a","sources":["first"]}]}' > "$3/nodes.json" && echo ok`)
	without := fakeBuilder(t, `mkdir -p "$3" && printf 'proxies:\n' > "$3/config.yaml" && rm -f "$3/nodes.json" && echo ok`)

	f, err := NewRefresher(RefreshConfig{Command: withManifest, Out: out, Rotator: rot, Sources: store(t)})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if len(f.NodeSources()) == 0 {
		t.Fatal("the first run kept no provenance: the test cannot see the clearing")
	}

	f.cfg.Command = without
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := f.NodeSources(); len(got) != 0 {
		t.Errorf("provenance survived a build that wrote no manifest: %v", got)
	}
}

// A daemon that restarts must show the panel the groups the controller is
// already serving: the manifest describes the loaded profile, and waiting for
// the next rebuild would leave the operator looking at an ungrouped list.
func TestTheManifestIsReadWithoutARebuild(t *testing.T) {
	recorder := &reloadRecorder{now: "DIRECT"}
	srv := recorder.server(t)
	rot, err := NewRotator(RotatorConfig{Controller: srv.URL, Group: "egress", ProbeCount: 2})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	out := t.TempDir()
	manifest := `{"nodes":[{"name":"node-a","sources":["first"]}]}`
	if err := os.WriteFile(filepath.Join(out, manifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatalf("writing the manifest: %v", err)
	}
	// A command that cannot run: any rebuild would fail the test, and the
	// provenance must appear without one.
	f, err := NewRefresher(RefreshConfig{Command: "/bin/false", Out: out, Rotator: rot, Sources: store(t)})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}
	if got := strings.Join(f.NodeSources()["node-a"], ","); got != "first" {
		t.Errorf("node-a is grouped under %q, want first, without any rebuild", got)
	}
}
