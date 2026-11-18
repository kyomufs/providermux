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
	// Plain weight and the terminal's own foreground: a monitor is read for its
	// numbers, and a fixed accent colour fights every theme it lands in.
	header := lipgloss.NewStyle().Bold(true).Render("providermux monitor")

	status := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		Padding(0, 1).
		Render(fmt.Sprintf("Mode: %s\nAddress: %s\nTransport: %s", m.cfg.Mode.Val, m.cfg.Listen.Val, m.cfg.Transport.Val))

	footer := lipgloss.NewStyle().Faint(true).Render("q — quit")

	content := lipgloss.JoinVertical(lipgloss.Left, header, "", status, "", footer)

	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = "Providermux Monitor"
	return v
}
