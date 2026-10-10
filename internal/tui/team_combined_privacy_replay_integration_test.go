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
	teamCombinedPrivacyCredential = "sk-team-ui-combined-private-credential-0123456789"
	teamCombinedPrivacyRole       = "PRIVATE TEAM ROLE: inspect confidential escalation playbook 91efc2"
)

type combinedPrivacyTeamChildRunner struct{ started chan agent.ChildRunInput }

func (r *combinedPrivacyTeamChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- input
	return agent.ChildRunResult{
		Status:  agent.DelegationSucceeded,
		Summary: "safe finding before " + input.RoleInstruction + " and credential " + teamCombinedPrivacyCredential + " after",
	}
}

// Role instructions and provider credentials can both appear in child output.
// They must reach the child as appropriate, then stay redacted in persisted
// replay and the user-visible parent delegation transcript.
func TestTeamChildRoleAndCredentialStayPrivateAcrossReplayAndTranscript(t *testing.T) {
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
	childRunner := &combinedPrivacyTeamChildRunner{started: make(chan agent.ChildRunInput, 1)}
	limits := agent.DefaultDelegationLimits()
	limits.Workers, limits.QueueCapacity = 1, 1
	reporter := conversation.NewDelegationEventReporter()
	pool, err := agent.NewPoolDelegator(limits, childRunner, reporter)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	role := agentcatalog.Definition{
		Name: "combined-private-role", Description: "Read-only private role", Model: "inherit",
		Instruction: teamCombinedPrivacyRole, Tools: []string{"read_file"}, MaxTurns: 1,
	}
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socketDir, err := os.MkdirTemp(filepath.Join("..", "..", ".tmp"), "team-privacy-")
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
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ProviderCredential:  teamCombinedPrivacyCredential,
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
		Intent: "combined privacy integration", Messages: []llm.Message{{Role: "user", Content: "Run a private role and report its safe result."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	if started, err := parentStream.Receive(); err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("start parent run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID, model.Pending = sessionID, parentRunID, true
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create combined-privacy")
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
		if child.RoleInstruction != role.Instruction || !strings.Contains(child.Task.Instruction, teamCombinedPrivacyRole) {
			t.Fatalf("private role was not delivered to child: %+v", child)
		}
	case <-reqctx.Done():
		t.Fatal("private role child did not start")
	}
	if err := waitForTeamRolePrivacyIdle(project, sessionID, team.ID, member.ID); err != nil {
		t.Fatal(err)
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
	for surface, data := range map[string]string{
		"raw session log": string(rawLog), "replayed events": string(encodedEvents),
		"team projection": mustMarshalCombinedPrivacy(t, projection),
		"team turn":       mustMarshalCombinedPrivacy(t, turn),
	} {
		if strings.Contains(data, teamCombinedPrivacyRole) || strings.Contains(data, teamCombinedPrivacyCredential) {
			t.Errorf("private role or credential leaked into %s", surface)
		}
	}
	if !strings.Contains(turn.Summary, "[agent role instructions omitted]") || !strings.Contains(turn.Summary, "[credential redacted]") || !strings.Contains(turn.Summary, "safe finding before") || !strings.Contains(turn.Summary, " after") {
		t.Fatalf("safe child summary lost context or redaction markers: %q", turn.Summary)
	}

	model.Events = transcript.Events
	model.Transcript.SetSize(100, 20)
	model.Transcript.SetEvents(model.Events)
	visible := model.Transcript.View()
	if strings.Contains(visible, teamCombinedPrivacyRole) || strings.Contains(visible, teamCombinedPrivacyCredential) {
		t.Fatalf("TUI transcript exposed private role or provider credential: %s", visible)
	}
	if !strings.Contains(visible, "[agent role instructions omitted]") || !strings.Contains(visible, "[credential redacted]") || !strings.Contains(visible, "safe finding before") {
		t.Fatalf("TUI transcript omitted safe redacted delegation result: %s", visible)
	}
}

func mustMarshalCombinedPrivacy(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
