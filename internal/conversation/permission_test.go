package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

func TestApprovalReconnectAndSingleUseResolution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	session, err := sessionlog.Create(root, "permission")
	if err != nil {
		t.Fatal(err)
	}
	formal, candidateRoot := filepath.Join(root, "formal"), filepath.Join(root, "candidate")
	for _, path := range []string{formal, candidateRoot} {
		if err = os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	updates := make(chan ServerMsg, 4)
	svc := &Service{deps: Deps{Store: state, ProjectRoot: root}, clients: map[chan ServerMsg]*clientSubscription{updates: {ch: updates, sessionID: session.ID}}}
	authority := permission.Authority{RunID: "run-approval", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidateRoot, Mode: permission.ModeDefault}
	operation := permission.Operation{ID: "op-1", Kind: permission.OpWrite, Name: "edit-file", Target: filepath.Join(candidateRoot, "board"), Parameters: []byte(`{"line":1}`)}
	decision, err := svc.AuthorizeOperation(ctx, authority, operation)
	if err != nil || decision.Kind != permission.DecisionAsk || decision.ApprovalID == "" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	message := <-updates
	if message.Type != "approval_pending" || message.Approval == nil || message.Approval.RunID != authority.RunID {
		t.Fatalf("push=%+v", message)
	}
	recovered, err := svc.pendingApprovals(ctx, session.ID)
	if err != nil || len(recovered) != 1 || recovered[0].ID != decision.ApprovalID {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	resolved, err := svc.resolveApproval(ctx, ClientMsg{SessionID: session.ID, ApprovalID: decision.ApprovalID, ApprovalChoice: string(permission.ChoiceAllowOnce)})
	if err != nil || resolved.Kind != permission.DecisionAllow {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	retry, err := svc.AuthorizeOperation(ctx, authority, operation)
	if err != nil || retry.Kind != permission.DecisionAllow {
		t.Fatalf("same action did not consume its one-time approval: %+v %v", retry, err)
	}
	if err = state.ConsumeApproval(ctx, decision.ApprovalID, resolved.ScopeDigest, resolved.OperationDigest); err == nil {
		t.Fatal("one-time approval remained consumable after authorization retry")
	}
	if _, err = svc.resolveApproval(ctx, ClientMsg{SessionID: session.ID, ApprovalID: decision.ApprovalID, ApprovalChoice: string(permission.ChoiceDeny)}); err == nil {
		t.Fatal("resolved approval was replayed")
	}
}

func TestApprovalCannotBeResolvedFromAnotherSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	session, _ := sessionlog.Create(root, "one")
	other, _ := sessionlog.Create(root, "two")
	formal, candidateRoot := filepath.Join(root, "formal"), filepath.Join(root, "candidate")
	_ = os.MkdirAll(formal, 0700)
	_ = os.MkdirAll(candidateRoot, 0700)
	svc := &Service{deps: Deps{Store: state, ProjectRoot: root}}
	authority := permission.Authority{RunID: "run", SessionID: session.ID, AllowedRoot: formal, FormalRoot: formal, CandidateRoot: candidateRoot}
	operation := permission.Operation{ID: "op", Kind: permission.OpWrite, Name: "edit", Target: filepath.Join(candidateRoot, "board")}
	decision, err := svc.AuthorizeOperation(ctx, authority, operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.resolveApproval(ctx, ClientMsg{SessionID: other.ID, ApprovalID: decision.ApprovalID, ApprovalChoice: string(permission.ChoiceAllowOnce)}); err == nil {
		t.Fatal("cross-session approval was accepted")
	}
}
