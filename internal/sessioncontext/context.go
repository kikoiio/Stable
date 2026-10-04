package sessioncontext

import (
	"stable/internal/decision"
)

const (
	// DefaultWindowTokens is the conservative context window used when the
	// provider does not advertise a larger one.
	DefaultWindowTokens = 8192
	// MinWindowTokens rejects configurations too small to hold the system
	// prefix plus one exchange.
	MinWindowTokens = 1024
	// TriggerRatio starts compaction once the estimated input passes this
	// share of the window.
	TriggerRatio = 0.8
	// OutputReserveRatio reserves room for the model response.
	OutputReserveRatio = 0.2
	// SafetyMarginRatio absorbs estimation error.
	SafetyMarginRatio = 0.1
)

// EffectiveWindow resolves the configured context window. Unset (zero)
// uses the default silently; invalid values fall back to the default and
// report the fallback so callers can log it.
func EffectiveWindow(configured int) (window int, fellBack bool) {
	if configured == 0 {
		return DefaultWindowTokens, false
	}
	if configured < MinWindowTokens {
		return DefaultWindowTokens, true
	}
	return configured, false
}

// Manager estimates context size, decides when to compact, and produces
// the compaction boundary. It never reads project files and never writes
// the session log: the caller stays the only log appender.
type Manager struct {
	window   int
	provider decision.ChatProvider
}

// NewManager resolves the effective window and reports whether an invalid
// configured value fell back to the default.
func NewManager(configuredWindow int, provider decision.ChatProvider) (*Manager, bool) {
	window, fellBack := EffectiveWindow(configuredWindow)
	return &Manager{window: window, provider: provider}, fellBack
}

// Window returns the effective context window in tokens.
func (m *Manager) Window() int { return m.window }

func (m *Manager) triggerTokens() int {
	return int(float64(m.window) * TriggerRatio)
}

// inputBudget is the window minus the output reserve and safety margin.
func (m *Manager) inputBudget() int {
	return int(float64(m.window) * (1 - OutputReserveRatio - SafetyMarginRatio))
}
