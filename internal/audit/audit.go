package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mf9z/buggy/internal/config"
	"github.com/mf9z/buggy/internal/provider"
)

type Finding struct {
	Severity    string `json:"severity"` // high, medium, low
	Category    string `json:"category"` // security, performance, bug
	File        string `json:"file"`
	Line        int    `json:"line,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Suggestion  string `json:"suggestion,omitempty"`
}

type Report struct {
	Root     string        `json:"root"`
	Backend  string        `json:"backend"`
	Model    string        `json:"model"`
	Files    int           `json:"files"`
	Lines    int           `json:"lines"`
	Skipped  []Skip        `json:"skipped,omitempty"`
	Findings []Finding     `json:"findings"`
	Errors   []string      `json:"errors,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

func (r *Report) Counts() (high, medium, low int) {
	for _, f := range r.Findings {
		switch f.Severity {
		case "high":
			high++
		case "medium":
			medium++
		default:
			low++
		}
	}
	return
}

type Progress struct {
	Stage        string // "scanning", "auditing", "done"
	Files        int
	BatchesTotal int
	BatchesDone  int
	Active       []string // files in flight
	New          []Finding
	Err          error
}

func SeverityRank(s string) int {
	switch s {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

type chunk struct {
	path       string
	start, end int // 1-based inclusive line range
	text       string
	whole      bool
}

type batch struct {
	chunks []chunk
	size   int
}

func (b batch) label() string {
	if len(b.chunks) == 1 {
		return b.chunks[0].path
	}
	return fmt.Sprintf("%s +%d more", b.chunks[0].path, len(b.chunks)-1)
}

// makeBatches packs files into ~budget-byte prompts, splitting big files by line.
func makeBatches(files []File, budget int) []batch {
	var chunks []chunk
	for _, f := range files {
		if len(f.Content) <= budget {
			chunks = append(chunks, chunk{path: f.Path, start: 1, end: f.Lines, text: f.Content, whole: true})
			continue
		}
		lines := strings.Split(f.Content, "\n")
		start, size := 0, 0
		for i, l := range lines {
			size += len(l) + 1
			if size >= budget || i == len(lines)-1 {
				chunks = append(chunks, chunk{
					path: f.Path, start: start + 1, end: i + 1,
					text: strings.Join(lines[start:i+1], "\n"),
				})
				start, size = i+1, 0
			}
		}
	}
	var out []batch
	var cur batch
	for _, c := range chunks {
		if cur.size > 0 && cur.size+len(c.text) > budget {
			out = append(out, cur)
			cur = batch{}
		}
		cur.chunks = append(cur.chunks, c)
		cur.size += len(c.text)
	}
	if len(cur.chunks) > 0 {
		out = append(out, cur)
	}
	return out
}

func SystemPrompt(cfg config.AuditConfig) string {
	var focus []string
	if cfg.Security {
		focus = append(focus, `- security: injection (SQL, command, template), XSS, path traversal, SSRF, auth/authz flaws, hardcoded secrets, unsafe deserialization, weak crypto, missing validation at trust boundaries, insecure defaults`)
	}
	if cfg.Performance {
		focus = append(focus, `- performance: algorithmic blowups, N+1 queries, repeated work in loops, unbounded growth or leaks, needless allocation/copying in hot paths, blocking I/O on async or UI paths, missing pagination or limits`)
	}
	if cfg.Bugs {
		focus = append(focus, `- bug: logic errors, off-by-one, null/nil dereference, race conditions, unhandled errors or rejections, resource leaks, wrong API usage, broken edge cases, type confusion`)
	}
	return `You are buggy, a senior code auditor. Review the provided source code and report real, actionable problems in these categories:
` + strings.Join(focus, "\n") + `

Severity:
- high: exploitable vulnerability, data loss or corruption, crash on a normal path, or a severe performance problem
- medium: a real defect or weakness under specific conditions, or a notable inefficiency
- low: a minor defect, robustness gap, or small inefficiency

Rules:
- Only report issues you can point to in the given code. No style nits, naming, formatting, missing comments, or "add tests".
- Use the line numbers from the left gutter. Use the file path exactly as given.
- Some files are partial chunks or reference code you cannot see; do not report problems that only come from missing context.
- One finding per distinct problem. Be specific and concise.

Respond with ONLY a JSON object and nothing else (no prose, no code fences):
{"findings":[{"severity":"high|medium|low","category":"security|performance|bug","file":"path/as/given","line":42,"title":"short summary under 80 chars","description":"what is wrong and why it matters","suggestion":"the concrete fix"}]}
If there are no issues, respond with {"findings":[]}.`
}

func renderBatch(project string, b batch) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Project: %s\nAudit these files:\n\n", project)
	for _, c := range b.chunks {
		if c.whole {
			fmt.Fprintf(&sb, "<file path=%q>\n", c.path)
		} else {
			fmt.Fprintf(&sb, "<file path=%q lines=\"%d-%d\" partial=\"true\">\n", c.path, c.start, c.end)
		}
		for i, line := range strings.Split(c.text, "\n") {
			fmt.Fprintf(&sb, "%5d| %s\n", c.start+i, line)
		}
		sb.WriteString("</file>\n\n")
	}
	return sb.String()
}

func Run(ctx context.Context, p provider.Provider, root string, cfg config.AuditConfig, progress func(Progress)) (*Report, error) {
	started := time.Now()
	if progress == nil {
		progress = func(Progress) {}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	progress(Progress{Stage: "scanning"})
	files, skips, err := Scan(abs, ScanOptions{MaxFileBytes: int64(cfg.MaxFileKB) << 10})
	if err != nil {
		return nil, err
	}
	rep := &Report{Root: abs, Backend: p.Name(), Model: p.Model(), Files: len(files), Skipped: skips}
	for _, f := range files {
		rep.Lines += f.Lines
	}
	if len(files) == 0 {
		rep.Duration = time.Since(started)
		return rep, errors.New("no auditable source files found")
	}

	batches := makeBatches(files, max(cfg.BatchKB, 1)<<10)
	system := SystemPrompt(cfg)
	project := filepath.Base(abs)
	known := map[string]bool{}
	for _, f := range files {
		known[f.Path] = true
	}

	var (
		mu     sync.Mutex
		done   int
		active = map[int]string{}
		seen   = map[string]bool{}
	)
	snapshot := func() []string {
		out := make([]string, 0, len(active))
		for _, v := range active {
			out = append(out, v)
		}
		sort.Strings(out)
		return out
	}
	progress(Progress{Stage: "auditing", Files: len(files), BatchesTotal: len(batches)})

	workers := cfg.Concurrency
	if workers < 1 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, b := range batches {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		mu.Lock()
		active[i] = b.label()
		progress(Progress{Stage: "auditing", Files: len(files), BatchesTotal: len(batches), BatchesDone: done, Active: snapshot()})
		mu.Unlock()
		go func(i int, b batch) {
			defer wg.Done()
			defer func() { <-sem }()
			found, err := auditBatch(ctx, p, system, project, abs, b)

			mu.Lock()
			defer mu.Unlock()
			delete(active, i)
			done++
			var fresh []Finding
			for _, f := range found {
				f = normalize(f, b)
				if f.File != "" && !known[f.File] {
					// Models sometimes mangle paths; a one-file batch is unambiguous.
					if len(b.chunks) == 1 {
						f.File = b.chunks[0].path
					}
				}
				if !keep(f, cfg) {
					continue
				}
				key := strings.ToLower(f.File + ":" + strconv.Itoa(f.Line) + ":" + f.Title)
				if seen[key] {
					continue
				}
				seen[key] = true
				fresh = append(fresh, f)
			}
			rep.Findings = append(rep.Findings, fresh...)
			if err != nil && !errors.Is(err, context.Canceled) {
				rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", b.label(), err))
			}
			progress(Progress{Stage: "auditing", Files: len(files), BatchesTotal: len(batches), BatchesDone: done, Active: snapshot(), New: fresh, Err: err})
		}(i, b)
	}
	wg.Wait()

	sortFindings(rep.Findings)
	rep.Duration = time.Since(started)
	progress(Progress{Stage: "done", Files: len(files), BatchesTotal: len(batches), BatchesDone: done})
	if ctx.Err() != nil {
		return rep, ctx.Err()
	}
	if len(rep.Errors) == len(batches) {
		return rep, fmt.Errorf("every request failed: %s", rep.Errors[0])
	}
	return rep, nil
}

func auditBatch(ctx context.Context, p provider.Provider, system, project, dir string, b batch) ([]Finding, error) {
	req := provider.Request{
		System:   system,
		Messages: []provider.Message{{Role: "user", Content: renderBatch(project, b)}},
		Purpose:  provider.PurposeAudit,
		Dir:      dir,
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		text, err := p.Stream(ctx, req, func(provider.Event) {})
		if err != nil {
			return nil, err
		}
		found, perr := ParseFindings(text)
		if perr == nil {
			return found, nil
		}
		lastErr = perr
		req.Messages = append(req.Messages,
			provider.Message{Role: "assistant", Content: text},
			provider.Message{Role: "user", Content: `That was not valid JSON. Reply with only the JSON object {"findings":[...]} and nothing else.`},
		)
	}
	return nil, lastErr
}

// ParseFindings tolerates code fences and prose around the JSON.
func ParseFindings(text string) ([]Finding, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty reply")
	}
	type raw struct {
		Severity    string          `json:"severity"`
		Category    string          `json:"category"`
		File        string          `json:"file"`
		Line        json.RawMessage `json:"line"`
		Title       string          `json:"title"`
		Description string          `json:"description"`
		Suggestion  string          `json:"suggestion"`
	}
	var obj struct {
		Findings *[]raw `json:"findings"`
	}
	var list []raw
	candidates := jsonCandidates(text)
	var err error = errors.New("no JSON object in reply")
	parsed := false
	for _, c := range candidates {
		if strings.HasPrefix(c, "[") {
			if err = json.Unmarshal([]byte(c), &list); err == nil {
				parsed = true
				break
			}
			continue
		}
		obj.Findings = nil
		if err = json.Unmarshal([]byte(c), &obj); err == nil {
			if obj.Findings == nil {
				err = errors.New(`missing "findings" key`)
				continue
			}
			list = *obj.Findings
			parsed = true
			break
		}
	}
	if !parsed {
		return nil, fmt.Errorf("could not parse findings: %w", err)
	}
	out := make([]Finding, 0, len(list))
	for _, r := range list {
		out = append(out, Finding{
			Severity: r.Severity, Category: r.Category, File: r.File, Line: parseLine(r.Line),
			Title: r.Title, Description: r.Description, Suggestion: r.Suggestion,
		})
	}
	return out, nil
}

func jsonCandidates(text string) []string {
	var c []string
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			c = append(c, strings.TrimSpace(rest[:j]))
		}
	}
	if i, j := strings.IndexByte(text, '{'), strings.LastIndexByte(text, '}'); i >= 0 && j > i {
		c = append(c, text[i:j+1])
	}
	if i, j := strings.IndexByte(text, '['), strings.LastIndexByte(text, ']'); i >= 0 && j > i {
		c = append(c, text[i:j+1])
	}
	return c
}

func parseLine(raw json.RawMessage) int {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0
	}
	// Accept "12", 12, "12-15", "L12".
	s = strings.TrimLeft(s, "Ll")
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}

func normalize(f Finding, b batch) Finding {
	f.Severity = strings.ToLower(strings.TrimSpace(f.Severity))
	switch f.Severity {
	case "critical", "high", "severe", "error":
		f.Severity = "high"
	case "medium", "moderate", "warning", "warn":
		f.Severity = "medium"
	default:
		f.Severity = "low"
	}
	f.Category = strings.ToLower(strings.TrimSpace(f.Category))
	switch {
	case strings.HasPrefix(f.Category, "sec"), f.Category == "vulnerability":
		f.Category = "security"
	case strings.HasPrefix(f.Category, "perf"), strings.HasPrefix(f.Category, "optim"), f.Category == "efficiency":
		f.Category = "performance"
	default:
		f.Category = "bug"
	}
	f.File = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(f.File)), "./")
	if f.File == "" && len(b.chunks) == 1 {
		f.File = b.chunks[0].path
	}
	f.Title = strings.TrimSpace(f.Title)
	if f.Title == "" {
		f.Title = firstSentence(f.Description)
	}
	return f
}

func keep(f Finding, cfg config.AuditConfig) bool {
	if SeverityRank(f.Severity) < SeverityRank(cfg.MinSeverity) {
		return false
	}
	switch f.Category {
	case "security":
		return cfg.Security
	case "performance":
		return cfg.Performance
	}
	return cfg.Bugs
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if ra, rb := SeverityRank(a.Severity), SeverityRank(b.Severity); ra != rb {
			return ra > rb
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
}
