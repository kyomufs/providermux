package egress

import (
	"context"
	"errors"
	"time"
)

// errNoRefresher is the refusal of a deployment that rebuilds its list
// elsewhere: the panel cannot start a rebuild it does not own.
var errNoRefresher = errors.New("egress: this deployment does not rebuild its own list")

// Exit is the movable exit as the panel sees it: the group of nodes behind the
// selector and the rebuild that keeps them alive.
//
// The two halves are one object because they are one decision: a node list
// without a selector is a list nobody uses, and a selector over a stale list is
// the failure this was built to end. The panel asks one surface, and the
// surface cannot answer about a group it does not also refresh.
type Exit struct {
	*Rotator
	// Refresher rebuilds the list. May be nil, which means the deployment
	// refreshes it elsewhere and the panel can only choose among the nodes.
	Refresher *Refresher
}

// Refresh rebuilds the node list now.
func (e *Exit) Refresh(ctx context.Context) (Report, error) {
	if e.Refresher == nil {
		return Report{}, errNoRefresher
	}
	return e.Refresher.Run(ctx)
}

// RefreshStatus reports the last rebuild without starting one.
func (e *Exit) RefreshStatus() Report {
	if e.Refresher == nil {
		// The panel shows why the button does nothing rather than an empty
		// success: a refresh that never runs and reports nothing reads as a
		// list that is already fresh.
		return Report{Error: errNoRefresher.Error()}
	}
	return e.Refresher.Status()
}

// Interval reports how often the list is rebuilt; zero means never.
func (e *Exit) Interval() time.Duration {
	if e.Refresher == nil {
		return 0
	}
	return e.Refresher.Interval()
}

// SetInterval changes how often the list is rebuilt.
func (e *Exit) SetInterval(every time.Duration) {
	if e.Refresher == nil {
		return
	}
	e.Refresher.SetInterval(every)
}

// NodeSources maps a node in the profile to the subscriptions it came from.
//
// Empty when the deployment does not rebuild its own list: the panel then
// shows the nodes without a group, which is the truth.
func (e *Exit) NodeSources() map[string][]string {
	if e == nil || e.Refresher == nil {
		return nil
	}
	return e.Refresher.NodeSources()
}
