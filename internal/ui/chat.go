package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mf9z/buggy/internal/audit"
	"github.com/mf9z/buggy/internal/config"
	"github.com/mf9z/buggy/internal/provider"
)

type chatMsg interface{ chat() }

type streamEventMsg struct {
	gen int
	ev  provider.Event
	ch  chan tea.Msg
}

type streamDoneMsg struct {
	gen  int
	text string
	err  error
}

func (streamEventMsg) chat() {}
func (streamDoneMsg) chat()  {}
func (spinTick) chat()       {}

type spinTick struct{ msg spinner.TickMsg }

type entryKind int

const (
	entryUser entryKind = iota
	entryAssistant
	entryInfo
	entryError
)

type chatEntry struct {
	kind  entryKind
	text  string
	extra string // e.g. attached file names
}

type attachment struct {
	path    string
	content string
	lines   int
}

const maxAttachBytes = 256 << 10

type chatModel struct {
	app   *App
	vp    viewport.Model
	input textarea.Model
	spin  spinner.Model

	history     []provider.Message
	entries     []chatEntry
	attachments []attachment
	system      string

	streaming bool
	partial   strings.Builder
	status    string
	started   time.Time
	gen       int
	cancel    context.CancelFunc

	width, height int
}

func newChat(a *App) *chatModel {
	ta := textarea.New()
	ta.Placeholder = "ask about your code · /help for commands"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Placeholder = faintStyle
	ta.FocusedStyle.Prompt = boldStyle
	ta.BlurredStyle = ta.FocusedStyle
	ta.SetPromptFunc(2, func(line int) string {
		if line == 0 {
			return "› "
		}
		return "  "
	})
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot

	m := &chatModel{app: a, input: ta, spin: sp, vp: viewport.New(80, 10)}
	m.vp.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
	}
	m.entries = append(m.entries, chatEntry{kind: entryInfo, text: "Chatting with " + a.describe() + ". Attach files with /add <path>."})
	return m
}

func (m *chatModel) focus() tea.Cmd {
	m.render()
	return m.input.Focus()
}

func (m *chatModel) resize(w, h int) {
	m.width, m.height = w, h
	m.input.SetWidth(w)
	vh := h - 8
	if vh < 3 {
		vh = 3
	}
	m.vp.Width, m.vp.Height = w, vh
	m.render()
}

func (m *chatModel) stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *chatModel) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case streamEventMsg:
		if msg.gen != m.gen {
			return nil
		}
		if msg.ev.Text != "" {
			m.partial.WriteString(msg.ev.Text)
			m.status = ""
		}
		if msg.ev.Status != "" {
			m.status = msg.ev.Status
		}
		m.render()
		return waitStream(msg.ch)
	case streamDoneMsg:
		if msg.gen != m.gen {
			return nil
		}
		m.finish(msg)
		return nil
	case spinTick:
		if !m.streaming {
			return nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg.msg)
		return wrapSpin(cmd)
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return nil
}

func wrapSpin(cmd tea.Cmd) tea.Cmd {
	return wrapTick(cmd, func(t spinner.TickMsg) tea.Msg { return spinTick{t} })
}

// wrapTick tags spinner ticks so the app can route them to their screen.
func wrapTick(cmd tea.Cmd, tag func(spinner.TickMsg) tea.Msg) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		if t, ok := cmd().(spinner.TickMsg); ok {
			return tag(t)
		}
		return nil
	}
}

func (m *chatModel) handleKey(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "ctrl+c":
		if m.streaming {
			m.cancelStream("stopped")
			return nil
		}
		return navigate(ScreenMenu)
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(k)
		return cmd
	case "ctrl+l":
		m.clear()
		return nil
	case "enter":
		if m.streaming {
			return nil
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return nil
		}
		m.input.Reset()
		if strings.HasPrefix(text, "/") {
			return m.command(text)
		}
		return m.send(text)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return cmd
}

func (m *chatModel) command(line string) tea.Cmd {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/help", "/?":
		m.info(strings.Join([]string{
			"/add <path...>   attach files to your next message",
			"/drop            remove pending attachments",
			"/clear           start a new conversation (ctrl+l)",
			"/audit           open the audit screen",
			"/options         open options",
			"/exit            back to the menu",
			"",
			"enter send · alt+enter newline · pgup/pgdn scroll · esc stop or back",
		}, "\n"))
	case "/add":
		if len(fields) < 2 {
			m.errorf("usage: /add <path> [path...]")
			break
		}
		for _, p := range fields[1:] {
			m.attach(p)
		}
	case "/drop":
		m.attachments = nil
		m.info("attachments cleared")
	case "/clear", "/new":
		m.clear()
	case "/audit":
		return navigate(ScreenAudit)
	case "/options", "/config":
		return navigate(ScreenOptions)
	case "/exit", "/quit", "/back":
		return navigate(ScreenMenu)
	default:
		m.errorf("unknown command %s — try /help", fields[0])
	}
	m.render()
	return nil
}

func (m *chatModel) attach(p string) {
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(m.app.root, p)
	}
	st, err := os.Stat(full)
	if err != nil {
		m.errorf("can't attach %s: %v", p, err)
		return
	}
	if st.IsDir() {
		m.errorf("%s is a directory; attach files, or use Audit for whole folders", p)
		return
	}
	if st.Size() > maxAttachBytes {
		m.errorf("%s is too large to attach (%d KB, max %d KB)", p, st.Size()>>10, maxAttachBytes>>10)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		m.errorf("can't read %s: %v", p, err)
		return
	}
	s := string(data)
	n := strings.Count(s, "\n") + 1
	rel, err := filepath.Rel(m.app.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = full
	}
	m.attachments = append(m.attachments, attachment{path: filepath.ToSlash(rel), content: s, lines: n})
	m.info(fmt.Sprintf("attached %s (%d lines)", rel, n))
}

func (m *chatModel) clear() {
	m.cancelStream("")
	m.history = nil
	m.attachments = nil
	m.entries = []chatEntry{{kind: entryInfo, text: "New conversation with " + m.app.describe() + "."}}
	m.system = ""
	m.render()
}

func (m *chatModel) send(text string) tea.Cmd {
	prov, err := m.app.newProvider()
	if err != nil {
		m.errorf("%v", err)
		m.input.SetValue(text) // keep what they typed
		m.render()
		return nil
	}
	if m.system == "" {
		m.system = chatSystemPrompt(m.app.root, m.app.cfg)
	}

	content := text
	var names []string
	for _, at := range m.attachments {
		content += fmt.Sprintf("\n\n<file path=%q>\n%s\n</file>", at.path, at.content)
		names = append(names, at.path)
	}
	m.attachments = nil
	extra := ""
	if len(names) > 0 {
		extra = "+ " + strings.Join(names, ", ")
	}
	m.entries = append(m.entries, chatEntry{kind: entryUser, text: text, extra: extra})
	m.history = append(m.history, provider.Message{Role: "user", Content: content})

	m.gen++
	gen := m.gen
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.streaming = true
	m.partial.Reset()
	m.status = "waiting for " + prov.Name()
	m.started = time.Now()
	m.vp.GotoBottom()
	m.render()

	req := provider.Request{
		System:   m.system,
		Messages: append([]provider.Message(nil), m.history...),
		Purpose:  provider.PurposeChat,
		Dir:      m.app.root,
	}
	ch := make(chan tea.Msg, 256)
	go func() {
		text, err := prov.Stream(ctx, req, func(ev provider.Event) {
			select {
			case ch <- streamEventMsg{gen: gen, ev: ev, ch: ch}:
			case <-ctx.Done():
			}
		})
		select {
		case ch <- streamDoneMsg{gen: gen, text: text, err: err}:
		case <-ctx.Done():
		}
		close(ch)
	}()
	return tea.Batch(waitStream(ch), wrapSpin(m.spin.Tick))
}

func waitStream(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func (m *chatModel) finish(msg streamDoneMsg) {
	m.streaming = false
	m.status = ""
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	text := msg.text
	if text == "" {
		text = m.partial.String()
	}
	m.partial.Reset()
	if strings.TrimSpace(text) != "" {
		m.entries = append(m.entries, chatEntry{kind: entryAssistant, text: strings.TrimSpace(text)})
		m.history = append(m.history, provider.Message{Role: "assistant", Content: text})
	}
	if msg.err != nil && msg.err != context.Canceled {
		m.errorf("%v", msg.err)
		if strings.TrimSpace(text) == "" && len(m.history) > 0 {
			// Drop the unanswered turn so the conversation stays well-formed.
			m.history = m.history[:len(m.history)-1]
		}
	}
	m.render()
}

func (m *chatModel) cancelStream(note string) {
	if !m.streaming {
		return
	}
	m.gen++ // ignore anything still in flight
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.streaming = false
	text := strings.TrimSpace(m.partial.String())
	m.partial.Reset()
	if text != "" {
		m.entries = append(m.entries, chatEntry{kind: entryAssistant, text: text})
		m.history = append(m.history, provider.Message{Role: "assistant", Content: text})
	} else if len(m.history) > 0 && m.history[len(m.history)-1].Role == "user" {
		m.history = m.history[:len(m.history)-1]
	}
	if note != "" {
		m.info(note)
	}
	m.render()
}

func (m *chatModel) info(s string) {
	m.entries = append(m.entries, chatEntry{kind: entryInfo, text: s})
}
func (m *chatModel) errorf(f string, args ...any) {
	m.entries = append(m.entries, chatEntry{kind: entryError, text: fmt.Sprintf(f, args...)})
}

func (m *chatModel) render() {
	w := m.vp.Width
	if w <= 0 {
		return
	}
	atBottom := m.vp.AtBottom() || m.streaming
	var b strings.Builder
	for i, e := range m.entries {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(renderEntry(e, w))
		b.WriteString("\n")
	}
	if m.streaming && m.partial.Len() > 0 {
		b.WriteString("\n")
		b.WriteString(renderEntry(chatEntry{kind: entryAssistant, text: m.partial.String()}, w))
		b.WriteString("\n")
	}
	m.vp.SetContent(b.String())
	if atBottom {
		m.vp.GotoBottom()
	}
}

func renderEntry(e chatEntry, w int) string {
	switch e.kind {
	case entryUser:
		s := boldStyle.Render("you") + "\n" + wrap(e.text, w)
		if e.extra != "" {
			s += "\n" + faintStyle.Render(wrap(e.extra, w))
		}
		return s
	case entryAssistant:
		return titleStyle.Render("buggy") + "\n" + colorizeTags(wrap(e.text, w))
	case entryError:
		return errStyle.Render(wrap("error: "+e.text, w))
	}
	return faintStyle.Render(wrap(e.text, w))
}

func (m *chatModel) view() string {
	var b strings.Builder
	b.WriteString(header("chat") + faintStyle.Render("  "+m.app.describe()))
	b.WriteString("\n\n")
	b.WriteString(m.vp.View())
	b.WriteString("\n\n")

	status := ""
	switch {
	case m.streaming:
		s := m.status
		if s == "" {
			s = "writing"
		}
		status = m.spin.View() + " " + faintStyle.Render(fmt.Sprintf("%s · %.0fs · esc to stop", s, time.Since(m.started).Seconds()))
	case len(m.attachments) > 0:
		var names []string
		for _, a := range m.attachments {
			names = append(names, a.path)
		}
		status = faintStyle.Render("attached: " + strings.Join(names, ", "))
	case !m.vp.AtBottom():
		status = faintStyle.Render("↓ more below (pgdn)")
	}
	b.WriteString(truncate(status, m.width))
	b.WriteString("\n")
	b.WriteString(m.input.View())
	b.WriteString("\n")
	b.WriteString(help("enter", "send", "alt+enter", "newline", "/help", "commands", "esc", "back"))
	return b.String()
}

func chatSystemPrompt(root string, cfg *config.Config) string {
	abs, _ := filepath.Abs(root)
	files, more := audit.List(abs, 400)
	var b strings.Builder
	b.WriteString(`You are buggy, a terminal assistant that helps developers find and fix bugs, security vulnerabilities and performance problems in their code.

Style:
- Be direct and concise. Lead with the answer.
- When you point out an issue, start its line with a severity tag: [HIGH], [MEDIUM] or [LOW].
  [HIGH] = exploitable vulnerability, data loss, crash on a normal path, or a severe slowdown.
  [MEDIUM] = a real defect under specific conditions or a notable inefficiency.
  [LOW] = minor defect or small inefficiency.
- Reference code as path:line. Show minimal fixes rather than whole rewrites.
- Plain text output; the terminal does not render markdown headings or tables.
- Do not invent code you have not seen. If you need a file, ask for it.
`)
	fmt.Fprintf(&b, "\nProject root: %s\n", filepath.Base(abs))
	if cfg.Mode == config.ModeProxy && cfg.Proxy.Backend == "claude-code" {
		b.WriteString("You can read files in the project with your Read, Grep and Glob tools. You cannot modify files.\n")
	} else {
		b.WriteString("You cannot open files yourself. The user attaches files with /add <path>; ask them to when you need to see code.\n")
	}
	if len(files) > 0 {
		b.WriteString("\nProject files:\n")
		for _, f := range files {
			b.WriteString(f)
			b.WriteString("\n")
		}
		if more {
			b.WriteString("(more files not listed)\n")
		}
	}
	return b.String()
}
