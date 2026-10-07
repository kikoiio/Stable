package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fixedSelector struct {
	refs []MemoryRef
	err  error
}

func (s fixedSelector) Select(context.Context, string, []MemoryHeader) ([]MemoryRef, error) {
	return append([]MemoryRef(nil), s.refs...), s.err
}

type fixedProcessor struct {
	extractChanges []MemoryChange
	extractErr     error
	consolidateErr error
	extractCalls   int
	consolidations int
}

func (p *fixedProcessor) Extract(context.Context, ExtractionInput) ([]MemoryChange, error) {
	p.extractCalls++
	return append([]MemoryChange(nil), p.extractChanges...), p.extractErr
}

func (p *fixedProcessor) Consolidate(context.Context, ConsolidationInput) ([]MemoryChange, error) {
	p.consolidations++
	return nil, p.consolidateErr
}

func newTestManager(t *testing.T, processor Processor, selector Selector, now func() time.Time, onEvent func(string, BackgroundEvent)) (*Service, string, string) {
	t.Helper()
	project := t.TempDir()
	user := t.TempDir()
	state := t.TempDir()
	manager, err := NewManager(Options{ProjectRoot: project, UserConfigDir: user, StateDir: state, Processor: processor, Selector: selector, Now: now, OnEvent: onEvent})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return manager, project, user
}

func TestManagerPrepareRunLoadsIndexAndSelectedMemory(t *testing.T) {
	manager, project, user := newTestManager(t, &fixedProcessor{}, nil, nil, nil)
	store, err := NewStore(project, user)
	if err != nil {
		t.Fatal(err)
	}
	header, err := store.Save(MemoryChange{Action: ActionUpsert, Scope: ScopeUser, Type: TypeUser, Name: "Style", Description: "Direct tone", Body: "Use short direct answers."})
	if err != nil {
		t.Fatal(err)
	}
	manager.selector = fixedSelector{refs: []MemoryRef{{Scope: ScopeUser, Filename: header.Filename}}}
	state, err := manager.loadState()
	if err != nil {
		t.Fatal(err)
	}
	state.SessionCursors["session-1"] = 19
	if err := manager.saveState(state); err != nil {
		t.Fatal(err)
	}
	got, err := manager.PrepareRun(context.Background(), project, project, "session-1", "be direct")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.UserIndex, "Style") || len(got.Selected) != 1 || got.Selected[0].Body != "Use short direct answers." || got.ExtractedThrough != 19 {
		t.Fatalf("PrepareRun() = %+v", got)
	}
}

func TestManagerCompleteRunRetriesCursorAfterExtractionFailure(t *testing.T) {
	processor := &fixedProcessor{extractErr: errors.New("temporary provider failure")}
	manager, project, _ := newTestManager(t, processor, fixedSelector{}, nil, nil)
	completion := RunCompletion{ProjectRoot: project, SessionID: "session-fail", RunID: "run-1", WorkKind: "session", Messages: []ConversationText{{Seq: 1, Kind: "session_text", Text: "remember my preference"}}, ThroughSeq: 8}
	manager.CompleteRun(context.Background(), completion)
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := manager.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionCursors[completion.SessionID] != 0 {
		t.Fatalf("cursor advanced after failed extraction: %+v", state.SessionCursors)
	}
	processor.extractErr = nil
	manager.processCompletion(context.Background(), completion)
	state, err = manager.loadState()
	if err != nil || state.SessionCursors[completion.SessionID] != completion.ThroughSeq {
		t.Fatalf("cursor did not advance after retry: %+v, %v", state.SessionCursors, err)
	}
}

func TestManagerAdvancesCursorWhenAgentAlreadySavedMemory(t *testing.T) {
	manager, project, _ := newTestManager(t, &fixedProcessor{}, nil, nil, nil)
	manager.CompleteRun(context.Background(), RunCompletion{
		ProjectRoot: project, SessionID: "session-saved", RunID: "run-1", WorkKind: "goal",
		Messages: []ConversationText{{Seq: 1, Kind: "goal_reply", Text: "user message"}}, ThroughSeq: 12,
		MainAgentWroteMemory: true,
	})
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := manager.loadState()
	if err != nil || state.SessionCursors["session-saved"] != 12 {
		t.Fatalf("cursor state = %+v, %v", state.SessionCursors, err)
	}
}

func TestManagerBackgroundEventsKeepSessionAttribution(t *testing.T) {
	var events []BackgroundEvent
	manager, project, _ := newTestManager(t, &fixedProcessor{}, nil, nil, func(_ string, event BackgroundEvent) {
		events = append(events, event)
	})
	manager.CompleteRun(context.Background(), RunCompletion{ProjectRoot: project, SessionID: "session-owner", RunID: "run-1", ThroughSeq: 3})
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Action != "extract" || events[0].SessionID != "session-owner" || events[1].Action != "consolidate" || events[1].SessionID != "session-owner" {
		t.Fatalf("background event attribution = %+v", events)
	}
}

func TestManagerConsolidatesOnlyAfterTimeAndSessionGates(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	processor := &fixedProcessor{}
	manager, project, _ := newTestManager(t, processor, nil, func() time.Time { return now }, nil)
	for i := 0; i < 5; i++ {
		manager.CompleteRun(context.Background(), RunCompletion{ProjectRoot: project, SessionID: "session-" + string(rune('a'+i)), RunID: "run", ThroughSeq: 1})
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if processor.consolidations != 1 {
		t.Fatalf("consolidations after five sessions = %d, want 1", processor.consolidations)
	}
	state, err := manager.loadState()
	if err != nil || state.LastConsolidatedAt.IsZero() || len(state.SessionsSinceConsolidation) != 0 {
		t.Fatalf("post-consolidation state = %+v, %v", state, err)
	}
	now = now.Add(time.Hour)
	for i := 0; i < 5; i++ {
		manager.processCompletion(context.Background(), RunCompletion{ProjectRoot: project, SessionID: "later-" + string(rune('a'+i)), RunID: "run", ThroughSeq: 1})
	}
	if processor.consolidations != 1 {
		t.Fatalf("consolidated again before 24 hours: %d calls", processor.consolidations)
	}
	if state, err = manager.loadState(); err != nil || len(state.SessionsSinceConsolidation) != 5 {
		t.Fatalf("sessions were not retained after interval skip: %+v, %v", state, err)
	}
	now = now.Add(23 * time.Hour)
	manager.processCompletion(context.Background(), RunCompletion{ProjectRoot: project, SessionID: "later-f", RunID: "run", ThroughSeq: 1})
	if processor.consolidations != 2 {
		t.Fatalf("consolidation did not start after both gates: %d calls", processor.consolidations)
	}
}

func TestManagerRejectsOtherProjectRoot(t *testing.T) {
	manager, _, _ := newTestManager(t, &fixedProcessor{}, nil, nil, nil)
	if _, err := manager.List(context.Background(), t.TempDir()); err == nil {
		t.Fatal("manager accepted a different project root")
	}
}
