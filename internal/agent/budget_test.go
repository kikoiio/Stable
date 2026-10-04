package agent

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseResourceBoundsDefaultsAndPartialValues(t *testing.T) {
	defaults := DefaultResourceBounds()
	for name, raw := range map[string]json.RawMessage{
		"empty": nil,
		"null":  json.RawMessage(`null`),
		"bad":   json.RawMessage(`{"max_tool_rounds":`),
	} {
		if got := ParseResourceBounds(raw); got != defaults {
			t.Errorf("%s: got %+v want %+v", name, got, defaults)
		}
	}
	got := ParseResourceBounds(json.RawMessage(`{"max_tool_rounds":7,"max_total_duration":"2m"}`))
	if got.MaxToolRounds != 7 || got.MaxTotalDuration != 2*time.Minute {
		t.Fatalf("got %+v", got)
	}
	got = ParseResourceBounds(json.RawMessage(`{"max_tool_rounds":-1,"max_total_duration":"bad"}`))
	if got != defaults {
		t.Fatalf("invalid values should default: got %+v want %+v", got, defaults)
	}
}

func TestResourceBoundsWithDefaultsAndCommandTimeout(t *testing.T) {
	defaults := DefaultResourceBounds()
	if got := (ResourceBounds{}).WithDefaults(); got != defaults {
		t.Fatalf("got %+v want %+v", got, defaults)
	}
	if MaxCommandTimeout != 600*time.Second {
		t.Fatalf("MaxCommandTimeout=%s", MaxCommandTimeout)
	}
}
