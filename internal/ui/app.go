package ui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mf9z/buggy/internal/config"
	"github.com/mf9z/buggy/internal/provider"
)

type Screen int

const (
	ScreenMenu Screen = iota
	ScreenChat
	ScreenAudit
	ScreenOptions
)

type navigateMsg struct{ to Screen }

func navigate(to Screen) tea.Cmd {
	return func() tea.Msg { return navigateMsg{to} }
}

type App struct {
	cfg    *config.Config
	root   string
	width  int
	height int
	screen Screen

	menu    *menuModel
	chat    *chatModel
	audit   *auditModel
	options *optionsModel
}

func Run(cfg *config.Config, root string, start Screen) error {
	a := &App{cfg: cfg, root: root, width: 80, height: 24}
	a.menu = newMenu(a)
	a.screen = ScreenMenu
	p := tea.NewProgram(a, tea.WithAltScreen())
	if start != ScreenMenu {
		go p.Send(navigateMsg{start})
	}
	_, err := p.Run()
	if a.chat != nil {
		a.chat.stop()
	}
	if a.audit != nil {
		a.audit.stop()
	}
	return err
}

func (a *App) Init() tea.Cmd { return nil }

func (a *App) innerSize() (int, int) {
	w, h := a.width-4, a.height-2
	if w < 20 {
		w = 20
	}
	if h < 8 {
		h = 8
	}
	return w, h
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		w, h := a.innerSize()
		if a.chat != nil {
			a.chat.resize(w, h)
		}
		if a.audit != nil {
			a.audit.resize(w, h)
		}
		if a.options != nil {
			a.options.resize(w, h)
		}
		return a, nil
	case navigateMsg:
		return a, a.enter(msg.to)
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC && a.screen == ScreenMenu {
			return a, tea.Quit
		}
	}

	// Route async results to their owner even when hidden, so a running
	// audit keeps going while you chat.
	switch msg.(type) {
	case chatMsg:
		if a.chat != nil {
			return a, a.chat.update(msg)
		}
		return a, nil
	case auditMsg:
		if a.audit != nil {
			return a, a.audit.update(msg)
		}
		return a, nil
	case optionsMsg:
		if a.options != nil {
			return a, a.options.update(msg)
		}
		return a, nil
	}

	switch a.screen {
	case ScreenMenu:
		return a, a.menu.update(msg)
	case ScreenChat:
		return a, a.chat.update(msg)
	case ScreenAudit:
		return a, a.audit.update(msg)
	case ScreenOptions:
		return a, a.options.update(msg)
	}
	return a, nil
}

func (a *App) enter(to Screen) tea.Cmd {
	w, h := a.innerSize()
	a.screen = to
	switch to {
	case ScreenChat:
		if a.chat == nil {
			a.chat = newChat(a)
		}
		a.chat.resize(w, h)
		return a.chat.focus()
	case ScreenAudit:
		if a.audit == nil {
			a.audit = newAudit(a)
		}
		a.audit.resize(w, h)
		return a.audit.focus()
	case ScreenOptions:
		a.options = newOptions(a)
		a.options.resize(w, h)
		return nil
	}
	return nil
}

func (a *App) View() string {
	var v string
	switch a.screen {
	case ScreenChat:
		v = a.chat.view()
	case ScreenAudit:
		v = a.audit.view()
	case ScreenOptions:
		v = a.options.view()
	default:
		v = a.menu.view()
	}
	return rootPadding.Render(v)
}

func (a *App) newProvider() (provider.Provider, error) {
	return provider.New(a.cfg)
}

func (a *App) describe() string {
	return provider.Describe(a.cfg)
}

func shortPath(p, home string) string {
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
