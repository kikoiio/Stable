package conversation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

type heldSecondTeamChildRunner struct {
	calls   atomic.Int32
	started chan agent.ChildRunInput
	release chan struct{}
}

func (r *heldSecondTeamChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	call := r.calls.Add(1)
	r.started <- input
	if call == 2 {
		<-r.release
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "fixture completed"}
}

func TestAcceptedTeamMemberCannotResumeIdlePeer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "member-resume-peer-lead")
	leadRequest.Model = "fixture-model"
	leadRequest.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: leadRequest.RunID, SessionID: leadRequest.Work.SessionID, AllowedRoot: root,
	})
	team, err := service.CreateTeam(context.Background(), leadRequest, "member-resume-peer-authorization")
	if err != nil {
		t.Fatal(err)
	}

	runner := &heldSecondTeamChildRunner{started: make(chan agent.ChildRunInput, 4), release: make(chan struct{}, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, runner, nil)
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
		select {
		case runner.release <- struct{}{}:
		default:
		}
		service.teamScheduler.close()
		pool.Close()
	})

	peer, err := service.SpawnTeamMember(context.Background(), leadRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "idle-peer", AgentName: "explore", Instruction: "complete before the actor starts", OriginCallID: "spawn-peer",
	})
	if err != nil {
		t.Fatalf("spawn peer: %v", err)
	}
	peerInput := receiveTeamMemberResumeStart(t, runner.started)
	if peerInput.TeamTurn == nil || peerInput.TeamTurn.MemberID != peer.ID {
		t.Fatalf("first child=%+v, want idle peer %s", peerInput.TeamTurn, peer.ID)
	}
	waitForTeamMemberStatus(t, root, leadRequest.Work.SessionID, team.ID, peer.ID, teams.MemberIdle)

	actor, err := service.SpawnTeamMember(context.Background(), leadRequest, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "active-actor", AgentName: "explore", Instruction: "attempt to resume a peer", OriginCallID: "spawn-actor",
	})
	if err != nil {
		t.Fatalf("spawn actor: %v", err)
	}
	actorInput := receiveTeamMemberResumeStart(t, runner.started)
	if actorInput.TeamTurn == nil || actorInput.TeamTurn.MemberID != actor.ID {
		t.Fatalf("second child=%+v, want active actor %s", actorInput.TeamTurn, actor.ID)
	}

	actorRequest := leadRequest
	actorRequest.RunID = actorInput.ChildRunID
	actorRequest.TeamTurn = actorInput.TeamTurn
	before, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeActor := before.Members[actor.ID]
	idlePeer := before.Members[peer.ID]
	activeTurn := before.Turns[actorInput.TeamTurn.TurnID]
	active, hasAcceptedTurn := acceptedTeamTurnForMember(before, actor.ID)
	runStart, hasRunStart, runStartErr := sessionlog.FindRunStart(root, leadRequest.Work.SessionID, actorInput.ChildRunID)
	if runStartErr != nil {
		t.Fatal(runStartErr)
	}
	if activeActor.Status != teams.MemberRunning || activeActor.TurnID != actorInput.TeamTurn.TurnID || !hasAcceptedTurn || active.ID != actorInput.TeamTurn.TurnID || activeTurn.Status != string(agent.DelegationQueued) || !hasRunStart || runStart.TeamID != team.ID || runStart.TeamMemberID != actor.ID || runStart.TeamTurnID != actorInput.TeamTurn.TurnID {
		t.Fatalf("actor does not have a persisted accepted running turn: member=%+v turn=%+v", activeActor, activeTurn)
	}
	if idlePeer.Status != teams.MemberIdle {
		t.Fatalf("resume target status=%s, want idle", idlePeer.Status)
	}
	transcript, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	teamHistory, err := sessionlog.TeamHistory(root, leadRequest.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	service.teamScheduler.mu.Lock()
	readyBefore := append([]string(nil), service.teamScheduler.ready...)
	waitingBefore := len(service.teamScheduler.waiting)
	activeBefore := len(service.teamScheduler.active)
	service.teamScheduler.mu.Unlock()
	providerCallsBefore := runner.calls.Load()

	arguments, err := json.Marshal(map[string]string{"team_id": team.ID, "member_id": peer.ID})
	if err != nil {
		t.Fatal(err)
	}
	outcome, callErr := service.ExecuteTeamTool(context.Background(), actorRequest, llm.ToolUse{
		ID: "member-resume-peer", Name: "team_member_resume", Arguments: arguments,
	})
	if callErr != nil || outcome.Status != agent.ToolDenied || !outcome.IsError {
		t.Fatalf("accepted member resume peer outcome=%+v err=%v, want denial", outcome, callErr)
	}

	after, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("denied peer resume changed replay projection: before=%+v after=%+v", before, after)
	}
	transcriptAfter, err := sessionlog.Replay(root, leadRequest.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcriptAfter.Events) != len(transcript.Events) {
		t.Fatalf("denied peer resume appended session facts: %d -> %d", len(transcript.Events), len(transcriptAfter.Events))
	}
	teamHistoryAfter, err := sessionlog.TeamHistory(root, leadRequest.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(teamHistoryAfter) != len(teamHistory) {
		t.Fatalf("denied peer resume appended team history: %d -> %d", len(teamHistory), len(teamHistoryAfter))
	}
	service.teamScheduler.mu.Lock()
	readyAfter := append([]string(nil), service.teamScheduler.ready...)
	waitingAfter := len(service.teamScheduler.waiting)
	activeAfter := len(service.teamScheduler.active)
	service.teamScheduler.mu.Unlock()
	if !reflect.DeepEqual(readyAfter, readyBefore) || waitingAfter != waitingBefore || activeAfter != activeBefore {
		t.Fatalf("denied peer resume changed scheduler queue/active state: ready %v -> %v, waiting %d -> %d, active %d -> %d", readyBefore, readyAfter, waitingBefore, waitingAfter, activeBefore, activeAfter)
	}
	if got := runner.calls.Load(); got != providerCallsBefore || got != 2 {
		t.Fatalf("denied peer resume invoked another child/provider: before=%d after=%d", providerCallsBefore, got)
	}
	select {
	case unexpected := <-runner.started:
		t.Fatalf("denied peer resume started a child: %+v", unexpected)
	default:
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, leadRequest.Work.SessionID, team.ID, actor.ID, teams.MemberIdle)
}

func receiveTeamMemberResumeStart(t *testing.T, starts <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-starts:
		return input
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an accepted team member run")
		return agent.ChildRunInput{}
	}
}
