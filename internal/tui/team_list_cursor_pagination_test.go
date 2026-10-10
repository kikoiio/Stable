package tui

import (
	"strings"
	"testing"

	"stable/internal/conversation"
)

func TestTeamsListCursorTUIForwardsAndRejectsCursor(t *testing.T) {
	const cursor = "team-9"
	socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: "team_list"})
	model := New(socket, t.TempDir())
	model.ActiveSession = "session"
	model.Pending, model.ActiveRunID, model.LastCursor = true, "parent-run", 17
	model.Composer.SetValue("/teams list 100 " + cursor)
	updated, command := model.submitComposer()
	got := updated.(Model)
	if command == nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 {
		t.Fatalf("valid team cursor changed parent state or was not dispatched: pending=%v run=%q cursor=%d", got.Pending, got.ActiveRunID, got.LastCursor)
	}
	result, ok := command().(resultMsg)
	if !ok {
		t.Fatal("team list command returned an unexpected result type")
	}
	if result.err != nil {
		t.Fatalf("team list command failed: %v", result.err)
	}
	request := agentFixtureRequest(t, requests)
	if request.Op != "team_list" || request.Limit != 100 || request.AfterTeamID != cursor {
		t.Fatalf("TUI request=%+v; want team list limit 100 after %q", request, cursor)
	}

	invalidModel := New("", t.TempDir())
	invalidModel.ActiveSession = "session"
	invalidModel.Composer.SetValue("/teams list 100 bad/cursor")
	invalidUpdate, invalidCommand := invalidModel.submitComposer()
	if invalidCommand != nil || !strings.Contains(invalidUpdate.(Model).Status, "用法") {
		t.Fatalf("invalid team cursor was accepted: command=%v status=%q", invalidCommand != nil, invalidUpdate.(Model).Status)
	}
}
