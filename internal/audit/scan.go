// Package audit scans a project and asks a model to find bugs, security
// issues and performance problems in it.
package audit

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// File is a source file selected for auditing.
type File struct {
	Path    string // slash-separated, relative to the root
	Content string
	Lines   int
}

// Skip records a file that was left out and why.
type Skip struct {
	Path   string
	Reason string
}

var ignoredDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "out": true, "target": true, ".next": true, ".nuxt": true,
	".svelte-kit": true, ".output": true, "__pycache__": true, ".venv": true, "venv": true,
	".tox": true, ".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
	".idea": true, ".vscode": true, "coverage": true, ".gradle": true, "Pods": true,
	".terraform": true, ".cache": true, "obj": true, ".turbo": true,
	".parcel-cache": true, "bower_components": true, ".dart_tool": true, ".zig-cache": true,
	"zig-out": true, "_build": true, ".expo": true,
}

var ignoredFiles = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "bun.lockb": true,
	"bun.lock": true, "Cargo.lock": true, "go.sum": true, "poetry.lock": true, "Pipfile.lock": true,
	"composer.lock": true, "Gemfile.lock": true, "mix.lock": true, "flake.lock": true,
	"uv.lock": true, "pubspec.lock": true, "packages.lock.json": true, "LICENSE": true,
	"LICENSE.md": true, "LICENSE.txt": true, "CHANGELOG.md": true, ".DS_Store": true,
	"buggy-report.md": true, "buggy-report.json": true,
}

// skippedExts are binary, generated, or prose formats with nothing to audit.
var skippedExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".ico": true,
	".bmp": true, ".tiff": true, ".svg": true, ".pdf": true, ".zip": true, ".gz": true,
	".tar": true, ".tgz": true, ".bz2": true, ".xz": true, ".7z": true, ".rar": true,
	".jar": true, ".war": true, ".class": true, ".exe": true, ".dll": true, ".so": true,
	".dylib": true, ".a": true, ".o": true, ".obj": true, ".lib": true, ".wasm": true,
	".pyc": true, ".pyo": true, ".mp3": true, ".mp4": true, ".mov": true, ".avi": true,
	".wav": true, ".flac": true, ".ogg": true, ".webm": true, ".ttf": true, ".otf": true,
	".woff": true, ".woff2": true, ".eot": true, ".psd": true, ".sqlite": true, ".db": true,
	".bin": true, ".dat": true, ".lock": true, ".map": true, ".md": true, ".mdx": true,
	".txt": true, ".rst": true, ".csv": true, ".tsv": true, ".log": true, ".snap": true,
	".pem": true, ".key": true, ".crt": true, ".p12": true, ".pfx": true, ".ipynb": true,
}

// ScanOptions limits what gets read.
type ScanOptions struct {
	MaxFileBytes int64
}

// Scan walks root and returns auditable text files in a stable order.
func Scan(root string, opt ScanOptions) ([]File, []Skip, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		f, skip := readFile(filepath.Dir(root), root, opt)
		if f == nil {
			return nil, []Skip{*skip}, nil
		}
		return []File{*f}, nil, nil
	}

	ign := loadIgnore(root)
	var files []File
	var skips []Skip
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if p == root {
			return nil
		}
		rel := filepath.ToSlash(mustRel(root, p))
		name := d.Name()
		if d.IsDir() {
			if ignoredDirs[name] || ign.match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if ignoredFiles[name] || ign.match(rel, false) {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if skippedExts[ext] || strings.HasSuffix(name, ".min.js") || strings.HasSuffix(name, ".min.css") {
			return nil
		}
		if name == ".env" || strings.HasPrefix(name, ".env.") && !strings.HasSuffix(name, ".example") {
			skips = append(skips, Skip{rel, "secrets file (not sent)"})
			return nil
		}
		f, skip := readFile(root, p, opt)
		if skip != nil {
			skips = append(skips, *skip)
			return nil
		}
		files = append(files, *f)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, skips, err
}

func readFile(root, p string, opt ScanOptions) (*File, *Skip) {
	rel := filepath.ToSlash(mustRel(root, p))
	st, err := os.Stat(p)
	if err != nil {
		return nil, &Skip{rel, "unreadable"}
	}
	if opt.MaxFileBytes > 0 && st.Size() > opt.MaxFileBytes {
		return nil, &Skip{rel, "too large"}
	}
	if st.Size() == 0 {
		return nil, &Skip{rel, "empty"}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, &Skip{rel, "unreadable"}
	}
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(head[:validPrefix(head)]) {
		return nil, &Skip{rel, "binary"}
	}
	if looksGenerated(head) {
		return nil, &Skip{rel, "generated or minified"}
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	return &File{Path: rel, Content: s, Lines: strings.Count(s, "\n") + 1}, nil
}

// validPrefix trims a possibly cut multi-byte rune off the end of head.
func validPrefix(b []byte) int {
	n := len(b)
	for i := 0; i < 3 && n > 0; i++ {
		if r, _ := utf8.DecodeLastRune(b[:n]); r != utf8.RuneError {
			break
		}
		n--
	}
	return n
}

func looksGenerated(head []byte) bool {
	s := string(head)
	if strings.Contains(s, "Code generated") && strings.Contains(s, "DO NOT EDIT") {
		return true
	}
	if strings.Contains(s, "@generated") {
		return true
	}
	// A very long first line usually means minified output.
	if i := strings.IndexByte(s, '\n'); i < 0 && len(s) >= 4000 || i > 1000 {
		return true
	}
	return false
}

func mustRel(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return r
}

// ignore is a small .gitignore / .buggyignore matcher. It supports globs,
// anchored patterns, directory-only patterns, "**/" prefixes and negation.
type ignore struct {
	rules []rule
}

type rule struct {
	pattern  string
	anchored bool
	dirOnly  bool
	negate   bool
}

func loadIgnore(root string) *ignore {
	ig := &ignore{}
	for _, name := range []string{".gitignore", ".buggyignore"} {
		f, err := os.Open(filepath.Join(root, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			r := rule{}
			if strings.HasPrefix(line, "!") {
				r.negate = true
				line = line[1:]
			}
			if strings.HasSuffix(line, "/") {
				r.dirOnly = true
				line = strings.TrimSuffix(line, "/")
			}
			line = strings.TrimPrefix(line, "**/")
			if strings.HasPrefix(line, "/") {
				r.anchored = true
				line = line[1:]
			} else if strings.Contains(line, "/") {
				r.anchored = true
			}
			line = strings.TrimSuffix(line, "/**")
			if line == "" {
				continue
			}
			r.pattern = line
			ig.rules = append(ig.rules, r)
		}
		f.Close()
	}
	return ig
}

func (ig *ignore) match(rel string, isDir bool) bool {
	matched := false
	base := path.Base(rel)
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		var ok bool
		if r.anchored {
			ok, _ = path.Match(r.pattern, rel)
		} else {
			ok, _ = path.Match(r.pattern, base)
		}
		if ok {
			matched = !r.negate
		}
	}
	return matched
}

// List returns up to limit project file paths (without reading them), using
// the same ignore rules as Scan. truncated reports whether more exist.
func List(root string, limit int) (paths []string, truncated bool) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, false
	}
	ign := loadIgnore(root)
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel := filepath.ToSlash(mustRel(root, p))
		if d.IsDir() {
			if ignoredDirs[d.Name()] || ign.match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignoredFiles[d.Name()] || ign.match(rel, false) {
			return nil
		}
		if skippedExts[strings.ToLower(filepath.Ext(d.Name()))] && !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil
		}
		if len(paths) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		paths = append(paths, rel)
		return nil
	})
	sort.Strings(paths)
	return paths, truncated
}
