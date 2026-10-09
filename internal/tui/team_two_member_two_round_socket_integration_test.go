package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// This covers a two-member, two-turn handoff through the user-facing TUI,
// conversation socket, and service. The fake children never call a provider.
func TestTeamTUITwoMembersTwoTurnsRoutePrivateHandoffAndKeepParentRun(t *testing.T) {
	ctx := context.Background()
	tmpParent := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(tmpParent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(tmpParent, "t2-")
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

	childRunner := &twoMemberTwoRoundChildRunner{started: make(chan agent.ChildRunInput, 4), release: make(chan struct{}, 4), calls: make(map[string]int), reportResults: make(chan error, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 2, 2
	pool, err := agent.NewPoolDelegator(limits, childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socket := filepath.Join(root, "s")
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
		for range 4 {
			select {
			case childRunner.release <- struct{}{}:
			default:
			}
		}
		if err := svc.Close(); err != nil {
			t.Errorf("close conversation service: %v", err)
		}
	})

	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
	const parentOnly = "PARENT_ONLY_TRANSCRIPT_SECRET_DO_NOT_COPY_TO_CHILD"
	parentStream, err := conversation.OpenRun(reqctx, socket, agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "two-member handoff fixture", Messages: []llm.Message{{Role: "user", Content: parentOnly}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if parentRun.request.RunID != parentRunID {
		t.Fatalf("active parent run=%q, want %q", parentRun.request.RunID, parentRunID)
	}
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("wait for parent run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model.Pending = true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create two-member-two-round")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("team creation omitted team")
	}
	model, spawnAResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn parser-reader explore inspect parser recovery")
	memberA := acceptanceTeamResponse(t, spawnAResult, "team_member_spawn").TeamMember
	model, spawnBResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn recovery-reviewer explore inspect recovery boundaries")
	memberB := acceptanceTeamResponse(t, spawnBResult, "team_member_spawn").TeamMember
	if memberA == nil || memberB == nil || memberA.ID == memberB.ID {
		t.Fatalf("spawn results A=%+v B=%+v", memberA, memberB)
	}

	firstByMember := make(map[string]agent.ChildRunInput, 2)
	for range 2 {
		input := receiveTwoMemberTwoRoundChild(t, childRunner.started)
		if input.ParentRunID != parentRunID || input.TeamTurn == nil || input.TeamTurn.TeamID != team.ID {
			t.Fatalf("first turn lost its parent/team binding: %+v", input)
		}
		firstByMember[input.TeamTurn.MemberID] = input
	}
	firstA, okA := firstByMember[memberA.ID]
	firstB, okB := firstByMember[memberB.ID]
	if !okA || !okB || firstA.TeamTurn.TurnID == firstB.TeamTurn.TurnID {
		t.Fatalf("expected independent first turns for both members: A=%+v B=%+v", firstA.TeamTurn, firstB.TeamTurn)
	}
	if strings.Contains(firstA.Task.Instruction, parentOnly) || strings.Contains(firstB.Task.Instruction, parentOnly) {
		t.Fatal("parent transcript content leaked into a first-round child instruction")
	}

	model, handoffResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" send "+memberB.ID+" validate the parser recovery handoff")
	handoff := acceptanceTeamResponse(t, handoffResult, "team_send").TeamMessage
	if handoff == nil || handoff.SenderID != teams.Lead || len(handoff.Recipients) != 1 || handoff.Recipients[0] != memberB.ID {
		t.Fatalf("private handoff=%+v, want only member B", handoff)
	}

	parentRun.events <- agent.ExecutionEvent{
		ID: "parent-stream-during-child-turns", RunID: parentRunID, SessionID: sessionID,
		RunSeq: 1, At: time.Now().UTC(), Kind: agent.EventTextDelta,
	}
	streamMsg, err := parentStream.Receive()
	if err != nil || streamMsg.Type != "run_event" || streamMsg.RunID != parentRunID || streamMsg.RunEvent == nil || streamMsg.RunEvent.ID != "parent-stream-during-child-turns" {
		t.Fatalf("parent stream stopped during team turns: message=%+v err=%v", streamMsg, err)
	}
	if !model.Pending || model.ActiveRunID != parentRunID {
		t.Fatalf("TUI parent run state changed after team command: pending=%v run=%q", model.Pending, model.ActiveRunID)
	}

	childRunner.release <- struct{}{}
	childRunner.release <- struct{}{}
	waitTwoMemberTwoRoundStatus(t, project, sessionID, team.ID, memberA.ID, teams.MemberIdle)
	waitTwoMemberTwoRoundStatus(t, project, sessionID, team.ID, memberB.ID, teams.MemberIdle)
	for _, member := range []*teams.Member{memberA, memberB} {
		_, resumeResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" resume "+member.ID)
		resumed := acceptanceTeamResponse(t, resumeResult, "team_member_resume").TeamMember
		if resumed == nil || (resumed.Status != teams.MemberQueued && resumed.Status != teams.MemberRunning) {
			t.Fatalf("resume %s result=%+v, want queued/running", member.ID, resumed)
		}
	}

	secondByMember := make(map[string]agent.ChildRunInput, 2)
	for range 2 {
		input := receiveTwoMemberTwoRoundChild(t, childRunner.started)
		if input.ParentRunID != parentRunID || input.TeamTurn == nil || input.TeamTurn.TeamID != team.ID {
			t.Fatalf("second turn lost its parent/team binding: %+v", input)
		}
		secondByMember[input.TeamTurn.MemberID] = input
	}
	secondA, okA := secondByMember[memberA.ID]
	secondB, okB := secondByMember[memberB.ID]
	if !okA || !okB || secondA.TeamTurn.TurnID == firstA.TeamTurn.TurnID || secondB.TeamTurn.TurnID == firstB.TeamTurn.TurnID {
		t.Fatalf("expected a fresh second turn per member: A=%+v B=%+v", secondA.TeamTurn, secondB.TeamTurn)
	}
	if strings.Contains(secondA.Task.Instruction, handoff.Body) || !strings.Contains(secondB.Task.Instruction, handoff.Body) {
		t.Fatalf("private handoff routing leaked or disappeared: A=%q B=%q", secondA.Task.Instruction, secondB.Task.Instruction)
	}
	if strings.Contains(secondA.Task.Instruction, parentOnly) || strings.Contains(secondB.Task.Instruction, parentOnly) {
		t.Fatal("parent transcript content leaked into a second-round child instruction")
	}
	for _, input := range []agent.ChildRunInput{secondA, secondB} {
		if !strings.Contains(input.Task.Instruction, "summary-"+input.TeamTurn.MemberName+"-round-1") {
			t.Fatalf("second turn omitted its member's prior summary: member=%s instruction=%q", input.TeamTurn.MemberName, input.Task.Instruction)
		}
	}

	childRunner.release <- struct{}{}
	childRunner.release <- struct{}{}
	waitTwoMemberTwoRoundStatus(t, project, sessionID, team.ID, memberA.ID, teams.MemberIdle)
	waitTwoMemberTwoRoundStatus(t, project, sessionID, team.ID, memberB.ID, teams.MemberIdle)
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Messages) != 0 {
		t.Fatalf("delivered private message should leave pending projection: %+v", projection.Messages)
	}
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var parentMarkerFound, durableMessageFound, durableHandoffFound bool
	for _, event := range transcript.Events {
		switch event.Type {
		case sessionlog.EventMessage:
			raw, err := json.Marshal(event.Data)
			if err != nil {
				t.Fatal(err)
			}
			var message sessionlog.Message
			if err := json.Unmarshal(raw, &message); err != nil {
				t.Fatal(err)
			}
			if message.Text == parentOnly {
				parentMarkerFound = true
			}
		case sessionlog.EventTeam:
			raw, err := json.Marshal(event.Data)
			if err != nil {
				t.Fatal(err)
			}
			var fact sessionlog.TeamEvent
			if err := json.Unmarshal(raw, &fact); err != nil {
				t.Fatal(err)
			}
			if fact.TeamID != team.ID {
				continue
			}
			if fact.Kind == sessionlog.TeamMessageSent && fact.Message != nil && fact.Message.ID == handoff.ID && fact.Message.Body == handoff.Body && len(fact.Message.Recipients) == 1 && fact.Message.Recipients[0] == memberB.ID {
				durableMessageFound = true
			}
			if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil && fact.Handoff.MessageID == handoff.ID && fact.Handoff.RecipientID == memberB.ID && fact.Handoff.DestinationTurnID == secondB.TeamTurn.TurnID {
				durableHandoffFound = true
			}
		}
	}
	if !parentMarkerFound {
		t.Fatal("parent-only marker was not retained in the parent transcript")
	}
	if !durableMessageFound || !durableHandoffFound {
		t.Fatalf("durable private message/handoff facts missing: message=%v handoff=%v", durableMessageFound, durableHandoffFound)
	}

	// A member report must be visible in the lead's team history and enter
	// exactly the next matching parent run, through the normal run socket path.
	const memberReport = "Recovery review confirms the retry boundary."
	childRunner.mu.Lock()
	childRunner.reportMember, childRunner.reportBody = memberB.ID, memberReport
	childRunner.mu.Unlock()
	model, resumeResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" resume "+memberB.ID)
	resumed := acceptanceTeamResponse(t, resumeResult, "team_member_resume").TeamMember
	if resumed == nil || (resumed.Status != teams.MemberQueued && resumed.Status != teams.MemberRunning) {
		t.Fatalf("report turn resume=%+v, want queued/running", resumed)
	}
	reportTurn := receiveTwoMemberTwoRoundChild(t, childRunner.started)
	if reportTurn.TeamTurn == nil || reportTurn.TeamTurn.MemberID != memberB.ID {
		t.Fatalf("report turn has wrong member identity: %+v", reportTurn.TeamTurn)
	}
	childRunner.release <- struct{}{}
	select {
	case err := <-childRunner.reportResults:
		if err != nil {
			t.Fatalf("member report to lead failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("member report tool did not complete")
	}
	model, messagesResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" messages")
	messages := acceptanceTeamResponse(t, messagesResult, "team_messages").TeamMessages
	var reportVisible bool
	for _, message := range messages {
		if message.Body == memberReport && message.SenderID == memberB.ID && len(message.Recipients) == 1 && message.Recipients[0] == teams.Lead {
			reportVisible = true
		}
	}
	if !reportVisible {
		t.Fatalf("TUI team history omitted member-to-lead report: %+v", messages)
	}
	waitTwoMemberTwoRoundStatus(t, project, sessionID, team.ID, memberB.ID, teams.MemberIdle)

	parentRun.finish(agent.RunCompleted)
	for {
		message, err := parentStream.Receive()
		if err != nil {
			t.Fatalf("wait for completed parent run: %v", err)
		}
		if message.Type == "run_outcome" {
			if message.Outcome == nil || message.Outcome.Status != agent.RunCompleted {
				t.Fatalf("parent run outcome=%+v, want completed", message.Outcome)
			}
			break
		}
	}
	matchingParentID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	matchingStream, err := conversation.OpenRun(reqctx, socket, agent.ExecutionRequest{
		RunID: matchingParentID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "receive member report", Messages: []llm.Message{{Role: "user", Content: "Review team findings."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer matchingStream.Close()
	matchingParent := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { matchingParent.finish(agent.RunCompleted) })
	if started, err := matchingStream.Receive(); err != nil || started.Type != "run_started" {
		t.Fatalf("matching parent did not start over socket: message=%+v err=%v", started, err)
	}
	var noticeFound bool
	for _, message := range matchingParent.request.Messages {
		if message.Role == "user" && strings.Contains(message.Content, memberReport) && strings.Contains(message.Content, "untrusted reference data") {
			noticeFound = true
		}
	}
	if !noticeFound {
		t.Fatalf("next matching parent run omitted member report: %+v", matchingParent.request.Messages)
	}
	matchingParent.finish(agent.RunCompleted)
}

type twoMemberTwoRoundChildRunner struct {
	started       chan agent.ChildRunInput
	release       chan struct{}
	mu            sync.Mutex
	calls         map[string]int
	service       *conversation.Service
	reportMember  string
	reportBody    string
	reportResults chan error
}

func (r *twoMemberTwoRoundChildRunner) Run(ctx context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	if input.TeamTurn == nil {
		return agent.ChildRunResult{Status: agent.DelegationFailed, Error: "missing team turn"}
	}
	r.mu.Lock()
	r.calls[input.TeamTurn.MemberID]++
	round := r.calls[input.TeamTurn.MemberID]
	reportMember, reportBody := r.reportMember, r.reportBody
	r.mu.Unlock()
	select {
	case r.started <- input:
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
	select {
	case <-r.release:
		if round == 3 && input.TeamTurn.MemberID == reportMember {
			arguments, err := json.Marshal(map[string]any{
				"team_id": input.TeamTurn.TeamID, "recipient": teams.Lead, "body": reportBody,
			})
			if err == nil {
				var outcome agent.ToolOutcome
				outcome, err = r.service.ExecuteTeamTool(ctx, agent.ExecutionRequest{
					RunID: input.ChildRunID, Work: input.Work, TeamTurn: input.TeamTurn,
				}, llm.ToolUse{ID: "member-report-to-lead", Name: "team_send", Arguments: arguments})
				if err == nil && (outcome.Status != agent.ToolSucceeded || outcome.IsError) {
					err = fmt.Errorf("team_send outcome=%+v", outcome)
				}
			}
			r.reportResults <- err
		}
		return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "summary-" + input.TeamTurn.MemberName + "-round-" + strconv.Itoa(round)}
	case <-ctx.Done():
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: ctx.Err().Error()}
	}
}

func receiveTwoMemberTwoRoundChild(t *testing.T, started <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-started:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("conversation service did not start a bounded child turn")
		return agent.ChildRunInput{}
	}
}

func waitTwoMemberTwoRoundStatus(t *testing.T, root, sessionID, teamID, memberID string, want teams.MemberStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Members[memberID].Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		t.Fatalf("replay team before status assertion: %v", err)
	}
	t.Fatalf("member status=%q, want %q", projection.Members[memberID].Status, want)
}
