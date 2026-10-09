package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

// A persisted accepted queue entry is recovered by actual Serve startup, then
// explicitly resumed through the TUI socket path. Recovery must not invoke
// the newly configured runner or consume the pending message by itself.
func TestTeamTUIResumesRecoveredInterruptedMember(t *testing.T) {
	ctx := context.Background()
	tempParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tempParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tempParent, "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	initialRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	initialChildren := &recoveredResumeChildRunner{started: make(chan agent.ChildRunInput, 2)}
	initialPool := newRecoveredResumePool(t, initialChildren)
	firstSocket := filepath.Join(root, "first.sock")
	firstService, err := conversation.Serve(ctx, recoveredResumeDeps(project, firstSocket, db, initialRunner, initialPool, agentcatalog.New("", "")))
	if err != nil {
		t.Fatal(err)
	}
	firstClosed := false
	t.Cleanup(func() {
		if !firstClosed {
			_ = firstService.Close()
		}
		initialPool.Close()
	})

	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	created, err := conversation.Request(requestCtx, firstSocket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	oldLeadID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	oldLeadStream, err := conversation.OpenRun(requestCtx, firstSocket, agent.ExecutionRequest{
		RunID: oldLeadID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "prepare queued team recovery fixture", Messages: []llm.Message{{Role: "user", Content: "prepare team"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer oldLeadStream.Close()
	oldLead := receiveAcceptanceParentRun(t, initialRunner.started)
	oldLeadFinished := false
	t.Cleanup(func() {
		if !oldLeadFinished {
			oldLead.finish(agent.RunCompleted)
		}
	})
	if started, err := oldLeadStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("old lead run did not start: message=%+v err=%v", started, err)
	}
	model := New(firstSocket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, oldLeadID
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create recovered-resume")
	createdTeam := acceptanceTeamResponse(t, createResult, "team_create")
	if createdTeam.Team == nil {
		t.Fatalf("create team result=%+v", createdTeam)
	}
	team := *createdTeam.Team

	roles := agentcatalog.New("", "")
	role, ok := roles.Resolve("explore")
	if !ok {
		t.Fatal("built-in explore role is unavailable")
	}
	roleHash, err := recoveredResumeRoleHash(role)
	if err != nil {
		t.Fatal(err)
	}
	memberID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: memberID, TeamID: team.ID, Name: "reader", AgentName: role.Name,
		RoleHash: hex.EncodeToString(roleHash[:]), Model: "fixture-model", Tools: role.EffectiveTools(),
		Status: teams.MemberCreated, Revision: 1,
	}
	appendRecoveredResumeTeamFact(t, project, sessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: oldLeadID, Member: &member,
	})

	model, sendResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" send "+member.ID+" Check the queued recovery case.")
	sent := acceptanceTeamResponse(t, sendResult, "team_send")
	if sent.TeamMessage == nil || sent.TeamMessage.Body != "Check the queued recovery case." {
		t.Fatalf("team message result=%+v", sent)
	}
	turnID, childRunID, taskID := mustTeamIDs(t)
	turn := sessionlog.TurnFact{
		ID: turnID, MemberID: member.ID, RunID: childRunID, TaskID: taskID,
		OriginRunID: oldLeadID, OriginCallID: "spawn-queued-recovery", Status: "intent",
	}
	appendRecoveredResumeTeamFact(t, project, sessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: oldLeadID, Turn: &turn,
	})
	accepted := turn
	accepted.Status = "queued"
	appendRecoveredResumeTeamFact(t, project, sessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: oldLeadID, Turn: &accepted,
	})
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		TeamID: team.ID, TeamMemberID: member.ID, TeamTurnID: turn.ID, RunID: childRunID,
		WorkKind: string(agent.WorkSession), Intent: "team member turn", OriginRunID: oldLeadID, OriginCallID: turn.OriginCallID,
	}); err != nil {
		t.Fatal(err)
	}
	delegationID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
		ID: delegationID, RunID: childRunID, SessionID: sessionID, RunSeq: 1, At: time.Now().UTC(),
		Kind: string(agent.EventDelegation), Payload: sessionlog.AgentTaskDelegation{
			SessionID: sessionID, BatchID: "batch-queued-recovery", TaskID: taskID, TaskName: member.Name,
			Status: "queued", UpdatedAt: time.Now().UTC(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Turns[turn.ID].Status != "queued" || queued.Members[member.ID].Budget.AcceptedTurns != 1 || len(queued.Handoffs) != 0 {
		t.Fatalf("fixture is not queued with an undelivered message: turn=%+v member=%+v handoffs=%+v", queued.Turns[turn.ID], queued.Members[member.ID], queued.Handoffs)
	}

	oldLead.finish(agent.RunCompleted)
	oldLeadFinished = true
	if err := firstService.Close(); err != nil {
		t.Fatal(err)
	}
	firstClosed = true

	resumedRunner := &recoveredResumeChildRunner{started: make(chan agent.ChildRunInput, 2)}
	resumedPool := newRecoveredResumePool(t, resumedRunner)
	defer resumedPool.Close()
	newLeadRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	secondSocket := filepath.Join(root, "second.sock")
	secondService, err := conversation.Serve(ctx, recoveredResumeDeps(project, secondSocket, db, newLeadRunner, resumedPool, roles))
	if err != nil {
		t.Fatalf("restart conversation service: %v", err)
	}
	t.Cleanup(func() { _ = secondService.Close() })
	recovered, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Turns[turn.ID].Status != string(agent.DelegationInterrupted) || recovered.Members[member.ID].Status != teams.MemberInterrupted {
		t.Fatalf("startup did not recover queued turn as interrupted: turn=%+v member=%+v", recovered.Turns[turn.ID], recovered.Members[member.ID])
	}
	if resumedRunner.calls.Load() != 0 {
		t.Fatalf("service startup automatically invoked child runner %d times", resumedRunner.calls.Load())
	}
	if len(recovered.Messages) != 1 || recovered.Messages[sent.TeamMessage.ID].ID == "" || len(recovered.Handoffs) != 0 {
		t.Fatalf("startup consumed or lost pending message: messages=%+v handoffs=%+v", recovered.Messages, recovered.Handoffs)
	}

	newLeadID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	newLeadStream, err := conversation.OpenRun(requestCtx, secondSocket, agent.ExecutionRequest{
		RunID: newLeadID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "explicitly resume recovered team member", Messages: []llm.Message{{Role: "user", Content: "resume team"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer newLeadStream.Close()
	newLead := receiveAcceptanceParentRun(t, newLeadRunner.started)
	newLeadFinished := false
	t.Cleanup(func() {
		if !newLeadFinished {
			newLead.finish(agent.RunCompleted)
		}
	})
	if started, err := newLeadStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("new lead run did not start: message=%+v err=%v", started, err)
	}
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	model = New(secondSocket, project)
	model.ActiveSession, model.ActiveRunID, model.Events = sessionID, newLeadID, transcript.Events
	model.Composer.SetValue("/team " + team.ID + " members")
	updated, cmd := model.submitComposer()
	if cmd != nil {
		t.Fatal("local TUI members command unexpectedly returned an asynchronous command")
	}
	var modelOK bool
	model, modelOK = updated.(Model)
	if !modelOK {
		t.Fatalf("members command returned %T, want Model", updated)
	}
	if len(model.TeamMembers) != 1 || model.TeamMembers[0].ID != member.ID || model.TeamMembers[0].Status != teams.MemberInterrupted || !strings.Contains(model.Status, "1") {
		t.Fatalf("TUI did not display recovered interrupted member: status=%q members=%+v", model.Status, model.TeamMembers)
	}

	model, resumeResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" resume "+member.ID)
	resumed := acceptanceTeamResponse(t, resumeResult, "team_member_resume")
	if resumed.TeamMember == nil || resumed.TeamMember.Status != teams.MemberQueued {
		t.Fatalf("TUI resume response=%+v, want queued member", resumed)
	}
	input := receiveRecoveredResumeChild(t, resumedRunner.started)
	if input.TeamTurn == nil || input.TeamTurn.TurnID == turn.ID || strings.Count(input.Task.Instruction, sent.TeamMessage.Body) != 1 {
		t.Fatalf("explicit resume did not create one new turn carrying the pending message once: %+v", input)
	}
	waitRecoveredResumeMemberIdle(t, project, sessionID, team.ID, member.ID)
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumedRunner.calls.Load() != 1 || projection.Members[member.ID].Budget.AcceptedTurns != 2 || projection.Turns[turn.ID].Status != string(agent.DelegationInterrupted) || projection.Turns[input.TeamTurn.TurnID].Status != string(agent.DelegationSucceeded) {
		t.Fatalf("resume runner/budget/turns=%d/%+v/%+v", resumedRunner.calls.Load(), projection.Members[member.ID].Budget, projection.Turns)
	}
	facts, err := sessionlog.TeamHistory(project, sessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	handoffs := 0
	for _, event := range facts {
		encoded, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		var fact sessionlog.TeamEvent
		if err := json.Unmarshal(encoded, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil && fact.Handoff.MessageID == sent.TeamMessage.ID {
			handoffs++
			if fact.Handoff.DestinationTurnID != input.TeamTurn.TurnID {
				t.Fatalf("pending message handoff targets turn %s, want %s", fact.Handoff.DestinationTurnID, input.TeamTurn.TurnID)
			}
		}
	}
	if handoffs != 1 {
		t.Fatalf("pending message has %d durable handoffs, want one", handoffs)
	}
	newLead.finish(agent.RunCompleted)
	newLeadFinished = true
}

type recoveredResumeChildRunner struct {
	started chan agent.ChildRunInput
	calls   atomic.Int32
}

func (r *recoveredResumeChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.calls.Add(1)
	r.started <- input
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "recovered turn completed"}
}

func newRecoveredResumePool(t *testing.T, runner agent.ChildRunner) *agent.PoolDelegator {
	t.Helper()
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func recoveredResumeDeps(project, socket string, db *store.Store, parent agent.Runner, pool agent.Delegator, roles agentcatalog.Catalog) conversation.Deps {
	return conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parent, Delegator: pool, Agents: roles, ForkProvider: acceptanceTeamProvider{},
		ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
	}
}

func appendRecoveredResumeTeamFact(t *testing.T, root, sessionID, teamID string, fact sessionlog.TeamEvent) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	team, ok := projection.Teams[teamID]
	if !ok {
		t.Fatalf("team %s not found", teamID)
	}
	fact.ID, fact.TeamID, fact.SessionID = mustSessionlogID(t), teamID, sessionID
	fact.Revision = team.Revision + 1
	if _, err := sessionlog.Append(root, sessionID, sessionlog.EventTeam, fact); err != nil {
		t.Fatal(err)
	}
}

func mustTeamIDs(t *testing.T) (string, string, string) {
	t.Helper()
	turnID, childRunID, taskID := mustSessionlogID(t), mustSessionlogID(t), mustSessionlogID(t)
	return turnID, childRunID, taskID
}

func mustSessionlogID(t *testing.T) string {
	t.Helper()
	id, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func recoveredResumeRoleHash(definition agentcatalog.Definition) ([32]byte, error) {
	encoded, err := json.Marshal(struct {
		Name        string   `json:"name"`
		Instruction string   `json:"instruction"`
		Model       string   `json:"model"`
		Tools       []string `json:"tools"`
		MaxTurns    int      `json:"max_turns"`
		Isolation   string   `json:"isolation"`
	}{definition.Name, definition.Instruction, definition.Model, definition.EffectiveTools(), definition.MaxTurns, definition.Isolation})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func receiveRecoveredResumeChild(t *testing.T, started <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-started:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("resumed child runner did not start")
		return agent.ChildRunInput{}
	}
}

func waitRecoveredResumeMemberIdle(t *testing.T, root, sessionID, teamID, memberID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Members[memberID].Status == teams.MemberIdle {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("resumed member status=%s, want idle", projection.Members[memberID].Status)
}
