package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMemberTurnCannotSpawnAnotherMember(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "member-spawn-lead")
	leadRequest.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: leadRequest.RunID, SessionID: leadRequest.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{leadRequest.RunID: leadRequest}
	team, err := service.CreateTeam(context.Background(), leadRequest, "member-spawn-authorization")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "member-spawn-reader"
	addTeamMessageMember(t, service, leadRequest, team.ID, memberID, "reader")

	turnID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	memberRequest := leadRequest
	memberRequest.RunID = "member-spawn-child"
	memberRequest.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: memberID, TurnID: turnID, MemberName: "reader"}
	memberRequest.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: memberRequest.RunID, SessionID: memberRequest.Work.SessionID, AllowedRoot: root,
	})
	turn := sessionlog.TurnFact{ID: turnID, MemberID: memberID, RunID: memberRequest.RunID, TaskID: turnID, OriginRunID: leadRequest.RunID, Status: "intent"}
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnIntent, turn)
	turn.Status = "queued"
	appendTeamMessageLimitFact(t, service, leadRequest, team.ID, sessionlog.TeamTurnAccepted, turn)
	if _, err := sessionlog.Append(root, leadRequest.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: memberRequest.RunID, WorkKind: string(leadRequest.Work.Kind), Intent: "member spawn authorization fixture",
		TeamID: team.ID, TeamMemberID: memberID, TeamTurnID: turnID, OriginRunID: leadRequest.RunID,
	}); err != nil {
		t.Fatal(err)
	}

	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1), release: make(chan struct{}, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.lifeCtx = context.Background()
	service.deps.Agents = fixedTeamRoleCatalog{definition: agentcatalog.Definition{
		Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 2,
	}}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})

	before, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SpawnTeamMember(context.Background(), memberRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "unauthorized-child", AgentName: "explore", Instruction: "try to add a sibling",
		OriginCallID: "forged-member-spawn",
	})
	if !errors.Is(err, teams.ErrPermission) {
		t.Fatalf("member spawn error=%v, want permission denied", err)
	}
	after, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Teams[team.ID].Revision != before.Teams[team.ID].Revision || len(after.Members) != len(before.Members) || len(after.Turns) != len(before.Turns) {
		t.Fatalf("rejected member spawn changed team facts: before=%+v after=%+v", before.Teams[team.ID], after.Teams[team.ID])
	}
	if _, exists := after.Members[memberID]; !exists {
		t.Fatal("rejected member spawn removed the authorized member")
	}
	select {
	case input := <-runner.inputs:
		t.Fatalf("rejected member spawn scheduled a child run: %+v", input)
	default:
	}
}
