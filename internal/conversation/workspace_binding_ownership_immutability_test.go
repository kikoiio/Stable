package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/workspace"
)

func TestWorkspaceBindingDenialsPreserveOwnerAndSiblingState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	formal := filepath.Join(root, "project")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "baseline.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	socket, err := filepath.Abs(filepath.Join(".tmp", fmt.Sprintf("m09-bind-%d.sock", os.Getpid())))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := Serve(ctx, Deps{
		Store: db, ProjectRoot: formal, WorkspaceStateRoot: filepath.Join(root, "workspace-state"),
		SocketPath: socket, PollEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	createSession := func() string {
		t.Helper()
		messages, err := Request(reqctx, socket, ClientMsg{Op: "session_create", ProjectRoot: formal})
		if err != nil || len(messages) != 1 || messages[0].Session == nil {
			t.Fatalf("create session: messages=%+v err=%v", messages, err)
		}
		return messages[0].Session.ID
	}
	firstID, siblingID := createSession(), createSession()
	_, firstScope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: firstID})
	if err != nil {
		t.Fatal(err)
	}
	_, siblingScope, err := svc.workspaceScope(ctx, ClientMsg{SessionID: siblingID})
	if err != nil {
		t.Fatal(err)
	}
	formalAbs, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []*workspace.Scope{&firstScope, &siblingScope} {
		scope.Authority = permission.Authority{
			RunID: "lead-" + scope.SessionID, SessionID: scope.SessionID,
			AllowedRoot: formalAbs, FormalRoot: formalAbs,
			CandidateRoot: filepath.Join(root, "candidate", scope.SessionID),
		}
	}
	manager, err := svc.workspaceService(formal)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Create(ctx, firstScope, "first session")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := manager.Create(ctx, siblingScope, "sibling session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, firstScope, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(ctx, siblingScope, sibling.ID); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256([]byte(filepath.Clean(formalAbs)))
	projectID := "p" + hex.EncodeToString(digest[:16])
	layout, err := workspace.NewLayout(filepath.Join(root, "workspace-state"), formalAbs, projectID)
	if err != nil {
		t.Fatal(err)
	}
	pathsFor := func(id string) workspace.Paths {
		t.Helper()
		paths, err := layout.Paths(id)
		if err != nil {
			t.Fatal(err)
		}
		return paths
	}
	seed := func(id, name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(pathsFor(id).Checkout, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	seed(first.ID, "owner.txt", "owner bytes")
	seed(sibling.ID, "sibling.txt", "sibling bytes")

	assertUnchanged := func(scope workspace.Scope, id, name, body string, generation uint64) {
		t.Helper()
		bound, err := manager.Binding(scope)
		if err != nil || bound != id {
			t.Fatalf("session %s binding=%q err=%v want=%q", scope.SessionID, bound, err, id)
		}
		snapshot, err := manager.Get(ctx, scope, id)
		if err != nil || snapshot.Generation != generation || snapshot.State != workspace.StateReady {
			t.Fatalf("session %s workspace changed: snapshot=%+v err=%v want generation=%d ready", scope.SessionID, snapshot, err, generation)
		}
		content, err := os.ReadFile(filepath.Join(pathsFor(id).Checkout, name))
		if err != nil || string(content) != body {
			t.Fatalf("session %s workspace content changed: %q err=%v want=%q", scope.SessionID, content, err, body)
		}
	}

	// A sibling session cannot inspect or bind the owner's workspace. Both
	// rejected operations must leave the two independent bindings and trees intact.
	if _, err := manager.Get(ctx, siblingScope, first.ID); !errors.Is(err, workspace.ErrOwnership) {
		t.Fatalf("sibling read owner's workspace: %v", err)
	}
	if _, err := manager.Enter(ctx, siblingScope, first.ID); !errors.Is(err, workspace.ErrOwnership) {
		t.Fatalf("sibling entered owner's workspace: %v", err)
	}
	assertUnchanged(firstScope, first.ID, "owner.txt", "owner bytes", first.Generation)
	assertUnchanged(siblingScope, sibling.ID, "sibling.txt", "sibling bytes", sibling.Generation)

	// An active run pins the owner's binding and run identity. Attempts
	// to exit or switch are denied without changing either session's workspace.
	activeRunID := "active-owner-run"
	authority := permission.Authority{
		RunID: activeRunID, SessionID: firstScope.SessionID,
		AllowedRoot: pathsFor(first.ID).Checkout,
		FormalRoot:  formalAbs, CandidateRoot: filepath.Join(root, "candidate", "active"),
	}
	permissionBounds, err := json.Marshal(authority)
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ExecutionRequest{
		RunID: activeRunID, Work: firstScope.Work, Intent: "pinned owner work",
		PermissionBounds: permissionBounds,
	}
	svc.mu.Lock()
	svc.activeRuns[activeRunID] = firstScope.SessionID
	svc.activeRequests[activeRunID] = request
	svc.mu.Unlock()
	if _, err := manager.Exit(ctx, firstScope); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("active run allowed binding exit: %v", err)
	}
	if _, err := manager.Enter(ctx, firstScope, sibling.ID); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("active run allowed binding switch: %v", err)
	}
	assertUnchanged(firstScope, first.ID, "owner.txt", "owner bytes", first.Generation)
	assertUnchanged(siblingScope, sibling.ID, "sibling.txt", "sibling bytes", sibling.Generation)
	svc.mu.Lock()
	gotRequest := svc.activeRequests[activeRunID]
	svc.mu.Unlock()
	if gotRequest.RunID != request.RunID || gotRequest.Work != request.Work || gotRequest.Intent != request.Intent ||
		string(gotRequest.PermissionBounds) != string(request.PermissionBounds) {
		t.Fatalf("denied binding mutation changed active run identity/authority: got=%+v want=%+v", gotRequest, request)
	}
}
