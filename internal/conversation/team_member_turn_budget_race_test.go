package conversation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestConcurrentMemberResumesCannotExceedLastAcceptedTurnBudget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "turn-budget-race-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	request.ProviderName, request.Model = "fixture", "fixture-model"
	role := agentcatalog.Definition{
		Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 1,
	}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 2), release: make(chan struct{}, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		for range 2 {
			select {
			case runner.release <- struct{}{}:
			default:
			}
		}
		service.teamScheduler.close()
		pool.Close()
	})
	team, err := service.CreateTeam(t.Context(), request, "turn-budget-race")
	if err != nil {
		t.Fatal(err)
	}
	roleFingerprint := teamRoleHash(role)
	member := teams.Member{
		ID: "member-turn-budget-race", TeamID: team.ID, Name: "reader", AgentName: "explore",
		RoleHash: hex.EncodeToString(roleFingerprint[:]), Model: "fixture-model", Tools: []string{"read_file"},
		Status: teams.MemberCreated, Revision: 1,
	}
	appendFact := func(event sessionlog.TeamEvent) {
		t.Helper()
		if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	appendFact(sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member})
	// Seed a valid durable history one turn below the lifetime count cap.
	// Each old turn has a complete accepted→terminal history and never invokes
	// the current pool/provider.
	for index := 0; index < teams.MaxMemberTurns-1; index++ {
		turnID := fmt.Sprintf("turn-budget-race-history-%02d", index)
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
			t.Fatalf("append historical turn %d run start: %v", index+1, err)
		}
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
			ID: "delegation-terminal-" + turn.RunID, RunID: turn.RunID, SessionID: request.Work.SessionID,
			RunSeq: 1, At: time.Now().UTC(), Kind: "delegation_event",
			Payload: sessionlog.AgentTaskDelegation{
				SessionID: request.Work.SessionID, BatchID: "batch-" + turn.RunID,
				TaskID: turn.TaskID, TaskName: member.Name, Status: "succeeded", UpdatedAt: time.Now().UTC(),
			},
		}); err != nil {
			t.Fatalf("append historical turn %d delegation terminal: %v", index+1, err)
		}
		if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{
			ID: "run-terminal-" + turn.RunID, RunID: turn.RunID, SessionID: request.Work.SessionID,
			RunSeq: 2, At: time.Now().UTC(), Kind: string(agent.EventTerminal),
			Payload: map[string]string{"status": string(agent.RunCompleted)},
		}); err != nil {
			t.Fatalf("append historical turn %d run terminal: %v", index+1, err)
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
	if got := projection.Members[member.ID].Budget.AcceptedTurns; got != teams.MaxMemberTurns-1 {
		t.Fatalf("fixture accepted turns=%d, want %d", got, teams.MaxMemberTurns-1)
	}
	before, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	type resumeResult struct {
		member teams.Member
		err    error
	}
	results := make(chan resumeResult, 2)
	var wg sync.WaitGroup
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			member, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, fmt.Sprintf("concurrent-resume-%d", index))
			results <- resumeResult{member: member, err: err}
		}(index)
	}
	for range 2 {
		<-ready
	}
	close(start)
	wg.Wait()
	close(results)
	successfulResponses := 0
	var resumeErrors []error
	for result := range results {
		if result.err == nil {
			successfulResponses++
		} else {
			resumeErrors = append(resumeErrors, result.err)
		}
	}
	if successfulResponses == 0 {
		t.Fatalf("concurrent resume calls had no successful response; errors=%v", resumeErrors)
	}
	input := receiveTeamChildInput(t, runner.inputs)
	if input.TeamTurn == nil || input.TeamTurn.TeamID != team.ID || input.TeamTurn.MemberID != member.ID {
		t.Fatalf("accepted final-budget resume launched wrong child: %+v", input.TeamTurn)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberBudgetExhausted)
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members[member.ID].Budget.AcceptedTurns; got != teams.MaxMemberTurns {
		t.Fatalf("concurrent resumes left accepted-turn budget=%d, want exactly %d", got, teams.MaxMemberTurns)
	}
	after, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
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
	runner.mu.Lock()
	childRuns := runner.started
	runner.mu.Unlock()
	if intents != teams.MaxMemberTurns || accepted != teams.MaxMemberTurns || childRuns != 1 {
		t.Fatalf("last-slot race exceeded quota or duplicated work: intents=%d accepted=%d child runs=%d", intents, accepted, childRuns)
	}
	if len(after.Events) <= len(before.Events) {
		t.Fatal("one authorized final-slot resume left no durable admission facts")
	}
}
