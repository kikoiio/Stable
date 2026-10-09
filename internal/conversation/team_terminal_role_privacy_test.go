package conversation

import (
	"context"
	"encoding/json"
	"errors"
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

const teamTerminalRoleSentinel = "PRIVATE TEAM ROLE BODY: " + "inspect the unreleased subsystem roadmap and preserve the internal escalation rules; unique-boundary-7a9c31."

type roleEchoingTeamChildRunner struct{}

func (roleEchoingTeamChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	return agent.ChildRunResult{
		Status:  agent.DelegationFailed,
		Summary: "safe summary before: " + input.RoleInstruction + " : safe summary after",
		Error:   "safe error before: " + input.RoleInstruction + " : safe error after",
	}
}

type closedTeamRolePrivacyPublisher struct{}

func (closedTeamRolePrivacyPublisher) PublishDelegation(string, agent.DelegationEvent) error {
	return errors.New("test event sink is closed")
}

func (closedTeamRolePrivacyPublisher) Start(context.Context, agent.ExecutionRequest) (*agent.RunHandle, error) {
	return nil, errors.New("unexpected parent run start")
}

func (closedTeamRolePrivacyPublisher) Cancel(string) error { return nil }

func TestTeamChildTerminalRoleBodyIsRedactedFromDurableOutputs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "role-privacy-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	role := agentcatalog.Definition{Name: "explore", Instruction: teamTerminalRoleSentinel, Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 1}
	reporter := NewDelegationEventReporter()
	reporter.Bind(service)
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), roleEchoingTeamChildRunner{}, reporter)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.Runner = closedTeamRolePrivacyPublisher{}
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

	team, err := service.CreateTeam(t.Context(), request, "terminal-role-privacy")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "call-role-privacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberInterrupted)

	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encodedEvents, err := json.Marshal(transcript.Events)
	if err != nil {
		t.Fatal(err)
	}
	path, err := sessionlog.SessionPath(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	turn := projection.Turns[member.TurnID]
	if turn.Status != "failed" {
		t.Fatalf("terminal turn status = %q, want failed", turn.Status)
	}
	encodedTurn, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	for label, data := range map[string][]byte{
		"raw session log":       raw,
		"replayed event stream": encodedEvents,
		"team turn projection":  encodedTurn,
	} {
		if strings.Contains(string(data), teamTerminalRoleSentinel) {
			t.Errorf("%s contains the private role body", label)
		}
	}
	for label, data := range map[string][]byte{"raw session log": raw, "replayed event stream": encodedEvents} {
		for _, safe := range []string{"safe summary before:", "safe summary after", "safe error before:", "safe error after", "[agent role instructions omitted]"} {
			if !strings.Contains(string(data), safe) {
				t.Errorf("%s lost safe child output %q", label, safe)
			}
		}
	}
	for label, text := range map[string]string{"team summary": turn.Summary, "team error": turn.Error} {
		if !strings.Contains(text, "[agent role instructions omitted]") {
			t.Errorf("%s lacks the role omission marker: %q", label, text)
		}
	}
	if !strings.Contains(turn.Summary, "safe summary before:") || !strings.Contains(turn.Summary, "safe summary after") {
		t.Errorf("team summary lost safe context: %q", turn.Summary)
	}
	if !strings.Contains(turn.Error, "safe error before:") || !strings.Contains(turn.Error, "safe error after") {
		t.Errorf("team error lost safe context: %q", turn.Error)
	}

	// Delegation events are also user-readable run notifications. The parent
	// event is emitted only after PoolDelegator terminal sanitization.
	var parentDelegation string
	for _, event := range transcript.Events {
		if event.Type != sessionlog.EventRunEvent {
			continue
		}
		var runEvent sessionlog.RunEvent
		if err := decodeSessionData(event.Data, &runEvent); err != nil || runEvent.RunID != request.RunID || runEvent.Kind != string(agent.EventDelegation) {
			continue
		}
		payload, marshalErr := json.Marshal(runEvent.Payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		parentDelegation += string(payload)
	}
	if parentDelegation == "" {
		t.Fatal("parent run did not persist a terminal delegation notification")
	}
	if strings.Contains(parentDelegation, teamTerminalRoleSentinel) {
		t.Fatal("parent delegation notification contains the private role body")
	}
	if !strings.Contains(parentDelegation, "safe summary before:") || !strings.Contains(parentDelegation, "safe error before:") {
		t.Fatalf("parent delegation notification lost safe child context: %s", parentDelegation)
	}
	for _, safe := range []string{"safe summary after", "safe error after", "[agent role instructions omitted]"} {
		if !strings.Contains(parentDelegation, safe) {
			t.Errorf("parent delegation notification lost safe output %q: %s", safe, parentDelegation)
		}
	}
}
