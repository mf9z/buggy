// Package ui is buggy's terminal interface.
package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Plain terminal colors; only the title and severities get color.
var (
	colorBlue   = lipgloss.Color("33")
	colorRed    = lipgloss.Color("196")
	colorOrange = lipgloss.Color("208")
	colorYellow = lipgloss.Color("220")
	colorGreen  = lipgloss.Color("41")

	titleStyle  = lipgloss.NewStyle().Foreground(colorBlue).Bold(true)
	boldStyle   = lipgloss.NewStyle().Bold(true)
	faintStyle  = lipgloss.NewStyle().Faint(true)
	errStyle    = lipgloss.NewStyle().Foreground(colorRed)
	okStyle     = lipgloss.NewStyle().Foreground(colorGreen)
	rootPadding = lipgloss.NewStyle().Padding(1, 2)
)

func severityColor(sev string) lipgloss.Color {
	switch sev {
	case "high":
		return colorRed
	case "medium":
		return colorOrange
	}
	return colorYellow
}

func severityStyle(sev string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(severityColor(sev)).Bold(true)
}

func badge(sev string) string {
	return severityStyle(sev).Render(padRight(strings.ToUpper(sev), 6))
}

func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func truncate(s string, n int) string {
	if n <= 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func header(section string) string {
	h := titleStyle.Render("buggy")
	if section != "" {
		h += faintStyle.Render("  /  ") + boldStyle.Render(section)
	}
	return h
}

func help(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, boldStyle.Render(pairs[i])+" "+faintStyle.Render(pairs[i+1]))
	}
	return strings.Join(parts, faintStyle.Render("  ·  "))
}

var sevTag = regexp.MustCompile(`\[(HIGH|MEDIUM|LOW|CRITICAL)\]`)

func colorizeTags(s string) string {
	return sevTag.ReplaceAllStringFunc(s, func(t string) string {
		sev := strings.ToLower(strings.Trim(t, "[]"))
		if sev == "critical" {
			sev = "high"
		}
		return severityStyle(sev).Render(t)
	})
}

func wrap(s string, width int) string {
	if width < 10 {
		width = 10
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}
