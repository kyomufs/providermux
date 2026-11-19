package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
