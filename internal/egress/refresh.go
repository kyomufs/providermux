package egress

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"providermux/internal/state"
)

// RefreshConfig are the settings of the subscription refresh.
type RefreshConfig struct {
	// Command is the tool that rebuilds the profile, usually proxprobe.
	Command string
	// Sources is the file the tool reads its subscriptions from.
	Sources string
	// Out is the directory the tool writes the profile to.
	Out string
	// Check is the liveness check the tool runs on every node.
	Check string
	// Rotator reloads the rebuilt profile and returns the exit home.
	Rotator *Rotator
	// Log is where the result is written. May be nil.
	Log *state.Logger
}

// profileFile is the name the tool writes its core profile under. The name is
// the tool's, not ours: it is the file a core reads when it is pointed at a
// directory, and renaming it here would break that contract.
const profileFile = "config.yaml"

// Report is the outcome of one refresh.
type Report struct {
	// Started — when the last refresh began.
	Started time.Time `json:"started"`
	// Finished — when it ended; zero while it runs.
	Finished time.Time `json:"finished"`
	// Running — whether a refresh is in flight.
	Running bool `json:"running"`
	// Nodes — how many nodes the rebuilt profile carries, as the tool said.
	Nodes string `json:"nodes,omitempty"`
	// Error — the failure, empty on success.
	Error string `json:"error,omitempty"`
}

// Refresher rebuilds the exit's node list and reloads the controller with it.
//
// WHY IN THE GATEWAY. The node list and the process that chooses an exit are
// two halves of one thing: a list refreshed by a timer in one place and a
// controller configured in another drift apart the first time either changes.
// Owning the refresh here means the interval is one value the panel shows and
// writes, not a systemd unit whose timer file has to be edited and reloaded.
//
// The tool is a separate binary on purpose: parsing four subscription formats
// is a program of its own, and the gateway only needs its result.
type Refresher struct {
	cfg    RefreshConfig
	mu     sync.Mutex
	report Report

	// gate serializes runs: two refreshes at once would write one profile over
	// the other and reload the controller with whichever finished last.
	gate chan struct{}

	// interval is live: the panel changes it without a restart.
	interval time.Duration
	wake     chan struct{}
}

// NewRefresher creates the refresher.
func NewRefresher(cfg RefreshConfig) (*Refresher, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("egress: refresh command must not be empty")
	}
	if strings.TrimSpace(cfg.Out) == "" {
		return nil, fmt.Errorf("egress: refresh output directory must not be empty")
	}
	if cfg.Rotator == nil {
		return nil, fmt.Errorf("egress: refresh needs a rotator to reload the profile")
	}
	if cfg.Check == "" {
		cfg.Check = "http"
	}
	return &Refresher{cfg: cfg, gate: make(chan struct{}, 1), wake: make(chan struct{}, 1)}, nil
}

// Run rebuilds the profile, reloads the controller and returns the exit home.
func (f *Refresher) Run(ctx context.Context) (Report, error) {
	select {
	case f.gate <- struct{}{}:
		defer func() { <-f.gate }()
	case <-ctx.Done():
		return f.Status(), ctx.Err()
	}

	f.mu.Lock()
	f.report = Report{Running: true, Started: time.Now()}
	started := f.report.Started
	f.mu.Unlock()

	report, err := f.run(ctx)

	f.mu.Lock()
	// The rebuild may have failed before it produced anything; the moment it
	// began is still the operator's answer to "how long did this take".
	report.Started = started
	report.Running = false
	report.Finished = time.Now()
	if err != nil {
		report.Error = err.Error()
	}
	f.report = report
	f.mu.Unlock()

	if err != nil {
		if f.cfg.Log != nil {
			f.cfg.Log.Warn("egress refresh: %v", err)
		}
		return report, err
	}
	if f.cfg.Log != nil {
		f.cfg.Log.Info("egress refresh: %s", report.Nodes)
	}
	return report, nil
}

// run performs one refresh.
func (f *Refresher) run(ctx context.Context) (Report, error) {
	args := []string{"--check=" + f.cfg.Check, "--mihomo", f.cfg.Out, "--out", f.cfg.Out}
	if f.cfg.Sources != "" {
		args = append(args, "--config", f.cfg.Sources)
	}
	cmd := exec.CommandContext(ctx, f.cfg.Command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Report{}, fmt.Errorf("%s: %w: %s", f.cfg.Command, err, tail(string(out), 300))
	}

	profile := filepath.Join(f.cfg.Out, profileFile)
	if err := f.cfg.Rotator.Reload(ctx, profile); err != nil {
		return Report{Nodes: nodesLine(string(out))}, err
	}
	// A reload keeps the selector's choice, and the node it names may be one the
	// new profile no longer carries. The home is the first member, and coming
	// back to it is what makes the next refusal the one that rotates.
	if err := f.cfg.Rotator.Settle(ctx); err != nil {
		return Report{Nodes: nodesLine(string(out))}, err
	}
	return Report{Nodes: nodesLine(string(out))}, nil
}

// Start runs the refresh on its interval until the context ends.
//
// A failure does not stop the loop: the next tick is a new attempt, and a
// subscription that was unreachable for one night is usually reachable the next.
func (f *Refresher) Start(ctx context.Context) {
	go func() {
		for {
			f.mu.Lock()
			every := f.interval
			f.mu.Unlock()
			if every <= 0 {
				select {
				case <-ctx.Done():
					return
				case <-f.wake:
					continue
				}
			}
			timer := time.NewTimer(every)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-f.wake:
				timer.Stop()
			case <-timer.C:
				if _, err := f.Run(ctx); err != nil && errors.Is(err, context.Canceled) {
					return
				}
			}
		}
	}()
}

// Interval reports how often the profile is rebuilt. Zero means never.
func (f *Refresher) Interval() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interval
}

// SetInterval changes how often the profile is rebuilt.
//
// The running loop is woken so the new interval takes effect now rather than
// after the old one expires: an operator who shortened it expects a refresh
// within the new period, not within the old.
func (f *Refresher) SetInterval(every time.Duration) {
	f.mu.Lock()
	f.interval = every
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Status reports the last refresh.
func (f *Refresher) Status() Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.report
}

// nodesLine pulls the tool's own summary line out of its output.
//
// The tool prints it and it is the most useful number the operator can see:
// how many nodes the list actually has after duplicate removal and liveness
// probing. Re-parsing the profile here would be a second implementation of the
// same count.
func nodesLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "config.yaml:"); i >= 0 {
			return strings.TrimSpace(line[i+len("config.yaml:"):])
		}
	}
	return ""
}

// tail keeps the end of a tool's output, which is where its failure is.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return strings.Join(strings.Fields(s), " ")
	}
	return strings.Join(strings.Fields(s[len(s)-n:]), " ")
}
