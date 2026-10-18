package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rohan/buggy/internal/audit"
)

// auditMsg marks async messages owned by the audit screen.
type auditMsg interface{ audit() }

type auditProgressMsg struct {
	gen int
	p   audit.Progress
	ch  chan tea.Msg
}

type auditDoneMsg struct {
	gen int
	rep *audit.Report
	err error
}

type auditSpinTick struct{ msg spinner.TickMsg }

func (auditProgressMsg) audit() {}
func (auditDoneMsg) audit()     {}
func (auditSpinTick) audit()    {}

type auditState int

const (
	auditInput auditState = iota
	auditRunning
	auditResults
)

var severityFilters = []string{"all", "high", "medium", "low"}
var categoryFilters = []string{"all", "security", "performance", "bug"}

type auditModel struct {
	app   *App
	state auditState
	path  textinput.Model
	spin  spinner.Model

	gen      int
	cancel   context.CancelFunc
	started  time.Time
	progress audit.Progress
	live     []audit.Finding
	errs     int
	backend  string

	rep      *audit.Report
	runErr   error
	cursor   int
	expanded map[int]bool
	sevF     int
	catF     int
	flash    string

	width, height int
}

func newAudit(a *App) *auditModel {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.PromptStyle = boldStyle
	ti.Placeholder = "."
	ti.SetValue(".")
	ti.CursorEnd()
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	return &auditModel{app: a, path: ti, spin: sp, expanded: map[int]bool{}}
}

func (m *auditModel) focus() tea.Cmd {
	if m.state == auditInput {
		return m.path.Focus()
	}
	return nil
}

func (m *auditModel) resize(w, h int) {
	m.width, m.height = w, h
	m.path.Width = w - 4
}

func (m *auditModel) stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *auditModel) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case auditProgressMsg:
		if msg.gen != m.gen {
			return nil
		}
		m.progress = msg.p
		m.live = append(m.live, msg.p.New...)
		if msg.p.Err != nil {
			m.errs++
		}
		return waitStream(msg.ch)
	case auditDoneMsg:
		if msg.gen != m.gen {
			return nil
		}
		m.cancel = nil
		m.rep, m.runErr = msg.rep, msg.err
		m.state = auditResults
		m.cursor, m.sevF, m.catF = 0, 0, 0
		m.expanded = map[int]bool{}
		return nil
	case auditSpinTick:
		if m.state != auditRunning {
			return nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg.msg)
		return wrapAuditSpin(cmd)
	case tea.KeyMsg:
		switch m.state {
		case auditInput:
			return m.keyInput(msg)
		case auditRunning:
			return m.keyRunning(msg)
		case auditResults:
			return m.keyResults(msg)
		}
	}
	return nil
}

func wrapAuditSpin(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		if t, ok := cmd().(spinner.TickMsg); ok {
			return auditSpinTick{t}
		}
		return nil
	}
}

func (m *auditModel) keyInput(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "ctrl+c":
		return navigate(ScreenMenu)
	case "enter":
		return m.start()
	}
	var cmd tea.Cmd
	m.path, cmd = m.path.Update(k)
	return cmd
}

func (m *auditModel) keyRunning(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "ctrl+c", "x":
		// Cancel; the done message delivers partial results.
		if m.cancel != nil {
			m.cancel()
		}
	case "esc", "q":
		return navigate(ScreenMenu) // keep running in the background
	}
	return nil
}

func (m *auditModel) start() tea.Cmd {
	target := strings.TrimSpace(m.path.Value())
	if target == "" {
		target = "."
	}
	if strings.HasPrefix(target, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			target = filepath.Join(home, target[2:])
		}
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.app.root, target)
	}
	if _, err := os.Stat(target); err != nil {
		m.flash = errStyle.Render(fmt.Sprintf("can't open %s: %v", target, err))
		return nil
	}
	prov, err := m.app.newProvider()
	if err != nil {
		m.flash = errStyle.Render(err.Error())
		return nil
	}
	m.flash = ""
	m.gen++
	gen := m.gen
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.state = auditRunning
	m.started = time.Now()
	m.progress = audit.Progress{Stage: "scanning"}
	m.live = nil
	m.errs = 0
	m.backend = m.app.describe()
	m.path.Blur()

	cfg := m.app.cfg.Audit
	ch := make(chan tea.Msg, 1024)
	go func() {
		rep, err := audit.Run(ctx, prov, target, cfg, func(p audit.Progress) {
			select {
			case ch <- auditProgressMsg{gen: gen, p: p, ch: ch}:
			default: // never stall the audit on a slow UI
				if len(p.New) > 0 || p.Stage == "done" {
					ch <- auditProgressMsg{gen: gen, p: p, ch: ch}
				}
			}
		})
		ch <- auditDoneMsg{gen: gen, rep: rep, err: err}
		close(ch)
	}()
	return tea.Batch(waitStream(ch), wrapAuditSpin(m.spin.Tick))
}

// visible returns the indices of findings that pass the filters.
func (m *auditModel) visible() []int {
	if m.rep == nil {
		return nil
	}
	var idx []int
	for i, f := range m.rep.Findings {
		if m.sevF > 0 && f.Severity != severityFilters[m.sevF] {
			continue
		}
		if m.catF > 0 && f.Category != categoryFilters[m.catF] {
			continue
		}
		idx = append(idx, i)
	}
	return idx
}

func (m *auditModel) keyResults(k tea.KeyMsg) tea.Cmd {
	vis := m.visible()
	m.flash = ""
	switch k.String() {
	case "esc", "q", "ctrl+c":
		return navigate(ScreenMenu)
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(vis)-1 {
			m.cursor++
		}
	case "pgup":
		m.cursor = max(0, m.cursor-10)
	case "pgdown":
		m.cursor = max(0, min(len(vis)-1, m.cursor+10))
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = max(0, len(vis)-1)
	case "enter", " ", "right", "l", "left", "h":
		if m.cursor < len(vis) {
			i := vis[m.cursor]
			m.expanded[i] = !m.expanded[i]
		}
	case "e":
		all := true
		for _, i := range vis {
			all = all && m.expanded[i]
		}
		for _, i := range vis {
			m.expanded[i] = !all
		}
	case "f":
		m.sevF = (m.sevF + 1) % len(severityFilters)
		m.cursor = 0
	case "c":
		m.catF = (m.catF + 1) % len(categoryFilters)
		m.cursor = 0
	case "s":
		m.save("md")
	case "J":
		m.save("json")
	case "r":
		m.state = auditInput
		return m.path.Focus()
	case "n":
		m.state = auditInput
		m.path.SetValue(".")
		return m.path.Focus()
	}
	return nil
}

func (m *auditModel) save(format string) {
	if m.rep == nil {
		return
	}
	dir := m.rep.Root
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		dir = filepath.Dir(dir)
	}
	var data []byte
	var err error
	name := "buggy-report.md"
	if format == "json" {
		name = "buggy-report.json"
		data, err = m.rep.JSON()
	} else {
		data = []byte(m.rep.Markdown())
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	if err != nil {
		m.flash = errStyle.Render("save failed: " + err.Error())
		return
	}
	home, _ := os.UserHomeDir()
	m.flash = okStyle.Render("saved " + shortPath(filepath.Join(dir, name), home))
}

func (m *auditModel) view() string {
	switch m.state {
	case auditRunning:
		return m.viewRunning()
	case auditResults:
		return m.viewResults()
	}
	return m.viewInput()
}

func (m *auditModel) viewInput() string {
	var b strings.Builder
	b.WriteString(header("audit"))
	b.WriteString("\n\n")
	b.WriteString("What should buggy audit?\n")
	b.WriteString(faintStyle.Render("a folder or file, relative to " + filepath.Base(absPath(m.app.root))))
	b.WriteString("\n\n")
	b.WriteString(m.path.View())
	b.WriteString("\n\n")

	c := m.app.cfg.Audit
	var cats []string
	if c.Security {
		cats = append(cats, "security")
	}
	if c.Performance {
		cats = append(cats, "performance")
	}
	if c.Bugs {
		cats = append(cats, "bugs")
	}
	b.WriteString(faintStyle.Render(fmt.Sprintf("backend   %s\nchecks    %s\nminimum   %s severity\nworkers   %d parallel requests",
		m.app.describe(), strings.Join(cats, ", "), c.MinSeverity, c.Concurrency)))
	b.WriteString("\n\n")
	if m.flash != "" {
		b.WriteString(m.flash + "\n\n")
	}
	b.WriteString(help("enter", "start", "esc", "back"))
	return b.String()
}

func (m *auditModel) viewRunning() string {
	var b strings.Builder
	p := m.progress
	b.WriteString(header("audit"))
	b.WriteString("\n\n")
	elapsed := time.Since(m.started).Round(time.Second)
	if p.Stage == "scanning" || p.BatchesTotal == 0 {
		b.WriteString(m.spin.View() + " scanning files…  " + faintStyle.Render(elapsed.String()))
		b.WriteString("\n")
	} else {
		b.WriteString(m.spin.View() + fmt.Sprintf(" auditing %s with %s", plural(p.Files, "file"), m.backend))
		b.WriteString("\n\n")
		barW := min(m.width-16, 50)
		b.WriteString(progressBar(p.BatchesDone, p.BatchesTotal, barW))
		b.WriteString(faintStyle.Render(fmt.Sprintf("  %d/%d  %s", p.BatchesDone, p.BatchesTotal, elapsed)))
		b.WriteString("\n\n")
		h, md, l := countSev(m.live)
		b.WriteString(sevCounts(h, md, l))
		if m.errs > 0 {
			b.WriteString(errStyle.Render(fmt.Sprintf("   %d failed", m.errs)))
		}
		b.WriteString("\n\n")
		for _, a := range p.Active {
			b.WriteString(faintStyle.Render("  · " + truncate(a, m.width-4)))
			b.WriteString("\n")
		}
		// Most recent findings, newest last.
		room := m.height - 12 - len(p.Active)
		if room > 0 && len(m.live) > 0 {
			b.WriteString("\n")
			start := max(0, len(m.live)-room)
			for _, f := range m.live[start:] {
				b.WriteString(findingLine(f, m.width, false))
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\n")
	b.WriteString(help("x", "stop", "esc", "run in background"))
	return b.String()
}

func (m *auditModel) viewResults() string {
	var b strings.Builder
	b.WriteString(header("audit"))
	b.WriteString("\n\n")
	rep := m.rep
	if rep == nil {
		b.WriteString(errStyle.Render(wrap("audit failed: "+errString(m.runErr), m.width)))
		b.WriteString("\n\n")
		b.WriteString(help("r", "retry", "esc", "back"))
		return b.String()
	}
	h, md, l := rep.Counts()
	b.WriteString(sevCounts(h, md, l))
	b.WriteString(faintStyle.Render(fmt.Sprintf("   %s · %s · %s · %s", plural(rep.Files, "file"), plural(rep.Lines, "line"), rep.Duration.Round(time.Second), rep.Model)))
	b.WriteString("\n")
	if m.runErr != nil {
		msg := errString(m.runErr)
		if m.runErr == context.Canceled {
			msg = "stopped early — partial results"
		}
		b.WriteString(errStyle.Render(truncate(msg, m.width)))
		b.WriteString("\n")
	} else if len(rep.Errors) > 0 {
		b.WriteString(errStyle.Render(truncate(fmt.Sprintf("%d request(s) failed: %s", len(rep.Errors), rep.Errors[0]), m.width)))
		b.WriteString("\n")
	}
	b.WriteString(faintStyle.Render("severity: ") + filterLabel(severityFilters[m.sevF]) + faintStyle.Render("   category: ") + categoryFilters[m.catF])
	b.WriteString("\n\n")

	vis := m.visible()
	if len(vis) == 0 {
		if len(rep.Findings) == 0 && m.runErr == nil {
			b.WriteString(okStyle.Render("No issues found.") + faintStyle.Render(" Nice."))
		} else {
			b.WriteString(faintStyle.Render("Nothing matches this filter."))
		}
		b.WriteString("\n")
	} else {
		if m.cursor >= len(vis) {
			m.cursor = len(vis) - 1
		}
		// Render every row, then window around the cursor.
		var lines []string
		selStart, selEnd := 0, 0
		for row, i := range vis {
			f := rep.Findings[i]
			if row == m.cursor {
				selStart = len(lines)
			}
			lines = append(lines, findingLine(f, m.width, row == m.cursor))
			if m.expanded[i] {
				lines = append(lines, detailLines(f, m.width)...)
			}
			if row == m.cursor {
				selEnd = len(lines)
			}
		}
		room := max(3, m.height-9)
		top := 0
		if selEnd > room {
			top = selEnd - room
		}
		if selStart < top {
			top = selStart
		}
		end := min(len(lines), top+room)
		if top > 0 {
			lines[top] = faintStyle.Render(fmt.Sprintf("  ↑ %d more", top))
		}
		if end < len(lines) {
			lines[end-1] = faintStyle.Render(fmt.Sprintf("  ↓ %d more", len(lines)-end+1))
		}
		b.WriteString(strings.Join(lines[top:end], "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if m.flash != "" {
		b.WriteString(m.flash + "\n")
	}
	b.WriteString(help("↑↓", "move", "enter", "details", "f", "severity", "c", "category", "s", "save .md", "J", "save .json", "r", "rerun", "esc", "back"))
	return b.String()
}

func findingLine(f audit.Finding, width int, selected bool) string {
	cursor := "  "
	if selected {
		cursor = boldStyle.Render("› ")
	}
	cat := faintStyle.Render(padRight(f.Category, 12))
	loc := f.Location()
	titleW := width - 2 - 7 - 12 - len(loc) - 2
	if titleW < 20 {
		titleW = width - 2 - 7 - 12
		loc = ""
	}
	title := padRight(truncate(f.Title, titleW), titleW)
	if selected {
		title = boldStyle.Render(title)
	}
	line := cursor + badge(f.Severity) + " " + cat + title
	if loc != "" {
		line += "  " + faintStyle.Render(loc)
	}
	return line
}

func detailLines(f audit.Finding, width int) []string {
	indent := "         "
	w := width - len(indent)
	var out []string
	add := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			out = append(out, indent+l)
		}
	}
	if f.Line == 0 && f.File != "" {
		add(faintStyle.Render(f.File))
	}
	if f.Description != "" {
		add(wrap(f.Description, w))
	}
	if f.Suggestion != "" {
		for i, l := range strings.Split(wrap(f.Suggestion, w-4), "\n") {
			if i == 0 {
				out = append(out, indent+okStyle.Render("fix ")+l)
			} else {
				out = append(out, indent+"    "+l)
			}
		}
	}
	out = append(out, "")
	return out
}

func progressBar(done, total, width int) string {
	if width < 10 {
		width = 10
	}
	filled := 0
	if total > 0 {
		filled = done * width / total
	}
	return boldStyle.Render(strings.Repeat("█", filled)) + faintStyle.Render(strings.Repeat("░", width-filled))
}

func sevCounts(h, m, l int) string {
	return severityStyle("high").Render(fmt.Sprintf("%d high", h)) + faintStyle.Render(" · ") +
		severityStyle("medium").Render(fmt.Sprintf("%d medium", m)) + faintStyle.Render(" · ") +
		severityStyle("low").Render(fmt.Sprintf("%d low", l))
}

func filterLabel(s string) string {
	if s == "all" {
		return s
	}
	return severityStyle(s).Render(s)
}

func countSev(fs []audit.Finding) (h, m, l int) {
	for _, f := range fs {
		switch f.Severity {
		case "high":
			h++
		case "medium":
			m++
		default:
			l++
		}
	}
	return
}

func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func absPath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}
