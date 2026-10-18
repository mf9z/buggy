// Package ui is buggy's terminal interface.
package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The theme is the terminal's own black and white; color is reserved for the
// title and severities.
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
	selStyle    = lipgloss.NewStyle().Bold(true).Reverse(true)
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

// badge renders a fixed-width severity label, e.g. "HIGH  ".
func badge(sev string) string {
	return severityStyle(sev).Render(padRight(strings.ToUpper(sev), 6))
}

func padRight(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// truncate shortens s to n display cells, preserving ANSI styling.
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

// header is the screen title line: "buggy" in blue plus a section name.
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

// colorizeTags paints [HIGH]/[MEDIUM]/[LOW] tags in model replies.
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
