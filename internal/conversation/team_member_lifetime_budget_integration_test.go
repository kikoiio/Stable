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

func TestTeamMemberLifetimeElapsedBudgetBlocksResumeAtLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "lifetime-budget-parent")
	permissionBounds, err := json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	request.PermissionBounds = permissionBounds
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

	team, err := service.CreateTeam(t.Context(), request, "lifetime-budget")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: "member-lifetime-budget", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: "lifetime-budget-role", Model: "fixture", Tools: []string{"read_file"},
		Status: teams.MemberCreated, Revision: 1,
	}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
		Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}

	// Four individually valid turns accumulate exactly the ten-minute lifetime
	// limit: three turns at the per-turn maximum, then one final minute.
	for index, elapsed := range []time.Duration{3 * time.Minute, 3 * time.Minute, 3 * time.Minute, time.Minute} {
		turnID := "turn-lifetime-budget-" + string(rune('1'+index))
		childRunID := "child-lifetime-budget-" + string(rune('1'+index))
		taskID := "task-lifetime-budget-" + string(rune('1'+index))
		callID := "call-lifetime-budget-" + string(rune('1'+index))
		turn := sessionlog.TurnFact{
			ID: turnID, MemberID: member.ID, RunID: childRunID, TaskID: taskID,
			OriginRunID: request.RunID, OriginCallID: callID, Status: "intent",
		}
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
			Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn,
		}); err != nil {
			t.Fatalf("append turn %d intent: %v", index+1, err)
		}
		accepted := turn
		accepted.Status = "queued"
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
			Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted,
		}); err != nil {
			t.Fatalf("append turn %d acceptance: %v", index+1, err)
		}
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: childRunID, WorkKind: string(request.Work.Kind), Intent: "historical lifetime budget fixture",
			TeamID: team.ID, TeamMemberID: member.ID, TeamTurnID: turnID,
			OriginRunID: request.RunID, OriginCallID: callID,
		}); err != nil {
			t.Fatalf("append turn %d run start: %v", index+1, err)
		}
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
			ID: "delegation-terminal-" + childRunID, RunID: childRunID, SessionID: request.Work.SessionID,
			RunSeq: 1, At: time.Now().UTC(), Kind: "delegation_event",
			Payload: sessionlog.AgentTaskDelegation{
				SessionID: request.Work.SessionID, BatchID: "batch-" + childRunID, TaskID: taskID,
				TaskName: member.Name, Status: "succeeded", UpdatedAt: time.Now().UTC(),
			},
		}); err != nil {
			t.Fatalf("append turn %d child terminal: %v", index+1, err)
		}
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
			ID: "run-terminal-" + childRunID, RunID: childRunID, SessionID: request.Work.SessionID,
			RunSeq: 2, At: time.Now().UTC(), Kind: string(agent.EventTerminal),
			Payload: map[string]string{"status": string(agent.RunCompleted)},
		}); err != nil {
			t.Fatalf("append turn %d run outcome: %v", index+1, err)
		}
		terminal := accepted
		terminal.Status = string(agent.DelegationSucceeded)
		terminal.Elapsed = elapsed
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
			Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: request.RunID, Turn: &terminal,
		}); err != nil {
			t.Fatalf("append turn %d terminal: %v", index+1, err)
		}
		projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		member = projection.Members[member.ID]
		member.Status = teams.MemberInterrupted
		member.Revision++
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{
			Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &member,
		}); err != nil {
			t.Fatalf("persist turn %d resumable state: %v", index+1, err)
		}
	}

	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member = projection.Members[member.ID]
	if member.Status != teams.MemberInterrupted || member.Budget.AcceptedTurns != 4 || member.Budget.Elapsed != teams.MaxMemberDuration {
		t.Fatalf("replayed lifetime budget=%+v status=%s, want 4 turns and exactly %s interrupted", member.Budget, member.Status, teams.MaxMemberDuration)
	}
	before, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-at-lifetime-limit"); !errors.Is(err, teams.ErrBudgetExhausted) {
		t.Fatalf("resume at exact cumulative duration limit=%v, want budget exhausted", err)
	}
	after, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events) {
		t.Fatalf("budget-rejected resume appended facts/events: before=%d after=%d", len(before.Events), len(after.Events))
	}
	var intentCount, acceptedCount int
	for _, event := range after.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		switch fact.Kind {
		case sessionlog.TeamTurnIntent:
			intentCount++
		case sessionlog.TeamTurnAccepted:
			acceptedCount++
		}
	}
	if intentCount != 4 || acceptedCount != 4 || runner.count() != 0 {
		t.Fatalf("budget-rejected resume changed admission or ran child: intents=%d accepted=%d childRuns=%d", intentCount, acceptedCount, runner.count())
	}
}
