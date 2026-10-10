package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// A child accepted into the durable shared-pool queue can be lost when the
// service exits before a worker starts it. Recovery must expose the turn as
// interrupted and wait for an explicit resume, preserving its message and
// budget accounting.
func TestAcceptedQueuedTeamTurnWaitsForExplicitResumeAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "lead-run-queued-recovery")
	service.deps.ProviderName = "fixture"
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	member := teams.Member{ID: "member-queued-recovery", Name: "reader", AgentName: role.Name, Model: "fixture", Tools: role.EffectiveTools(), Status: teams.MemberCreated, Revision: 1}
	roleHash := teamRoleHash(role)
	member.RoleHash = hex.EncodeToString(roleHash[:])

	team, err := service.CreateTeam(t.Context(), request, "queued-restart-recovery")
	if err != nil {
		t.Fatal(err)
	}
	member.TeamID = team.ID
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	message, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
		TeamID: team.ID, Recipient: member.ID, Body: "Check the queued recovery case.", Token: "queued-recovery-message",
	})
	if err != nil {
		t.Fatal(err)
	}

	turn := sessionlog.TurnFact{ID: "turn-queued-recovery", MemberID: member.ID, RunID: "child-queued-recovery", TaskID: "task-queued-recovery", OriginRunID: request.RunID, OriginCallID: "spawn-queued-recovery", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	identity := &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name}
	child := agent.ChildRunInput{
		TeamTurn: identity, ParentRunID: request.RunID, BatchID: "batch-queued-recovery", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "Inspect the assigned area."}, Work: request.Work,
	}
	seq := uint64(0)
	if err := service.persistTeamChildQueuedLocked(root, team.Scope, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	queued, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := queued.Turns[turn.ID].Status; got != "queued" {
		t.Fatalf("pre-restart turn status=%s, want queued", got)
	}
	if queued.Members[member.ID].Budget.AcceptedTurns != 1 || queued.Members[member.ID].Revision != 1 {
		t.Fatalf("pre-restart budget/revision=%+v/%d", queued.Members[member.ID].Budget, queued.Members[member.ID].Revision)
	}
	if len(queued.Handoffs) != 0 || len(queued.Messages) != 1 || queued.Messages[message.ID].ID == "" {
		t.Fatalf("fixture did not leave one pending, unhanded message: messages=%+v handoffs=%+v", queued.Messages, queued.Handoffs)
	}

	// Recreate the service and execute startup recovery before admitting any
	// explicit work. The newly configured runner must remain untouched here.
	runner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Service{deps: Deps{
		ProjectRoot: root, Agents: fixedTeamRoleCatalog{definition: role}, Delegator: pool,
		ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
		ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "fixture",
	}, lifeCtx: context.Background(), activeRuns: map[string]string{request.RunID: request.Work.SessionID}, activeRequests: map[string]agent.ExecutionRequest{request.RunID: request}}
	restarted.teamScheduler = newTeamScheduler(restarted)
	t.Cleanup(func() {
		restarted.teamScheduler.close()
		pool.Close()
	})
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Turns[turn.ID].Status; got != string(agent.DelegationInterrupted) {
		t.Fatalf("recovered turn status=%s, want interrupted", got)
	}
	recovered := projection.Members[member.ID]
	if recovered.Status != teams.MemberInterrupted || recovered.Budget.AcceptedTurns != 1 || recovered.Revision != queued.Members[member.ID].Revision+1 {
		t.Fatalf("recovered member did not preserve budget and monotonic revision: %+v", recovered)
	}
	if runner.count() != 0 {
		t.Fatalf("startup recovery automatically started %d child runs", runner.count())
	}
	if len(projection.Messages) != 1 || projection.Messages[message.ID].ID == "" || len(projection.Handoffs) != 0 {
		t.Fatalf("recovery lost or consumed pending message: messages=%+v handoffs=%+v", projection.Messages, projection.Handoffs)
	}

	if _, err := restarted.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "explicit-resume-queued-recovery"); err != nil {
		t.Fatal(err)
	}
	resumed := receiveTeamChildInput(t, runner.inputs)
	if resumed.TeamTurn == nil || resumed.TeamTurn.TurnID == turn.ID || strings.Count(resumed.Task.Instruction, message.Body) != 1 {
		t.Fatalf("explicit resume did not carry the pending message once in a new turn: %+v", resumed)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runner.count() != 1 || projection.Members[member.ID].Budget.AcceptedTurns != 2 {
		t.Fatalf("explicit resume calls/budget=%d/%+v, want one call and two accepted turns", runner.count(), projection.Members[member.ID].Budget)
	}
	facts, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	var handoffs []sessionlog.HandoffFact
	for _, event := range facts {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil && fact.Handoff.MessageID == message.ID {
			handoffs = append(handoffs, *fact.Handoff)
		}
	}
	if len(handoffs) != 1 || handoffs[0].DestinationTurnID != resumed.TeamTurn.TurnID {
		t.Fatalf("pending message was not handed off exactly once to resumed turn: %+v", handoffs)
	}
}
