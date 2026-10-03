package tui

import (
	"strings"

	"github.com/charmbracelet/glamour"
	"stable/internal/sessionlog"
)

type TranscriptRenderer interface {
	Render(events []sessionlog.Event, width int) (string, error)
}

type markdownTranscriptRenderer struct{}

func (markdownTranscriptRenderer) Render(events []sessionlog.Event, width int) (string, error) {
	return projectTranscript(events, width, false), nil
}

func renderMarkdown(markdown string, width int, color bool) string {
	if width < 1 {
		width = 1
	}
	style := "notty"
	if color {
		style = "dark"
	}
	r, err := glamour.NewTermRenderer(glamour.WithStylePath(style), glamour.WithWordWrap(width))
	if err != nil {
		return strings.TrimSpace(markdown)
	}
	out, err := r.Render(markdown)
	if err != nil {
		return strings.TrimSpace(markdown)
	}
	return strings.TrimRight(out, "\n")
}
