package tui

import (
	"strings"
	"testing"

	"stable/internal/conversation"
)

func TestTeamTaskRequestAcceptsAndRejectsTaskCursor(t *testing.T) {
	teamID := "00000000000000000000000000000001"
	cursor := "00000000000000000000000000000002"
	base := conversation.ClientMsg{TeamID: teamID}

	request, status := teamTaskRequest(base, "list 100 "+cursor)
	if request.Op != "team_task_list" || request.Limit != 100 || request.AfterTaskID != cursor || status != "正在读取团队任务…" {
		t.Fatalf("valid cursor request=%+v status=%q", request, status)
	}

	invalid, invalidStatus := teamTaskRequest(base, "list 100 bad/cursor")
	if invalid.Op != "" || invalid.AfterTaskID != "" || !strings.Contains(invalidStatus, "用法") {
		t.Fatalf("invalid cursor request=%+v status=%q; want rejected usage", invalid, invalidStatus)
	}
}
