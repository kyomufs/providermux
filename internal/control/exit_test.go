package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"providermux/internal/egress"
	"providermux/internal/subscriptions"
)

// fakeController is a mihomo controller with one selector group.
func fakeController(t *testing.T) *httptest.Server {
	t.Helper()
	now := "node-b"
	mux := http.NewServeMux()
	mux.HandleFunc("/proxies/egress", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeTest(t, w, map[string]any{
				"all": []string{"DIRECT", "node-a", "node-b"},
				"now": now,
			})
		case http.MethodPut:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			now = body.Name
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/proxies/node-a/delay", func(w http.ResponseWriter, r *http.Request) {
		writeTest(t, w, map[string]any{"delay": 209})
	})
	mux.HandleFunc("/proxies/node-b/delay", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "timeout", http.StatusGatewayTimeout)
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeTest(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("writing the answer: %v", err)
	}
}

// exitUnderTest builds a handler whose exit is a real rotator over a fake
// controller: the handlers do not know the difference, which is the point.
func exitUnderTest(t *testing.T) *Handler {
	t.Helper()
	rot, err := egress.NewRotator(egress.RotatorConfig{
		Controller: fakeController(t).URL,
		Group:      "egress",
		ProbeCount: 3,
	})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	return &Handler{Exit: &egress.Exit{Rotator: rot}}
}

func call(t *testing.T, h *Handler, method, route, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	req := httptest.NewRequest(method, Prefix+route, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestThePanelShowsTheNodesAndTheChoice(t *testing.T) {
	h := exitUnderTest(t)

	rec := call(t, h, http.MethodGet, "exit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	var state ExitState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if state.Group != "egress" {
		t.Errorf("group is %q, want %q", state.Group, "egress")
	}
	if state.Now != "node-b" {
		t.Errorf("now is %q, want %q", state.Now, "node-b")
	}
	if state.Home != "DIRECT" {
		t.Errorf("home is %q, want %q: the first member is where a rotation returns", state.Home, "DIRECT")
	}
	if len(state.Nodes) != 3 {
		t.Fatalf("%d nodes, want 3: %+v", len(state.Nodes), state.Nodes)
	}
	for _, node := range state.Nodes {
		if node.Now != (node.Name == "node-b") {
			t.Errorf("node %q reports now=%v", node.Name, node.Now)
		}
	}
	if state.Error != "" {
		t.Errorf("error is %q, want empty", state.Error)
	}
}

func TestAnUnreachableControllerIsReportedAndNotSwallowed(t *testing.T) {
	rot, err := egress.NewRotator(egress.RotatorConfig{
		Controller: "http://127.0.0.1:1",
		Group:      "egress",
		ProbeCount: 3,
	})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	h := &Handler{Exit: &egress.Exit{Rotator: rot}}

	rec := call(t, h, http.MethodGet, "exit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: the failure belongs in the body", rec.Code)
	}
	var state ExitState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if state.Error == "" {
		t.Fatal("an unreachable controller reported no error: the panel would show an empty but healthy group")
	}
}

func TestAProbeReportsTheDelayAndTheFailureApart(t *testing.T) {
	h := exitUnderTest(t)

	rec := call(t, h, http.MethodPost, "exit/probe", `{"name":"node-a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	var answer struct {
		Nodes []struct {
			Name  string `json:"name"`
			Delay int    `json:"delay"`
			Error string `json:"error"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if len(answer.Nodes) != 1 || answer.Nodes[0].Delay != 209 || answer.Nodes[0].Error != "" {
		t.Fatalf("probe answered %+v, want one node with delay 209 and no error", answer.Nodes)
	}

	rec = call(t, h, http.MethodPost, "exit/probe", `{"name":"node-b"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if answer.Nodes[0].Error == "" {
		t.Fatal("a node the controller could not measure reported no error")
	}
	if answer.Nodes[0].Delay != 0 {
		t.Errorf("delay is %d, want 0 for an unmeasured node", answer.Nodes[0].Delay)
	}
}

func TestEveryNodeIsProbedWhenNoNameIsGiven(t *testing.T) {
	h := exitUnderTest(t)

	rec := call(t, h, http.MethodPost, "exit/probe", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	var answer struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if len(answer.Nodes) != 3 {
		t.Fatalf("probed %d nodes, want every one of 3: %+v", len(answer.Nodes), answer.Nodes)
	}
}

func TestAListOfNamesProbesOnlyThatList(t *testing.T) {
	// The panel has a button per subscription: the group is a slice of the
	// group members, so a probe of one group must not spend its budget on the
	// nodes of another subscription.
	h := exitUnderTest(t)

	rec := call(t, h, http.MethodPost, "exit/probe", `{"names":["node-a","node-b"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	var answer struct {
		Nodes []struct {
			Name  string `json:"name"`
			Delay int    `json:"delay"`
			Error string `json:"error"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if len(answer.Nodes) != 2 {
		t.Fatalf("probed %d nodes, want the 2 that were named: %+v", len(answer.Nodes), answer.Nodes)
	}
	if answer.Nodes[0].Name != "node-a" || answer.Nodes[0].Delay != 209 {
		t.Errorf("first node is %+v, want node-a at 209 ms", answer.Nodes[0])
	}
	if answer.Nodes[1].Name != "node-b" || answer.Nodes[1].Error == "" {
		t.Errorf("second node is %+v, want node-b with an error", answer.Nodes[1])
	}
}

func TestASingleNameWinsOverAList(t *testing.T) {
	// The per-row ping sends both fields when the panel reuses one payload
	// builder; one name means one node, and a list in the same body must not
	// silently widen it.
	h := exitUnderTest(t)

	rec := call(t, h, http.MethodPost, "exit/probe", `{"name":"node-a","names":["node-a","node-b","node-c"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	var answer struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if len(answer.Nodes) != 1 || answer.Nodes[0].Name != "node-a" {
		t.Fatalf("probed %+v, want only node-a", answer.Nodes)
	}
}

func TestSelectingANodeMovesTheGroup(t *testing.T) {
	h := exitUnderTest(t)

	rec := call(t, h, http.MethodPost, "exit/select", `{"name":"DIRECT"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	status := call(t, h, http.MethodGet, "exit", "")
	var state ExitState
	if err := json.Unmarshal(status.Body.Bytes(), &state); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, status.Body)
	}
	if state.Now != "DIRECT" {
		t.Errorf("now is %q after selecting DIRECT", state.Now)
	}
}

func TestAnIntervalIsWrittenAndZeroTurnsTheRefreshOff(t *testing.T) {
	rot, err := egress.NewRotator(egress.RotatorConfig{
		Controller: fakeController(t).URL,
		Group:      "egress",
		ProbeCount: 3,
	})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	store, err := subscriptions.Open(filepath.Join(t.TempDir(), "subs.json"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	if _, err := store.Add(subscriptions.Item{ID: "main", URL: "https://sub.example.test/a"}); err != nil {
		t.Fatalf("adding a subscription: %v", err)
	}
	refresher, err := egress.NewRefresher(egress.RefreshConfig{
		Command: "proxprobe",
		Out:     t.TempDir(),
		Rotator: rot,
		Sources: store,
	})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}
	h := &Handler{Exit: &egress.Exit{Rotator: rot, Refresher: refresher}}

	rec := call(t, h, http.MethodPut, "exit/schedule", `{"interval":"6h"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	if every := refresher.Interval(); every != 6*time.Hour {
		t.Errorf("interval is %s, want 6h", every)
	}

	rec = call(t, h, http.MethodPut, "exit/schedule", `{"interval":"0s"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d for 0s, want 200: %s", rec.Code, rec.Body)
	}
	if every := refresher.Interval(); every != 0 {
		t.Errorf("interval is %s after 0s, want 0", every)
	}

	rec = call(t, h, http.MethodPut, "exit/schedule", `{"interval":"-1h"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status is %d for a negative interval, want 400", rec.Code)
	}
}

func TestARefreshOnADeploymentWithoutOneIsRefused(t *testing.T) {
	h := exitUnderTest(t)

	rec := call(t, h, http.MethodPost, "exit/refresh", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status is %d, want 202: the refresh is detached from the request", rec.Code)
	}
	// The refresh is detached from the request, so the refusal lands in the
	// status a moment later.
	var state ExitState
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := call(t, h, http.MethodGet, "exit", "")
		if err := json.Unmarshal(status.Body.Bytes(), &state); err != nil {
			t.Fatalf("decoding the answer: %v: %s", err, status.Body)
		}
		if state.Refresh.Error != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(state.Refresh.Error, "does not rebuild its own list") {
		t.Errorf("refresh error is %q, want the refusal of a deployment without a refresher", state.Refresh.Error)
	}
}

// The operator sees the exit grouped by subscription, so the panel answer has
// to carry the provenance of each node. The controller cannot say it and the
// profile does not contain it: it exists only in the manifest a rebuild writes.
func TestThePanelGroupsNodesByTheSubscriptionThatProducedThem(t *testing.T) {
	rot, err := egress.NewRotator(egress.RotatorConfig{
		Controller: fakeController(t).URL,
		Group:      "egress",
		ProbeCount: 3,
	})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}

	builder := filepath.Join(t.TempDir(), "builder.sh")
	script := `#!/bin/sh
mkdir -p "$3"
printf 'proxies:\n' > "$3/config.yaml"
cat > "$3/nodes.json" <<'JSON'
{"nodes":[{"name":"node-a","sources":["first"]},{"name":"node-b","sources":["first","second"]}]}
JSON
echo "$3/config.yaml: 2 nodes + DIRECT"
`
	if err := os.WriteFile(builder, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the builder: %v", err)
	}

	storePath := filepath.Join(t.TempDir(), "subs.json")
	if err := os.WriteFile(storePath, []byte(`{"dedupe":"none","items":[{"id":"first","url":"https://example.test/first"}]}`), 0o600); err != nil {
		t.Fatalf("writing the store: %v", err)
	}
	store, err := subscriptions.Open(storePath)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	f, err := egress.NewRefresher(egress.RefreshConfig{
		Command: builder,
		Sources: store,
		Out:     t.TempDir(),
		Check:   "none",
		Rotator: rot,
	})
	if err != nil {
		t.Fatalf("building the refresher: %v", err)
	}
	if _, err := f.Run(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	h := &Handler{Exit: &egress.Exit{Rotator: rot, Refresher: f}}
	rec := call(t, h, http.MethodGet, "exit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var state ExitState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	byName := map[string][]string{}
	for _, node := range state.Nodes {
		byName[node.Name] = node.Sources
	}
	if got := strings.Join(byName["node-a"], ","); got != "first" {
		t.Errorf("node-a is grouped under %q, want first", got)
	}
	if got := strings.Join(byName["node-b"], ","); got != "first,second" {
		t.Errorf("node-b is grouped under %q, want both subscriptions", got)
	}
	if got := byName["DIRECT"]; len(got) != 0 {
		t.Errorf("DIRECT carries sources %v, want none: it comes from no subscription", got)
	}
}

// TestEveryNodeIsProbedConcurrently pins the reason the panel used to show a
// wall of failures: a sequential walk spent the ten-second budget on the first
// dozen nodes. The fake controller holds each request long enough that a
// sequential implementation can never have two in flight.
func TestEveryNodeIsProbedConcurrently(t *testing.T) {
	const count = 24
	var inFlight, maxInFlight int32
	names := make([]string, count)
	mux := http.NewServeMux()
	for i := range names {
		names[i] = fmt.Sprintf("node-%02d", i)
	}
	mux.HandleFunc("/proxies/egress", func(w http.ResponseWriter, r *http.Request) {
		writeTest(t, w, map[string]any{"all": names, "now": names[0]})
	})
	for _, name := range names {
		name := name
		mux.HandleFunc("/proxies/"+name+"/delay", func(w http.ResponseWriter, r *http.Request) {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				max := atomic.LoadInt32(&maxInFlight)
				if cur <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			writeTest(t, w, map[string]any{"delay": 120})
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rot, err := egress.NewRotator(egress.RotatorConfig{
		Controller: srv.URL,
		Group:      "egress",
		ProbeCount: 3,
	})
	if err != nil {
		t.Fatalf("building the rotator: %v", err)
	}
	h := &Handler{Exit: &egress.Exit{Rotator: rot}}
	body, err := json.Marshal(map[string]any{"names": names})
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}

	rec := call(t, h, http.MethodPost, "exit/probe", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&maxInFlight); got < 2 {
		t.Fatalf("max in-flight requests = %d, want at least 2: the probe is sequential", got)
	}

	var answer struct {
		Nodes []struct {
			Name     string `json:"name"`
			Delay    int    `json:"delay"`
			Measured bool   `json:"measured"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if len(answer.Nodes) != count {
		t.Fatalf("got %d results, want %d", len(answer.Nodes), count)
	}
	for i, node := range answer.Nodes {
		if node.Name != names[i] {
			t.Fatalf("result %d is %q, want %q: the answer lost the request order", i, node.Name, names[i])
		}
		if !node.Measured || node.Delay != 120 {
			t.Errorf("node %q = measured %v, delay %d; want measured true, delay 120", node.Name, node.Measured, node.Delay)
		}
	}
}

// blockingExit keeps the handler waiting until its request context expires.
// Embedding the real type supplies the methods the probe path does not call.
type blockingExit struct{ *egress.Exit }

func (blockingExit) Delay(ctx context.Context, name string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestExpiredBudgetIsNotReportedAsADeadNode(t *testing.T) {
	h := &Handler{Exit: blockingExit{}}
	req := httptest.NewRequest(http.MethodPost, Prefix+"exit/probe", strings.NewReader(`{"names":["node-a","node-b"]}`))
	ctx, cancel := context.WithTimeout(req.Context(), 50*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Routes(mux)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status is %d, want 200: %s", rec.Code, rec.Body)
	}
	var answer struct {
		Nodes []struct {
			Name     string `json:"name"`
			Measured bool   `json:"measured"`
			Error    string `json:"error"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decoding the answer: %v: %s", err, rec.Body)
	}
	if len(answer.Nodes) != 2 {
		t.Fatalf("got %d results, want 2", len(answer.Nodes))
	}
	for _, node := range answer.Nodes {
		if node.Measured {
			t.Errorf("node %q is marked measured after the budget expired", node.Name)
		}
		if !strings.Contains(node.Error, "not measured") {
			t.Errorf("node %q error = %q, want a not-measured reason", node.Name, node.Error)
		}
	}
}
