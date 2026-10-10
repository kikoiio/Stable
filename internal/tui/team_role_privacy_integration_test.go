package tui

import (
	"context"
	"encoding/json"
	"fmt"
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
	"stable/internal/teams"
)

const teamRolePrivacyMarker = "PRIVATE_TEAM_ROLE_INSTRUCTION_MARKER"

type teamRolePrivacyCatalog struct{ definition agentcatalog.Definition }

func (c teamRolePrivacyCatalog) Snapshot() agentcatalog.Snapshot {
	return agentcatalog.Snapshot{Definitions: []agentcatalog.Metadata{c.definition.Metadata()}}
}
func (c teamRolePrivacyCatalog) Reload() agentcatalog.Snapshot { return c.Snapshot() }
func (c teamRolePrivacyCatalog) Resolve(name string) (agentcatalog.Definition, bool) {
	if name != c.definition.Name {
		return agentcatalog.Definition{}, false
	}
	return c.definition, true
}

type teamRolePrivacyChildRunner struct{ started chan agent.ChildRunInput }

func (r *teamRolePrivacyChildRunner) Run(_ context.Context, input agent.ChildRunInput) agent.ChildRunResult {
	r.started <- input
	return agent.ChildRunResult{Status: agent.DelegationSucceeded, Summary: "reported a safe finding"}
}

// Role instructions must reach the private child input but stay out of the
// durable team projection, socket response, and user-visible member listing.
func TestTeamRoleInstructionStaysPrivateAcrossReplayAndTUI(t *testing.T) {
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
	childRunner := &teamRolePrivacyChildRunner{started: make(chan agent.ChildRunInput, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), childRunner, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	parentRunner := &acceptanceTeamParentRunner{started: make(chan *acceptanceTeamParentRun, 1)}
	socketDir, err := os.MkdirTemp("/tmp", "m09-rp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "s")
	role := agentcatalog.Definition{
		Name: "private-role", Description: "Read-only inspection", Model: "inherit",
		Instruction: "Use this private rule: " + teamRolePrivacyMarker,
		Tools:       []string{"read_file"}, MaxTurns: 2,
	}
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
		Runner: parentRunner, Delegator: pool, Agents: teamRolePrivacyCatalog{definition: role},
		ForkProvider: acceptanceTeamProvider{}, ProviderName: "fixture", Model: "fixture-model",
		ForkExecutorFactory: agent.FakeExecutorFactory{Executor: &agent.FakeExecutor{}},
		ForkToolSchemas:     []llm.ToolSchema{{Name: "read_file"}},
	})
	if err != nil {
		t.Fatal(err)
	}
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
	parentRequest := agent.ExecutionRequest{
		RunID: parentRunID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID},
		Intent: "role privacy fixture", Messages: []llm.Message{{Role: "user", Content: "Start a read-only team member."}},
	}
	parentStream, err := conversation.OpenRun(reqctx, socket, parentRequest)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentStream.Close() })
	parentRun := receiveAcceptanceParentRun(t, parentRunner.started)
	t.Cleanup(func() { parentRun.finish(agent.RunCompleted) })
	started, err := parentStream.Receive()
	if err != nil || started.Type != "run_started" || started.RunID != parentRunID {
		t.Fatalf("wait for active parent run: message=%+v err=%v", started, err)
	}

	model := New(socket, project)
	model.ActiveSession, model.ActiveRunID = sessionID, parentRunID
	model, createResult := submitAcceptanceTeamCommand(t, model, "/teams create role-privacy")
	team := acceptanceTeamResponse(t, createResult, "team_create").Team
	if team == nil {
		t.Fatal("TUI team create omitted team")
	}
	model, spawnResult := submitAcceptanceTeamCommand(t, model, "/team "+team.ID+" spawn reader private-role inspect the assigned area")
	member := acceptanceTeamResponse(t, spawnResult, "team_member_spawn").TeamMember
	if member == nil {
		t.Fatal("TUI member spawn omitted member")
	}
	child := receiveTeamRolePrivacyInput(t, childRunner.started)
	if child.RoleInstruction != role.Instruction || !strings.Contains(child.Task.Instruction, teamRolePrivacyMarker) {
		t.Fatalf("private child did not receive the role instruction: %+v", child)
	}
	if err := waitForTeamRolePrivacyIdle(project, sessionID, team.ID, member.ID); err != nil {
		t.Fatal(err)
	}

	responseBytes, err := json.Marshal(spawnResult.msgs)
	if err != nil {
		t.Fatal(err)
	}
	assertTeamRoleMarkerAbsent(t, "TUI spawn response", string(responseBytes))
	transcript, err := sessionlog.Replay(project, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	encodedEvents, err := json.Marshal(transcript.Events)
	if err != nil {
		t.Fatal(err)
	}
	assertTeamRoleMarkerAbsent(t, "session log", string(encodedEvents))
	projection, err := sessionlog.ReplayTeams(project, sessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	encodedProjection, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	assertTeamRoleMarkerAbsent(t, "ReplayTeams projection", string(encodedProjection))

	model.Events = transcript.Events
	model.Composer.SetValue("/team " + team.ID + " members")
	updated, command := model.submitComposer()
	if command != nil {
		t.Fatal("members listing should be rendered locally from the loaded replay")
	}
	model, ok := updated.(Model)
	if !ok || model.Status != "团队成员：1/1" || len(model.TeamMembers) != 1 || model.TeamMembers[0].ID != member.ID {
		t.Fatalf("TUI members listing=%T status=%q members=%+v", updated, model.Status, model.TeamMembers)
	}
	uiEvents, err := json.Marshal(model.Events)
	if err != nil {
		t.Fatal(err)
	}
	assertTeamRoleMarkerAbsent(t, "TUI member listing", string(uiEvents))
}

func receiveTeamRolePrivacyInput(t *testing.T, started <-chan agent.ChildRunInput) agent.ChildRunInput {
	t.Helper()
	select {
	case input := <-started:
		return input
	case <-time.After(5 * time.Second):
		t.Fatal("team role child did not start")
		return agent.ChildRunInput{}
	}
}

func waitForTeamRolePrivacyIdle(root, sessionID, teamID, memberID string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil && projection.Members[memberID].Status == teams.MemberIdle {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("team member %s did not reach idle after its child completed", memberID)
}

func assertTeamRoleMarkerAbsent(t *testing.T, surface, value string) {
	t.Helper()
	if strings.Contains(value, teamRolePrivacyMarker) {
		t.Fatalf("private role instruction leaked into %s", surface)
	}
}
