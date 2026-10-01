package conversation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/store"
)

func TestSessionProtocolClientLifecycle(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := Serve(ctx, Deps{Store: db, ProjectRoot: root, SocketPath: filepath.Join(root, "conversation.sock"), PollEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	reqctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	msgs, err := Request(reqctx, svc.deps.SocketPath, ClientMsg{Op: "session_list", ProjectRoot: root})
	if err != nil || len(msgs) != 1 || msgs[0].Type != "sessions" || len(msgs[0].Sessions) != 0 {
		t.Fatalf("list: %+v %v", msgs, err)
	}
	msgs, err = Request(reqctx, svc.deps.SocketPath, ClientMsg{Op: "session_create", ProjectRoot: root})
	if err != nil || len(msgs) != 1 || msgs[0].Session == nil {
		t.Fatalf("create: %+v %v", msgs, err)
	}
	id := msgs[0].Session.ID
	msgs, err = Request(reqctx, svc.deps.SocketPath, ClientMsg{Op: "session_load", ProjectRoot: root, SessionID: id})
	if err != nil || len(msgs) != 1 || msgs[0].Transcript == nil || msgs[0].Transcript.Session.ID != id {
		t.Fatalf("load: %+v %v", msgs, err)
	}
}
