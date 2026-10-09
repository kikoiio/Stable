package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/candidate"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/store"
	"stable/internal/teams"
	"stable/internal/workspace"
)

type worktreeTeamWriterRunner struct {
	started  chan agent.ChildRunInput
	finished chan agent.ChildRunResult
}

func (r *worktreeTeamWriterRunner) Run(ctx context.Context, input agent.ChildRunInput) (result agent.ChildRunResult) {
	defer func() { r.finished <- result }()
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
	}
	executor, err := input.ExecutorFactory.ForRun(agent.ExecutionRequest{RunID: input.ChildRunID, Work: input.Work, PermissionBounds: input.PermissionBounds})
	if err != nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
	}
	if _, err := executor.Execute(ctx, llm.ToolUse{ID: "team-read", Name: "read_file", Arguments: json.RawMessage(`{"file_path":"team-result.txt"}`)}); err != nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "workspace read: " + err.Error()}
	}
	out, err := executor.Execute(ctx, llm.ToolUse{ID: "team-write", Name: "write_file", Arguments: json.RawMessage(`{"file_path":"team-result.txt","content":"team candidate bytes"}`)})
	if err != nil || out.IsError {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: fmt.Sprintf("workspace write: %s (%v)", out.Content, err)}
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "wrote team candidate"}
}

func TestWorktreeTeamMemberFlowsThroughExportReviewAndAcceptance(t *testing.T) {
	ctx := context.Background()
	formal := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "team-result.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, formal, "team-worktree-parent")
	formal, err := filepath.Abs(formal)
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds, err = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: formal,
		FormalRoot: formal, CandidateRoot: filepath.Join(t.TempDir(), "unused-candidate"), Mode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service.deps.Store = db
	service.deps.WorkspaceStateRoot = filepath.Join(t.TempDir(), "workspace-state")
	service.deps.CandidateCheckers = []candidate.Checker{passingWorkspaceChecker{}}
	service.workspaces = map[string]*workspace.LifecycleService{}
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	service.deps.ForkExecutorFactory = execution.NewToolExecutorFactory(execution.ToolExecutorDeps{
		Gate: leadRunAllowGate{}, Sandbox: &leadRunToolSandbox{}, HelperPath: helper,
	})
	service.deps.ForkToolSchemas = execution.WorkspaceWriterToolSchemas()
	service.deps.ToolSchemas = append([]llm.ToolSchema(nil), execution.WorkspaceWriterToolSchemas()...)
	for _, name := range agent.TeamMemberToolNames() {
		service.deps.ToolSchemas = append(service.deps.ToolSchemas, llm.ToolSchema{Name: name})
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: agentcatalog.Definition{
		Name: "builder", Instruction: "Implement the assigned change in the isolated worktree.", Model: "fixture",
		Tools: []string{"read_file", "write_file"}, Isolation: "worktree", MaxTurns: 2,
	}}
	service.deps.ForkProvider, service.deps.ProviderName, service.deps.Model = forkSkillFixtureProvider{}, "fixture", "fixture"
	runner := &worktreeTeamWriterRunner{started: make(chan agent.ChildRunInput, 2), finished: make(chan agent.ChildRunResult, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Delegator = pool
	service.lifeCtx = ctx
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
		for _, manager := range service.workspaces {
			if err := manager.Close(context.Background()); err != nil {
				t.Errorf("close workspace manager: %v", err)
			}
		}
	})
	team, err := service.CreateTeam(ctx, request, "isolated-builder")
	if err != nil {
		t.Fatal(err)
	}
	request.TeamCoordinator, request.TeamCoordinatorTeamID = true, team.ID
	member, err := service.SpawnTeamMember(ctx, request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "builder-one", AgentName: "builder", Instruction: "Write the requested result.", OriginCallID: "spawn-builder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if member.WorkspaceID == "" {
		t.Fatal("worktree member has no service-issued workspace ID")
	}
	var first agent.ChildRunInput
	select {
	case first = <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("worktree team child did not start")
	}
	if first.WorkspaceID != member.WorkspaceID || first.WorkspaceGeneration == 0 || first.ChildRunID == request.RunID {
		t.Fatalf("child workspace attribution is incomplete: input=%+v member=%+v", first, member)
	}
	select {
	case result := <-runner.finished:
		if result.Status != agent.DelegationSucceeded {
			t.Fatalf("worktree team child result=%+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worktree team child did not finish")
	}
	waitForTeamMemberStatus(t, formal, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	unauthorizedResume := request
	unauthorizedResume.TeamCoordinator = false
	if _, err := service.ResumeTeamMember(ctx, unauthorizedResume, team.ID, member.ID, "resume-without-coordinator"); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("worktree resume without coordinator authorization = %v", err)
	}
	wrongTeamResume := request
	wrongTeamResume.TeamCoordinatorTeamID = "f123456789abcdef0123456789abcdef"
	if _, err := service.ResumeTeamMember(ctx, wrongTeamResume, team.ID, member.ID, "resume-wrong-coordinator-team"); !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("worktree resume with wrong coordinator team = %v", err)
	}
	resumed, err := service.ResumeTeamMember(ctx, request, team.ID, member.ID, "resume-builder")
	if err != nil {
		t.Fatal(err)
	}
	var second agent.ChildRunInput
	select {
	case second = <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("resumed worktree team child did not start")
	}
	select {
	case result := <-runner.finished:
		if result.Status != agent.DelegationSucceeded {
			t.Fatalf("resumed worktree team child result=%+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resumed worktree team child did not finish")
	}
	if resumed.WorkspaceID != member.WorkspaceID || second.WorkspaceID != first.WorkspaceID || second.WorkspaceGeneration <= first.WorkspaceGeneration || second.ChildRunID == first.ChildRunID {
		t.Fatalf("resume did not reuse workspace with a fresh writer generation: first=%+v second=%+v member=%+v resumed=%+v", first, second, member, resumed)
	}
	waitForTeamMemberStatus(t, formal, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if got, err := os.ReadFile(filepath.Join(formal, "team-result.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("team child changed formal source before acceptance: %q err=%v", got, err)
	}
	projectRoot, scope, err := service.workspaceScope(ctx, ClientMsg{SessionID: request.Work.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := service.workspaceService(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := manager.Get(ctx, scope, member.WorkspaceID)
	if err != nil || kept.State != workspace.StateKept || kept.WriterRunID != "" || kept.ChangedFiles == 0 {
		t.Fatalf("member workspace was not kept after its turn: %+v err=%v", kept, err)
	}
	exported, err := manager.Export(ctx, scope, member.WorkspaceID)
	if err != nil || exported.State != workspace.StateExported || exported.CandidateID == "" {
		t.Fatalf("member workspace export=%+v err=%v", exported, err)
	}
	candidateRecord, err := db.GetCandidate(ctx, exported.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	candidateBytes, err := os.ReadFile(filepath.Join(candidateRecord.Candidate.CandidateRoot, "team-result.txt"))
	if err != nil || string(candidateBytes) != "team candidate bytes" {
		t.Fatalf("team result missing from exported candidate: %q err=%v", candidateBytes, err)
	}
	review, err := service.reviewCandidate(ctx, exported.CandidateID, request.Work.SessionID)
	if err != nil || review.Digest == "" || len(review.Findings) == 0 || review.Findings[0].Result != candidate.FindingPass {
		t.Fatalf("exported member result review=%+v err=%v", review, err)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "team-result.txt")); err != nil || string(got) != "formal baseline" {
		t.Fatalf("formal source changed during export/review: %q err=%v", got, err)
	}
	decisionID, err := workspace.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.acceptReviewedCandidate(ctx, ClientMsg{
		CandidateID: exported.CandidateID, SessionID: request.Work.SessionID, DecisionID: decisionID,
		PreviewDigest: review.Digest, CandidateDigest: review.CandidateDigest, FormalDigest: review.FormalDigest,
		AcceptanceMode: string(candidate.AcceptNormal),
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(formal, "team-result.txt")); err != nil || string(got) != "team candidate bytes" {
		t.Fatalf("explicitly accepted member result was not installed: %q err=%v", got, err)
	}
}
