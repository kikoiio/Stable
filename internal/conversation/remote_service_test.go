package conversation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/core"
	"stable/internal/sessionlog"
)

func approveRemoteGrant(t *testing.T, registry *RemoteAccessRegistry, root, connectionID, requestID string) RemoteGrant {
	t.Helper()
	result := make(chan struct {
		grant RemoteGrant
		err   error
	}, 1)
	go func() {
		grant, err := registry.RequestWithID(context.Background(), requestID, connectionID, "Browser", root)
		result <- struct {
			grant RemoteGrant
			err   error
		}{grant, err}
	}()
	request := waitRemoteAccessRequest(t, registry)
	if err := registry.Resolve(request.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.grant
}

func TestAuthorizeRemoteMessageUsesGrantAndServerRunBounds(t *testing.T) {
	root := t.TempDir()
	info, err := sessionlog.Create(root, "remote session")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{remoteAccess: NewRemoteAccessRegistry()}
	grant := approveRemoteGrant(t, service.remoteAccess, root, "connection-1", "request-1")

	permissionBounds := json.RawMessage(`{"allowed_root":"/outside/project"}`)
	resourceBounds := json.RawMessage(`{"max_steps":999}`)
	requestRun := &agent.ExecutionRequest{
		Work:             agent.WorkRef{Kind: agent.WorkGoal, SessionID: info.ID, GoalID: "goal-unsafe"},
		Intent:           "review files",
		ProviderName:     "attacker-provider",
		Model:            "attacker-model",
		AllowedScope:     []string{"/outside/project"},
		PermissionBounds: permissionBounds,
		ResourceBounds:   resourceBounds,
	}
	msg := ClientMsg{Op: "run_start", SessionID: info.ID, ProjectRoot: "/outside/project", RemoteConnectionID: "connection-1", RemoteGrantID: grant.ID, Run: requestRun}
	gotRoot, err := service.authorizeRemoteMessage(&msg)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != root || msg.ProjectRoot != root {
		t.Fatalf("authorized root = %q, request root = %q, want %q", gotRoot, msg.ProjectRoot, root)
	}
	if msg.Ephemeral || msg.Run.Work.Kind != agent.WorkSession || msg.Run.Work.SessionID != info.ID || msg.Run.Work.GoalID != "" || msg.Run.ProviderName != "" || msg.Run.Model != "" {
		t.Fatalf("remote run retained client authority fields: %+v", msg.Run)
	}
	if msg.Run.PermissionBounds != nil || msg.Run.ResourceBounds != nil || msg.Run.AllowedScope != nil {
		t.Fatalf("remote run retained client bounds: %+v", msg.Run)
	}
	if _, err := service.requestProjectRoot(msg); err != nil {
		t.Fatalf("requestProjectRoot() rejected approved root: %v", err)
	}
	if got := service.sessionProjectRoot(info.ID); got != root {
		t.Fatalf("sessionProjectRoot after authorization = %q, want %q", got, root)
	}
}

func TestAuthorizeRemoteMessageRejectsUnapprovedOperationsAndForeignSession(t *testing.T) {
	root := t.TempDir()
	service := &Service{remoteAccess: NewRemoteAccessRegistry()}
	grant := approveRemoteGrant(t, service.remoteAccess, root, "connection-1", "request-2")
	if _, err := service.authorizeRemoteMessage(&ClientMsg{Op: "create_goal", RemoteConnectionID: "connection-1", RemoteGrantID: grant.ID}); err == nil {
		t.Fatal("remote create_goal was allowed")
	}
	otherRoot := t.TempDir()
	info, err := sessionlog.Create(otherRoot, "private session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.authorizeRemoteMessage(&ClientMsg{Op: "session_load", SessionID: info.ID, RemoteConnectionID: "connection-1", RemoteGrantID: grant.ID}); err == nil {
		t.Fatal("remote grant accessed a session from another root")
	}
}

func TestFilterRemoteMessageIncludesOnlyGoalsForApprovedRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	message := ServerMsg{Goals: []core.Goal{
		{ID: "allowed", AllowedRoot: root},
		{ID: "private", AllowedRoot: other},
		{ID: "unknown"},
	}, GoalEvents: []GoalEventSummary{
		{GoalID: "allowed", Kind: "design_changed", Status: "processed"},
		{GoalID: "private", Kind: "secret", Status: "processed"},
	}}
	filterRemoteMessage(&message, root)
	if len(message.Goals) != 1 || message.Goals[0].ID != "allowed" {
		t.Fatalf("filtered goals = %+v", message.Goals)
	}
	if len(message.GoalEvents) != 1 || message.GoalEvents[0].GoalID != "allowed" {
		t.Fatalf("filtered goal events = %+v", message.GoalEvents)
	}
	private := core.Goal{ID: "private", AllowedRoot: other}
	message.Goal = &private
	filterRemoteMessage(&message, root)
	if message.Goal != nil {
		t.Fatalf("foreign goal was not removed: %+v", message.Goal)
	}
}

func TestRemoteSessionRootRequiresExistingDirectory(t *testing.T) {
	service := &Service{deps: Deps{ProjectRoot: t.TempDir()}}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := service.requestProjectRoot(ClientMsg{ProjectRoot: missing}); err == nil {
		t.Fatal("missing local root was accepted")
	}
}
