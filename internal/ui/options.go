package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/mf9z/buggy/internal/config"
	"github.com/mf9z/buggy/internal/provider"
)

type optionsMsg interface{ options() }

type modelsMsg struct {
	models []string
	err    error
}

type testMsg struct {
	reply string
	err   error
	took  time.Duration
}

func (modelsMsg) options() {}
func (testMsg) options()   {}

type rowKind int

const (
	rowHeader rowKind = iota
	rowCycle
	rowText
	rowSecret
	rowNumber
	rowToggle
	rowModel
	rowAction
)

type optRow struct {
	label  string
	kind   rowKind
	value  func() string
	set    func(string)  // text, secret, number, model
	cycle  func(dir int) // cycle, toggle
	action func() tea.Cmd
	hint   string
}

type optionsModel struct {
	app    *App
	rows   []optRow
	cursor int

	editing bool
	input   textinput.Model

	picking   bool
	models    []string
	loading   bool
	pickErr   error
	pickIdx   int
	filter    textinput.Model
	pickApply func(string)

	flash string
	busy  bool

	width, height int
}

func newOptions(a *App) *optionsModel {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.PromptStyle = boldStyle
	fi := textinput.New()
	fi.Prompt = "filter › "
	fi.PromptStyle = boldStyle
	fi.Placeholder = "type to filter, or enter any model name"
	m := &optionsModel{app: a, input: ti, filter: fi}
	m.build()
	m.cursor = m.nextSelectable(0, 1)
	return m
}

func (m *optionsModel) resize(w, h int) {
	m.width, m.height = w, h
	m.input.Width = w - 6
	m.filter.Width = w - 12
}

func (m *optionsModel) cfg() *config.Config { return m.app.cfg }

func (m *optionsModel) save() {
	if err := m.cfg().Save(); err != nil {
		m.flash = errStyle.Render("could not save: " + err.Error())
		return
	}
	m.flash = okStyle.Render("saved")
}

func cycleStr(list []string, cur string, dir int) string {
	i := 0
	for j, v := range list {
		if v == cur {
			i = j
		}
	}
	return list[(i+dir+len(list))%len(list)]
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return strings.Repeat("•", len(k))
	}
	return k[:4] + strings.Repeat("•", 8) + k[len(k)-4:]
}

func (m *optionsModel) build() {
	c := m.cfg()
	var rows []optRow
	rows = append(rows, optRow{label: "Connection", kind: rowHeader})
	rows = append(rows, optRow{
		label: "Mode", kind: rowCycle,
		value: func() string {
			if c.Mode == config.ModeProxy {
				return "proxy  (route through a tool you're logged into)"
			}
			return "byok  (bring your own API key)"
		},
		cycle: func(int) {
			if c.Mode == config.ModeProxy {
				c.Mode = config.ModeBYOK
			} else {
				c.Mode = config.ModeProxy
			}
		},
	})

	if c.Mode == config.ModeBYOK {
		ids := make([]string, len(provider.Catalog))
		for i, s := range provider.Catalog {
			ids[i] = s.ID
		}
		spec, _ := provider.LookupSpec(c.Provider)
		pc := c.ProviderSettings(spec.ID)
		rows = append(rows,
			optRow{
				label: "Provider", kind: rowCycle,
				value: func() string { return spec.Name },
				cycle: func(d int) { c.Provider = cycleStr(ids, c.Provider, d) },
			},
			optRow{
				label: "API key", kind: rowSecret,
				value: func() string {
					key, src := provider.ResolveKey(spec, pc)
					switch {
					case key == "" && spec.KeyOptional:
						return faintStyle.Render("not needed")
					case key == "":
						return errStyle.Render("not set")
					case src == "config":
						return maskKey(key)
					}
					return maskKey(key) + faintStyle.Render("  from "+src)
				},
				set:  func(v string) { pc.APIKey = strings.TrimSpace(v) },
				hint: envHint(spec),
			},
			optRow{
				label: "Model", kind: rowModel,
				value: func() string {
					if pc.Model == "" {
						return spec.DefaultModel + faintStyle.Render("  (default)")
					}
					return pc.Model
				},
				set: func(v string) { pc.Model = strings.TrimSpace(v) },
			},
			optRow{
				label: "Base URL", kind: rowText,
				value: func() string {
					if pc.BaseURL == "" {
						if spec.BaseURL == "" {
							return errStyle.Render("required")
						}
						return faintStyle.Render(spec.BaseURL)
					}
					return pc.BaseURL
				},
				set:  func(v string) { pc.BaseURL = strings.TrimSpace(v) },
				hint: "leave empty for the provider default",
			},
		)
	} else {
		p := &c.Proxy
		rows = append(rows, optRow{
			label: "Backend", kind: rowCycle,
			value: func() string { return provider.ProxyBackendLabel(p.Backend) },
			cycle: func(d int) { p.Backend = cycleStr(provider.ProxyBackends, p.Backend, d) },
		})
		switch p.Backend {
		case "claude-code", "codex", "gemini-cli":
			bin := map[string]string{"claude-code": "claude", "codex": "codex", "gemini-cli": "gemini"}[p.Backend]
			rows = append(rows, optRow{
				label: "Status", kind: rowAction,
				value: func() string { return binStatus(bin) },
				action: func() tea.Cmd {
					m.flash = faintStyle.Render("buggy runs `" + bin + "` headless with read-only tools, using your existing login")
					return nil
				},
			})
		case "command":
			rows = append(rows, optRow{
				label: "Command", kind: rowText,
				value: func() string {
					if p.Command == "" {
						return errStyle.Render("not set")
					}
					return p.Command
				},
				set:  func(v string) { p.Command = strings.TrimSpace(v) },
				hint: "runs via sh -c; prompt on stdin, reply on stdout; $BUGGY_MODEL is set",
			})
		case "http":
			rows = append(rows,
				optRow{
					label: "URL", kind: rowText,
					value: func() string {
						if p.URL == "" {
							return errStyle.Render("not set")
						}
						return p.URL
					},
					set:  func(v string) { p.URL = strings.TrimSpace(v) },
					hint: "e.g. http://localhost:4000/v1 (LiteLLM) or http://localhost:8082 (claude-code-proxy)",
				},
				optRow{
					label: "Format", kind: rowCycle,
					value: func() string { return p.Format },
					cycle: func(d int) { p.Format = cycleStr([]string{"openai", "anthropic"}, p.Format, d) },
				},
				optRow{
					label: "API key", kind: rowSecret,
					value: func() string {
						if p.APIKey == "" {
							return faintStyle.Render("none")
						}
						return maskKey(p.APIKey)
					},
					set: func(v string) { p.APIKey = strings.TrimSpace(v) },
				},
			)
		}
		rows = append(rows, optRow{
			label: "Model", kind: rowModel,
			value: func() string {
				if p.Model == "" {
					return faintStyle.Render("backend default")
				}
				return p.Model
			},
			set: func(v string) { p.Model = strings.TrimSpace(v) },
		})
	}
	rows = append(rows, optRow{
		label: "Test connection", kind: rowAction,
		value:  func() string { return faintStyle.Render("send a tiny request") },
		action: func() tea.Cmd { return m.test() },
	})

	a := &c.Audit
	rows = append(rows,
		optRow{label: "Audit", kind: rowHeader},
		optRow{label: "Security", kind: rowToggle, value: func() string { return onOff(a.Security) }, cycle: func(int) { toggleCheck(&a.Security, a) }},
		optRow{label: "Performance", kind: rowToggle, value: func() string { return onOff(a.Performance) }, cycle: func(int) { toggleCheck(&a.Performance, a) }},
		optRow{label: "Bugs", kind: rowToggle, value: func() string { return onOff(a.Bugs) }, cycle: func(int) { toggleCheck(&a.Bugs, a) }},
		optRow{
			label: "Min severity", kind: rowCycle,
			value: func() string { return severityStyle(a.MinSeverity).Render(a.MinSeverity) },
			cycle: func(d int) { a.MinSeverity = cycleStr([]string{"low", "medium", "high"}, a.MinSeverity, d) },
		},
		optRow{
			label: "Parallel requests", kind: rowNumber,
			value: func() string { return strconv.Itoa(a.Concurrency) },
			set:   func(v string) { a.Concurrency = clampInt(v, 1, 32, a.Concurrency) },
			cycle: func(d int) { a.Concurrency = max(1, min(32, a.Concurrency+d)) },
		},
		optRow{
			label: "Batch size (KB)", kind: rowNumber,
			value: func() string { return strconv.Itoa(a.BatchKB) },
			set:   func(v string) { a.BatchKB = clampInt(v, 8, 2048, a.BatchKB) },
			cycle: func(d int) { a.BatchKB = max(8, min(2048, a.BatchKB+d*16)) },
			hint:  "code sent per request; bigger = fewer requests, needs a larger context window",
		},
		optRow{
			label: "Max file size (KB)", kind: rowNumber,
			value: func() string { return strconv.Itoa(a.MaxFileKB) },
			set:   func(v string) { a.MaxFileKB = clampInt(v, 1, 10240, a.MaxFileKB) },
			cycle: func(d int) { a.MaxFileKB = max(16, min(10240, a.MaxFileKB+d*64)) },
		},
	)
	m.rows = rows
}

func envHint(s provider.Spec) string {
	if s.EnvKey == "" {
		return ""
	}
	return "stored in your config file (mode 600); $" + s.EnvKey + " is used when empty"
}

func binStatus(bin string) string {
	if p, err := lookPath(bin); err == nil {
		home, _ := os.UserHomeDir()
		return okStyle.Render("found") + faintStyle.Render("  "+shortPath(p, home))
	}
	return errStyle.Render("not installed") + faintStyle.Render("  ("+bin+" not on PATH)")
}

var lookPath = exec.LookPath

// toggleCheck flips a check but never turns off the last enabled one.
func toggleCheck(target *bool, a *config.AuditConfig) {
	n := 0
	for _, v := range []bool{a.Security, a.Performance, a.Bugs} {
		if v {
			n++
		}
	}
	if *target && n == 1 {
		return
	}
	*target = !*target
}

func clampInt(s string, lo, hi, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

func (m *optionsModel) nextSelectable(from, dir int) int {
	n := len(m.rows)
	for i := 0; i < n; i++ {
		j := ((from+dir*i)%n + n) % n
		if m.rows[j].kind != rowHeader {
			return j
		}
	}
	return 0
}

func (m *optionsModel) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case modelsMsg:
		m.loading = false
		m.models, m.pickErr = msg.models, msg.err
		m.pickIdx = 0
		return nil
	case testMsg:
		m.busy = false
		if msg.err != nil {
			m.flash = errStyle.Render(truncate("✗ "+msg.err.Error(), m.width*3))
		} else {
			m.flash = okStyle.Render("✓ connected") + faintStyle.Render(fmt.Sprintf("  %.1fs · replied %q", msg.took.Seconds(), truncate(strings.TrimSpace(msg.reply), 40)))
		}
		return nil
	case tea.KeyMsg:
		if m.picking {
			return m.keyPicker(msg)
		}
		if m.editing {
			return m.keyEdit(msg)
		}
		return m.keyList(msg)
	}
	return nil
}

func (m *optionsModel) keyList(k tea.KeyMsg) tea.Cmd {
	row := m.rows[m.cursor]
	switch k.String() {
	case "esc", "q", "ctrl+c":
		return navigate(ScreenMenu)
	case "up", "k", "shift+tab":
		m.cursor = m.nextSelectable(m.cursor-1, -1)
		m.flash = ""
	case "down", "j", "tab":
		m.cursor = m.nextSelectable(m.cursor+1, 1)
		m.flash = ""
	case "left", "h", "right", "l":
		dir := 1
		if s := k.String(); s == "left" || s == "h" {
			dir = -1
		}
		if row.cycle != nil {
			row.cycle(dir)
			m.changed()
		}
	case "backspace", "delete":
		if row.set != nil && (row.kind == rowSecret || row.kind == rowText || row.kind == rowModel) {
			row.set("")
			m.changed()
			m.flash = okStyle.Render("cleared")
		}
	case "enter", " ":
		switch row.kind {
		case rowCycle, rowToggle:
			row.cycle(1)
			m.changed()
		case rowText, rowSecret, rowNumber:
			m.startEdit(row)
			return textinput.Blink
		case rowModel:
			return m.openPicker(row)
		case rowAction:
			if row.action != nil {
				return row.action()
			}
		}
	}
	return nil
}

func (m *optionsModel) changed() {
	label := m.rows[m.cursor].label
	m.build()
	// Keep the cursor on the same setting even if rows shifted.
	for i, r := range m.rows {
		if r.label == label && r.kind != rowHeader {
			m.cursor = i
			break
		}
	}
	if m.cursor >= len(m.rows) {
		m.cursor = m.nextSelectable(0, 1)
	}
	m.save()
}

func (m *optionsModel) startEdit(row optRow) {
	m.editing = true
	m.flash = ""
	m.input.Reset()
	m.input.EchoMode = textinput.EchoNormal
	m.input.Placeholder = ""
	if row.kind == rowSecret {
		m.input.EchoMode = textinput.EchoPassword
		m.input.EchoCharacter = '•'
		m.input.Placeholder = "paste key"
	} else {
		v := row.value()
		if strings.Contains(v, "\x1b") { // styled placeholder, not a real value
			v = ""
		}
		m.input.SetValue(v)
		m.input.CursorEnd()
	}
	m.input.Focus()
}

func (m *optionsModel) keyEdit(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.editing = false
		m.input.Blur()
		return nil
	case "enter":
		m.editing = false
		m.input.Blur()
		m.rows[m.cursor].set(m.input.Value())
		m.changed()
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return cmd
}

func (m *optionsModel) openPicker(row optRow) tea.Cmd {
	m.picking = true
	m.loading = true
	m.models = nil
	m.pickErr = nil
	m.pickIdx = 0
	m.pickApply = row.set
	m.filter.Reset()
	m.filter.Focus()
	prov, err := m.providerForListing()
	if err != nil {
		m.loading = false
		m.pickErr = err
		return textinput.Blink
	}
	return tea.Batch(textinput.Blink, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		ms, err := prov.Models(ctx)
		return modelsMsg{ms, err}
	})
}

func (m *optionsModel) providerForListing() (provider.Provider, error) {
	c := *m.cfg()
	if c.Mode == config.ModeBYOK {
		pcs := map[string]*config.ProviderConfig{}
		for k, v := range c.Providers {
			cp := *v
			pcs[k] = &cp
		}
		c.Providers = pcs
		if pc := c.ProviderSettings(c.Provider); pc.Model == "" {
			pc.Model = "list"
		}
	}
	return provider.New(&c)
}

func (m *optionsModel) filtered() []string {
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	if q == "" {
		return m.models
	}
	var out []string
	for _, id := range m.models {
		if strings.Contains(strings.ToLower(id), q) {
			out = append(out, id)
		}
	}
	return out
}

func (m *optionsModel) keyPicker(k tea.KeyMsg) tea.Cmd {
	list := m.filtered()
	switch k.String() {
	case "esc", "ctrl+c":
		m.picking = false
		m.filter.Blur()
		return nil
	case "up", "ctrl+p":
		if m.pickIdx > 0 {
			m.pickIdx--
		}
		return nil
	case "down", "ctrl+n":
		if m.pickIdx < len(list)-1 {
			m.pickIdx++
		}
		return nil
	case "enter":
		choice := strings.TrimSpace(m.filter.Value())
		if len(list) > 0 && m.pickIdx < len(list) {
			choice = list[m.pickIdx]
		}
		if choice == "" {
			return nil
		}
		m.picking = false
		m.filter.Blur()
		m.pickApply(choice)
		m.changed()
		return nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(k)
	m.pickIdx = 0
	return cmd
}

func (m *optionsModel) test() tea.Cmd {
	if m.busy {
		return nil
	}
	prov, err := m.app.newProvider()
	if err != nil {
		m.flash = errStyle.Render("✗ " + err.Error())
		return nil
	}
	m.busy = true
	m.flash = faintStyle.Render("testing " + prov.Name() + "…")
	root := m.app.root
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		start := time.Now()
		reply, err := prov.Stream(ctx, provider.Request{
			Messages: []provider.Message{{Role: "user", Content: "Reply with exactly the word: ok"}},
			Purpose:  provider.PurposeAudit,
			Dir:      root,
		}, func(provider.Event) {})
		return testMsg{reply, err, time.Since(start)}
	}
}

func (m *optionsModel) view() string {
	if m.picking {
		return m.viewPicker()
	}
	var b strings.Builder
	b.WriteString(header("options"))
	b.WriteString("\n")
	labelW := 20
	for i, r := range m.rows {
		if r.kind == rowHeader {
			b.WriteString("\n" + boldStyle.Render(r.label) + "\n")
			continue
		}
		cursor := "  "
		label := padRight(r.label, labelW)
		if i == m.cursor {
			cursor = boldStyle.Render("› ")
			label = boldStyle.Render(label)
		}
		val := r.value()
		if i == m.cursor && m.editing {
			val = m.input.View()
		}
		b.WriteString(cursor + label + truncate(val, m.width-labelW-2))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	cur := m.rows[m.cursor]
	if cur.hint != "" && !m.editing {
		b.WriteString(faintStyle.Render(truncate(cur.hint, m.width)) + "\n")
	}
	if m.flash != "" {
		b.WriteString(m.flash + "\n")
	}
	home, _ := os.UserHomeDir()
	b.WriteString(faintStyle.Render("config: "+shortPath(m.cfg().File(), home)) + "\n\n")
	if m.editing {
		b.WriteString(help("enter", "save", "esc", "cancel"))
	} else {
		b.WriteString(help("↑↓", "move", "enter", "edit", "←→", "change", "del", "clear", "esc", "back"))
	}
	return b.String()
}

func (m *optionsModel) viewPicker() string {
	var b strings.Builder
	b.WriteString(header("options") + faintStyle.Render("  /  ") + boldStyle.Render("model"))
	b.WriteString("\n\n")
	b.WriteString(m.filter.View())
	b.WriteString("\n\n")
	switch {
	case m.loading:
		b.WriteString(faintStyle.Render("loading models…"))
		b.WriteString("\n")
	case m.pickErr != nil:
		b.WriteString(faintStyle.Render(truncate("couldn't list models: "+m.pickErr.Error(), m.width)))
		b.WriteString("\n" + faintStyle.Render("type a model name and press enter") + "\n")
	default:
		list := m.filtered()
		if len(list) == 0 {
			b.WriteString(faintStyle.Render("no match — enter uses what you typed") + "\n")
		}
		room := max(3, m.height-8)
		top := 0
		if m.pickIdx >= room {
			top = m.pickIdx - room + 1
		}
		for i := top; i < len(list) && i < top+room; i++ {
			if i == m.pickIdx {
				b.WriteString(boldStyle.Render("› "+list[i]) + "\n")
			} else {
				b.WriteString("  " + list[i] + "\n")
			}
		}
	}
	b.WriteString("\n")
	b.WriteString(help("↑↓", "move", "enter", "choose", "esc", "cancel"))
	return b.String()
}
