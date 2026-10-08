package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"stable/internal/llm"
	"stable/internal/teams"
)

func validTeamTurnInput() TeamTurnInput {
	return TeamTurnInput{Identity: TeamTurnIdentity{TeamID: "team-1", MemberID: "member-1", TurnID: "turn-1", MemberName: "researcher"}, Summary: "Previous finding: config.yaml", Messages: []teams.Message{{ID: "message-1", TeamID: "team-1", SenderID: teams.Lead, Recipients: []string{"member-1"}, Body: "Inspect the parser next."}}}
}

func TestTeamTurnInputIsBoundedReferenceDataAndPreservesFullRole(t *testing.T) {
	input := validTeamTurnInput()
	role := "Use concrete file paths and report parser behavior."
	task, err := BuildTeamTurnTask("task-1", role, input)
	if err != nil {
		t.Fatal(err)
	}
	if task.Name != "researcher" || !strings.HasPrefix(task.Instruction, role+"\n") || !strings.Contains(task.Instruction, input.Summary) || !strings.Contains(task.Instruction, input.Messages[0].Body) {
		t.Fatalf("incorrect team input: %+v", task)
	}
	input.PlanFeedback = "Clarify the parser entry point."
	task, err = BuildTeamTurnTask("task-1", role, input)
	if err != nil || !strings.Contains(task.Instruction, input.PlanFeedback) {
		t.Fatalf("plan feedback was not carried as bounded reference data: task=%+v err=%v", task, err)
	}
	for _, change := range []func(*TeamTurnInput){
		func(i *TeamTurnInput) { i.Identity.MemberID = teams.Lead },
		func(i *TeamTurnInput) { i.Messages[0].TeamID = "team-other" },
		func(i *TeamTurnInput) { i.Messages[0].Recipients = []string{"member-other"} },
		func(i *TeamTurnInput) { i.Messages[0].Body = strings.Repeat("x", teams.MaxMessageBytes+1) },
		func(i *TeamTurnInput) { i.Messages = append(i.Messages, i.Messages[0]) },
		func(i *TeamTurnInput) { i.Summary = strings.Repeat("x", teams.MaxSummaryBytes+1) },
		func(i *TeamTurnInput) { i.PlanFeedback = strings.Repeat("x", teams.MaxFeedbackBytes+1) },
	} {
		bad := validTeamTurnInput()
		change(&bad)
		if _, err := BuildTeamTurnTask("task-1", role, bad); err == nil {
			t.Fatal("invalid scoped/oversized team reference data accepted")
		}
	}
	if _, err := BuildTeamTurnTask("task-1", strings.Repeat("x", teams.MaxInputBytes), input); err == nil {
		t.Fatal("encoded role + task exceeds 64 KiB but was accepted")
	}
	batch := validTeamTurnInput()
	batch.Messages = nil
	for i := 0; i < 5; i++ {
		message := input.Messages[0]
		message.ID = "message-" + string(rune('a'+i))
		message.Body = strings.Repeat("x", teams.MaxMessageBytes)
		batch.Messages = append(batch.Messages, message)
	}
	if _, err := BuildTeamTurnTask("task-1", role, batch); err == nil {
		t.Fatal("message batch exceeded 32 KiB")
	}
}

func TestTeamMemberHardExecutorAllowsOnlyScopedTeamAndRoleInspectionTools(t *testing.T) {
	inspection := &captureChildExecutor{}
	var hostCalls []string
	factory, err := NewTeamMemberExecutorFactory(captureChildFactory{exec: inspection}, []string{"read_file"}, func(_ context.Context, request ExecutionRequest, call llm.ToolUse) (ToolOutcome, error) {
		if request.RunID != "child-1" {
			t.Fatal("host tool lost trusted child request")
		}
		hostCalls = append(hostCalls, call.Name)
		return ToolOutcome{CallID: call.ID, ToolName: call.Name, Status: ToolSucceeded}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := factory.ForRun(ExecutionRequest{RunID: "child-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read_file", "team_send", "team_task_update", "team_plan_submit"} {
		outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "call", Name: name})
		if err != nil || outcome.Status != ToolSucceeded {
			t.Fatalf("allowed %s failed: %+v %v", name, outcome, err)
		}
	}
	for _, name := range []string{"grep", "glob", "write_file", "command", "mcp_call", "run_agent", "delegate_tasks", "team_create", "team_member_spawn", "team_member_resume", "team_close", "team_shutdown_request"} {
		outcome, err := executor.Execute(context.Background(), llm.ToolUse{ID: "call", Name: name})
		if err != nil || outcome.Status != ToolDenied || !outcome.IsError {
			t.Fatalf("forbidden %s was dispatched: %+v %v", name, outcome, err)
		}
	}
	if len(inspection.calls) != 1 || inspection.calls[0] != "read_file" || len(hostCalls) != 3 {
		t.Fatalf("unsafe dispatch: inspection=%v host=%v", inspection.calls, hostCalls)
	}
	schemas, err := TeamMemberToolSchemas([]llm.ToolSchema{{Name: "read_file"}, {Name: "grep"}, {Name: "team_send"}, {Name: "team_member_spawn"}, {Name: "command"}, {Name: "team_send"}}, []string{"read_file"})
	if err != nil || len(schemas) != 2 || schemas[0].Name != "read_file" || schemas[1].Name != "team_send" {
		t.Fatalf("member schemas=%v err=%v", schemas, err)
	}
	if _, err := TeamMemberToolSchemas(nil, []string{"command"}); err == nil {
		t.Fatal("inspection role could request command")
	}
}

func TestStreamingTeamTurnGetsMemberGuidanceAndRetainsRolePrivacy(t *testing.T) {
	input := validTeamTurnInput()
	role := "PRIVATE_TEAM_ROLE_MARKER must never appear in public results."
	task, err := BuildTeamTurnTask("task-1", role, input)
	if err != nil {
		t.Fatal(err)
	}
	var first llm.Request
	provider := providerFunc(func(_ context.Context, request llm.Request) (<-chan llm.Event, <-chan error) {
		first = request
		events := make(chan llm.Event, 3)
		events <- llm.Event{Kind: llm.ThinkingDelta, Text: "PRIVATE_THINKING_MARKER"}
		events <- llm.Event{Kind: llm.TextDelta, Text: "Found parser.go. " + role}
		events <- llm.Event{Kind: llm.StreamEnd}
		close(events)
		errs := make(chan error)
		close(errs)
		return events, errs
	})
	identity := input.Identity
	result := (StreamingChildRunner{}).Run(context.Background(), ChildRunInput{TeamTurn: &identity, ChildRunID: "child", Work: WorkRef{Kind: WorkSession, SessionID: "session"}, Task: task, RoleInstruction: role, ProjectRoot: "/project", Provider: provider, Model: "fake", PermissionBounds: json.RawMessage(`{"run_id":"child"}`), Budget: DefaultDelegationLimits(), ExecutorFactory: captureChildFactory{exec: &captureChildExecutor{}}})
	if result.Status != DelegationSucceeded || strings.Contains(result.Summary, "PRIVATE_") || !strings.Contains(result.Summary, "parser.go") {
		t.Fatalf("team result privacy: %+v", result)
	}
	if len(first.Messages) != 2 || !strings.Contains(first.Messages[0].Content, "scoped team") || !strings.Contains(first.Messages[0].Content, "does not grant write") || first.Messages[1].Content != task.Instruction {
		t.Fatalf("wrong team provider input: %+v", first.Messages)
	}
}
