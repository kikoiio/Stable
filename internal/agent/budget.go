package agent

import (
	"bytes"
	"encoding/json"
	"time"
)

const (
	DefaultMaxToolRounds    = 40
	DefaultMaxTotalDuration = 15 * time.Minute
	MaxCommandTimeout       = 600 * time.Second
)

type ResourceBounds struct {
	MaxToolRounds    int           `json:"max_tool_rounds"`
	MaxTotalDuration time.Duration `json:"max_total_duration"`
}

func DefaultResourceBounds() ResourceBounds {
	return ResourceBounds{
		MaxToolRounds:    DefaultMaxToolRounds,
		MaxTotalDuration: DefaultMaxTotalDuration,
	}
}

// ParseResourceBounds applies defaults independently to missing or invalid
// fields. This keeps a malformed optional request bound from disabling limits.
func ParseResourceBounds(raw json.RawMessage) ResourceBounds {
	return parseResourceBounds(raw, DefaultResourceBounds())
}

func parseResourceBounds(raw json.RawMessage, defaults ResourceBounds) ResourceBounds {
	defaults = defaults.WithDefaults()
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return defaults
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return defaults
	}
	bounds := defaults
	if value, ok := fields["max_tool_rounds"]; ok {
		var rounds int
		if json.Unmarshal(value, &rounds) == nil && rounds > 0 {
			bounds.MaxToolRounds = rounds
		}
	}
	if value, ok := fields["max_total_duration"]; ok {
		var duration string
		if json.Unmarshal(value, &duration) == nil {
			if parsed, err := time.ParseDuration(duration); err == nil && parsed > 0 {
				bounds.MaxTotalDuration = parsed
			}
		} else {
			// encoding/json represents time.Duration as nanoseconds when a
			// ResourceBounds value is marshaled directly.
			var nanos int64
			if json.Unmarshal(value, &nanos) == nil && nanos > 0 {
				bounds.MaxTotalDuration = time.Duration(nanos)
			}
		}
	}
	return bounds
}

// ResourceBoundsFromJSON is a descriptive alias for callers parsing request
// payloads outside the runner package.
func ResourceBoundsFromJSON(raw json.RawMessage) ResourceBounds {
	return ParseResourceBounds(raw)
}

func (b ResourceBounds) WithDefaults() ResourceBounds {
	defaults := DefaultResourceBounds()
	if b.MaxToolRounds <= 0 {
		b.MaxToolRounds = defaults.MaxToolRounds
	}
	if b.MaxTotalDuration <= 0 {
		b.MaxTotalDuration = defaults.MaxTotalDuration
	}
	return b
}
