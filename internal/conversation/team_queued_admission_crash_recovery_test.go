package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

const teamQueuedAdmissionCrashChildEnv = "STABLE_M09_TEAM_QUEUED_ADMISSION_CRASH_CHILD"

type teamQueuedAdmissionCrashMarker struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	TeamID    string `json:"team_id"`
	MemberID  string `json:"member_id"`
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Message   string `json:"message"`
}

type teamQueuedAdmissionCrashRunner struct {
	blockerStarted chan struct{}
	providerMarker string
}

func (r *teamQueuedAdmissionCrashRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	if input.Task.ID == "hold-pool-worker" {
		close(r.blockerStarted)
		<-ctx.Done()
		return agent.ChildRunResult{Status: agent.DelegationCanceled, Error: ctx.Err().Error()}
	}
	if input.TeamTurn != nil {
		_ = os.WriteFile(r.providerMarker, []byte(input.TeamTurn.TurnID), 0600)
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "unexpected pre-crash provider start"}
}

// A real PoolDelegator worker is held by unrelated work while a team child is
// committed into its queue. The helper is killed only after the durable team
// turn and pending message can be replayed, proving the callback-to-worker gap.
func TestTeamQueuedPoolAdmissionCrashRecoversOnlyOnExplicitResume(t *testing.T) {
	if os.Getenv(teamQueuedAdmissionCrashChildEnv) == "1" {
		runTeamQueuedAdmissionCrashChild(t)
		return
	}

	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(base, "queued-admission.json")
	providerMarker := filepath.Join(base, "provider-started")
	child := exec.Command(os.Args[0], "-test.run=^TestTeamQueuedPoolAdmissionCrashRecoversOnlyOnExplicitResume$", "-test.v")
	child.Env = append(os.Environ(),
		teamQueuedAdmissionCrashChildEnv+"=1",
		"STABLE_M09_TEAM_QUEUED_ROOT="+root,
		"STABLE_M09_TEAM_QUEUED_MARKER="+markerPath,
		"STABLE_M09_TEAM_PROVIDER_MARKER="+providerMarker,
	)
	child.Stdout, child.Stderr = os.Stderr, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	childWaited := false
	t.Cleanup(func() {
		if !childWaited && child.Process != nil {
			_ = child.Process.Kill()
			<-childDone
			childWaited = true
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	var marker teamQueuedAdmissionCrashMarker
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(markerPath); err == nil {
			if err := json.Unmarshal(data, &marker); err != nil {
				t.Fatalf("decode queued marker: %v", err)
			}
			break
		}
		select {
		case childErr := <-childDone:
			childWaited = true
			t.Fatalf("admission helper exited before queued barrier: %v", childErr)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if marker.SessionID == "" || marker.TeamID == "" || marker.MemberID == "" || marker.TurnID == "" || marker.MessageID == "" {
		t.Fatal("helper did not publish the committed queued turn and message identity")
	}
	if _, err := os.Stat(providerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child provider started before process kill: %v", err)
	}
	queuedProjection, err := sessionlog.ReplayTeams(root, marker.SessionID, marker.TeamID)
	if err != nil {
		t.Fatalf("replay queued state before kill: %v", err)
	}
	queuedMember := queuedProjection.Members[marker.MemberID]
	queuedTurn := queuedProjection.Turns[marker.TurnID]
	if queuedMember.Status != teams.MemberQueued || queuedMember.TurnID != marker.TurnID || queuedMember.Budget.AcceptedTurns != 1 || queuedTurn.Status != string(agent.DelegationQueued) {
		t.Fatalf("pre-kill durable admission is not queued: member=%+v turn=%+v", queuedMember, queuedTurn)
	}
	if len(queuedProjection.Messages) != 1 || queuedProjection.Messages[marker.MessageID].ID == "" || len(queuedProjection.Handoffs) != 0 {
		t.Fatalf("pre-kill pending message was not durable and unhanded: messages=%+v handoffs=%+v", queuedProjection.Messages, queuedProjection.Handoffs)
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill helper at the committed admission barrier: %v", err)
	}
	childErr := <-childDone
	childWaited = true
	var exitErr *exec.ExitError
	if !errors.As(childErr, &exitErr) {
		t.Fatalf("helper exit=%v, want SIGKILL", childErr)
	}
	if _, err := os.Stat(providerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queued child provider started before crash recovery: %v", err)
	}

	// Serve performs the same startup recovery used by production. A second
	// recovery pass must append no new terminal facts or rerun the provider.
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	restartedRunner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, restartedRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are capped at roughly 108 bytes. A short relative path
	// keeps this socket within the limit despite the long test fixture root.
	if err := os.MkdirAll(".tmp", 0700); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	socketDir, err := os.MkdirTemp(".tmp", "m09ac6-")
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	restarted, err := Serve(context.Background(), Deps{
		ProjectRoot: root, SocketPath: filepath.Join(socketDir, "conversation.sock"), PollEvery: time.Hour,
		Agents: fixedTeamRoleCatalog{definition: role}, Delegator: pool,
		ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
		ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "fixture",
	})
	if err != nil {
		pool.Close()
		t.Fatalf("restart conversation service: %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Errorf("close restarted service: %v", err)
		}
		pool.Close()
	})
	projection, err := sessionlog.ReplayTeams(root, marker.SessionID, marker.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := projection.Members[marker.MemberID]
	if interrupted.Status != teams.MemberInterrupted || interrupted.TurnID != marker.TurnID || interrupted.Budget.AcceptedTurns != 1 || projection.Turns[marker.TurnID].Status != string(agent.DelegationInterrupted) {
		t.Fatalf("startup recovery did not interrupt exact queued admission while retaining budget: member=%+v turn=%+v", interrupted, projection.Turns[marker.TurnID])
	}
	if len(projection.Messages) != 1 || projection.Messages[marker.MessageID].ID == "" || len(projection.Handoffs) != 0 || restartedRunner.count() != 0 {
		t.Fatalf("startup recovery consumed pending message or started provider: messages=%+v handoffs=%+v calls=%d", projection.Messages, projection.Handoffs, restartedRunner.count())
	}
	transcript, err := sessionlog.Replay(root, marker.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	eventCount := len(transcript.Events)
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("second startup recovery: %v", err)
	}
	transcript, err = sessionlog.Replay(root, marker.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != eventCount || restartedRunner.count() != 0 {
		t.Fatalf("second recovery changed facts or started provider: events %d -> %d, calls=%d", eventCount, len(transcript.Events), restartedRunner.count())
	}

	resumeRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(root, marker.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: resumeRunID, WorkKind: string(agent.WorkSession), Intent: "explicitly resume interrupted queued member"}); err != nil {
		t.Fatal(err)
	}
	resumeRequest := agent.ExecutionRequest{RunID: resumeRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: marker.SessionID}, Intent: "resume accepted work"}
	resumeRequest.PermissionBounds, err = json.Marshal(permission.Authority{RunID: resumeRunID, SessionID: marker.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	restarted.mu.Lock()
	restarted.activeRuns[resumeRunID] = marker.SessionID
	restarted.activeRequests[resumeRunID] = resumeRequest
	restarted.mu.Unlock()
	if _, err := restarted.ResumeTeamMember(context.Background(), resumeRequest, marker.TeamID, marker.MemberID, "explicit-admission-retry"); err != nil {
		t.Fatalf("explicit resume of queued admission: %v", err)
	}
	resumed := receiveTeamChildInput(t, restartedRunner.inputs)
	if resumed.TeamTurn == nil || resumed.TeamTurn.MemberID != marker.MemberID || resumed.TeamTurn.TurnID == marker.TurnID || strings.Count(resumed.Task.Instruction, marker.Message) != 1 {
		t.Fatalf("explicit resume did not create one fresh turn with the pending message: %+v", resumed)
	}
	waitForTeamMemberStatus(t, root, marker.SessionID, marker.TeamID, marker.MemberID, teams.MemberIdle)
	projection, err = sessionlog.ReplayTeams(root, marker.SessionID, marker.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if restartedRunner.count() != 1 || projection.Members[marker.MemberID].Budget.AcceptedTurns != 2 {
		t.Fatalf("explicit retry provider calls/budget=%d/%+v, want exactly one call and two accepted turns", restartedRunner.count(), projection.Members[marker.MemberID].Budget)
	}
	if projection.Turns[marker.TurnID].Status != string(agent.DelegationInterrupted) {
		t.Fatalf("original queued turn changed after explicit retry: %+v", projection.Turns[marker.TurnID])
	}
	facts, err := sessionlog.TeamHistory(root, marker.SessionID, marker.TeamID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	var matchingHandoffs []sessionlog.HandoffFact
	for _, event := range facts {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil && fact.Handoff.MessageID == marker.MessageID {
			matchingHandoffs = append(matchingHandoffs, *fact.Handoff)
		}
	}
	if len(matchingHandoffs) != 1 || matchingHandoffs[0].DestinationTurnID != resumed.TeamTurn.TurnID {
		t.Fatalf("explicit retry did not hand off the message exactly once to its new turn: %+v", matchingHandoffs)
	}
}

func runTeamQueuedAdmissionCrashChild(t *testing.T) {
	root := os.Getenv("STABLE_M09_TEAM_QUEUED_ROOT")
	markerPath := os.Getenv("STABLE_M09_TEAM_QUEUED_MARKER")
	providerMarker := os.Getenv("STABLE_M09_TEAM_PROVIDER_MARKER")
	if root == "" || markerPath == "" || providerMarker == "" {
		t.Fatal("missing queued-admission helper parameters")
	}
	service, request := teamServiceFixture(t, root, "lead-queued-admission-crash")
	request.Model, request.ProviderName = "fixture", "fixture"
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "fixture"
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	runner := &teamQueuedAdmissionCrashRunner{blockerStarted: make(chan struct{}), providerMarker: providerMarker}
	pool, err := agent.NewPoolDelegator(limits, runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	blockerBounds, err := json.Marshal(permission.Authority{RunID: "pool-blocker-parent", SessionID: request.Work.SessionID, AllowedRoot: root})
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	blocker := agent.ParentRun{RunID: "pool-blocker-parent", Work: request.Work, ProjectRoot: root, PermissionBounds: blockerBounds, Provider: forkSkillFixtureProvider{}, Model: "fixture", ProviderName: "fixture", Budget: agent.DefaultDelegationLimits()}
	if _, err := pool.SubmitTask(context.Background(), blocker, agent.DelegationTask{ID: "hold-pool-worker", Name: "pool-blocker", Instruction: "Hold the only shared worker."}); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	select {
	case <-runner.blockerStarted:
	case <-time.After(10 * time.Second):
		pool.Close()
		t.Fatal("pool blocker did not occupy the only worker")
	}
	service.deps.Delegator = pool
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})
	team, err := service.CreateTeam(context.Background(), request, "queued-admission-crash")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(context.Background(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name,
		Instruction: "Inspect the durable admission boundary.", OriginCallID: "initial-spawn-call",
	})
	if err != nil {
		t.Fatalf("commit team child into real shared-pool queue: %v", err)
	}
	message, err := service.SendTeamMessage(context.Background(), request, TeamSendRequest{
		TeamID: team.ID, Recipient: member.ID, Body: "Preserve this queued message exactly once.", Token: "queued-crash-message",
	})
	if err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	queuedMember := projection.Members[member.ID]
	if queuedMember.Status != teams.MemberQueued || queuedMember.Budget.AcceptedTurns != 1 || projection.Turns[member.TurnID].Status != string(agent.DelegationQueued) {
		t.Fatalf("real pool admission did not persist queued state: member=%+v turn=%+v", queuedMember, projection.Turns[member.TurnID])
	}
	if len(projection.Messages) != 1 || projection.Messages[message.ID].ID == "" || len(projection.Handoffs) != 0 {
		t.Fatalf("queued message fixture is not pending: messages=%+v handoffs=%+v", projection.Messages, projection.Handoffs)
	}
	select {
	case <-runner.blockerStarted:
	default:
		t.Fatal("blocker stopped holding the only worker before the team child was queued")
	}
	if _, err := os.Stat(providerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("team child reached provider before crash barrier: %v", err)
	}
	marker := teamQueuedAdmissionCrashMarker{
		SessionID: request.Work.SessionID, RunID: request.RunID, TeamID: team.ID,
		MemberID: member.ID, TurnID: member.TurnID, MessageID: message.ID, Message: message.Body,
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}
