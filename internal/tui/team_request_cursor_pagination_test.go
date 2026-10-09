package tui

import (
	"strings"
	"testing"

	"stable/internal/conversation"
)

func TestTeamRequestListCursorTUIForwardsAndRejectsCursor(t *testing.T) {
	const cursor = "request-9"
	socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: "team_request_list"})
	model := New(socket, t.TempDir())
	model.ActiveSession = "session"
	model.Pending, model.ActiveRunID, model.LastCursor = true, "parent-run", 17
	model.Composer.SetValue("/team team-1 requests list 100 " + cursor)
	updated, command := model.submitComposer()
	got := updated.(Model)
	if command == nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 {
		t.Fatalf("valid request cursor changed parent state or was not dispatched: pending=%v run=%q cursor=%d", got.Pending, got.ActiveRunID, got.LastCursor)
	}
	result, ok := command().(resultMsg)
	if !ok {
		t.Fatal("request command returned an unexpected result type")
	}
	if result.err != nil {
		t.Fatalf("request command failed: %v", result.err)
	}
	request := agentFixtureRequest(t, requests)
	if request.Op != "team_request_list" || request.Limit != 100 || request.AfterTeamRequestID != cursor {
		t.Fatalf("TUI request=%+v; want request list limit 100 after %q", request, cursor)
	}

	invalidModel := New("", t.TempDir())
	invalidModel.ActiveSession = "session"
	invalidModel.Composer.SetValue("/team team-1 requests list 100 bad/cursor")
	invalidUpdate, invalidCommand := invalidModel.submitComposer()
	if invalidCommand != nil || !strings.Contains(invalidUpdate.(Model).Status, "用法") {
		t.Fatalf("invalid request cursor was accepted: command=%v status=%q", invalidCommand != nil, invalidUpdate.(Model).Status)
	}
}
