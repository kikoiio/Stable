package conversation

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
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// A message sent while a resumed turn is durably committing its selected
// handoff batch must remain pending for the next explicit turn.
func TestTeamMessageSentDuringHandoffCommitRemainsPendingForNextTurn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "handoff-race-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 4}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 3), release: make(chan struct{}, 3)}
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
		for range 3 {
			select {
			case runner.release <- struct{}{}:
			default:
			}
		}
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "handoff-race")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect area one.", OriginCallID: "spawn-handoff-race",
	})
	if err != nil {
		t.Fatal(err)
	}
	initial := receiveTeamChildInput(t, runner.inputs)
	if initial.TeamTurn == nil || initial.TeamTurn.MemberID != member.ID {
		t.Fatalf("initial turn identity=%+v", initial.TeamTurn)
	}

	firstMessage, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
		TeamID: team.ID, Recipient: member.ID, Body: "first queued message", Token: "handoff-race-first",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)

	appendEntered := make(chan sessionlog.HandoffFact, 1)
	releaseAppend := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseAppend:
		default:
			close(releaseAppend)
		}
	})
	service.teamScheduler.appendHandoff = func(eventRoot, sessionID, teamID string, handoff sessionlog.HandoffFact) error {
		appendEntered <- handoff
		<-releaseAppend
		return appendTeamFactLocked(eventRoot, sessionID, teamID, sessionlog.TeamEvent{
			Kind: sessionlog.TeamMessageHandoff, ActorID: "service", ActorRunID: request.RunID, Handoff: &handoff,
		})
	}

	type resumeResult struct {
		member teams.Member
		err    error
	}
	resumeDone := make(chan resumeResult, 1)
	go func() {
		resumed, resumeErr := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-handoff-race-first")
		resumeDone <- resumeResult{member: resumed, err: resumeErr}
	}()
	var firstCommit sessionlog.HandoffFact
	select {
	case firstCommit = <-appendEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("resume did not reach the durable handoff barrier")
	}
	if firstCommit.MessageID != firstMessage.ID || firstCommit.RecipientID != member.ID {
		t.Fatalf("first batch handoff=%+v, want message %s for member %s", firstCommit, firstMessage.ID, member.ID)
	}

	secondMessageStarted := make(chan struct{})
	type messageSendResult struct {
		message teams.Message
		err     error
	}
	secondMessageDone := make(chan messageSendResult, 1)
	go func() {
		close(secondMessageStarted)
		message, sendErr := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
			TeamID: team.ID, Recipient: member.ID, Body: "second concurrent message", Token: "handoff-race-second",
		})
		secondMessageDone <- messageSendResult{message: message, err: sendErr}
	}()
	select {
	case <-secondMessageStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent message sender did not start")
	}
	// Keep the append barrier held across the message call so the test creates
	// an explicit overlap instead of depending on scheduler timing.
	var earlySend *messageSendResult
	select {
	case result := <-secondMessageDone:
		earlySend = &result
		// Some append implementations may permit the message fact before the
		// handoff fact; either order must still keep it out of the fixed batch.
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseAppend)

	var resumed resumeResult
	select {
	case resumed = <-resumeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("resume did not finish after releasing the handoff barrier")
	}
	if resumed.err != nil || resumed.member.ID != member.ID {
		t.Fatalf("first resumed turn=%+v err=%v", resumed.member, resumed.err)
	}
	var secondMessage teams.Message
	if earlySend != nil {
		if earlySend.err != nil {
			t.Fatalf("second concurrent message send: %v", earlySend.err)
		}
		secondMessage = earlySend.message
	} else {
		select {
		case result := <-secondMessageDone:
			if result.err != nil {
				t.Fatalf("second concurrent message send: %v", result.err)
			}
			secondMessage = result.message
		case <-time.After(3 * time.Second):
			t.Fatal("second message did not finish after the handoff commit")
		}
	}
	firstFollowUp := receiveTeamChildInput(t, runner.inputs)
	if firstFollowUp.TeamTurn == nil || firstFollowUp.TeamTurn.MemberID != member.ID || firstFollowUp.TeamTurn.TurnID == initial.TeamTurn.TurnID {
		t.Fatalf("first follow-up turn identity=%+v", firstFollowUp.TeamTurn)
	}
	if !strings.Contains(firstFollowUp.Task.Instruction, firstMessage.Body) || strings.Contains(firstFollowUp.Task.Instruction, secondMessage.Body) {
		t.Fatalf("first follow-up did not contain only its fixed message batch: %q", firstFollowUp.Task.Instruction)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := projection.Messages[secondMessage.ID]; !exists {
		t.Fatalf("second message was lost from the durable projection: %+v", projection.Messages)
	}
	if projection.Members[member.ID].Status != teams.MemberRunning {
		t.Fatalf("member stopped or idled before the first follow-up completed: %+v", projection.Members[member.ID])
	}
	for _, handoff := range projection.Handoffs {
		if handoff.MessageID == secondMessage.ID {
			t.Fatalf("second message was handed off into the already selected turn: %+v", handoff)
		}
	}

	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)
	if _, err := service.ResumeTeamMember(t.Context(), request, team.ID, member.ID, "resume-handoff-race-second"); err != nil {
		t.Fatal(err)
	}
	secondFollowUp := receiveTeamChildInput(t, runner.inputs)
	if secondFollowUp.TeamTurn == nil || secondFollowUp.TeamTurn.MemberID != member.ID || secondFollowUp.TeamTurn.TurnID == firstFollowUp.TeamTurn.TurnID {
		t.Fatalf("second follow-up turn identity=%+v", secondFollowUp.TeamTurn)
	}
	if strings.Count(secondFollowUp.Task.Instruction, secondMessage.Body) != 1 || strings.Contains(secondFollowUp.Task.Instruction, firstMessage.Body) {
		t.Fatalf("second follow-up lost, duplicated, or redelivered a message: %q", secondFollowUp.Task.Instruction)
	}
	runner.release <- struct{}{}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberIdle)

	history, err := sessionlog.TeamHistory(root, request.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	handoffs := map[string][]sessionlog.HandoffFact{}
	for _, event := range history {
		if event.Type != sessionlog.EventTeam {
			continue
		}
		var fact sessionlog.TeamEvent
		if err := decodeSessionData(event.Data, &fact); err != nil {
			t.Fatal(err)
		}
		if fact.Kind == sessionlog.TeamMessageHandoff && fact.Handoff != nil {
			handoffs[fact.Handoff.MessageID] = append(handoffs[fact.Handoff.MessageID], *fact.Handoff)
		}
	}
	if got := handoffs[firstMessage.ID]; len(got) != 1 || got[0].DestinationTurnID != firstFollowUp.TeamTurn.TurnID {
		t.Fatalf("first message handoff=%+v, want one handoff to %s", got, firstFollowUp.TeamTurn.TurnID)
	}
	if got := handoffs[secondMessage.ID]; len(got) != 1 || got[0].DestinationTurnID != secondFollowUp.TeamTurn.TurnID {
		t.Fatalf("second message handoff=%+v, want one handoff to %s", got, secondFollowUp.TeamTurn.TurnID)
	}
}
