package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rohan/buggy/internal/config"
	"github.com/rohan/buggy/internal/provider"
)

func TestParseFindings(t *testing.T) {
	cases := map[string]string{
		"plain":  `{"findings":[{"severity":"HIGH","category":"security","file":"a.go","line":3,"title":"t"}]}`,
		"fenced": "Here you go:\n```json\n{\"findings\":[{\"severity\":\"high\",\"file\":\"a.go\",\"line\":\"3\",\"title\":\"t\"}]}\n```\n",
		"prose":  `Sure! {"findings":[{"severity":"high","file":"a.go","line":"L3-5","title":"t"}]} hope that helps`,
		"array":  `[{"severity":"high","file":"a.go","line":3,"title":"t"}]`,
	}
	for name, in := range cases {
		fs, err := ParseFindings(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(fs) != 1 || fs[0].Line != 3 || fs[0].File != "a.go" {
			t.Fatalf("%s: got %+v", name, fs)
		}
	}
	if fs, err := ParseFindings(`{"findings":[]}`); err != nil || len(fs) != 0 {
		t.Fatalf("empty: %v %v", fs, err)
	}
	if _, err := ParseFindings("no json here"); err == nil {
		t.Fatal("expected error")
	}
}

func TestNormalize(t *testing.T) {
	b := batch{chunks: []chunk{{path: "x.py"}}}
	f := normalize(Finding{Severity: "Critical", Category: "Optimization", File: "./x.py", Description: "Leaks memory. Bad."}, b)
	if f.Severity != "high" || f.Category != "performance" || f.File != "x.py" || f.Title != "Leaks memory" {
		t.Fatalf("got %+v", f)
	}
	f = normalize(Finding{Severity: "info", Category: "correctness"}, b)
	if f.Severity != "low" || f.Category != "bug" || f.File != "x.py" {
		t.Fatalf("got %+v", f)
	}
}

func TestMakeBatchesSplitsLargeFiles(t *testing.T) {
	big := strings.Repeat("line of code here\n", 500) // ~9KB
	files := []File{
		{Path: "a.go", Content: "package a\n", Lines: 2},
		{Path: "big.go", Content: big, Lines: 501},
		{Path: "c.go", Content: "package c\n", Lines: 2},
	}
	bs := makeBatches(files, 2048)
	covered := 0
	lastEnd := 0
	for _, b := range bs {
		if b.size > 2048+64 {
			t.Fatalf("batch too large: %d", b.size)
		}
		for _, c := range b.chunks {
			if c.path == "big.go" {
				if c.start != lastEnd+1 {
					t.Fatalf("gap in chunks: start %d after %d", c.start, lastEnd)
				}
				lastEnd = c.end
				covered += c.end - c.start + 1
			}
		}
	}
	if covered != 501 {
		t.Fatalf("big.go lines covered = %d, want 501", covered)
	}
}

func TestRenderBatchLineNumbers(t *testing.T) {
	out := renderBatch("p", batch{chunks: []chunk{{path: "f.js", start: 10, end: 11, text: "a\nb"}}})
	if !strings.Contains(out, "   10| a\n   11| b\n") || !strings.Contains(out, `partial="true"`) {
		t.Fatalf("unexpected render:\n%s", out)
	}
}

func TestScanRespectsIgnores(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write("src/app.ts", "export const x = 1\n")
	write("node_modules/dep/index.js", "x")
	write("generated/out.js", "x")
	write("secret.log", "x")
	write("keep.log.js", "ok\n")
	write(".env", "KEY=1")
	write("logo.png", "\x89PNG\x00\x00")
	write("data.bin.go", "pack\x00age")
	write("package-lock.json", "{}")
	write(".gitignore", "generated/\n*.log\n")

	files, skips, err := Scan(dir, ScanOptions{MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
	}
	want := ".gitignore,keep.log.js,main.go,src/app.ts"
	if strings.Join(got, ",") != want {
		t.Fatalf("files = %v, want %s", got, want)
	}
	reasons := map[string]string{}
	for _, s := range skips {
		reasons[s.Path] = s.Reason
	}
	if !strings.Contains(reasons[".env"], "secret") || reasons["data.bin.go"] != "binary" {
		t.Fatalf("skips = %v", reasons)
	}
}

// fakeProvider returns canned findings for whichever file it is shown.
type fakeProvider struct{ calls atomic.Int32 }

func (f *fakeProvider) Name() string  { return "fake" }
func (f *fakeProvider) Model() string { return "fake-1" }
func (f *fakeProvider) Models(context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeProvider) Stream(_ context.Context, req provider.Request, _ func(provider.Event)) (string, error) {
	f.calls.Add(1)
	if req.Purpose != provider.PurposeAudit || !strings.Contains(req.System, "security") {
		return "", nil
	}
	body := req.Messages[0].Content
	switch {
	case strings.Contains(body, `path="db.py"`):
		return `{"findings":[
			{"severity":"high","category":"security","file":"db.py","line":2,"title":"SQL injection"},
			{"severity":"low","category":"performance","file":"db.py","line":1,"title":"minor"},
			{"severity":"high","category":"security","file":"db.py","line":2,"title":"SQL injection"}]}`, nil
	case strings.Contains(body, `path="util.py"`):
		return "not json at all", nil
	}
	return `{"findings":[]}`, nil
}

func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	pad := strings.Repeat("# padding\n", 70) // ~700 bytes, so each file gets its own 1KB batch
	os.WriteFile(filepath.Join(dir, "db.py"), []byte("import db\ndb.q('select * from u where id=' + uid)\n"+pad), 0o644)
	os.WriteFile(filepath.Join(dir, "ok.py"), []byte("print('hi')\n"+pad), 0o644)
	os.WriteFile(filepath.Join(dir, "util.py"), []byte("x = 1\n"+pad), 0o644)

	cfg := config.Default().Audit
	cfg.BatchKB = 1
	cfg.MinSeverity = "medium"
	p := &fakeProvider{}
	var stages []string
	rep, err := Run(context.Background(), p, dir, cfg, func(pr Progress) {
		if len(stages) == 0 || stages[len(stages)-1] != pr.Stage {
			stages = append(stages, pr.Stage)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Title != "SQL injection" {
		t.Fatalf("findings = %+v", rep.Findings)
	}
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "util.py") {
		t.Fatalf("errors = %v", rep.Errors)
	}
	// db, ok, util (+1 retry for util's bad JSON)
	if n := p.calls.Load(); n != 4 {
		t.Fatalf("calls = %d", n)
	}
	if strings.Join(stages, ",") != "scanning,auditing,done" {
		t.Fatalf("stages = %v", stages)
	}
	md := rep.Markdown()
	if !strings.Contains(md, "## High (1)") || !strings.Contains(md, "`db.py:2`") {
		t.Fatalf("markdown:\n%s", md)
	}
}
