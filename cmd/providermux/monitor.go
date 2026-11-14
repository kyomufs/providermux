package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"providermux/cmd/providermux/monitor"
	"providermux/internal/state"
)

// cmdMonitor launches the TUI.
func cmdMonitor(args []string) error {
	cfg, _, err := loadConfig(args)
	if err != nil || cfg == nil {
		return err
	}

	st, _ := state.Open(cfg.Paths.StateFile)
	snap := state.Snapshot{}
	if st != nil {
		snap = st.Snapshot()
	}

	m := monitor.InitialModel(cfg, snap)
	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return nil
}
