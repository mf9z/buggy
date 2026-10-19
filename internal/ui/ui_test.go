package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mf9z/buggy/internal/config"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// harness drives the App like the Bubble Tea runtime would: it executes
// returned commands and feeds their messages back into Update.
type harness struct {
	t    *testing.T
	app  *App
	msgs chan tea.Msg
}

func newHarness(t *testing.T, cfg *config.Config, root string) *harness {
	a := &App{cfg: cfg, root: root}
	a.menu = newMenu(a)
	h := &harness{t: t, app: a, msgs: make(chan tea.Msg, 1024)}
	h.send(tea.WindowSizeMsg{Width: 110, Height: 34})
	return h
}

func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		switch m := msg.(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range m {
				h.run(c)
			}
		case spinTick, auditSpinTick:
			// Skip animation ticks so tests don't loop forever.
		default:
			h.msgs <- msg
		}
	}()
}

func (h *harness) send(msg tea.Msg) {
	_, cmd := h.app.Update(msg)
	h.run(cmd)
}

func (h *harness) key(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		h.send(msg)
		h.drain(30 * time.Millisecond)
	}
}

func (h *harness) typeText(s string) {
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func (h *harness) drain(d time.Duration) {
	deadline := time.After(d)
	for {
		select {
		case msg := <-h.msgs:
			h.send(msg)
		case <-deadline:
			return
		}
	}
}

func (h *harness) screen() string {
	return ansiRE.ReplaceAllString(h.app.View(), "")
}

// waitFor processes messages until the screen contains want.
func (h *harness) waitFor(want string) string {
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if s := h.screen(); strings.Contains(s, want) {
			return s
		}
		select {
		case msg := <-h.msgs:
			h.send(msg)
		case <-deadline:
			h.t.Fatalf("timed out waiting for %q; screen:\n%s", want, h.screen())
		}
	}
}

func testConfig(t *testing.T, command string) *config.Config {
	t.Setenv("BUGGY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mode = config.ModeProxy
	cfg.Proxy.Backend = "command"
	cfg.Proxy.Command = command
	return cfg
}

func TestMenuRenders(t *testing.T) {
	h := newHarness(t, testConfig(t, "cat"), t.TempDir())
	s := h.screen()
	for _, want := range []string{"buggy", "› Chat", "Audit", "Options", "Exit", "proxy · command"} {
		if !strings.Contains(s, want) {
			t.Fatalf("menu missing %q:\n%s", want, s)
		}
	}
	h.key("down")
	if !strings.Contains(h.screen(), "› Audit") {
		t.Fatalf("cursor did not move:\n%s", h.screen())
	}
}

func TestAuditFlow(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("import os\nos.system(input())\n"), 0o644)
	findings := `{"findings":[` +
		`{"severity":"high","category":"security","file":"app.py","line":2,"title":"Command injection from stdin","description":"input() flows into os.system.","suggestion":"Use subprocess with an argument list."},` +
		`{"severity":"medium","category":"bug","file":"app.py","line":1,"title":"Medium thing"},` +
		`{"severity":"low","category":"performance","file":"app.py","line":1,"title":"Low thing"}]}`
	h := newHarness(t, testConfig(t, "cat >/dev/null; echo '"+findings+"'"), dir)

	h.key("a")
	h.waitFor("What should buggy audit?")
	h.key("enter")
	s := h.waitFor("1 high · 1 medium · 1 low")
	if !strings.Contains(s, "HIGH   security") || !strings.Contains(s, "app.py:2") {
		t.Fatalf("results:\n%s", s)
	}
	h.key("enter")
	s = h.screen()
	if !strings.Contains(s, "input() flows into os.system.") || !strings.Contains(s, "fix Use subprocess") {
		t.Fatalf("details not expanded:\n%s", s)
	}
	h.key("f") // severity filter → high only
	s = h.screen()
	if strings.Contains(s, "Medium thing") || !strings.Contains(s, "Command injection") {
		t.Fatalf("filter failed:\n%s", s)
	}
	h.key("s")
	if _, err := os.Stat(filepath.Join(dir, "buggy-report.md")); err != nil {
		t.Fatalf("report not saved: %v\n%s", err, h.screen())
	}
	h.key("esc")
	h.waitFor("› Audit")
}

func TestChatFlow(t *testing.T) {
	h := newHarness(t, testConfig(t, "cat >/dev/null; printf '[HIGH] race in worker.go:12'"), t.TempDir())
	h.key("c")
	h.waitFor("ask about your code")
	h.typeText("any bugs?")
	h.key("enter")
	s := h.waitFor("[HIGH] race in worker.go:12")
	if !strings.Contains(s, "you") || !strings.Contains(s, "any bugs?") {
		t.Fatalf("chat:\n%s", s)
	}
	if n := len(h.app.chat.history); n != 2 {
		t.Fatalf("history len = %d", n)
	}
	h.typeText("/nope")
	h.key("enter")
	h.waitFor("unknown command /nope")
	h.key("esc")
	h.waitFor("› Chat")
}

func TestOptionsFlow(t *testing.T) {
	cfg := testConfig(t, "cat")
	h := newHarness(t, cfg, t.TempDir())
	h.key("o")
	s := h.waitFor("Connection")
	if !strings.Contains(s, "› Mode") || !strings.Contains(s, "Custom command") {
		t.Fatalf("options:\n%s", s)
	}
	h.key("enter") // mode → byok
	s = h.waitFor("Provider")
	if cfg.Mode != config.ModeBYOK || !strings.Contains(s, "Anthropic") || !strings.Contains(s, "saved") {
		t.Fatalf("mode switch:\n%s", s)
	}
	h.key("down", "right") // provider → openai
	h.waitFor("OpenAI")
	if cfg.Provider != "openai" {
		t.Fatalf("provider = %s", cfg.Provider)
	}
	h.key("down", "enter") // edit API key
	h.typeText("sk-test-123456789")
	h.key("enter")
	if cfg.ProviderSettings("openai").APIKey != "sk-test-123456789" {
		t.Fatalf("key not set")
	}
	if !strings.Contains(h.screen(), "sk-t••••••••6789") {
		t.Fatalf("key not masked:\n%s", h.screen())
	}
	reloaded, err := config.Load()
	if err != nil || reloaded.Provider != "openai" || reloaded.ProviderSettings("openai").APIKey != "sk-test-123456789" {
		t.Fatalf("config not persisted: %+v %v", reloaded, err)
	}
	if st, _ := os.Stat(cfg.File()); st.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v", st.Mode().Perm())
	}
	h.key("esc")
	h.waitFor("byok · openai · gpt-5")
}
