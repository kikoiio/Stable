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

const teamTerminalCredentialSentinel = "sk-team-terminal-fixture-credential-0123456789"

type credentialEchoingTeamChildRunner struct{}

func (credentialEchoingTeamChildRunner) Run(context.Context, agent.ChildRunInput) agent.ChildRunResult {
	return agent.ChildRunResult{
		Status:  agent.DelegationFailed,
		Summary: "child summary contains " + teamTerminalCredentialSentinel,
		Error:   "child error contains " + teamTerminalCredentialSentinel,
	}
}

func TestTeamChildTerminalCredentialIsRedactedFromSessionLogAndProjection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "privacy-parent")
	request.PermissionBounds, _ = json.Marshal(permission.Authority{RunID: request.RunID, SessionID: request.Work.SessionID, AllowedRoot: root})
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 1}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), credentialEchoingTeamChildRunner{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.deps.Agents = fixedTeamRoleCatalog{definition: role}
	service.deps.Delegator = pool
	service.deps.ForkProvider = forkSkillFixtureProvider{}
	service.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	service.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	service.deps.ProviderName, service.deps.Model = "fixture", "model-v1"
	service.deps.ProviderCredential = teamTerminalCredentialSentinel
	service.lifeCtx = context.Background()
	service.teamScheduler = newTeamScheduler(service)
	t.Cleanup(func() {
		service.teamScheduler.close()
		pool.Close()
	})

	team, err := service.CreateTeam(t.Context(), request, "terminal-credential-privacy")
	if err != nil {
		t.Fatal(err)
	}
	member, err := service.SpawnTeamMember(t.Context(), request, TeamMemberSpawnRequest{
		TeamID: team.ID, Name: "reader", AgentName: role.Name, Instruction: "Inspect the area.", OriginCallID: "call-spawn",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTeamMemberStatus(t, root, request.Work.SessionID, team.ID, member.ID, teams.MemberInterrupted)

	path, err := sessionlog.SessionPath(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), teamTerminalCredentialSentinel) {
		t.Fatal("provider credential was persisted in child terminal events")
	}
	transcript, err := sessionlog.Replay(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(transcript.Events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), teamTerminalCredentialSentinel) {
		t.Fatal("provider credential remained in replayed child terminal events")
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	turn := projection.Turns[member.TurnID]
	if strings.Contains(turn.Summary, teamTerminalCredentialSentinel) || strings.Contains(turn.Error, teamTerminalCredentialSentinel) {
		t.Fatalf("provider credential remained in projected terminal result: %+v", turn)
	}
	if !strings.Contains(turn.Summary, "[credential redacted]") || !strings.Contains(turn.Error, "[credential redacted]") {
		t.Fatalf("redacted terminal result lost its safe context: %+v", turn)
	}
}
