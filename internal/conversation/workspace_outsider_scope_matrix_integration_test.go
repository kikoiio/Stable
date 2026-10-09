package conversation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceSocketRejectsOutsiderLifecycleOperationsWithoutOwnerChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	formalFile := filepath.Join(formal, "formal.txt")
	if err := os.WriteFile(formalFile, []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	workspaceState := filepath.Join(root, "workspace-state")
	socketDir, err := os.MkdirTemp("", "m09-outsider-scope-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	runner := &workspaceCreateScopeRunner{
		started: make(chan agent.ExecutionRequest, 1),
		events:  make(chan agent.ExecutionEvent),
		done:    make(chan agent.RunOutcome, 1),
	}
	service, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: workspaceState,
		SocketPath: socket, PollEvery: time.Hour, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})
	createSession := func() string {
		t.Helper()
		messages, err := Request(ctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
		if err != nil || len(messages) != 1 || messages[0].Session == nil {
			t.Fatalf("create session: messages=%+v err=%v", messages, err)
		}
		return messages[0].Session.ID
	}
	ownerSession, outsiderSession := createSession(), createSession()
	runID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := OpenRun(ctx, socket, agent.ExecutionRequest{
		RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: ownerSession},
		Intent: "create owner workspaces for scope isolation", Model: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	streamClosed := false
	t.Cleanup(func() {
		if !streamClosed {
			_ = stream.Cancel(ownerSession, runID)
			_ = stream.Close()
		}
	})
	started, err := stream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != runID {
		t.Fatalf("start owner lead run: message=%+v err=%v", started, err)
	}
	select {
	case request := <-runner.started:
		if request.RunID != runID || request.Work.SessionID != ownerSession {
			t.Fatalf("runner started with wrong scope: %+v", request)
		}
	case <-ctx.Done():
		t.Fatal("owner lead run did not reach runner")
	}
	createWorkspace := func(label string) *workspace.Snapshot {
		t.Helper()
		messages, err := Request(ctx, socket, ClientMsg{
			Op: "worktree_create", SessionID: ownerSession, RunID: runID, Text: label,
		})
		if err != nil || len(messages) != 1 || messages[0].Worktree == nil {
			t.Fatalf("create owner workspace %q: messages=%+v err=%v", label, messages, err)
		}
		return messages[0].Worktree
	}
	boundWorkspace := createWorkspace("owner-bound")
	removableWorkspace := createWorkspace("owner-clean-removal-target")
	discardWorkspace := createWorkspace("owner-discard-target")
	if boundWorkspace.ID == removableWorkspace.ID || boundWorkspace.ID == discardWorkspace.ID || removableWorkspace.ID == discardWorkspace.ID {
		t.Fatalf("workspace IDs are not unique: %q %q %q", boundWorkspace.ID, removableWorkspace.ID, discardWorkspace.ID)
	}
	if err := stream.Cancel(ownerSession, runID); err != nil {
		t.Fatal(err)
	}
	for {
		message, receiveErr := stream.Receive()
		if receiveErr != nil {
			t.Fatalf("receive owner run cancellation: %v", receiveErr)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCancelled {
				t.Fatalf("owner run outcome=%+v, want cancelled", message.Outcome)
			}
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	streamClosed = true

	ownerEnter, err := Request(ctx, socket, ClientMsg{Op: "worktree_enter", SessionID: ownerSession, ID: boundWorkspace.ID})
	if err != nil || len(ownerEnter) != 1 || ownerEnter[0].Worktree == nil || ownerEnter[0].Worktree.ID != boundWorkspace.ID {
		t.Fatalf("owner enter: messages=%+v err=%v", ownerEnter, err)
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	projectHash := sha256.Sum256([]byte(filepath.Clean(formalAbs)))
	projectID := "p" + hex.EncodeToString(projectHash[:16])
	layout, err := workspace.NewLayout(workspaceState, formalAbs, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Close()
	workspaceFiles := map[string]string{}
	for _, item := range []struct {
		id      string
		name    string
		content string
	}{
		{id: boundWorkspace.ID, name: "bound.txt", content: "owner bound checkout bytes"},
		{id: discardWorkspace.ID, name: "dirty.txt", content: "owner dirty checkout bytes"},
	} {
		paths, pathErr := layout.Paths(item.id)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		file := filepath.Join(paths.Checkout, item.name)
		if err := os.WriteFile(file, []byte(item.content), 0600); err != nil {
			t.Fatal(err)
		}
		workspaceFiles[file] = item.content
	}
	discardPreview, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_discard_preview", SessionID: ownerSession, ID: discardWorkspace.ID,
	})
	if err != nil || len(discardPreview) != 1 || discardPreview[0].Worktree == nil {
		t.Fatalf("owner discard preview: messages=%+v err=%v", discardPreview, err)
	}
	decision := discardPreview[0].Worktree
	if decision.DiscardID == "" || decision.DiscardDigest == "" || decision.Generation == 0 || decision.ChangedFiles == 0 {
		t.Fatalf("owner discard preview lacks a current confirmation: %+v", decision)
	}

	_, ownerScope, err := service.workspaceScope(ctx, ClientMsg{SessionID: ownerSession, WorkKind: "session"})
	if err != nil {
		t.Fatal(err)
	}
	_, outsiderScope, err := service.workspaceScope(ctx, ClientMsg{SessionID: outsiderSession, WorkKind: "session"})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := service.workspaceService(formal)
	if err != nil {
		t.Fatal(err)
	}
	ownerBinding, err := manager.Binding(ownerScope)
	if err != nil || ownerBinding != boundWorkspace.ID {
		t.Fatalf("owner binding=%q err=%v; want %q", ownerBinding, err, boundWorkspace.ID)
	}
	outsiderBinding, err := manager.Binding(outsiderScope)
	if err != nil || outsiderBinding != "" {
		t.Fatalf("outsider binding=%q err=%v; want no binding", outsiderBinding, err)
	}
	initial := map[string]workspace.Snapshot{}
	for _, id := range []string{boundWorkspace.ID, removableWorkspace.ID, discardWorkspace.ID} {
		snapshot, getErr := manager.Get(ctx, ownerScope, id)
		if getErr != nil {
			t.Fatalf("get initial owner workspace %s: %v", id, getErr)
		}
		initial[id] = snapshot
	}
	if initial[boundWorkspace.ID].State != workspace.StateReady || initial[removableWorkspace.ID].State != workspace.StateReady || initial[discardWorkspace.ID].State != workspace.StateReady {
		t.Fatalf("unexpected workspace states before outsider requests: %+v", initial)
	}
	formalBefore, err := os.ReadFile(formalFile)
	if err != nil {
		t.Fatal(err)
	}

	assertUnchanged := func(op string) {
		t.Helper()
		binding, bindingErr := manager.Binding(ownerScope)
		if bindingErr != nil || binding != ownerBinding {
			t.Fatalf("%s changed owner binding: got=%q err=%v want=%q", op, binding, bindingErr, ownerBinding)
		}
		for id, want := range initial {
			got, getErr := manager.Get(ctx, ownerScope, id)
			if getErr != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s changed owner workspace %s: got=%+v err=%v want=%+v", op, id, got, getErr, want)
			}
		}
		for file, want := range workspaceFiles {
			got, readErr := os.ReadFile(file)
			if readErr != nil || string(got) != want {
				t.Fatalf("%s changed owner checkout %s: got=%q err=%v want=%q", op, file, got, readErr, want)
			}
		}
		gotFormal, readErr := os.ReadFile(formalFile)
		if readErr != nil || !reflect.DeepEqual(gotFormal, formalBefore) {
			t.Fatalf("%s changed formal bytes: got=%q err=%v want=%q", op, gotFormal, readErr, formalBefore)
		}
		candidateID, idErr := expectedWorkspaceCandidateID(ctx, removableWorkspace.ID, initial[removableWorkspace.ID].Generation, formalAbs, layout)
		if idErr != nil {
			t.Fatalf("compute candidate ID for no-write assertion: %v", idErr)
		}
		if _, getErr := db.GetCandidate(ctx, candidateID); !errors.Is(getErr, sql.ErrNoRows) {
			t.Fatalf("%s created a store candidate %s: %v", op, candidateID, getErr)
		}
		candidatePath := filepath.Join(filepath.Dir(formalAbs), ".stable-candidates", candidateID)
		if _, statErr := os.Lstat(candidatePath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s created candidate tree %s: %v", op, candidatePath, statErr)
		}
	}

	// Exit has no workspace ID and is scoped only to the caller's binding. With
	// no outsider binding the product contract is an empty no-op response.
	exitMessages, err := Request(ctx, socket, ClientMsg{Op: "worktree_exit", SessionID: outsiderSession})
	if err != nil || len(exitMessages) != 1 || exitMessages[0].Type != "worktree" || exitMessages[0].Worktree == nil || exitMessages[0].Worktree.ID != "" {
		t.Fatalf("outsider unbound exit should be an empty no-op: messages=%+v err=%v", exitMessages, err)
	}
	assertUnchanged("outsider exit")

	for _, item := range []struct {
		op string
		id string
	}{
		{op: "worktree_keep", id: removableWorkspace.ID},
		{op: "worktree_export", id: removableWorkspace.ID},
		{op: "worktree_preview", id: removableWorkspace.ID},
		{op: "worktree_remove", id: removableWorkspace.ID},
		{op: "worktree_discard_preview", id: discardWorkspace.ID},
	} {
		if _, err := Request(ctx, socket, ClientMsg{Op: item.op, SessionID: outsiderSession, ID: item.id}); err == nil {
			t.Fatalf("outsider operation %s on owner workspace %s was accepted", item.op, item.id)
		}
		assertUnchanged(item.op)
	}
	if _, err := Request(ctx, socket, ClientMsg{
		Op: "worktree_discard", SessionID: outsiderSession, ID: discardWorkspace.ID,
		DecisionID: decision.DiscardID, PreviewDigest: decision.DiscardDigest,
		WorktreeGeneration: decision.Generation,
	}); err == nil {
		t.Fatal("outsider consumed the owner's current discard confirmation")
	}
	assertUnchanged("worktree_discard")
}

func expectedWorkspaceCandidateID(ctx context.Context, workspaceID string, generation uint64, formalRoot string, layout *workspace.Layout) (string, error) {
	paths, err := layout.Paths(workspaceID)
	if err != nil {
		return "", err
	}
	baseline, err := workspace.BuildManifest(ctx, paths.Baseline, workspace.DefaultLimits())
	if err != nil {
		return "", err
	}
	formal, err := workspace.BuildManifest(ctx, formalRoot, workspace.DefaultLimits())
	if err != nil {
		return "", err
	}
	working, err := workspace.BuildManifest(ctx, paths.Checkout, workspace.DefaultLimits())
	if err != nil {
		return "", err
	}
	preview, err := workspace.ThreeWayPreview(baseline, formal, working, workspace.DefaultLimits())
	if err != nil {
		return "", err
	}
	identity := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", workspaceID, generation, preview.BaselineDigest, preview.FormalDigest, preview.WorkspaceDigest, preview.Manifest.Digest)
	digest := sha256.Sum256([]byte(identity))
	return "worktree-" + hex.EncodeToString(digest[:16]), nil
}
