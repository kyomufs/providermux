package monitor

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"providermux/internal/config"
	"providermux/internal/state"
)

type model struct {
	width, height int
	cfg           *config.Config
	snap          state.Snapshot
}

func InitialModel(cfg *config.Config, snap state.Snapshot) model {
	return model{cfg: cfg, snap: snap}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	// Simple grid-like dashboard
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#5A56E0")).
		Render("Providermux Dashboard")

	status := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1).
		Width(30).
		Render(fmt.Sprintf("Mode: %s\nAddress: %s\nTransport: %s", m.cfg.Mode.Val, m.cfg.Listen.Val, m.cfg.Transport.Val))

	content := lipgloss.JoinVertical(lipgloss.Left, header, status)

	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = "Providermux Monitor"
	return v
}
