package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestTeamTUIFactsSurviveParentCompactionAndSocketReconnect(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	// Keep the Unix socket below the platform's short AF_UNIX path limit.
	socketDir, err := os.MkdirTemp("/tmp", "m09-ac9-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "s")
	childRunner := &acceptanceTeamChildRunner{started: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, Agents: agentcatalog.New("", ""),
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := conversation.Request(requestCtx, socket, conversation.ClientMsg{Op: "session_create", ProjectRoot: project})
	if err != nil || len(created) != 1 || created[0].Session == nil {
		t.Fatalf("create session: messages=%+v err=%v", created, err)
	}
	sessionID := created[0].Session.ID
	parentRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parentStream, err := conversation.OpenRun(requestCtx, socket, agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "team facts survive compaction and reconnect", Messages: []llm.Message{{Role: "user", Content: "Keep parent run active while the team collaborates."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() {
		parentRun.finish(agent.RunCompleted)
		_ = parentStream.Close()
	})
	if parentRun.request.RunID != parentRunID {
		t.Fatalf("parent runner got run %q, want %q", parentRun.request.RunID, parentRunID)
	}
	started, err := parentStream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("start parent run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, parentRunID, true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create compaction-socket")
	createdTeam := acceptanceTeamResponse(t, createResult, "team_create").Team
	if createdTeam == nil {
		t.Fatal("TUI team creation omitted team")
	}
	teamID := createdTeam.ID
	memberID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	member := teams.Member{
		ID: memberID, TeamID: teamID, Name: "reader", AgentName: "explore", RoleHash: "fixture-role-hash",
		Model: "fixture-model", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1,
	}
	memberEventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: memberEventID, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamMemberAdded,
		Revision: createdTeam.Revision + 1, ActorID: teams.Lead, ActorRunID: parentRunID, Member: &member,
	}); err != nil {
		t.Fatal(err)
	}
	model, sendResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" send "+memberID+" preserve this message across parent compaction")
	sent := acceptanceTeamResponse(t, sendResult, "team_send").TeamMessage
	if sent == nil || sent.Body != "preserve this message across parent compaction" {
		t.Fatalf("TUI team message=%+v", sent)
	}
	model, taskResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks create preserve this task")
	task := acceptanceTeamResponse(t, taskResult, "team_task_create").TeamTask
	if task == nil || task.Title != "preserve this task" {
		t.Fatalf("TUI task creation=%+v", task)
	}
	before, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Messages[sent.ID].ID == "" || before.Tasks[task.ID].ID == "" {
		t.Fatalf("TUI-created durable facts missing before compaction: %+v", before)
	}

	baseTime := time.Now().UTC()
	firstID, boundaryID, afterID := "parent-before-compaction", "parent-compaction-boundary", "parent-after-compaction"
	parentRun.events <- agent.ExecutionEvent{
		ID: firstID, RunID: parentRunID, SessionID: sessionID, RunSeq: 1, At: baseTime,
		Kind: agent.EventTextDelta, Payload: json.RawMessage(`{"text":"before compaction"}`),
	}
	firstEvent, err := parentStream.Receive()
	if err != nil || firstEvent.Type != "run_event" || firstEvent.RunEvent == nil || firstEvent.RunEvent.ID != firstID {
		t.Fatalf("receive pre-compaction parent event: message=%+v err=%v", firstEvent, err)
	}
	boundaryPayload, err := json.Marshal(agent.ContextBoundary{RunID: parentRunID, FromSeq: 1, ToSeq: 1, Summary: "parent context summary"})
	if err != nil {
		t.Fatal(err)
	}
	parentRun.events <- agent.ExecutionEvent{
		ID: boundaryID, RunID: parentRunID, SessionID: sessionID, RunSeq: 2, At: baseTime.Add(time.Millisecond),
		Kind: agent.EventCompactionBoundary, Payload: boundaryPayload,
	}
	boundaryEvent, err := parentStream.Receive()
	if err != nil || boundaryEvent.Type != "run_event" || boundaryEvent.RunEvent == nil || boundaryEvent.RunEvent.ID != boundaryID || boundaryEvent.Cursor <= firstEvent.Cursor {
		t.Fatalf("receive compaction parent event: message=%+v err=%v", boundaryEvent, err)
	}
	postPayload, _ := json.Marshal(map[string]string{"text": "after compaction"})
	parentRun.events <- agent.ExecutionEvent{
		ID: afterID, RunID: parentRunID, SessionID: sessionID, RunSeq: 3, At: baseTime.Add(2 * time.Millisecond),
		Kind: agent.EventTextDelta, Payload: postPayload,
	}
	postEvent, err := parentStream.Receive()
	if err != nil || postEvent.Type != "run_event" || postEvent.RunEvent == nil || postEvent.RunEvent.ID != afterID || postEvent.Cursor <= boundaryEvent.Cursor {
		t.Fatalf("receive post-compaction parent event: message=%+v err=%v", postEvent, err)
	}
	if err := parentStream.Close(); err != nil {
		t.Fatal(err)
	}

	afterCompact, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, afterCompact) {
		t.Fatalf("parent compaction changed team projection:\nbefore=%+v\nafter=%+v", before, afterCompact)
	}
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var storedBoundary *sessionlog.Boundary
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventBoundary {
			continue
		}
		var boundary sessionlog.Boundary
		raw, marshalErr := json.Marshal(event.Data)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := json.Unmarshal(raw, &boundary); err != nil {
			t.Fatal(err)
		}
		if boundary.RunID == parentRunID {
			storedBoundary = &boundary
		}
	}
	if storedBoundary == nil || storedBoundary.EffectiveScope() != sessionlog.BoundaryScopeRun || storedBoundary.FromSeq != 1 || storedBoundary.ToSeq != 1 {
		t.Fatalf("parent run compaction boundary not persisted: %+v", storedBoundary)
	}

	reconnected, err := conversation.SubscribeRun(requestCtx, socket, sessionID, parentRunID, firstEvent.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reconnected.Close() })
	for index, want := range []struct {
		id     string
		cursor uint64
	}{{boundaryID, boundaryEvent.Cursor}, {afterID, postEvent.Cursor}} {
		message, receiveErr := reconnected.Receive()
		if receiveErr != nil || message.Type != "run_event" || message.RunID != parentRunID || message.RunEvent == nil || message.RunEvent.ID != want.id || message.Cursor != want.cursor {
			t.Fatalf("reconnected parent event %d=%+v err=%v; want %s at cursor %d", index, message, receiveErr, want.id, want.cursor)
		}
	}
	type receiveResult struct {
		message conversation.ServerMsg
		err     error
	}
	extraResult := make(chan receiveResult, 1)
	go func() {
		message, receiveErr := reconnected.Receive()
		extraResult <- receiveResult{message: message, err: receiveErr}
	}()
	select {
	case result := <-extraResult:
		if result.err == nil {
			t.Fatalf("reconnect duplicated or added an event: %+v", result.message)
		}
	case <-time.After(100 * time.Millisecond):
	}
	if err := reconnected.Close(); err != nil {
		t.Fatal(err)
	}

	model, getResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" get")
	gotTeam := acceptanceTeamResponse(t, getResult, "team_get").Team
	if gotTeam == nil || gotTeam.ID != teamID {
		t.Fatalf("team_get after reconnect=%+v", gotTeam)
	}
	_, listResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" messages")
	listedMessages := acceptanceTeamResponse(t, listResult, "team_messages").TeamMessages
	if len(listedMessages) != 1 || listedMessages[0].ID != sent.ID || listedMessages[0].Body != sent.Body {
		t.Fatalf("TUI team messages after compaction/reconnect=%+v", listedMessages)
	}
	_, taskListResult := submitAcceptanceTeamCommand(t, model, "/team "+teamID+" tasks list")
	listedTasks := acceptanceTeamResponse(t, taskListResult, "team_task_list").TeamTasks
	if len(listedTasks) != 1 || listedTasks[0].ID != task.ID || listedTasks[0].Title != task.Title {
		t.Fatalf("TUI team tasks after compaction/reconnect=%+v", listedTasks)
	}
	after, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("TUI reconnect changed team facts: err=%v projection-equal=%v", err, reflect.DeepEqual(before, after))
	}
}
