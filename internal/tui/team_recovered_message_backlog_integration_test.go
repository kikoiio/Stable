package tui

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// A queued member's multi-message backlog survives service restart and is
// delivered in order exactly once after explicit TUI resume.
func TestTeamTUIExplicitResumeDrainsRecoveredMessageBacklogOnce(t *testing.T) {
	ctx := context.Background()
	tmpParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpParent, "trmb-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	roles := agentcatalog.New("", "")
	role, ok := roles.Resolve("explore")
	if !ok {
		t.Fatal("built-in explore role is unavailable")
	}
	roleHash, err := recoveredResumeRoleHash(role)
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(project, "recovered multi-message backlog")
	if err != nil {
		t.Fatal(err)
	}
	oldLeadRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(project, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: oldLeadRunID, WorkKind: string(agent.WorkSession), Intent: "old lead queued team turn",
	}); err != nil {
		t.Fatal(err)
	}
	teamID, memberID := mustSessionlogID(t), mustSessionlogID(t)
	team := teams.Team{
		ID: teamID, Name: "message-backlog",
		Scope:        teams.Scope{SessionID: session.ID, WorkKind: string(agent.WorkSession), ProjectRoot: project, ProviderName: "fixture"},
		CreatorRunID: oldLeadRunID, Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC(),
	}
	if _, err := sessionlog.Append(project, session.ID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: mustSessionlogID(t), TeamID: teamID, SessionID: session.ID, Kind: sessionlog.TeamCreated,
		Revision: 1, ActorID: teams.Lead, ActorRunID: oldLeadRunID, Team: &team,
	}); err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: memberID, TeamID: teamID, Name: "reader", AgentName: role.Name,
		RoleHash: hex.EncodeToString(roleHash[:]), Model: "fixture-model", Tools: role.EffectiveTools(),
		Status: teams.MemberCreated, Revision: 1,
	}
	appendRecoveredResumeTeamFact(t, project, session.ID, teamID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: oldLeadRunID, Member: &member,
	})

	messages := []teams.Message{
		{ID: mustSessionlogID(t), TeamID: teamID, SenderID: teams.Lead, Recipients: []string{memberID}, Body: "Backlog item one: inspect parser recovery.", CreatedAt: time.Now().UTC()},
		{ID: mustSessionlogID(t), TeamID: teamID, SenderID: teams.Lead, Recipients: []string{memberID}, Body: "Backlog item two: verify retry boundaries.", CreatedAt: time.Now().UTC()},
	}
	for i := range messages {
		appendRecoveredResumeTeamFact(t, project, session.ID, teamID, sessionlog.TeamEvent{
			Kind: sessionlog.TeamMessageSent, ActorID: teams.Lead, ActorRunID: oldLeadRunID, Message: &messages[i],
		})
	}
	turnID, childRunID, taskID := mustTeamIDs(t)
	turn := sessionlog.TurnFact{
		ID: turnID, MemberID: memberID, RunID: childRunID, TaskID: taskID,
		OriginRunID: oldLeadRunID, OriginCallID: "spawn-before-backlog-restart", Status: "intent",
	}
	appendRecoveredResumeTeamFact(t, project, session.ID, teamID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: oldLeadRunID, Turn: &turn,
	})
	accepted := turn
	accepted.Status = "queued"
	appendRecoveredResumeTeamFact(t, project, session.ID, teamID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: oldLeadRunID, Turn: &accepted,
	})
	if _, err := sessionlog.Append(project, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: childRunID, WorkKind: string(agent.WorkSession), Intent: "queued member turn",
		TeamID: teamID, TeamMemberID: memberID, TeamTurnID: turnID, OriginRunID: oldLeadRunID, OriginCallID: turn.OriginCallID,
	}); err != nil {
		t.Fatal(err)
	}
	delegationID := mustSessionlogID(t)
	if _, err := sessionlog.Append(project, session.ID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: delegationID, RunID: childRunID, SessionID: session.ID, RunSeq: 1, At: time.Now().UTC(),
		Kind: string(agent.EventDelegation), Payload: sessionlog.AgentTaskDelegation{
			SessionID: session.ID, BatchID: "batch-recovered-message-backlog", TaskID: taskID,
			TaskName: member.Name, Status: "queued", UpdatedAt: time.Now().UTC(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := sessionlog.ReplayTeams(project, session.ID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Turns[turnID].Status != "queued" || queued.Members[memberID].Budget.AcceptedTurns != 1 || len(queued.Messages) != len(messages) {
		t.Fatalf("pre-restart fixture turn/member/messages = %+v / %+v / %+v", queued.Turns[turnID], queued.Members[memberID], queued.Messages)
	}

	childRunner := &recoveredResumeChildRunner{started: make(chan agent.ChildRunInput, 2)}
	pool := newRecoveredResumePool(t, childRunner)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socket := filepath.Join(root, "s")
	svc, err := conversation.Serve(ctx, recoveredResumeDeps(project, socket, db, parentRunner, pool, roles))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
		pool.Close()
	})
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	recovered, err := sessionlog.ReplayTeams(project, session.ID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Turns[turnID].Status != string(agent.DelegationInterrupted) || recovered.Members[memberID].Status != teams.MemberInterrupted || recovered.Members[memberID].Budget.AcceptedTurns != 1 || len(recovered.Messages) != len(messages) || len(recovered.Handoffs) != 0 {
		t.Fatalf("startup did not preserve interrupted backlog/budget: turn=%+v member=%+v messages=%+v handoffs=%+v", recovered.Turns[turnID], recovered.Members[memberID], recovered.Messages, recovered.Handoffs)
	}
	if childRunner.calls.Load() != 0 {
		t.Fatalf("service startup automatically invoked child runner %d times", childRunner.calls.Load())
	}

	newLeadRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	leadStream, err := conversation.OpenRun(requestCtx, socket, agent.ExecutionRequest{
		RunID: newLeadRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: session.ID}, Intent: "explicitly resume queued member",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leadStream.Close() })
	leadRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { leadRun.finish(agent.RunCompleted) })
	if started, err := leadStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("new lead run did not start: message=%+v err=%v", started, err)
	}
	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = session.ID, newLeadRunID
	_, resumeResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" resume "+memberID)
	resumed := acceptanceTeamResponse(t, resumeResult, "team_member_resume").TeamMember
	if resumed == nil || (resumed.Status != teams.MemberQueued && resumed.Status != teams.MemberRunning) {
		t.Fatalf("TUI resume result=%+v", resumed)
	}
	input := receiveRecoveredResumeChild(t, childRunner.started)
	if input.TeamTurn == nil || input.TeamTurn.MemberID != memberID || input.TeamTurn.TurnID == turnID {
		t.Fatalf("resume did not start one fresh turn: %+v", input.TeamTurn)
	}
	previousIndex := -1
	for _, message := range messages {
		position := strings.Index(input.Task.Instruction, message.Body)
		if position < 0 || position <= previousIndex || strings.Count(input.Task.Instruction, message.Body) != 1 {
			t.Fatalf("backlog message missing, reordered, or duplicated: body=%q instruction=%q", message.Body, input.Task.Instruction)
		}
		previousIndex = position
	}
	waitRecoveredResumeMemberIdle(t, project, session.ID, teamID, memberID)
	final, err := sessionlog.ReplayTeams(project, session.ID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if childRunner.calls.Load() != 1 || final.Members[memberID].Budget.AcceptedTurns != 2 || final.Turns[input.TeamTurn.TurnID].Status != string(agent.DelegationSucceeded) {
		t.Fatalf("explicit resume calls/budget/turn = %d/%+v/%+v", childRunner.calls.Load(), final.Members[memberID].Budget, final.Turns[input.TeamTurn.TurnID])
	}
	facts, err := sessionlog.TeamHistory(project, session.ID, teamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	handoffs := map[string][]string{}
	for _, event := range facts {
		var fact sessionlog.TeamEvent
		if err := decodeTUIEvent(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil {
			handoffs[fact.Handoff.MessageID] = append(handoffs[fact.Handoff.MessageID], fact.Handoff.DestinationTurnID)
		}
	}
	for _, message := range messages {
		if got := handoffs[message.ID]; len(got) != 1 || got[0] != input.TeamTurn.TurnID {
			t.Fatalf("message %s handoffs=%v, want one to resumed turn %s", message.ID, got, input.TeamTurn.TurnID)
		}
	}
}
