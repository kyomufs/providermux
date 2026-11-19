package egress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeController is a proxy controller with a node list, one current choice and
// a set of dead nodes.
type fakeController struct {
	mu      sync.Mutex
	nodes   []string
	now     string
	dead    map[string]bool
	secret  string
	probes  []string
	chosen  []string
	queries []url.Values
}

func (c *fakeController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.secret != "" && r.Header.Get("authorization") != "Bearer "+c.secret {
		http.Error(w, "no secret", http.StatusUnauthorized)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/proxies/")
	switch {
	case rest == "egress" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"all": c.nodes, "now": c.now})
	case rest == "egress" && r.Method == http.MethodPut:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.chosen = append(c.chosen, body.Name)
		c.now = body.Name
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(rest, "/delay"):
		name := strings.TrimSuffix(rest, "/delay")
		c.probes = append(c.probes, name)
		c.queries = append(c.queries, r.URL.Query())
		if c.dead[name] {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = io.WriteString(w, `{"message":"Timeout"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"delay": 42})
	default:
		http.NotFound(w, r)
	}
}

func newRotator(t *testing.T, c *fakeController, probeCount int) (*Rotator, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	rot, err := NewRotator(RotatorConfig{
		Controller: srv.URL,
		Group:      "egress",
		Secret:     c.secret,
		ProbeURL:   "https://1.1.1.1/cdn-cgi/trace",
		ProbeCount: probeCount,
	})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	return rot, srv
}

// TestRotateSkipsDeadNodesAndWalksForward — the dead node is passed over, and the
// walk continues from the current choice rather than from the head of the list.
//
// Starting from the head would rotate between the same two nodes forever, and the
// second one is the address that just refused.
func TestRotateSkipsDeadNodesAndWalksForward(t *testing.T) {
	c := &fakeController{nodes: []string{"a", "b", "c"}, now: "a", dead: map[string]bool{"b": true}}
	rot, _ := newRotator(t, c, 6)

	got, err := rot.Rotate(context.Background())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got != "c" {
		t.Fatalf("rotated to %q, want %q", got, "c")
	}
	if len(c.chosen) != 1 || c.chosen[0] != "c" {
		t.Fatalf("chosen = %v, want [c]", c.chosen)
	}
	if len(c.probes) != 2 {
		t.Fatalf("probes = %v, want the dead one and the live one", c.probes)
	}
}

// TestRotateEscapesNodeNames — node names are recorded as they are displayed,
// spaces and punctuation included, and one name must remain one path segment.
func TestRotateEscapesNodeNames(t *testing.T) {
	c := &fakeController{nodes: []string{"Нидерланды ALT #1", "x"}, now: "x"}
	rot, _ := newRotator(t, c, 6)

	got, err := rot.Rotate(context.Background())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got != "Нидерланды ALT #1" {
		t.Fatalf("rotated to %q", got)
	}
	if len(c.probes) != 1 || c.probes[0] != "Нидерланды ALT #1" {
		t.Fatalf("the controller saw probes %q, want the name as one segment", c.probes)
	}
	if q := c.queries[0].Get("url"); q != "https://1.1.1.1/cdn-cgi/trace" {
		t.Fatalf("probe url = %q", q)
	}
	if q := c.queries[0].Get("timeout"); q != "4000" {
		t.Fatalf("probe timeout = %q", q)
	}
}

// TestRotateRefusesWithoutALiveNode — an exit that cannot carry traffic must not
// be selected: the group keeps the spent address the caller already had, and the
// refusal names the search that failed.
func TestRotateRefusesWithoutALiveNode(t *testing.T) {
	c := &fakeController{
		nodes: []string{"a", "b"},
		now:   "a",
		dead:  map[string]bool{"a": true, "b": true},
	}
	rot, _ := newRotator(t, c, 6)

	if _, err := rot.Rotate(context.Background()); err == nil {
		t.Fatal("a rotation to nothing was reported as success")
	} else if !strings.Contains(err.Error(), "no live node") {
		t.Fatalf("the error does not describe the search: %v", err)
	}
	if len(c.chosen) != 0 {
		t.Fatalf("a node was selected anyway: %v", c.chosen)
	}
}

// TestRotateRespectsTheProbeCount — the budget is a budget: a rotation inside a
// client request may not probe the whole list.
func TestRotateRespectsTheProbeCount(t *testing.T) {
	c := &fakeController{
		nodes: []string{"a", "b", "c"},
		now:   "a",
		dead:  map[string]bool{"b": true},
	}
	rot, _ := newRotator(t, c, 1)

	if _, err := rot.Rotate(context.Background()); err == nil {
		t.Fatal("the probe budget was exceeded")
	}
	if len(c.probes) != 1 {
		t.Fatalf("probes = %d, want the budget of 1", len(c.probes))
	}
}

// TestRotateSendsTheSecret — a controller with a shared secret refuses the rest.
func TestRotateSendsTheSecret(t *testing.T) {
	c := &fakeController{nodes: []string{"a", "b"}, now: "a", secret: "s3cret"}
	rot, srv := newRotator(t, c, 6)

	unauthenticated, err := NewRotator(RotatorConfig{
		Controller: srv.URL, Group: "egress", ProbeCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unauthenticated.Rotate(context.Background()); err == nil {
		t.Fatal("an unauthenticated rotation succeeded")
	}

	got, err := rot.Rotate(context.Background())
	if err != nil {
		t.Fatalf("Rotate with the secret: %v", err)
	}
	if got != "b" {
		t.Fatalf("rotated to %q, want b", got)
	}
}

// TestRotateNeedsSomethingToRotateTo — a single-node group cannot rotate, and
// saying so is more useful than probing the node it already uses.
func TestRotateNeedsSomethingToRotateTo(t *testing.T) {
	c := &fakeController{nodes: []string{"only"}, now: "only"}
	rot, _ := newRotator(t, c, 6)

	if _, err := rot.Rotate(context.Background()); err == nil {
		t.Fatal("a rotation with one node was reported as success")
	}
}
