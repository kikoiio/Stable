package tui

// LayoutMetrics describes the usable terminal area for the chat view.
type LayoutMetrics struct {
	Width, Height    int
	TranscriptWidth  int
	TranscriptHeight int
	ComposerHeight   int
	Compact          bool
	TooSmall         bool
}

func ComputeLayout(width, height, composerHeight int) LayoutMetrics {
	m := LayoutMetrics{Width: width, Height: height, Compact: width < 80, ComposerHeight: composerHeight}
	if width < 45 || height < 12 {
		m.TooSmall = true
		return m
	}
	m.TranscriptWidth = max(1, width-4)
	statusAndPadding := 5
	m.TranscriptHeight = max(1, height-statusAndPadding-composerHeight)
	return m
}
