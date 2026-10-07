package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"stable/internal/conversation"
)

func TestRemoteAccessDialogShowsClientAndCanonicalDirectory(t *testing.T) {
	request := conversation.RemoteAccessRequest{
		ID: "request-1", ClientLabel: "Laptop", ProjectRoot: "/tmp/approved/project",
		ExpiresAt: time.Date(2030, 1, 2, 3, 4, 0, 0, time.Local),
	}
	view := renderRemoteAccessDialog([]conversation.RemoteAccessRequest{request}, 0, 120)
	for _, want := range []string{"Laptop", "/tmp/approved/project", "批准当前连接", "拒绝"} {
		if !strings.Contains(view, want) {
			t.Fatalf("remote access dialog missing %q: %s", want, view)
		}
	}
}

func TestRemoteAccessDialogHasPriorityAndResolvesRequests(t *testing.T) {
	m := Model{
		Socket:               "/tmp/not-used.sock",
		RemoteAccessRequests: []conversation.RemoteAccessRequest{{ID: "request-1", ClientLabel: "Browser", ProjectRoot: "/project"}},
	}
	if got := m.pendingDialog(); got != DialogRemoteAccess {
		t.Fatalf("pending dialog = %v, want remote access priority", got)
	}
	updated, cmd := m.handleRemoteAccessKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}})
	if cmd == nil {
		t.Fatal("approve key did not submit a decision")
	}
	if updated.(Model).Status != "正在批准远程目录访问…" {
		t.Fatalf("status = %q", updated.(Model).Status)
	}
	updated, cmd = m.handleRemoteAccessKey(tea.KeyMsg{Type: tea.KeyEscape})
	if cmd == nil {
		t.Fatal("escape did not submit a denial")
	}
}

func TestRemoteAccessPollStartsWithModel(t *testing.T) {
	m := Model{Socket: "/tmp/not-used.sock"}
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init() did not start initial requests and remote access polling")
	}
}
