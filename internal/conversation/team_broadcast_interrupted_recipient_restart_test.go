package conversation

import (
	"context"
	"encoding/json"
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

const teamBroadcastRestartBody = "broadcast-restart-parser-findings"

type broadcastRestartChildRunner struct {
	started        chan agent.ChildRunInput
	interruptedID  string
	interruptedOne bool
}

func (r *broadcastRestartChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- input
	if input.TeamTurn != nil && input.TeamTurn.MemberID == r.interruptedID && strings.Contains(input.Task.Instruction, teamBroadcastRestartBody) && !r.interruptedOne {
		r.interruptedOne = true
		return agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: "fixture interruption after broadcast handoff"}
	}
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "member findings complete"}
}

func TestInterruptedBroadcastRecipientRetriesOnlyItsSnapshotDeliveryAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "broadcast-restart-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{
		RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root,
	})
	service.activeRequests = map[string]agent.ExecutionRequest{request.RunID: request}
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &broadcastRestartChildRunner{started: make(chan agent.ChildRunInput, 4)}
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
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "broadcast-restart")
	if err != nil {
		t.Fatal(err)
	}
	memberA, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-a", AgentName: role.Name, Instruction: "Inspect parser area A.", OriginCallID: "spawn-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.interruptedID = memberA.ID
	firstA := receiveTeamChildInput(t, runner.started)
	if firstA.TeamTurn == nil || firstA.TeamTurn.MemberID != memberA.ID {
		t.Fatalf("unexpected first member A turn: %+v", firstA.TeamTurn)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberIdle)
	memberB, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader-b", AgentName: role.Name, Instruction: "Inspect parser area B.", OriginCallID: "spawn-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstB := receiveTeamChildInput(t, runner.started)
	if firstB.TeamTurn == nil || firstB.TeamTurn.MemberID != memberB.ID {
		t.Fatalf("unexpected first member B turn: %+v", firstB.TeamTurn)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberIdle)

	broadcast, err := service.SendTeamMessage(t.Context(), request, TeamSendRequest{
		TeamID: team.ID, Broadcast: true, Body: teamBroadcastRestartBody, Token: "broadcast-restart-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(broadcast.Recipients) != 2 || !containsString(broadcast.Recipients, memberA.ID) || !containsString(broadcast.Recipients, memberB.ID) {
		t.Fatalf("broadcast did not snapshot both active members: %+v", broadcast)
	}
	var destinationA, destinationB agent.ChildRunInput
	for range 2 {
		input := receiveTeamChildInput(t, runner.started)
		if input.TeamTurn == nil || strings.Count(input.Task.Instruction, teamBroadcastRestartBody) != 1 {
			t.Fatalf("broadcast was missing or duplicated in destination input: %+v", input)
		}
		switch input.TeamTurn.MemberID {
		case memberA.ID:
			destinationA = input
		case memberB.ID:
			destinationB = input
		default:
			t.Fatalf("broadcast started unexpected recipient: %+v", input.TeamTurn)
		}
	}
	if destinationA.TeamTurn == nil || destinationB.TeamTurn == nil {
		t.Fatalf("broadcast did not start one destination turn per recipient: A=%+v B=%+v", destinationA.TeamTurn, destinationB.TeamTurn)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberInterrupted)
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberB.ID, teams.MemberIdle)
	if !runner.interruptedOne {
		t.Fatal("fixture did not interrupt member A's broadcast destination")
	}

	service.teamScheduler.close()
	pool.Close()
	if err := recoverTeamRuns(root); err != nil {
		t.Fatalf("service restart recovery: %v", err)
	}
	if got, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID); err != nil {
		t.Fatal(err)
	} else if got.Members[memberA.ID].Status != teams.MemberInterrupted || got.Members[memberB.ID].Status != teams.MemberInterrupted {
		t.Fatalf("restart states = %s/%s, want both interrupted", got.Members[memberA.ID].Status, got.Members[memberB.ID].Status)
	}

	retryRunner := &capturingTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1)}
	retryPool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), retryRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Service{deps: Deps{
		ProjectRoot: root, Agents: fixedTeamRoleCatalog{definition: role}, Delegator: retryPool,
		ForkProvider: forkSkillFixtureProvider{}, ForkExecutorFactory: forkSkillFixtureExecutorFactory{},
		ForkToolSchemas: []llm.ToolSchema{{Name: "read_file"}}, ProviderName: "fixture", Model: "model-v1",
	}, lifeCtx: context.Background(), activeRuns: map[string]string{request.RunID: request.Work.SessionID}, activeRequests: map[string]agent.ExecutionRequest{request.RunID: request}}
	restarted.teamScheduler = newTeamScheduler(restarted)
	t.Cleanup(func() {
		restarted.teamScheduler.close()
		retryPool.Close()
	})
	if len(retryRunner.inputs) != 0 {
		t.Fatal("restart automatically reran a child before explicit resume")
	}
	if _, err := restarted.ResumeTeamMember(t.Context(), request, team.ID, memberA.ID, "retry-member-a"); err != nil {
		t.Fatalf("explicit resume of interrupted recipient: %v", err)
	}
	retry := receiveTeamChildInput(t, retryRunner.inputs)
	if retry.TeamTurn == nil || retry.TeamTurn.MemberID != memberA.ID || retry.TeamTurn.TurnID == destinationA.TeamTurn.TurnID || strings.Count(retry.Task.Instruction, teamBroadcastRestartBody) != 1 {
		t.Fatalf("member A retry did not carry exactly one copy of its broadcast delivery: %+v", retry)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, memberA.ID, teams.MemberIdle)
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Messages[broadcast.ID].Recipients) != 2 || !containsString(projection.Messages[broadcast.ID].Recipients, memberA.ID) || !containsString(projection.Messages[broadcast.ID].Recipients, memberB.ID) {
		t.Fatalf("restart changed the original broadcast recipient snapshot: %+v", projection.Messages[broadcast.ID])
	}
	var handoffsA, handoffsB []sessionlog.HandoffFact
	for _, handoff := range projection.Handoffs {
		if handoff.MessageID != broadcast.ID {
			continue
		}
		switch handoff.RecipientID {
		case memberA.ID:
			handoffsA = append(handoffsA, handoff)
		case memberB.ID:
			handoffsB = append(handoffsB, handoff)
		default:
			t.Fatalf("broadcast handoff was delivered to an unsnapshotted recipient: %+v", handoff)
		}
	}
	if len(handoffsA) != 2 || handoffsA[0].DestinationTurnID != destinationA.TeamTurn.TurnID || handoffsA[1].DestinationTurnID != retry.TeamTurn.TurnID || handoffsA[1].RetryOfTurnID != destinationA.TeamTurn.TurnID {
		t.Fatalf("member A broadcast retry chain=%+v", handoffsA)
	}
	if len(handoffsB) != 1 || handoffsB[0].DestinationTurnID != destinationB.TeamTurn.TurnID || handoffsB[0].RetryOfTurnID != "" {
		t.Fatalf("member B successful broadcast delivery was duplicated or retried: %+v", handoffsB)
	}
}
