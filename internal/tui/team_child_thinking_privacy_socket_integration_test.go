package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/conversation"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

const (
	teamChildThinkingPrivacySentinel = "PRIVATE_CHILD_THINKING_SENTINEL_6d71c4"
	teamChildThinkingPrivacySummary  = "safe child result: parser boundary confirmed"
)

type teamChildThinkingPrivacyRunner struct{ started chan agent.ChildRunInput }

func (r *teamChildThinkingPrivacyRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- input
	return (agent.StreamingChildRunner{}).Run(ctx, input)
}

type teamChildThinkingPrivacyProvider struct{}

func (teamChildThinkingPrivacyProvider) Stream(context.Context, llm.Request) (<-chan llm.Event, <-chan error) {
	events := make(chan llm.Event, 3)
	events <- llm.Event{Kind: llm.ThinkingDelta, Text: teamChildThinkingPrivacySentinel}
	events <- llm.Event{Kind: llm.TextDelta, Text: teamChildThinkingPrivacySummary}
	events <- llm.Event{Kind: llm.StreamEnd}
	close(events)
	errs := make(chan error)
	close(errs)
	return events, errs
}

// Child reasoning must remain private through the actual child provider loop,
// socket-created team member, durable replay, and parent TUI transcript.
func TestTeamChildThinkingStaysPrivateAcrossSocketReplayAndParentStream(t *testing.T) {
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
	childRunner := &teamChildThinkingPrivacyRunner{started: make(chan agent.ChildRunInput, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	reporter := conversation.NewDelegationEventReporter()
	pool, err := agent.NewPoolDelegator(limits, childRunner, reporter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	role := agentcatalog.Definition{
		Name: "thinking-private-role", Description: "Read-only private role", Model: "inherit",
		Instruction: "private team instructions stay with the child", Tools: []string{"read_file"}, MaxTurns: 1,
	}
	socketDir, err := os.MkdirTemp(filepath.Join("..", "..", ".tmp"), "team-thinking-privacy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketDir, err = filepath.Abs(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketDir, "s")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, Agents: teamRolePrivacyCatalog{definition: role},
		ForkProvider: teamChildThinkingPrivacyProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
		ForkToolSchemas:     []llm.ToolSchema{{Name: "read_file"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reporter.Bind(svc)
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
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "child thinking privacy integration", Messages: []llm.Message{{Role: "user", Content: "Inspect the assigned area and report a safe result."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	started, err := parentStream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("start parent stream: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, parentRunID, true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create thinking-privacy")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI team create omitted team")
	}
	_, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader "+role.Name+" inspect the assigned area")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("TUI member spawn omitted member")
	}
	select {
	case child := <-childRunner.started:
		if child.Provider == nil || child.RoleInstruction != role.Instruction || !strings.Contains(child.Task.Instruction, role.Instruction) {
			t.Fatalf("socket-created child did not receive its private run context: %+v", child)
		}
	case <-reqctx.Done():
		t.Fatal("socket-created child did not start")
	}
	if err := waitForTeamRolePrivacyIdle(project, sessionID, team.ID, member.ID); err != nil {
		t.Fatal(err)
	}

	// Read the live parent subscription through its terminal child summary.
	// The child thinking delta is not a parent event and consumes no parent
	// run sequence number or stream cursor.
	var parentEvents []conversation.ServerMsg
	deadline := time.After(5 * time.Second)
streamLoop:
	for {
		messages := make(chan struct {
			msg conversation.ServerMsg
			err error
		}, 1)
		go func() {
			msg, receiveErr := parentStream.Receive()
			messages <- struct {
				msg conversation.ServerMsg
				err error
			}{msg: msg, err: receiveErr}
		}()
		select {
		case got := <-messages:
			if got.err != nil {
				t.Fatalf("read parent stream: %v", got.err)
			}
			if got.msg.Type != "run_event" || got.msg.RunID != parentRunID || got.msg.RunEvent == nil {
				t.Fatalf("unexpected parent stream message: %+v", got.msg)
			}
			parentEvents = append(parentEvents, got.msg)
			if got.msg.RunEvent.Kind == string(agent.EventDelegation) {
				var payload agent.DelegationEvent
				encoded, marshalErr := json.Marshal(got.msg.RunEvent.Payload)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if err := json.Unmarshal(encoded, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Status == agent.DelegationSucceeded {
					if !strings.Contains(payload.Summary, teamChildThinkingPrivacySummary) {
						t.Fatalf("parent stream lost safe child summary: %+v", payload)
					}
					break streamLoop
				}
			}
		case <-deadline:
			t.Fatal("parent stream did not receive terminal safe delegation summary")
		}
	}

	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	encodedEvents, err := json.Marshal(transcript.Events)
	if err != nil {
		t.Fatal(err)
	}
	path, err := sessionlog.SessionPath(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rawLog, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	turn := projection.Turns[member.TurnID]
	if turn.Status != "succeeded" || !strings.Contains(turn.Summary, teamChildThinkingPrivacySummary) {
		t.Fatalf("team projection lost safe child result: %+v", turn)
	}
	teamProjectionJSON, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	for surface, data := range map[string]string{
		"raw session log": string(rawLog), "replayed transcript": string(encodedEvents),
		"team projection": string(teamProjectionJSON),
	} {
		if strings.Contains(data, teamChildThinkingPrivacySentinel) {
			t.Errorf("private child thinking leaked into %s", surface)
		}
	}

	var parentRunSeq uint64
	var matchingParentEvents int
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		encoded, err := json.Marshal(event.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &runEvent); err != nil {
			t.Fatal(err)
		}
		if runEvent.RunID != parentRunID {
			continue
		}
		matchingParentEvents++
		if runEvent.RunSeq != parentRunSeq+1 {
			t.Fatalf("parent run sequence advanced unexpectedly: got %d after %d", runEvent.RunSeq, parentRunSeq)
		}
		parentRunSeq = runEvent.RunSeq
		if runEvent.Kind != string(agent.EventDelegation) {
			t.Errorf("child thinking produced unexpected parent run event kind %q", runEvent.Kind)
		}
	}
	if matchingParentEvents != len(parentEvents) || parentRunSeq != uint64(len(parentEvents)) {
		t.Fatalf("parent stream/cursor mismatch: streamed events=%d persisted parent events=%d final run sequence=%d", len(parentEvents), matchingParentEvents, parentRunSeq)
	}
	for _, message := range parentEvents {
		if message.Cursor == 0 || message.RunEvent.RunSeq == 0 || strings.Contains(mustMarshalCombinedPrivacy(t, message.RunEvent.Payload), teamChildThinkingPrivacySentinel) {
			t.Fatalf("parent stream cursor or payload exposed private child state: %+v", message)
		}
	}

	model.Events = transcript.Events
	model.Transcript.SetSize(100, 24)
	model.Transcript.SetEvents(model.Events)
	visible := model.Transcript.View()
	if strings.Contains(visible, teamChildThinkingPrivacySentinel) {
		t.Fatalf("parent TUI transcript exposed private child thinking: %s", visible)
	}
	if !strings.Contains(visible, teamChildThinkingPrivacySummary) {
		t.Fatalf("parent TUI transcript omitted safe child summary: %s", visible)
	}
}
