package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMemberAcceptedTurnBudgetBlocksResumeAtLimitWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "turn-count-budget-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	runner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Delegator = pool
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "turn-count-budget")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-turn-count-budget", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "turn-count-budget-role", Model: "fixture", Tools: []string{"read_file"},
		Status: teams.MemberCreated, Revision: 1,
	}
	appendFact := func(event sessionlog.TeamEvent) {
		t.Helper()
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member})
	for index := 0; index < teams.MaxMemberTurns; index++ {
		turnID := "turn-count-budget-" + string(rune('a'+index))
		turn := sessionlog.TurnFact{
			ID: turnID, MemberID: member.ID, RunID: "child-" + turnID, TaskID: "task-" + turnID,
			OriginRunID: request.RunID, OriginCallID: "call-" + turnID, Status: "intent",
		}
		appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn})
		accepted := turn
		accepted.Status = "queued"
		appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted})
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: turn.RunID, WorkKind: string(request.Work.Kind), Intent: "historical accepted-turn budget fixture",
			TeamID: team.ID, TeamMemberID: member.ID, TeamTurnID: turn.ID,
			OriginRunID: request.RunID, OriginCallID: turn.OriginCallID,
		}); err != nil {
			t.Fatalf("append turn %d run start: %v", index+1, err)
		}
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
			ID: "delegation-terminal-" + turn.RunID, RunID: turn.RunID, SessionID: request.Work.SessionID,
			RunSeq: 1, At: time.Now().UTC(), Kind: "delegation_event",
			Payload: sessionlog.AgentTaskDelegation{
				SessionID: request.Work.SessionID, BatchID: "batch-" + turn.RunID, TaskID: turn.TaskID,
				TaskName: member.Name, Status: "succeeded", UpdatedAt: time.Now().UTC(),
			},
		}); err != nil {
			t.Fatalf("append turn %d child terminal: %v", index+1, err)
		}
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
			ID: "run-terminal-" + turn.RunID, RunID: turn.RunID, SessionID: request.Work.SessionID,
			RunSeq: 2, At: time.Now().UTC(), Kind: string(agent.EventTerminal),
			Payload: map[string]string{"status": string(agent.RunCompleted)},
		}); err != nil {
			t.Fatalf("append turn %d run outcome: %v", index+1, err)
		}
		terminal := accepted
		terminal.Status = string(agent.DelegationSucceeded)
		appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: request.RunID, Turn: &terminal})
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member = projection.Members[member.ID]
	member.Status = teams.MemberInterrupted
	member.Revision++
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &member})
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member = projection.Members[member.ID]
	if member.Status != teams.MemberInterrupted || member.Budget.AcceptedTurns != teams.MaxMemberTurns {
		t.Fatalf("replayed member budget=%+v status=%s, want exactly %d accepted turns and interrupted", member.Budget, member.Status, teams.MaxMemberTurns)
	}
	before, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-at-turn-count-limit"); !errors.Is(err, teams.ErrBudgetExhausted) {
		t.Fatalf("resume at accepted-turn limit=%v, want budget exhausted", err)
	}
	after, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events) {
		t.Fatalf("budget-rejected resume appended session facts: before=%d after=%d", len(before.Events), len(after.Events))
	}
	var intents, accepted int
	for _, event := range after.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnIntent {
			intents++
		}
		if fact.Kind == sessionlog.TeamTurnAccepted {
			accepted++
		}
	}
	if intents != teams.MaxMemberTurns || accepted != teams.MaxMemberTurns || runner.count() != 0 {
		t.Fatalf("budget-rejected resume changed admission or called provider: intents=%d accepted=%d childRuns=%d", intents, accepted, runner.count())
	}
}
