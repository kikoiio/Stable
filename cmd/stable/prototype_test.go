package main

import (
	"strings"
	"testing"
)

func TestPrototypeArgumentValidationPrecedesRuntimeInitialization(t *testing.T) {
	err := run([]string{"prototype", "unexpected"})
	if err == nil || !strings.Contains(err.Error(), "usage: stable prototype") {
		t.Fatalf("got %v, want prototype usage error", err)
	}
}
