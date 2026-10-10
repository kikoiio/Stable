package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestTeamMessageHandoffDrainsPendingMessagesInOrderedBoundedBatches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "message-batch-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 4}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 4), release: make(chan struct{}, 4)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		for range 4 {
			runner.release <- struct{}{}
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "message-batch-boundary")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect area one.", OriginCallID: "spawn-message-batch",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != member.ID {
		t.Fatalf("initial turn identity = %+v", first.TeamTurn)
	}

	messages := make([]teams.Message, teams.MaxBatchMessages+1)
	for i := range messages {
		body := fmt.Sprintf("pending batch marker %02d", i)
		message, sendErr := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
			TeamID: team.ID, Recipient: member.ID, Body: body, Token: fmt.Sprintf("batch-message-%02d", i),
		})
		if sendErr != nil {
			t.Fatalf("send message %d: %v", i, sendErr)
		}
		messages[i] = message
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)

	if _, err = service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-message-batch-1"); err != nil {
		t.Fatal(err)
	}
	second := receiveTeamChildInput(t, runner.inputs)
	if second.TeamTurn == nil || second.TeamTurn.MemberID != member.ID || second.TeamTurn.TurnID == first.TeamTurn.TurnID {
		t.Fatalf("first bounded follow-up identity = %+v", second.TeamTurn)
	}
	previousIndex := -1
	for i := 0; i < teams.MaxBatchMessages; i++ {
		currentIndex := strings.Index(second.Task.Instruction, messages[i].Body)
		if currentIndex < 0 {
			t.Fatalf("ordered batch omitted pending message %d", i)
		}
		if currentIndex <= previousIndex || strings.Count(second.Task.Instruction, messages[i].Body) != 1 {
			t.Fatalf("pending message %d was reordered or duplicated", i)
		}
		previousIndex = currentIndex
	}
	if strings.Contains(second.Task.Instruction, messages[teams.MaxBatchMessages].Body) {
		t.Fatal("message beyond the batch-count limit was handed off early")
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)

	assertHandoffs := func(wantIDs, wantDestinations []string) {
		t.Helper()
		facts, historyErr := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
		if historyErr != nil {
			t.Fatal(historyErr)
		}
		var got, gotDestinations []string
		for _, event := range facts {
			var fact sessionlog.TeamEvent
			if err := decodeSessionData(event.Data, &fact); err != nil {
				t.Fatal(err)
			}
			if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil && fact.Handoff.RecipientID == member.ID {
				got = append(got, fact.Handoff.MessageID)
				gotDestinations = append(gotDestinations, fact.Handoff.DestinationTurnID)
			}
		}
		if len(got) != len(wantIDs) || len(wantDestinations) != len(wantIDs) {
			t.Fatalf("durable message handoffs=%v destinations=%v, want %v / %v", got, gotDestinations, wantIDs, wantDestinations)
		}
		for i := range got {
			if got[i] != wantIDs[i] || gotDestinations[i] != wantDestinations[i] {
				t.Fatalf("durable message handoffs=%v destinations=%v, want ordered %v / %v", got, gotDestinations, wantIDs, wantDestinations)
			}
		}
	}
	firstBatchIDs := make([]string, teams.MaxBatchMessages)
	firstBatchDestinations := make([]string, teams.MaxBatchMessages)
	for i := range firstBatchIDs {
		firstBatchIDs[i] = messages[i].ID
		firstBatchDestinations[i] = second.TeamTurn.TurnID
	}
	assertHandoffs(firstBatchIDs, firstBatchDestinations)

	if _, err = service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-message-batch-2"); err != nil {
		t.Fatal(err)
	}
	third := receiveTeamChildInput(t, runner.inputs)
	if third.TeamTurn == nil || third.TeamTurn.MemberID != member.ID || third.TeamTurn.TurnID == second.TeamTurn.TurnID {
		t.Fatalf("second bounded follow-up identity = %+v", third.TeamTurn)
	}
	if strings.Count(third.Task.Instruction, messages[teams.MaxBatchMessages].Body) != 1 {
		t.Fatal("message left pending after first batch was lost")
	}
	for i := 0; i < teams.MaxBatchMessages; i++ {
		if strings.Contains(third.Task.Instruction, messages[i].Body) {
			t.Fatalf("message %d was handed off twice", i)
		}
	}
	allIDs := append(firstBatchIDs, messages[teams.MaxBatchMessages].ID)
	allDestinations := append(firstBatchDestinations, third.TeamTurn.TurnID)
	assertHandoffs(allIDs, allDestinations)
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
}

func TestTeamMessageHandoffBatchesAtThirtyTwoKiBBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "message-byte-batch-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 4}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 4), release: make(chan struct{}, 4)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		for range 4 {
			runner.release <- struct{}{}
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "message-byte-batch-boundary")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect area one.", OriginCallID: "spawn-message-byte-batch",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveTeamChildInput(t, runner.inputs)
	if first.TeamTurn == nil || first.TeamTurn.MemberID != member.ID {
		t.Fatalf("initial turn identity = %+v", first.TeamTurn)
	}

	messages := make([]teams.Message, 5)
	for i := range 4 {
		prefix := fmt.Sprintf("byte-batch-message-%d:", i)
		body := prefix + strings.Repeat(string(rune('a'+i)), teams.MaxMessageBytes-len(prefix))
		if len(body) != teams.MaxMessageBytes {
			t.Fatalf("message %d body bytes = %d, want %d", i, len(body), teams.MaxMessageBytes)
		}
		message, sendErr := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
			TeamID: team.ID, Recipient: member.ID, Body: body, Token: fmt.Sprintf("byte-batch-message-%d", i),
		})
		if sendErr != nil {
			t.Fatalf("send maximum-size message %d: %v", i, sendErr)
		}
		messages[i] = message
	}
	messages[4], err = service.SendTeamMessage(t.Context(), request, TeamSendRequest{
		TeamID: team.ID, Recipient: member.ID, Body: "message-after-byte-boundary", Token: "message-after-byte-boundary",
	})
	if err != nil {
		t.Fatal(err)
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if _, err = service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-byte-batch-1"); err != nil {
		t.Fatal(err)
	}
	second := receiveTeamChildInput(t, runner.inputs)
	if second.TeamTurn == nil || second.TeamTurn.MemberID != member.ID || second.TeamTurn.TurnID == first.TeamTurn.TurnID {
		t.Fatalf("first byte-bounded follow-up identity = %+v", second.TeamTurn)
	}
	previousIndex := -1
	for i := range 4 {
		currentIndex := strings.Index(second.Task.Instruction, messages[i].Body)
		if currentIndex < 0 || currentIndex <= previousIndex || strings.Count(second.Task.Instruction, messages[i].Body) != 1 {
			t.Fatalf("maximum-size message %d missing, reordered, or duplicated", i)
		}
		previousIndex = currentIndex
	}
	if strings.Contains(second.Task.Instruction, messages[4].Body) {
		t.Fatal("message beyond the 32 KiB batch limit was handed off early")
	}

	assertHandoffs := func(wantIDs, wantDestinations []string) {
		t.Helper()
		facts, historyErr := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
		if historyErr != nil {
			t.Fatal(historyErr)
		}
		var got, gotDestinations []string
		for _, event := range facts {
			var fact sessionlog.TeamEvent
			if err := decodeSessionData(event.Data, &fact); err != nil {
				t.Fatal(err)
			}
			if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil && fact.Handoff.RecipientID == member.ID {
				got = append(got, fact.Handoff.MessageID)
				gotDestinations = append(gotDestinations, fact.Handoff.DestinationTurnID)
			}
		}
		if len(got) != len(wantIDs) || len(gotDestinations) != len(wantIDs) {
			t.Fatalf("durable handoffs=%v destinations=%v, want %v / %v", got, gotDestinations, wantIDs, wantDestinations)
		}
		for i := range got {
			if got[i] != wantIDs[i] || gotDestinations[i] != wantDestinations[i] {
				t.Fatalf("durable handoffs=%v destinations=%v, want ordered %v / %v", got, gotDestinations, wantIDs, wantDestinations)
			}
		}
	}
	firstBatchIDs := make([]string, 4)
	firstBatchDestinations := make([]string, 4)
	for i := range firstBatchIDs {
		firstBatchIDs[i] = messages[i].ID
		firstBatchDestinations[i] = second.TeamTurn.TurnID
	}
	assertHandoffs(firstBatchIDs, firstBatchDestinations)

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if _, err = service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-byte-batch-2"); err != nil {
		t.Fatal(err)
	}
	third := receiveTeamChildInput(t, runner.inputs)
	if third.TeamTurn == nil || third.TeamTurn.MemberID != member.ID || third.TeamTurn.TurnID == second.TeamTurn.TurnID {
		t.Fatalf("second byte-bounded follow-up identity = %+v", third.TeamTurn)
	}
	if strings.Count(third.Task.Instruction, messages[4].Body) != 1 {
		t.Fatal("message excluded by byte boundary was lost or duplicated")
	}
	for i := range 4 {
		if strings.Contains(third.Task.Instruction, messages[i].Body) {
			t.Fatalf("maximum-size message %d was handed off twice", i)
		}
	}
	assertHandoffs(
		[]string{messages[0].ID, messages[1].ID, messages[2].ID, messages[3].ID, messages[4].ID},
		[]string{second.TeamTurn.TurnID, second.TeamTurn.TurnID, second.TeamTurn.TurnID, second.TeamTurn.TurnID, third.TeamTurn.TurnID},
	)
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
}
