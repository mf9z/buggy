package audit

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Markdown renders the report as a shareable markdown document.
func (r *Report) Markdown() string {
	var b strings.Builder
	high, med, low := r.Counts()
	fmt.Fprintf(&b, "# buggy report\n\n")
	fmt.Fprintf(&b, "- **Project:** `%s`\n", r.Root)
	fmt.Fprintf(&b, "- **Backend:** %s (%s)\n", r.Backend, r.Model)
	fmt.Fprintf(&b, "- **Scanned:** %d files, %d lines in %s\n", r.Files, r.Lines, r.Duration.Round(time.Second))
	fmt.Fprintf(&b, "- **Findings:** %d high, %d medium, %d low\n\n", high, med, low)
	if len(r.Findings) == 0 {
		b.WriteString("No issues found.\n")
	}
	for _, sev := range []string{"high", "medium", "low"} {
		var group []Finding
		for _, f := range r.Findings {
			if f.Severity == sev {
				group = append(group, f)
			}
		}
		if len(group) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s (%d)\n\n", strings.ToUpper(sev[:1])+sev[1:], len(group))
		for _, f := range group {
			fmt.Fprintf(&b, "### %s\n\n", f.Title)
			fmt.Fprintf(&b, "`%s` · %s\n\n", f.Location(), f.Category)
			if f.Description != "" {
				fmt.Fprintf(&b, "%s\n\n", f.Description)
			}
			if f.Suggestion != "" {
				fmt.Fprintf(&b, "**Fix:** %s\n\n", f.Suggestion)
			}
		}
	}
	if len(r.Errors) > 0 {
		b.WriteString("## Errors\n\n")
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
	return b.String()
}

// JSON renders the report as indented JSON.
func (r *Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Location is "file:line" or just "file".
func (f Finding) Location() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return f.File
}
