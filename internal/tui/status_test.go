package tui

import (
	"strings"
	"testing"
)

func TestStatusPhasesAndActionsAreVisible(t *testing.T) {
	idle := renderStatus(StatusState{Phase: StatusIdle}, "s1", ChatView, 160)
	loading := renderStatus(StatusState{Phase: StatusLoading}, "s1", ChatView, 160)
	err := renderStatus(StatusState{Phase: StatusError, Text: "offline"}, "s1", ChatView, 160)
	if idle == loading || loading == err || !strings.Contains(idle, "Enter 发送") || !strings.Contains(idle, "Ctrl+J 换行") || !strings.Contains(err, "错误") {
		t.Fatalf("status does not describe state/actions: %q / %q / %q", idle, loading, err)
	}
}
