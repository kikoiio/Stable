package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
	"stable/internal/workspace"
)

func TestTeamWorkspaceCleanupRetainsCreatedWorkspaceWhenTeamLogIsDamaged(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "lead-run")
	team, err := service.CreateTeam(ctx, request, "cleanup-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(t.TempDir(), "workspace-state")
	layout, err := workspace.NewLayout(stateRoot, root, "project1")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := workspace.NewService(layout, workspace.DefaultLimits(), workspace.ServiceDependencies{IdleGuard: service})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close workspace service: %v", err)
		}
	})
	formal, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	scope := workspace.Scope{
		ProjectID: "project1", SessionID: request.Work.SessionID,
		OriginRunID: request.RunID, OriginTaskID: "member-cleanup",
		Work: request.Work,
		Authority: permission.Authority{
			RunID: request.RunID, SessionID: request.Work.SessionID,
			AllowedRoot: formal, FormalRoot: formal,
			CandidateRoot: filepath.Join(root, "candidate"),
		},
	}
	created, err := manager.Create(ctx, scope, "team member workspace")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireWriter(ctx, scope, created.ID, "child-run")
	if err != nil {
		t.Fatal(err)
	}
	memberID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: memberID, TeamID: team.ID, Name: "builder", AgentName: "explore",
		RoleHash: "fixture-role", Model: "fixture", Tools: []string{"read_file"},
		Status: teams.MemberCreated, Revision: 1, WorkspaceID: created.ID,
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	factID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: factID, TeamID: team.ID, SessionID: request.Work.SessionID,
		Kind: sessionlog.TeamMemberAdded, Revision: projection.Teams[team.ID].Revision + 1,
		ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	logPath, err := sessionlog.SessionPath(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.WriteString("{damaged"); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID); err == nil {
		t.Fatal("damaged team log unexpectedly replayed")
	}
	service.mu.Lock()
	delete(service.activeRuns, request.RunID)
	service.mu.Unlock()

	cleanupTeamMemberWorkspaceUnlessReferenced(root, request.Work.SessionID, team.ID, memberID, manager, lease, true)

	got, err := manager.Get(ctx, scope, created.ID)
	if err != nil {
		t.Fatalf("query workspace after uncertain team replay: %v", err)
	}
	if got.State == workspace.StateRemoved {
		t.Fatal("cleanup removed a workspace whose durable team reference could not be replayed")
	}
	paths, err := layout.Paths(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.Root); err != nil {
		t.Fatalf("referenced workspace root was not retained: %v", err)
	}
	base, err := os.ReadFile(filepath.Join(paths.Checkout, "base.txt"))
	if err != nil || string(base) != "baseline" {
		t.Fatalf("referenced workspace data changed: content=%q err=%v", base, err)
	}
	if got.State != workspace.StateKept || got.WriterRunID != "" {
		t.Fatalf("safely releasable writer lease was not retained as kept: state=%s writer=%q", got.State, got.WriterRunID)
	}
}
