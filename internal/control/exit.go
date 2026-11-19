package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"providermux/internal/egress"
)

// ExitState is what the panel shows about the exit.
type ExitState struct {
	// Group — the selector whose choice is the exit.
	Group string `json:"group"`
	// Now — the node currently carrying traffic. Named as the controller names
	// it, because the name is what a selection is made by.
	Now string `json:"now"`
	// Home — the first member, which is the direct exit and where a rotation
	// returns to.
	Home string `json:"home,omitempty"`
	// Nodes — every member, in the controller's order.
	Nodes []ExitNode `json:"nodes"`
	// Refresh — the last rebuild of the list, and whether one is in flight.
	Refresh egress.Report `json:"refresh"`
	// Interval — how often the list is rebuilt. Zero means never, which is also
	// what a deployment without a refresher reports.
	Interval string `json:"interval,omitempty"`
	// Error — why the controller could not be read, when it could not.
	Error string `json:"error,omitempty"`
}

// ExitNode is one member of the group.
type ExitNode struct {
	// Name — the node's name at the controller.
	Name string `json:"name"`
	// Now — whether this node is the current choice.
	Now bool `json:"now"`
}

// ExitControl is the movable exit: its nodes, the subscription refresh and the
// interval between rebuilds.
//
// Filled in by the entry point when the deployment has a controller and a
// refresher. nil means the deployment has no movable exit, and the routes are
// not registered — an endpoint that always answers "not configured" is a
// surface with nothing behind it.
type ExitControl interface {
	// Members is the group's nodes and its current choice.
	Members(ctx context.Context) ([]string, string, error)
	// Delay measures one node through the controller.
	Delay(ctx context.Context, name string) (int, error)
	// Select makes the group's choice the named node.
	Select(ctx context.Context, name string) error
	// Group is the selector the controller moves.
	Group() string
	// Refresh rebuilds the node list now.
	Refresh(ctx context.Context) (egress.Report, error)
	// RefreshStatus is the last rebuild, without starting one.
	RefreshStatus() egress.Report
	// Interval is how often the list is rebuilt; zero means never.
	Interval() time.Duration
	// SetInterval changes the interval on the live daemon.
	SetInterval(every time.Duration)
}

// exitBudget bounds a panel request that reaches the controller.
//
// The panel is a person waiting: a controller that does not answer must not
// leave the page spinning. A refresh is the exception — it runs a builder over
// the whole subscription and is answered by a job, not by the request.
const exitBudget = 10 * time.Second

// handleExitStatus reports the exit and its nodes.
func (h *Handler) handleExitStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "exit")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), exitBudget)
	defer cancel()

	state := ExitState{Group: h.Exit.Group(), Refresh: h.Exit.RefreshStatus()}
	if every := h.Exit.Interval(); every > 0 {
		state.Interval = every.String()
	}

	all, now, err := h.Exit.Members(ctx)
	if err != nil {
		// The controller is unreachable, which is the state the operator needs
		// to see. Answering an empty list with 200 would hide it behind a page
		// that looks configured and does nothing.
		state.Error = err.Error()
		writeJSON(w, http.StatusOK, state)
		return
	}
	state.Now = now
	if len(all) > 0 {
		state.Home = all[0]
	}
	state.Nodes = make([]ExitNode, 0, len(all))
	for _, name := range all {
		state.Nodes = append(state.Nodes, ExitNode{Name: name, Now: name == now})
	}
	writeJSON(w, http.StatusOK, state)
}

// handleExitProbe measures one node, or every node when no name is given.
func (h *Handler) handleExitProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "exit")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), exitBudget)
	defer cancel()

	names := []string{req.Name}
	if req.Name == "" {
		all, _, err := h.Exit.Members(ctx)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		names = all
	}

	type result struct {
		Name  string `json:"name"`
		Delay int    `json:"delay"`
		Error string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(names))
	for _, name := range names {
		delay, err := h.Exit.Delay(ctx, name)
		item := result{Name: name, Delay: delay}
		if err != nil {
			item.Error = err.Error()
		}
		out = append(out, item)
		if ctx.Err() != nil {
			// Out of budget: the remaining nodes are reported as unmeasured
			// rather than as dead, which is a different statement.
			for _, rest := range names[len(out):] {
				out = append(out, result{Name: rest, Error: "not measured: the budget expired"})
			}
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// handleExitSelect makes a node the exit.
func (h *Handler) handleExitSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "exit")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), exitBudget)
	defer cancel()
	if err := h.Exit.Select(ctx, req.Name); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	h.info("exit: %q was chosen in the panel", req.Name)
	writeJSON(w, http.StatusOK, map[string]any{"now": req.Name})
}

// handleExitRefresh starts a rebuild of the node list.
//
// It answers at once: proxprobe probes every node in the subscription, which
// takes minutes, and a request held open that long would be a timeout on every
// proxy in between. The panel polls the status, where the same rebuild shows as
// running and then as finished or failed.
func (h *Handler) handleExitRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "exit")
		return
	}
	if status := h.Exit.RefreshStatus(); status.Running {
		writeJSON(w, http.StatusAccepted, map[string]any{"running": true, "started": status.Started})
		return
	}
	go func() {
		// Detached from the request on purpose: the rebuild outlives the page
		// that asked for it.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := h.Exit.Refresh(ctx); err != nil {
			h.warn("exit refresh: %v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"running": true})
}

// handleExitSchedule reads or changes the interval.
func (h *Handler) handleExitSchedule(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		state := map[string]any{"refresh": h.Exit.RefreshStatus()}
		if every := h.Exit.Interval(); every > 0 {
			state["interval"] = every.String()
		}
		writeJSON(w, http.StatusOK, state)
	case http.MethodPut:
		var req struct {
			Interval string `json:"interval"`
		}
		if err := decodeBody(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		every, err := parseInterval(req.Interval)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		h.Exit.SetInterval(every)
		h.info("exit refresh: interval is now %s", every)
		writeJSON(w, http.StatusOK, map[string]any{"interval": every.String()})
	default:
		methodNotAllowed(w, "exit")
	}
}

// parseInterval reads the interval the panel writes.
//
// Zero is a value, not an absence: an operator who turns the refresh off expects
// it to stop, and rejecting "0s" would leave them editing the file and
// restarting the daemon to do the same thing.
func parseInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("interval is required")
	}
	every, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("interval must be a duration such as 6h or 24h: %w", err)
	}
	if every < 0 {
		return 0, errors.New("interval must not be negative")
	}
	return every, nil
}
