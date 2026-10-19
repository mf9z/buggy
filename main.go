package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/mf9z/buggy/internal/audit"
	"github.com/mf9z/buggy/internal/config"
	"github.com/mf9z/buggy/internal/provider"
	"github.com/mf9z/buggy/internal/ui"
)

var version = "0.1.0"

const usage = `buggy — find bugs, security holes and slow code in your project

usage:
  buggy                     open the interactive menu
  buggy chat [dir]          chat about a project
  buggy audit [path]        audit a project and print the report
  buggy models              list models for the current provider
  buggy config              show settings and where they're stored
  buggy providers           list supported providers and proxy backends
  buggy version

audit flags:
  --json                    print the report as JSON
  --out FILE                also write the report (.md or .json by extension)
  --fail-on SEVERITY        exit 1 if any finding is at least high|medium|low
  --min SEVERITY            only report findings at or above this severity
  --only LIST               checks to run: security,performance,bugs
  --concurrency N           parallel requests

backend flags (any command):
  --provider ID             use a BYOK provider (anthropic, openai, openrouter, nvidia, …)
  --model NAME              override the model
  --proxy BACKEND           use a proxy: claude-code, codex, gemini-cli, command, http
  --key KEY                 API key for this run (prefer env vars)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("error: ")+err.Error())
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		os.Exit(1)
	}
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

type backendFlags struct {
	provider, model, proxy, key string
}

func (b *backendFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&b.provider, "provider", "", "")
	fs.StringVar(&b.model, "model", "", "")
	fs.StringVar(&b.proxy, "proxy", "", "")
	fs.StringVar(&b.key, "key", "", "")
}

func (b *backendFlags) apply(cfg *config.Config) error {
	if b.proxy != "" {
		cfg.Mode = config.ModeProxy
		cfg.Proxy.Backend = b.proxy
		if b.model != "" {
			cfg.Proxy.Model = b.model
		}
		if b.key != "" {
			cfg.Proxy.APIKey = b.key
		}
		return nil
	}
	if b.provider != "" {
		if _, ok := provider.LookupSpec(b.provider); !ok {
			return fmt.Errorf("unknown provider %q (see `buggy providers`)", b.provider)
		}
		cfg.Mode = config.ModeBYOK
		cfg.Provider = b.provider
	}
	if b.model != "" {
		if cfg.Mode == config.ModeProxy {
			cfg.Proxy.Model = b.model
		} else {
			cfg.ProviderSettings(cfg.Provider).Model = b.model
		}
	}
	if b.key != "" && cfg.Mode == config.ModeBYOK {
		cfg.ProviderSettings(cfg.Provider).APIKey = b.key
	}
	return nil
}

// parseInterspersed lets flags appear before or after positional args.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	return fs
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "chat":
		var bf backendFlags
		fs := newFlagSet(cmd)
		bf.register(fs)
		help := fs.Bool("help", false, "")
		fs.BoolVar(help, "h", false, "")
		ver := fs.Bool("version", false, "")
		pos, err := parseInterspersed(fs, args)
		if err != nil {
			return err
		}
		if *help {
			fmt.Print(usage)
			return nil
		}
		if *ver {
			fmt.Println("buggy", version)
			return nil
		}
		if err := bf.apply(cfg); err != nil {
			return err
		}
		root := "."
		if len(pos) > 0 {
			root = pos[0]
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			return fmt.Errorf("%s is not a directory", root)
		}
		root, _ = filepath.Abs(root)
		start := ui.ScreenMenu
		if cmd == "chat" {
			start = ui.ScreenChat
		}
		if !term.IsTerminal(int(os.Stdout.Fd())) {
			return errors.New("the interactive UI needs a terminal; use `buggy audit` for scripts and CI")
		}
		return ui.Run(cfg, root, start)
	case "audit":
		return runAudit(cfg, args)
	case "models":
		var bf backendFlags
		fs := newFlagSet("models")
		bf.register(fs)
		if _, err := parseInterspersed(fs, args); err != nil {
			return err
		}
		if err := bf.apply(cfg); err != nil {
			return err
		}
		if cfg.Mode == config.ModeBYOK && cfg.ProviderSettings(cfg.Provider).Model == "" {
			if spec, _ := provider.LookupSpec(cfg.Provider); spec.DefaultModel == "" {
				cfg.ProviderSettings(cfg.Provider).Model = "list"
			}
		}
		p, err := provider.New(cfg)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ms, err := p.Models(ctx)
		if err != nil {
			return err
		}
		for _, m := range ms {
			fmt.Println(m)
		}
		return nil
	case "config":
		printConfig(cfg)
		return nil
	case "providers":
		printProviders()
		return nil
	case "version", "--version":
		fmt.Println("buggy", version)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
}

func runAudit(cfg *config.Config, args []string) error {
	var bf backendFlags
	fs := newFlagSet("audit")
	bf.register(fs)
	asJSON := fs.Bool("json", false, "")
	out := fs.String("out", "", "")
	failOn := fs.String("fail-on", "", "")
	minSev := fs.String("min", "", "")
	only := fs.String("only", "", "")
	conc := fs.Int("concurrency", 0, "")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if err := bf.apply(cfg); err != nil {
		return err
	}
	if *conc > 0 {
		cfg.Audit.Concurrency = *conc
	}
	if *minSev != "" {
		if audit.SeverityRank(*minSev) == 0 {
			return fmt.Errorf("--min must be high, medium or low")
		}
		cfg.Audit.MinSeverity = *minSev
	}
	if *failOn != "" && audit.SeverityRank(*failOn) == 0 {
		return fmt.Errorf("--fail-on must be high, medium or low")
	}
	if *only != "" {
		cfg.Audit.Security, cfg.Audit.Performance, cfg.Audit.Bugs = false, false, false
		for _, c := range strings.Split(*only, ",") {
			switch strings.TrimSpace(strings.ToLower(c)) {
			case "security", "sec":
				cfg.Audit.Security = true
			case "performance", "perf", "optimization":
				cfg.Audit.Performance = true
			case "bugs", "bug", "code":
				cfg.Audit.Bugs = true
			default:
				return fmt.Errorf("unknown check %q in --only", c)
			}
		}
	}
	target := "."
	if len(pos) > 0 {
		target = pos[0]
	}

	p, err := provider.New(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tty := term.IsTerminal(int(os.Stderr.Fd()))
	faint := lipgloss.NewStyle().Faint(true)
	started := time.Now()
	rep, runErr := audit.Run(ctx, p, target, cfg.Audit, func(pr audit.Progress) {
		if !tty {
			return
		}
		switch pr.Stage {
		case "scanning":
			fmt.Fprint(os.Stderr, faint.Render("\rscanning files…"))
		case "auditing":
			fmt.Fprint(os.Stderr, "\r\x1b[K"+faint.Render(fmt.Sprintf("auditing %d files with %s · %d/%d · %s",
				pr.Files, provider.Describe(cfg), pr.BatchesDone, pr.BatchesTotal, time.Since(started).Round(time.Second))))
		case "done":
			fmt.Fprint(os.Stderr, "\r\x1b[K")
		}
	})
	if rep == nil {
		return runErr
	}

	if *out != "" {
		var data []byte
		if strings.HasSuffix(strings.ToLower(*out), ".json") {
			data, err = rep.JSON()
			if err != nil {
				return err
			}
		} else {
			data = []byte(rep.Markdown())
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			return err
		}
	}
	if *asJSON {
		data, err := rep.JSON()
		if err != nil {
			return err
		}
		fmt.Println(string(data))
	} else {
		printReport(rep)
		if *out != "" {
			fmt.Println(faint.Render("report written to " + *out))
		}
	}
	if runErr != nil {
		return runErr
	}
	if *failOn != "" {
		for _, f := range rep.Findings {
			if audit.SeverityRank(f.Severity) >= audit.SeverityRank(*failOn) {
				return exitCode(1)
			}
		}
	}
	return nil
}

func printReport(rep *audit.Report) {
	sev := func(s string) lipgloss.Style {
		c := map[string]string{"high": "196", "medium": "208", "low": "220"}[s]
		return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Bold(true)
	}
	faint := lipgloss.NewStyle().Faint(true)
	bold := lipgloss.NewStyle().Bold(true)
	width := 100
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		width = w
	}
	body := lipgloss.NewStyle().Width(width - 9)

	h, m, l := rep.Counts()
	fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Bold(true).Render("buggy") + faint.Render(" audit · "+rep.Root))
	fmt.Println()
	for _, f := range rep.Findings {
		label := sev(f.Severity).Render(fmt.Sprintf("%-6s", strings.ToUpper(f.Severity)))
		fmt.Printf("%s %s %s\n", label, bold.Render(f.Title), faint.Render(f.Category))
		fmt.Printf("       %s\n", faint.Render(f.Location()))
		if f.Description != "" {
			for _, line := range strings.Split(body.Render(f.Description), "\n") {
				line = strings.TrimRight(line, " ")
				fmt.Println("       " + line)
			}
		}
		if f.Suggestion != "" {
			for i, line := range strings.Split(body.Render("fix: "+f.Suggestion), "\n") {
				line = strings.TrimRight(line, " ")
				if i == 0 {
					line = strings.Replace(line, "fix:", lipgloss.NewStyle().Foreground(lipgloss.Color("41")).Render("fix:"), 1)
				}
				fmt.Println("       " + line)
			}
		}
		fmt.Println()
	}
	if len(rep.Findings) == 0 {
		fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color("41")).Render("No issues found."))
		fmt.Println()
	}
	for _, e := range rep.Errors {
		fmt.Println(lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("failed: ") + e)
	}
	fmt.Printf("%s · %s · %s   %s\n",
		sev("high").Render(fmt.Sprintf("%d high", h)),
		sev("medium").Render(fmt.Sprintf("%d medium", m)),
		sev("low").Render(fmt.Sprintf("%d low", l)),
		faint.Render(fmt.Sprintf("%d files · %d lines · %s · %s", rep.Files, rep.Lines, rep.Duration.Round(time.Second), rep.Model)))
}

func printConfig(cfg *config.Config) {
	fmt.Println("config file:", cfg.File())
	fmt.Println("active:     ", provider.Describe(cfg))
	fmt.Println()
	for _, s := range provider.Catalog {
		pc := cfg.Providers[s.ID]
		key, src := provider.ResolveKey(s, pc)
		state := "no key"
		switch {
		case key != "":
			state = "key from " + src
		case s.KeyOptional:
			state = "no key needed"
		}
		marker := "  "
		if cfg.Mode == config.ModeBYOK && cfg.Provider == s.ID {
			marker = "› "
		}
		fmt.Printf("%s%-12s %s\n", marker, s.ID, state)
	}
	fmt.Println()
	fmt.Printf("proxy backend: %s\n", cfg.Proxy.Backend)
	fmt.Printf("audit: security=%v performance=%v bugs=%v min=%s concurrency=%d batch=%dKB\n",
		cfg.Audit.Security, cfg.Audit.Performance, cfg.Audit.Bugs, cfg.Audit.MinSeverity, cfg.Audit.Concurrency, cfg.Audit.BatchKB)
}

func printProviders() {
	fmt.Println("BYOK providers:")
	for _, s := range provider.Catalog {
		env := s.EnvKey
		if env == "" {
			env = "-"
		}
		fmt.Printf("  %-12s %-28s env: %-20s default: %s\n", s.ID, s.Name, env, s.DefaultModel)
	}
	fmt.Println()
	fmt.Println("proxy backends:")
	for _, b := range provider.ProxyBackends {
		fmt.Printf("  %-12s %s\n", b, provider.ProxyBackendLabel(b))
	}
}
