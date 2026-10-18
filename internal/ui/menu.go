package ui

import (
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type menuItem struct {
	name string
	desc string
	to   Screen
	quit bool
}

type menuModel struct {
	app    *App
	items  []menuItem
	cursor int
}

func newMenu(a *App) *menuModel {
	return &menuModel{app: a, items: []menuItem{
		{name: "Chat", desc: "ask about bugs, security and performance in your code", to: ScreenChat},
		{name: "Audit", desc: "scan this project and rank every issue by severity", to: ScreenAudit},
		{name: "Options", desc: "providers, API keys, proxy and audit settings", to: ScreenOptions},
		{name: "Exit", desc: "", quit: true},
	}}
}

func (m *menuModel) update(msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "up", "k", "shift+tab":
		m.cursor = (m.cursor - 1 + len(m.items)) % len(m.items)
	case "down", "j", "tab":
		m.cursor = (m.cursor + 1) % len(m.items)
	case "1", "2", "3", "4":
		m.cursor = int(key.String()[0] - '1')
		return m.choose()
	case "c":
		m.cursor = 0
		return m.choose()
	case "a":
		m.cursor = 1
		return m.choose()
	case "o":
		m.cursor = 2
		return m.choose()
	case "enter", " ":
		return m.choose()
	case "q", "esc", "e":
		return tea.Quit
	}
	return nil
}

func (m *menuModel) choose() tea.Cmd {
	it := m.items[m.cursor]
	if it.quit {
		return tea.Quit
	}
	return navigate(it.to)
}

func (m *menuModel) view() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("buggy"))
	b.WriteString("\n")
	b.WriteString(faintStyle.Render("find the bugs before your users do"))
	b.WriteString("\n\n")

	for i, it := range m.items {
		name := padRight(it.name, 10)
		if i == m.cursor {
			b.WriteString(boldStyle.Render("› " + name))
		} else {
			b.WriteString("  " + name)
		}
		if it.desc != "" {
			b.WriteString(faintStyle.Render(it.desc))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	home, _ := os.UserHomeDir()
	abs, _ := filepath.Abs(m.app.root)
	b.WriteString(faintStyle.Render(m.app.describe() + "  ·  " + shortPath(abs, home)))
	if m.app.audit != nil && m.app.audit.state == auditRunning {
		b.WriteString("\n" + faintStyle.Render("audit running in the background…"))
	}
	b.WriteString("\n\n")
	b.WriteString(help("↑↓", "move", "enter", "select", "q", "quit"))
	return b.String()
}
