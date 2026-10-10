package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

type recoveryMustNotRunTeamChild struct{ started chan agent.ChildRunInput }

func (r *recoveryMustNotRunTeamChild) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- input
	return agent.ChildRunResult{Status: agent.DelegationSucceeded}
}

// A process restart while a force-stop is waiting on a running child must
// treat that child as interrupted, preserve the typed stop decision, and wait
// for an explicit resume. Startup recovery must not replay the model turn.
func TestStoppingTeamTurnRecoveryAfterRestartIsExplicitAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	original, request := teamServiceFixture(t, root, "stopping-recovery-parent")
	team, err := original.CreateTeam(t.Context(), request, "stopping-recovery")
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: "member-stopping-recovery", TeamID: team.ID, Name: "reader", AgentName: "explore", RoleHash: "role-hash", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	turn := sessionlog.TurnFact{ID: "turn-stopping-recovery", MemberID: member.ID, RunID: "child-stopping-recovery", TaskID: "task-stopping-recovery", OriginRunID: request.RunID, OriginCallID: "spawn-stopping-recovery", Status: "intent"}
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
		t.Fatal(err)
	}
	accepted := turn
	accepted.Status = "queued"
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	member = projection.Members[member.ID]
	member.Status, member.TurnID, member.RunID = teams.MemberQueued, turn.ID, turn.RunID
	member.Revision++
	if err := appendTeamFactLocked(root, request.Work.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: request.RunID, Member: &member}); err != nil {
		t.Fatal(err)
	}
	child := agent.ChildRunInput{
		TeamTurn:    &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turn.ID, MemberName: member.Name},
		ParentRunID: request.RunID, BatchID: "batch-stopping-recovery", ChildRunID: turn.RunID,
		Task: agent.DelegationTask{ID: turn.TaskID, Name: member.Name, Instruction: "Inspect the running turn."}, Work: request.Work,
	}
	scope := teams.Scope{SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind), ProjectRoot: root}
	seq := uint64(0)
	if err := original.persistTeamChildQueuedLocked(root, scope, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}
	if err := original.persistTeamChildStart(root, scope, member.ID, turn.ID, request.RunID, turn.OriginCallID, child, &seq); err != nil {
		t.Fatal(err)
	}

	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	team = projection.Teams[team.ID]
	shutdown, err := original.createTeamRequest(root, team, request.RunID, teams.Lead, member.ID, teams.RequestShutdown, "")
	if err != nil {
		t.Fatal(err)
	}
	team.Revision++
	shutdown.Status = teams.RequestApproved
	shutdown.Revision++
	if err := original.appendTeamRequest(root, team, request.RunID, "service", sessionlog.TeamRequestResponded, shutdown); err != nil {
		t.Fatal(err)
	}
	team.Revision++
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	team = projection.Teams[team.ID]
	member = projection.Members[member.ID]
	member.Status = teams.MemberStopping
	member.Revision++
	if err := original.appendTeamMemberState(root, team, request.RunID, "service", member); err != nil {
		t.Fatal(err)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[member.ID].Status != teams.MemberStopping || projection.Requests[shutdown.ID].Status != teams.RequestApproved {
		t.Fatalf("fixture did not persist force-stop-in-progress: member=%s request=%s", projection.Members[member.ID].Status, projection.Requests[shutdown.ID].Status)
	}

	childRunner := &recoveryMustNotRunTeamChild{started: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	socketDir, err := os.MkdirTemp("/tmp", "m09-tr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	serverCtx, cancel := context.WithCancel(context.Background())
	service, err := Serve(serverCtx, Deps{ProjectRoot: root, SocketPath: filepath.Join(socketDir, "s"), Delegator: pool})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := service.Close(); err != nil {
			t.Errorf("close restarted service: %v", err)
		}
	})

	assertRecoveredStoppingState(t, root, request.Work.SessionID, team.ID, member.ID, turn.ID, shutdown.ID)
	select {
	case input := <-childRunner.started:
		t.Fatalf("startup recovery reran the stopped child turn: %+v", input)
	default:
	}

	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	terminalCount := countTeamTurnTerminals(t, transcript.Events, turn.ID)
	if terminalCount != 1 {
		t.Fatalf("recovered turn terminal facts=%d, want exactly one", terminalCount)
	}
	eventCount := len(transcript.Events)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatal(err)
	}
	transcript, err = sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount || countTeamTurnTerminals(t, transcript.Events, turn.ID) != 1 {
		t.Fatalf("second recovery changed the journal: events %d -> %d", eventCount, len(transcript.Events))
	}
	assertRecoveredStoppingState(t, root, request.Work.SessionID, team.ID, member.ID, turn.ID, shutdown.ID)
	select {
	case input := <-childRunner.started:
		t.Fatalf("second recovery reran the stopped child turn: %+v", input)
	default:
	}
}

func assertRecoveredStoppingState(t *testing.T, root, sessionID, teamID, memberID, turnID, requestID string) {
	t.Helper()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	member := projection.Members[memberID]
	turn := projection.Turns[turnID]
	shutdown := projection.Requests[requestID]
	if member.Status != teams.MemberInterrupted || member.TurnID != turnID {
		t.Fatalf("recovered stopping member=%+v, want interrupted with original turn", member)
	}
	if turn.Status != string(agent.DelegationInterrupted) {
		t.Fatalf("recovered stopping turn=%+v, want interrupted", turn)
	}
	if shutdown.Status != teams.RequestApproved || shutdown.Type != teams.RequestShutdown || shutdown.MemberID != memberID {
		t.Fatalf("typed force-stop decision changed during recovery: %+v", shutdown)
	}
}

func countTeamTurnTerminals(t *testing.T, events []sessionlog.Event, turnID string) int {
	t.Helper()
	count := 0
	for _, event := range events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamTurnTerminal && fact.Turn != nil && fact.Turn.ID == turnID {
			count++
		}
	}
	return count
}
