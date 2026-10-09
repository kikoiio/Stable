package conversation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/agent"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTeamLeadMessagesEnterOnlyNextMatchingParentRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "lead-notification-origin")
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	team, err := service.CreateTeam(t.Context(), leadRequest, "lead-notification")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "lead-notification-member"
	addTeamMessageMember(t, service, leadRequest, team.ID, memberID, "reader")
	memberRequest := appendLeadNotificationTurn(t, service, leadRequest, team.ID, memberID, "lead-notification-child")
	const body = "Untrusted team finding: inspect the parser recovery path."
	message, err := service.SendTeamMessage(t.Context(), memberRequest, TeamSendRequest{
		TeamID: team.ID, Recipient: teams.Lead, Body: body, Token: "lead-notification-message",
	})
	if err != nil {
		t.Fatal(err)
	}

	first := startCapturedParent(t, service, leadRequest, "lead-notification-next")
	firstNotice := findTeamLeadNotice(t, first.request.Messages, body)
	if firstNotice.Role != "user" || !strings.Contains(firstNotice.Content, "untrusted reference data") || !strings.Contains(firstNotice.Content, message.ID) {
		t.Fatalf("parent notification is not labeled reference data: %+v", firstNotice)
	}
	projection, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, pending := projection.Messages[message.ID]; pending {
		t.Fatalf("message remained pending after matching parent start: %+v", projection.Messages)
	}
	second := startCapturedParent(t, service, leadRequest, "lead-notification-later")
	for _, input := range second.request.Messages {
		if strings.Contains(input.Content, body) || strings.Contains(input.Content, message.ID) {
			t.Fatalf("already delivered team message repeated in later parent run: %+v", input)
		}
	}
	history, err := sessionlog.TeamHistory(root, leadRequest.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	handOffFacts := 0
	for _, event := range history {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamLeadHandoff && fact.Handoff != nil && fact.Handoff.MessageID == message.ID {
			handOffFacts++
		}
	}
	if handOffFacts != 1 {
		t.Fatalf("lead handoff facts for message=%d, want exactly one", handOffFacts)
	}
	for _, event := range history {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamLeadHandoff && fact.Handoff != nil && fact.Handoff.MessageID == message.ID && fact.Handoff.DestinationRunID != "lead-notification-next" {
			t.Fatalf("lead handoff targeted %q, want first matching parent run", fact.Handoff.DestinationRunID)
		}
	}
}

func TestTeamLeadNotificationBatchLeavesOverLimitMessagesPending(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, leadRequest := teamServiceFixture(t, root, "lead-notification-batch-origin")
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	team, err := service.CreateTeam(t.Context(), leadRequest, "lead-notification-batch")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "lead-notification-batch-member"
	addTeamMessageMember(t, service, leadRequest, team.ID, memberID, "reader")
	memberRequest := appendLeadNotificationTurn(t, service, leadRequest, team.ID, memberID, "lead-notification-batch-child")
	body := strings.Repeat("n", teams.MaxMessageBytes)
	for i := 0; i < 5; i++ {
		if _, err := service.SendTeamMessage(t.Context(), memberRequest, TeamSendRequest{
			TeamID: team.ID, Recipient: teams.Lead, Body: body, Token: fmt.Sprintf("lead-batch-%d", i),
		}); err != nil {
			t.Fatalf("send notification %d: %v", i, err)
		}
	}

	runner := startCapturedParent(t, service, leadRequest, "lead-notification-batch-next")
	var delivered int
	for _, input := range runner.request.Messages {
		if strings.Contains(input.Content, "Team notifications (") {
			var notices []teamLeadNotice
			encoded := input.Content[strings.IndexByte(input.Content, '\n')+1:]
			if err := json.Unmarshal([]byte(encoded), &notices); err != nil {
				t.Fatalf("decode notification batch %q: %v", input.Content, err)
			}
			delivered += len(notices)
		}
	}
	if delivered != teams.MaxBatchBytes/teams.MaxMessageBytes {
		t.Fatalf("first parent received %d notices, want byte-bounded batch of %d", delivered, teams.MaxBatchBytes/teams.MaxMessageBytes)
	}
	projection, err := sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Messages) != 1 {
		t.Fatalf("over-limit notification remainder=%+v, want one pending message", projection.Messages)
	}
	for i := 0; i < teams.MaxBatchMessages; i++ {
		if _, err := service.SendTeamMessage(t.Context(), memberRequest, TeamSendRequest{
			TeamID: team.ID, Recipient: teams.Lead, Body: "x", Token: fmt.Sprintf("lead-count-batch-%d", i),
		}); err != nil {
			t.Fatalf("send count-bounded notification %d: %v", i, err)
		}
	}
	secondParent := startCapturedParent(t, service, leadRequest, "lead-notification-batch-later")
	var secondBatch int
	for _, input := range secondParent.request.Messages {
		if strings.Contains(input.Content, "Team notifications (") {
			var notices []teamLeadNotice
			encoded := input.Content[strings.IndexByte(input.Content, '\n')+1:]
			if err := json.Unmarshal([]byte(encoded), &notices); err != nil {
				t.Fatalf("decode count-bounded notification batch %q: %v", input.Content, err)
			}
			secondBatch += len(notices)
		}
	}
	if secondBatch != teams.MaxBatchMessages {
		t.Fatalf("second parent received %d notices, want count-bounded batch of %d", secondBatch, teams.MaxBatchMessages)
	}
	projection, err = sessionlog.ReplayTeams(root, leadRequest.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Messages) != 1 {
		t.Fatalf("count-bounded notification remainder=%+v, want one pending message", projection.Messages)
	}
}

func TestTeamLeadMessageWaitsForExactGoalWorkItem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal parent notification")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if _, err := state.CreateGoal(t.Context(), coreGoal("goal-parent-notification", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-parent-notification", WorkItemID: "item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-parent-notification", WorkItemID: "item-b"}
	for runID, work := range map[string]agent.WorkRef{"goal-parent-origin": workA, "goal-parent-other-item": workB} {
		if _, err := sessionlog.Append(root, session.ID, sessionlog.EventRunStarted, sessionlog.RunStarted{
			RunID: runID, WorkKind: string(work.Kind), GoalID: work.GoalID, WorkItemID: work.WorkItemID, Intent: "goal notification fixture",
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state, ProviderName: "fixture", Model: "model-v1"},
		activeRuns: map[string]string{"goal-parent-origin": session.ID, "goal-parent-other-item": session.ID},
	}
	requestA := agent.ExecutionRequest{RunID: "goal-parent-origin", Work: workA, ProviderName: "fixture", Model: "model-v1"}
	requestB := agent.ExecutionRequest{RunID: "goal-parent-other-item", Work: workB, ProviderName: "fixture", Model: "model-v1"}
	team, err := service.CreateTeam(t.Context(), requestA, "goal-parent-notification")
	if err != nil {
		t.Fatal(err)
	}
	const memberID = "goal-notification-member"
	addTeamMessageMember(t, service, requestA, team.ID, memberID, "reader")
	memberRequest := appendLeadNotificationTurn(t, service, requestA, team.ID, memberID, "goal-notification-child")
	const body = "This finding belongs to item A only."
	if _, err := service.SendTeamMessage(t.Context(), memberRequest, TeamSendRequest{
		TeamID: team.ID, Recipient: teams.Lead, Body: body, Token: "goal-notification-message",
	}); err != nil {
		t.Fatal(err)
	}

	otherItem := startCapturedParent(t, service, requestB, "goal-parent-item-b-next")
	for _, input := range otherItem.request.Messages {
		if strings.Contains(input.Content, body) {
			t.Fatalf("different valid WorkItem received item A's private team message: %+v", input)
		}
	}
	projection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Messages) != 1 {
		t.Fatalf("different WorkItem consumed the private notification: %+v", projection.Messages)
	}

	matchingItem := startCapturedParent(t, service, requestA, "goal-parent-item-a-next")
	findTeamLeadNotice(t, matchingItem.request.Messages, body)
}

func appendLeadNotificationTurn(t *testing.T, service *Service, lead agent.ExecutionRequest, teamID, memberID, runID string) agent.ExecutionRequest {
	t.Helper()
	turnID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	request := lead
	request.RunID = runID
	request.TeamTurn = &agent.TeamTurnIdentity{TeamID: teamID, MemberID: memberID, TurnID: turnID, MemberName: "reader"}
	turn := sessionlog.TurnFact{ID: turnID, MemberID: memberID, RunID: runID, TaskID: turnID, OriginRunID: lead.RunID, Status: "intent"}
	appendTeamMessageLimitFact(t, service, lead, teamID, sessionlog.TeamTurnIntent, turn)
	turn.Status = "queued"
	appendTeamMessageLimitFact(t, service, lead, teamID, sessionlog.TeamTurnAccepted, turn)
	if _, err := sessionlog.Append(service.deps.ProjectRoot, lead.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: runID, WorkKind: string(lead.Work.Kind), GoalID: lead.Work.GoalID, WorkItemID: lead.Work.WorkItemID, Intent: "notify team lead",
		TeamID: teamID, TeamMemberID: memberID, TeamTurnID: turnID, OriginRunID: lead.RunID,
	}); err != nil {
		t.Fatal(err)
	}
	return request
}

func startCapturedParent(t *testing.T, service *Service, lead agent.ExecutionRequest, runID string) *fixedRunner {
	t.Helper()
	events := make(chan agent.ExecutionEvent, 1)
	events <- agent.ExecutionEvent{
		ID: "event-" + runID, RunID: runID, SessionID: lead.Work.SessionID, RunSeq: 1,
		At: time.Now().UTC(), Kind: agent.EventTerminal, Payload: json.RawMessage(`{"status":"completed"}`),
	}
	close(events)
	done := make(chan agent.RunOutcome, 1)
	done <- agent.RunOutcome{RunID: runID, Status: agent.RunCompleted}
	close(done)
	runner := &fixedRunner{handle: &agent.RunHandle{Events: events, Done: done}}
	service.deps.Runner = runner
	updates := make(chan ServerMsg, 8)
	service.mu.Lock()
	service.clients = map[chan ServerMsg]*clientSubscription{updates: {ch: updates, sessionID: lead.Work.SessionID}}
	service.mu.Unlock()
	request := lead
	request.RunID = runID
	request.Intent = "continue the matching work"
	request.ProviderName, request.Model = service.deps.ProviderName, service.deps.Model
	request.Messages = []llm.Message{{Role: "user", Content: request.Intent}}
	if err := service.startRun(t.Context(), ClientMsg{SessionID: request.Work.SessionID, Run: &request}, updates); err != nil {
		t.Fatalf("start parent run %s: %v", runID, err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case update := <-updates:
			if update.Type == "run_outcome" && update.RunID == runID {
				return runner
			}
		case <-deadline:
			t.Fatalf("parent run %s did not finish its captured fixture", runID)
		}
	}
}

func findTeamLeadNotice(t *testing.T, messages []llm.Message, body string) llm.Message {
	t.Helper()
	for _, message := range messages {
		if strings.Contains(message.Content, body) {
			return message
		}
	}
	t.Fatalf("matching parent run received no team lead notification containing %q: %+v", body, messages)
	return llm.Message{}
}
