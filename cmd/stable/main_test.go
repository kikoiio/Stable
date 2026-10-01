package main

import (
	"strings"
	"testing"
)

func TestLegacyChatCommandIsRemoved(t *testing.T) {
	if err := run([]string{"chat", "--say", "hello"}); err == nil || !strings.Contains(err.Error(), "chat was removed") {
		t.Fatalf("legacy command error: %v", err)
	}
}
