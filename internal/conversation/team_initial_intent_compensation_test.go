package conversation

import (
	"context"
	"encoding/hex"
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

func TestCompensateUnpublishedInitialIntentMarksMemberInterruptedAndResumable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "initial-intent-compensation-parent")
	request.Model = "fixture"
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "fixture"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	defer service.teamScheduler.close()

	team, err := service.CreateTeam(t.Context(), request, "initial-intent-recovery")
	if err != nil {
		t.Fatal(err)
	}
	roleHash := teamRoleHash(role)
	member := teams.Member{
		ID: "member-initial-intent-compensation", TeamID: team.ID, Name: "reader", AgentName: role.Name,
		RoleHash: hex.EncodeToString(roleHash[:]), Model: request.Model, Tools: role.EffectiveTools(),
		Status: teams.MemberCreated, Revision: 1,
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{
		ID: "turn-initial-intent-compensation", MemberID: member.ID, TaskID: "task-initial-intent-compensation",
		RunID: "child-initial-intent-compensation", OriginRunID: request.RunID,
		OriginCallID: "call-initial-intent-compensation", Status: "intent",
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn,
	}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "inspect"}, Work: request.Work,
	}
	var runSeq uint64
	if err := service.compensateUnpublishedTeamAdmission(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, &runSeq, errors.New("injected accepted-fact append failure")); err != nil {
		t.Fatalf("compensate durable intent: %v", err)
	}
	if runner.count() != 0 {
		t.Fatalf("compensation started provider work: calls=%d", runner.count())
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turn.ID].Status; got != "aborted" {
		t.Fatalf("turn status after compensation=%q, want aborted", got)
	}
	if got := projection.Members[member.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("member status after compensation=%q, want interrupted", got)
	}
	// Also exercise a process stop after the abort append but before its member
	// repair append. A later compensation retry must complete this partial pair.
	partialMember := member
	partialMember.ID = "member-aborted-intent-gap"
	partialMember.Name = "reader-two"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &partialMember,
	}); err != nil {
		t.Fatal(err)
	}
	partialTurn := sessionlog.TurnFact{
		ID: "turn-aborted-intent-gap", MemberID: partialMember.ID, TaskID: "task-aborted-intent-gap",
		RunID: "child-aborted-intent-gap", OriginRunID: request.RunID,
		OriginCallID: "call-aborted-intent-gap", Status: "intent",
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &partialTurn,
	}); err != nil {
		t.Fatal(err)
	}
	partialTurn.Status = "aborted"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnAborted, ActorID: "service", ActorRunID: request.RunID, Turn: &partialTurn,
	}); err != nil {
		t.Fatal(err)
	}
	partialChild := child
	partialChild.TeamTurn = &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: partialMember.ID, TurnID: partialTurn.ID, MemberName: partialMember.Name}
	partialChild.ChildRunID = partialTurn.RunID
	if err := service.compensateUnpublishedTeamAdmission(root, request.Work.SessionID, team.ID, partialMember.ID, partialTurn.ID, partialChild, &runSeq, errors.New("retry after abort append")); err != nil {
		t.Fatalf("repair member state after durable abort: %v", err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[partialMember.ID].Status; got != teams.MemberInterrupted {
		t.Fatalf("member status after abort-gap repair=%q, want interrupted", got)
	}

	// Re-entering compensation models recovery after the abort fact was durable;
	// it must remain idempotent after the member-state fact is repaired.
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRetry := len(transcript.Events)
	if err := service.compensateUnpublishedTeamAdmission(root, request.Work.SessionID, team.ID, member.ID, turn.ID, child, &runSeq, errors.New("retry compensation")); err != nil {
		t.Fatalf("retry compensation: %v", err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != beforeRetry {
		t.Fatalf("retry compensation appended events: %d -> %d", beforeRetry, len(transcript.Events))
	}

	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "explicit-resume-after-compensation"); err != nil {
		t.Fatalf("explicit resume after compensated initial intent: %v", err)
	}
	resumed := receiveTeamChildInput(t, runner.inputs)
	if resumed.TeamTurn == nil || resumed.TeamTurn.MemberID != member.ID || resumed.TeamTurn.TurnID == turn.ID {
		t.Fatalf("resume did not create a fresh turn for the same member: %+v", resumed.TeamTurn)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if runner.count() != 1 {
		t.Fatalf("explicit resume provider calls=%d, want 1", runner.count())
	}
}
