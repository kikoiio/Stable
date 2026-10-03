package llm

import (
	"context"
	"strings"
	"testing"
)

func TestReadSSEFrames(t *testing.T) {
	var got []sseFrame
	err := readSSE(context.Background(), strings.NewReader(": ping\n\nevent: delta\ndata: {\"a\":\ndata: 1}\n\n"), func(frame sseFrame) error {
		got = append(got, frame)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Event != "delta" || got[0].Data != "{\"a\":\n1}" {
		t.Fatalf("frames = %#v", got)
	}
}

func TestReadSSERejectsIncompleteAndOversizedFrames(t *testing.T) {
	if err := readSSE(context.Background(), strings.NewReader("data: incomplete"), func(sseFrame) error { return nil }); err == nil {
		t.Fatal("incomplete final frame accepted")
	}
	if err := readSSE(context.Background(), strings.NewReader("data: "+strings.Repeat("x", maxSSEEventBytes+1)+"\n\n"), func(sseFrame) error { return nil }); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
