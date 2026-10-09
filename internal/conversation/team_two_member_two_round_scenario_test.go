package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

type twoRoundScenarioRunner struct {
	inputs  chan agent.ChildRunInput
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (r *twoRoundScenarioRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.mu.Lock()
	r.calls++
	call := r.calls
	r.mu.Unlock()
	r.inputs <- input
	if call <= 2 {
		<-r.release
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: fmt.Sprintf("result-%s-round-%d", input.TeamTurn.MemberName, (call+1)/2)}
}

func TestTwoMemberTwoRoundMessagingScenarioKeepsParentRunAndRoutesNextTurn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "two-round-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	const parentMarker = "PARENT_STREAM_EVENT_DURING_CHILD_TURNS"
	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Text: parentMarker}); err != nil {
		t.Fatal(err)
	}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Investigate only your assigned area and report concise findings.", Model: "inherit", Tools: []string{"read_file", "team_send"}, MaxTurns: 3}
	runner := &twoRoundScenarioRunner{inputs: make(chan agent.ChildRunInput, 4), release: make(chan struct{}, 2)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}, {Name: "team_send"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		runner.release <- struct{}{}
		runner.release <- struct{}{}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "two-round-investigation")
	if err != nil {
		t.Fatal(err)
	}
	memberA, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "parser-reader", AgentName: role.Name, Instruction: "Investigate parser recovery.", OriginCallID: "spawn-parser"})
	if err != nil {
		t.Fatal(err)
	}
	memberB, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{TeamID: team.ID, Name: "recovery-reviewer", AgentName: role.Name, Instruction: "Review recovery edge cases.", OriginCallID: "spawn-reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	firstByMember := map[string]agent.ChildRunInput{}
	for range 2 {
		input := receiveTeamChildInput(t, runner.inputs)
		if input.TeamTurn == nil || input.ParentRunID != request.RunID {
			t.Fatalf("first round was not bound to the matching parent run: %+v", input)
		}
		firstByMember[input.TeamTurn.MemberID] = input
	}
	firstA, okA := firstByMember[memberA.ID]
	firstB, okB := firstByMember[memberB.ID]
	if !okA || !okB || firstA.TeamTurn.TurnID == firstB.TeamTurn.TurnID {
		t.Fatalf("two independent member turns did not start: A=%+v B=%+v", firstA.TeamTurn, firstB.TeamTurn)
	}
	if !strings.Contains(firstA.Task.Instruction, "Investigate parser recovery") || strings.Contains(firstA.Task.Instruction, "Review recovery edge cases") || strings.Contains(firstA.Task.Instruction, parentMarker) {
		t.Fatalf("parser member received sibling or parent conversation content: %q", firstA.Task.Instruction)
	}
	if !strings.Contains(firstB.Task.Instruction, "Review recovery edge cases") || strings.Contains(firstB.Task.Instruction, "Investigate parser recovery") || strings.Contains(firstB.Task.Instruction, parentMarker) {
		t.Fatalf("review member received sibling or parent conversation content: %q", firstB.Task.Instruction)
	}

	p2p, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{TeamID: team.ID, Recipient: memberB.ID, Body: "Cross-check the parser finding.", Token: "scenario-p2p"})
	if err != nil {
		t.Fatal(err)
	}
	broadcast, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{TeamID: team.ID, Broadcast: true, Body: "Compare findings before reporting.", Token: "scenario-broadcast"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2p.Recipients) != 1 || p2p.Recipients[0] != memberB.ID || len(broadcast.Recipients) != 2 || !containsString(broadcast.Recipients, memberA.ID) || !containsString(broadcast.Recipients, memberB.ID) || broadcast.Seq <= p2p.Seq {
		t.Fatalf("message routing/order = p2p %+v, broadcast %+v", p2p, broadcast)
	}

	// A child can report to the lead while the ordinary parent run remains live.
	memberRequest := agent.ExecutionRequest{RunID: firstA.ChildRunID, Work: request.Work, TeamTurn: firstA.TeamTurn}
	args, err := json.Marshal(map[string]any{"team_id": team.ID, "recipient": teams.Lead, "body": "Parser recovery path is verified."})
	if err != nil {
		t.Fatal(err)
	}
	toolResult, err := service.ExecuteTeamTool(t.Context(), memberRequest, llm.ToolUse{ID: "scenario-result-to-lead", Name: "team_send", Arguments: args})
	if err != nil || toolResult.Status != agent.ToolSucceeded || toolResult.IsError {
		t.Fatalf("member result to lead = %+v, %v", toolResult, err)
	}
	if _, err := sessionlog.Append(root, request.Work.SessionID, sessionlog.EventMessage, sessionlog.Message{Role: "assistant", Text: "Parent run continues while team members investigate."}); err != nil {
		t.Fatal(err)
	}

	runner.release <- struct{}{}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberIdle)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberIdle)
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, memberA.ID, "scenario-resume-parser"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, memberB.ID, "scenario-resume-reviewer"); err != nil {
		t.Fatal(err)
	}
	secondByMember := map[string]agent.ChildRunInput{}
	for range 2 {
		input := receiveTeamChildInput(t, runner.inputs)
		secondByMember[input.TeamTurn.MemberID] = input
	}
	secondA, okA := secondByMember[memberA.ID]
	secondB, okB := secondByMember[memberB.ID]
	if !okA || !okB || secondA.TeamTurn.TurnID == firstA.TeamTurn.TurnID || secondB.TeamTurn.TurnID == firstB.TeamTurn.TurnID {
		t.Fatalf("second round did not create a fresh turn for each member: A=%+v B=%+v", secondA.TeamTurn, secondB.TeamTurn)
	}
	if !strings.Contains(secondA.Task.Instruction, "result-parser-reader-round-1") || !strings.Contains(secondA.Task.Instruction, broadcast.Body) || strings.Contains(secondA.Task.Instruction, p2p.Body) {
		t.Fatalf("parser member second round did not receive only its summary and broadcast: %q", secondA.Task.Instruction)
	}
	if !strings.Contains(secondB.Task.Instruction, "result-recovery-reviewer-round-1") || !strings.Contains(secondB.Task.Instruction, p2p.Body) || !strings.Contains(secondB.Task.Instruction, broadcast.Body) {
		t.Fatalf("review member second round did not receive its summary and ordered p2p/broadcast messages: %q", secondB.Task.Instruction)
	}
	if strings.Contains(secondA.Task.Instruction, parentMarker) || strings.Contains(secondB.Task.Instruction, parentMarker) || strings.Contains(secondB.Task.Instruction, "Investigate parser recovery") {
		t.Fatal("a second-round child received parent history or sibling task context")
	}
	// Receiving ChildRunInput only proves the scheduler dispatched the round.
	// Wait for both child terminal facts before temp-project cleanup, or the
	// asynchronous team watcher can still append while testing.T removes root.
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberIdle)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberIdle)

	facts, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	var sentMessages []teams.Message
	for _, event := range facts {
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageSent && fact.Message != nil {
			sentMessages = append(sentMessages, *fact.Message)
		}
	}
	if len(sentMessages) != 3 {
		t.Fatalf("durable team message count=%d, want p2p, broadcast, and member result: %+v", len(sentMessages), sentMessages)
	}
	var leadResult teams.Message
	for _, message := range sentMessages {
		if message.Body == "Parser recovery path is verified." {
			leadResult = message
		}
	}
	if leadResult.ID == "" || leadResult.SenderID != memberA.ID || len(leadResult.Recipients) != 1 || leadResult.Recipients[0] != teams.Lead {
		t.Fatalf("member result was not durably routed to lead: %+v", leadResult)
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	parentStarts := 0
	for _, event := range transcript.Events {
		if event.Type == sessionlog.EventRunStarted {
			var started sessionlog.RunStarted
			if err := decodeSessionData(event.Data, &started); err != nil {
				t.Fatal(err)
			}
			if started.RunID == request.RunID {
				parentStarts++
			}
		}
	}
	if parentStarts != 1 {
		t.Fatalf("team messaging started or duplicated the ordinary parent run: starts=%d", parentStarts)
	}
}
