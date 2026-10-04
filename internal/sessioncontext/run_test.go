package sessioncontext

import (
	"context"
	"errors"
	"strings"
	"testing"

	"stable/internal/llm"
)

func runMessages(n int, size int) ([]llm.Message, []uint64) {
	messages := []llm.Message{{Role: "system", Content: "system"}}
	seqs := []uint64{0}
	for i := 0; i < n; i++ {
		messages = append(messages, llm.Message{Role: "user", Content: strings.Repeat("x", size)})
		seqs = append(seqs, uint64(i+1))
	}
	return messages, seqs
}

func TestPrepareRunUnderTriggerPassesThrough(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	messages := []llm.Message{{Role: "user", Content: "hi"}}
	prepared, err := manager.PrepareRun(context.Background(), "run-1", messages, []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Boundary != nil || prepared.TailStart != 0 || len(provider.requests) != 0 {
		t.Fatalf("unexpected compaction: %+v", prepared)
	}
}

func TestPrepareRunCompactsWithRunScope(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	messages, seqs := runMessages(80, 200)
	prepared, err := manager.PrepareRun(context.Background(), "run-1", messages, seqs)
	if err != nil {
		t.Fatal(err)
	}
	b := prepared.Boundary
	if b == nil {
		t.Fatal("expected compaction")
	}
	if b.RunID != "run-1" || b.FromSeq == 0 || b.ToSeq < b.FromSeq || b.ToSeq >= 80 {
		t.Fatalf("boundary = %+v", b)
	}
	// System message kept at the head, summary second, recent tail verbatim.
	if prepared.HeadKept != 1 || prepared.Messages[0].Role != "system" {
		t.Fatalf("head not preserved: %+v", prepared.Messages[0])
	}
	if !strings.Contains(prepared.Messages[1].Content, "摘要") {
		t.Fatalf("summary missing: %+v", prepared.Messages[1])
	}
	if prepared.TailStart <= prepared.HeadKept {
		t.Fatalf("tail start = %d", prepared.TailStart)
	}
	// The boundary ToSeq aligns with the last covered message, so the
	// conversation consumer can persist it against the run stream.
	if b.ToSeq != seqs[prepared.TailStart-1] {
		t.Fatalf("boundary ToSeq %d != covered message seq %d", b.ToSeq, seqs[prepared.TailStart-1])
	}
	if approxRunTokens(prepared.Messages) > manager.inputBudget() {
		t.Fatal("compacted request still over budget")
	}
}

func TestPrepareRunProtectsToolExchange(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	messages, seqs := runMessages(78, 200)
	messages = append(messages,
		llm.Message{Role: "assistant", ToolUses: []llm.ToolUse{{ID: "c1", Name: "write"}}},
		llm.Message{Role: "user", ToolResults: []llm.ToolResultPart{{ToolUseID: "c1", Content: "ok"}}},
		llm.Message{Role: "user", Content: "tail"},
	)
	seqs = append(seqs, 79, 80, 81)
	prepared, err := manager.PrepareRun(context.Background(), "run-1", messages, seqs)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Boundary == nil {
		t.Fatal("expected compaction")
	}
	callCovered := prepared.Boundary.ToSeq >= 79
	resultCovered := prepared.Boundary.ToSeq >= 80
	if callCovered != resultCovered {
		t.Fatalf("boundary split the tool exchange: %+v", prepared.Boundary)
	}
}

func TestPrepareRunRejectsPreRunOnlyHistory(t *testing.T) {
	provider := &fakeProvider{}
	manager, _ := NewManager(testWindow, provider)
	messages, _ := runMessages(80, 200)
	seqs := make([]uint64, len(messages)) // all history predates the run
	if _, err := manager.PrepareRun(context.Background(), "run-1", messages, seqs); err == nil {
		t.Fatal("unboundable history compacted silently")
	}
}

func TestPrepareRunSummaryFailure(t *testing.T) {
	provider := &fakeProvider{err: errors.New("provider down")}
	manager, _ := NewManager(testWindow, provider)
	messages, seqs := runMessages(80, 200)
	if _, err := manager.PrepareRun(context.Background(), "run-1", messages, seqs); err == nil {
		t.Fatal("summary failure hidden")
	}
}

func TestPrepareRunSequenceMisalignment(t *testing.T) {
	manager, _ := NewManager(testWindow, &fakeProvider{})
	if _, err := manager.PrepareRun(context.Background(), "run-1", []llm.Message{{Role: "user", Content: "x"}}, nil); err == nil {
		t.Fatal("misaligned sequences accepted")
	}
}
