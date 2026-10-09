package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// This exercises lazy expiry through the user-facing slash command and the
// conversation socket. The timestamp fixture moves durable request-created
// events into the past while keeping their original one-minute validity.
func TestExpiredTeamRequestsTUIRemainUnansweredAndDoNotChangeCapacity(t *testing.T) {
	ctx := context.Background()
	tmpRoot := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpRoot, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpRoot, "te-")
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

	childRunner := &expiryGateChildRunner{started: make(chan agent.ChildRunInput, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socket := filepath.Join(root, "conversation.sock")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, Agents: agentcatalog.New("", ""),
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	childRunner.service = svc
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	created, err := conversation.Request(reqctx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parentStream, err := conversation.OpenRun(reqctx, socket, agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}, Intent: "request expiry fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("start lead run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, teamResult := submitAcceptanceTeamCommand(t, model, "/teams create expiry-check")
	team := acceptanceTeamResponse(t, teamResult, "team_create").Team
	if team == nil {
		t.Fatal("team create response omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader explore inspect-parser --plan")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("spawn response omitted plan-required member")
	}
	var childInput agent.ChildRunInput
	select {
	case childInput = <-childRunner.started:
	case <-reqctx.Done():
		t.Fatal("child did not start and submit its plan")
	}
	_, initialListResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	var plan teams.Request
	for _, request := range acceptanceTeamResponse(t, initialListResult, "team_request_list").TeamRequests {
		if request.Type == teams.RequestPlan && request.MemberID == member.ID && request.Status == teams.RequestPending {
			plan = request
		}
	}
	if plan.ID == "" {
		t.Fatal("child did not persist its pending plan request")
	}
	model, shutdownResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" shutdown "+member.ID)
	shutdown := acceptanceTeamResponse(t, shutdownResult, "team_shutdown_request").TeamRequest
	if shutdown == nil || shutdown.Type != teams.RequestShutdown || shutdown.Status != teams.RequestPending {
		t.Fatalf("busy shutdown request=%+v, want pending", shutdown)
	}

	// Make both creation facts old but valid under the durable 10-minute
	// maximum, avoiding a wall-clock wait and preserving the request protocol.
	expiredAt := time.Now().UTC().Add(-time.Minute)
	if err := rewriteTeamRequestCreationTimes(project, sessionID, map[string]bool{
		team.ID + "/" + plan.ID:     true,
		team.ID + "/" + shutdown.ID: true,
	}, expiredAt); err != nil {
		t.Fatal(err)
	}

	model.ActiveRunID = "" // TUI request listing/responses use trusted lead session scope.
	_, listResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" requests")
	listed := acceptanceTeamResponse(t, listResult, "team_request_list").TeamRequests
	if len(listed) != 2 {
		t.Fatalf("request list=%+v, want expired plan and shutdown", listed)
	}
	for _, request := range listed {
		if request.Status != teams.RequestExpired || request.Revision != 2 {
			t.Fatalf("listed request=%+v, want expired revision 2", request)
		}
	}

	// A lead approval attempt crosses the TUI/socket boundary but must fail;
	// a member shutdown response is checked through the same live service.
	var expiredPlan teams.Request
	for _, request := range listed {
		if request.Type == teams.RequestPlan {
			expiredPlan = request
		}
	}
	if expiredPlan.ID == "" {
		t.Fatal("expired plan request missing from TUI list")
	}
	model.Composer.SetValue("/team " + team.ID + " respond " + expiredPlan.ID + " 1 approve proceed")
	updated, cmd := model.submitComposer()
	if cmd == nil {
		t.Fatal("expired request response did not produce a TUI command")
	}
	if _, ok := updated.(Model); !ok {
		t.Fatalf("response update model=%T, want Model", updated)
	}
	result, ok := cmd().(resultMsg)
	if !ok || result.err == nil {
		t.Fatalf("expired TUI response result=%T %+v, want reported expiry error", result, result)
	}
	childRequest := agent.ExecutionRequest{RunID: childInput.ChildRunID, Work: childInput.Work, TeamTurn: childInput.TeamTurn}
	shutdownResponse, err := svc.RespondTeamRequest(ctx, childRequest, team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestRejected), "continue")
	if err == nil || shutdownResponse.Status != teams.RequestExpired {
		t.Fatalf("expired shutdown response=%+v err=%v, want rejected response and expired state", shutdownResponse, err)
	}

	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Members) != 1 || projection.Members[member.ID].Status != teams.MemberRunning || projection.Members[member.ID].PlanApproved {
		t.Fatalf("expiry changed member/capacity state: members=%+v", projection.Members)
	}
	if childRunner.starts.Load() != 1 {
		t.Fatalf("request expiry started %d child turns, want only the existing capacity-consuming turn", childRunner.starts.Load())
	}
	for _, id := range []string{expiredPlan.ID, shutdown.ID} {
		if got := projection.Requests[id]; got.Status != teams.RequestExpired || got.Revision != 2 {
			t.Fatalf("replayed expired request=%+v", got)
		}
	}
	finalTranscript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	expiredFacts, responseFacts := 0, 0
	for _, event := range finalTranscript.Events {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		raw, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Request == nil || fact.Request.ID != expiredPlan.ID && fact.Request.ID != shutdown.ID {
			continue
		}
		switch fact.Kind {
		case sessionlog.TeamRequestExpired:
			expiredFacts++
		case sessionlog.TeamRequestResponded:
			responseFacts++
		}
	}
	if expiredFacts != 2 || responseFacts != 0 {
		t.Fatalf("durable request facts: expiry=%d responses=%d, want two expiries and no response", expiredFacts, responseFacts)
	}
}

type expiryGateChildRunner struct {
	service *conversation.Service
	started chan agent.ChildRunInput
	starts  atomic.Int32
}

func (r *expiryGateChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	if input.TeamTurn == nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "missing trusted team turn"}
	}
	_, err := r.service.ExecuteTeamTool(ctx, agent.ExecutionRequest{
		RunID: input.ChildRunID, Work: input.Work, TeamTurn: input.TeamTurn,
	}, llm.ToolUse{
		ID: "submit-expiry-plan", Name: "team_plan_submit",
		Arguments: json.RawMessage(`{"team_id":"` + input.TeamTurn.TeamID + `","body":"Inspect parser entry point."}`),
	})
	if err != nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: err.Error()}
	}
	r.starts.Add(1)
	r.started <- input
	<-ctx.Done()
	return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
}

func rewriteTeamRequestCreationTimes(root, sessionID string, requested map[string]bool, expiresAt time.Time) error {
	transcript, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		return err
	}
	matched := map[string]bool{}
	for i := range transcript.Events {
		event := &transcript.Events[i]
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		raw, err := json.Marshal(event.Data)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &fact); err != nil {
			return err
		}
		if fact.Kind != sessionlog.TeamRequestCreated || fact.Request == nil {
			continue
		}
		key := fact.TeamID + "/" + fact.Request.ID
		if !requested[key] {
			continue
		}
		fact.Request.ExpiresAt = expiresAt
		factRaw, err := json.Marshal(fact)
		if err != nil {
			return err
		}
		var decoded any
		if err := json.Unmarshal(factRaw, &decoded); err != nil {
			return err
		}
		event.Data = decoded
		event.At = expiresAt.Add(-time.Minute).UTC()
		matched[key] = true
	}
	if len(matched) != len(requested) {
		return fmt.Errorf("matched %d request-created events, want %d", len(matched), len(requested))
	}
	path, err := sessionlog.SessionPath(root, sessionID)
	if err != nil {
		return err
	}
	var output bytes.Buffer
	for _, event := range transcript.Events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		output.Write(encoded)
		output.WriteByte('\n')
	}
	return os.WriteFile(path, output.Bytes(), 0600)
}
