package tui

import "testing"

func TestComputeLayout(t *testing.T) {
	for _, tc := range []struct {
		w, h, c        int
		compact, small bool
	}{{120, 32, 3, false, false}, {60, 20, 5, true, false}, {44, 20, 3, true, true}, {80, 11, 3, false, true}} {
		got := ComputeLayout(tc.w, tc.h, tc.c)
		if got.Compact != tc.compact || got.TooSmall != tc.small {
			t.Fatalf("ComputeLayout(%d,%d): %+v", tc.w, tc.h, got)
		}
		if !got.TooSmall && (got.TranscriptHeight < 1 || got.TranscriptWidth < 1) {
			t.Fatalf("invalid dimensions: %+v", got)
		}
	}
}
