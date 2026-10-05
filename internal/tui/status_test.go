package tui

import (
	"strings"
	"testing"
)

func TestStatusPhasesAndActionsAreVisible(t *testing.T) {
	idle := renderStatus(StatusState{Phase: StatusIdle}, "s1", ChatView, 160, false)
	loading := renderStatus(StatusState{Phase: StatusLoading}, "s1", ChatView, 160, false)
	err := renderStatus(StatusState{Phase: StatusError, Text: "offline"}, "s1", ChatView, 160, false)
	if idle == loading || loading == err || !strings.Contains(idle, "Enter 发送") || !strings.Contains(idle, "Ctrl+J 换行") || !strings.Contains(err, "错误") {
		t.Fatalf("status does not describe state/actions: %q / %q / %q", idle, loading, err)
	}
	if strings.Contains(idle, "计划模式") {
		t.Fatalf("plan segment leaked into default status: %q", idle)
	}
	if plan := renderStatus(StatusState{Phase: StatusIdle}, "s1", ChatView, 160, true); !strings.Contains(plan, "计划模式") {
		t.Fatalf("plan mode segment missing: %q", plan)
	}
}
