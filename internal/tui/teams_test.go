package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stable/internal/conversation"
	"stable/internal/sessionlog"
	"stable/internal/store"
	"stable/internal/teams"
)

func TestTeamListTUIRequestUsesConversationServiceAndPreservesParentRun(t *testing.T) {
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
	socketDir, err := os.MkdirTemp("", "m09-tui-team-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "c.sock")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
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
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{RunID: "lead-run", WorkKind: "session", Intent: "team fixture"}); err != nil {
		t.Fatal(err)
	}
	teamID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	team := teams.Team{
		ID: teamID, Name: "research", Scope: teams.Scope{SessionID: sessionID, WorkKind: "session", ProjectRoot: project},
		CreatorRunID: "lead-run", Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC(),
	}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: eventID, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamCreated,
		Revision: 1, ActorID: teams.Lead, ActorRunID: "lead-run", Team: &team,
	}); err != nil {
		t.Fatal(err)
	}

	m := New(socket, project)
	m.ActiveSession = sessionID
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent-run", 17
	parentStream := &conversation.StreamClient{}
	m.stream = parentStream
	m.Composer.SetValue("/teams list")
	updated, command := m.submitComposer()
	got := updated.(Model)
	if command == nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
		t.Fatalf("team request changed parent run state before response: pending=%v run=%q cursor=%d stream=%p", got.Pending, got.ActiveRunID, got.LastCursor, got.stream)
	}
	result, ok := command().(resultMsg)
	if !ok || result.err != nil {
		t.Fatalf("TUI request result=%+v, err=%v", result, result.err)
	}
	updated, _ = got.handleResult(result)
	got = updated.(Model)
	if len(got.Teams) != 1 || got.Teams[0].ID != teamID || got.Teams[0].Name != "research" {
		t.Fatalf("TUI did not render the service-owned team list: %+v", got.Teams)
	}
	if !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
		t.Fatal("one-shot team response ended or replaced the parent run")
	}
	if !strings.Contains(got.Status, "团队列表已更新（1）") || len(got.Events) == 0 {
		t.Fatalf("TUI did not expose the team response: status=%q events=%d", got.Status, len(got.Events))
	}
	last, ok := got.Events[len(got.Events)-1].Data.(sessionlog.Message)
	if !ok || !strings.Contains(last.Text, "research") {
		t.Fatalf("TUI response event=%+v, want rendered team name", got.Events[len(got.Events)-1])
	}
}

func TestTeamStopTUIRequestUsesConversationServiceAndPreservesParentRun(t *testing.T) {
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
	socketDir := t.TempDir()
	socket := filepath.Join(socketDir, "c.sock")
	svc, err := conversation.Serve(ctx, conversation.Deps{
		Store: db, ProjectRoot: project, SocketPath: socket, PollEvery: time.Hour,
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
	teamID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	memberID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	turnID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	childRunID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	team := teams.Team{
		ID: teamID, Name: "research", Scope: teams.Scope{SessionID: sessionID, WorkKind: "session", ProjectRoot: project},
		Status: teams.TeamOpen, Revision: 1, CreatedAt: time.Now().UTC(),
	}
	appendTeamEvent := func(kind string, revision uint64, actor string, member *teams.Member, turn *sessionlog.TurnFact) {
		t.Helper()
		eventID, idErr := sessionlog.NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		_, appendErr := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
			ID: eventID, TeamID: teamID, SessionID: sessionID, Kind: kind,
			Revision: revision, ActorID: actor, Team: nil, Member: member, Turn: turn,
		})
		if appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	eventID, err := sessionlog.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionlog.Append(project, sessionID, sessionlog.EventTeam, sessionlog.TeamEvent{
		ID: eventID, TeamID: teamID, SessionID: sessionID, Kind: sessionlog.TeamCreated,
		Revision: 1, ActorID: teams.Lead, Team: &team,
	}); err != nil {
		t.Fatal(err)
	}
	member := teams.Member{ID: memberID, TeamID: teamID, Name: "reader", AgentName: "explore", RoleHash: "fixture", Model: "fixture", Tools: []string{"read_file"}, Status: teams.MemberCreated, Revision: 1}
	appendTeamEvent(sessionlog.TeamMemberAdded, 2, teams.Lead, &member, nil)
	turn := sessionlog.TurnFact{ID: turnID, MemberID: memberID, RunID: childRunID, TaskID: turnID, Status: "intent"}
	appendTeamEvent(sessionlog.TeamTurnIntent, 3, "service", nil, &turn)
	turn.Status = "queued"
	appendTeamEvent(sessionlog.TeamTurnAccepted, 4, "service", nil, &turn)
	member.Status, member.TurnID, member.RunID, member.Revision = teams.MemberQueued, turnID, childRunID, 2
	member.Budget.AcceptedTurns = 1
	appendTeamEvent(sessionlog.TeamMemberState, 5, "service", &member, nil)
	member.Status, member.Revision = teams.MemberStopping, 3
	appendTeamEvent(sessionlog.TeamMemberState, 6, "service", &member, nil)

	m := New(socket, project)
	m.ActiveSession = sessionID
	m.Pending, m.ActiveRunID, m.LastCursor = true, "parent-run", 17
	parentStream := &conversation.StreamClient{}
	m.stream = parentStream
	m.Composer.SetValue("/team " + teamID + " stop " + memberID)
	updated, command := m.submitComposer()
	got := updated.(Model)
	if command == nil || !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
		t.Fatalf("team stop changed parent run state before response: pending=%v run=%q cursor=%d stream=%p", got.Pending, got.ActiveRunID, got.LastCursor, got.stream)
	}
	result, ok := command().(resultMsg)
	if !ok || result.err != nil {
		t.Fatalf("TUI stop result=%+v, err=%v", result, result.err)
	}
	updated, _ = got.handleResult(result)
	got = updated.(Model)
	if len(got.TeamMembers) != 1 || got.TeamMembers[0].ID != memberID || got.TeamMembers[0].Status != teams.MemberStopping {
		t.Fatalf("TUI did not render service-owned stopping member: %+v", got.TeamMembers)
	}
	if !got.Pending || got.ActiveRunID != "parent-run" || got.LastCursor != 17 || got.stream != parentStream {
		t.Fatal("one-shot team stop response ended or replaced the parent run")
	}
	projection, err := sessionlog.ReplayTeams(project, sessionID, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Members[memberID].Status != teams.MemberStopping || projection.Turns[turnID].Status != "queued" {
		t.Fatalf("TUI stop changed the child terminal state before actual exit: member=%s turn=%s", projection.Members[memberID].Status, projection.Turns[turnID].Status)
	}
}

func TestTeamCommandsUseSessionScopeAndPreserveParentRun(t *testing.T) {
	cases := []struct {
		line, op, teamID, runID, text, recipient, decision string
		memberID, memberName, agentName                    string
		after, limit                                       uint64
		broadcast                                          bool
		planRequired                                       bool
		coordinatorOn                                      bool
		acceptRoleChange                                   bool
	}{
		{line: "/teams list", op: "team_list"},
		{line: "/teams list 100", op: "team_list", limit: 100},
		{line: "/teams coordinator on", op: "team_coordinator", coordinatorOn: true},
		{line: "/teams create squad", op: "team_create", runID: "parent"},
		{line: "/teams close team-1", op: "team_close", teamID: "team-1"},
		{line: "/team team-1 get", op: "team_get", teamID: "team-1"},
		{line: "/team team-1 tasks", op: "team_task_list", teamID: "team-1"},
		{line: "/team team-1 tasks list 100", op: "team_task_list", teamID: "team-1", limit: 100},
		{line: "/team team-1 tasks create write docs", op: "team_task_create", teamID: "team-1", text: "write docs"},
		{line: "/team team-1 messages after 12 7", op: "team_messages", teamID: "team-1", after: 12, limit: 7},
		{line: "/team team-1 requests", op: "team_request_list", teamID: "team-1"},
		{line: "/team team-1 requests list 100", op: "team_request_list", teamID: "team-1", limit: 100},
		{line: "/team team-1 send member-1 hello team", op: "team_send", teamID: "team-1", text: "hello team", recipient: "member-1"},
		{line: "/team team-1 send all hello team", op: "team_send", teamID: "team-1", text: "hello team", broadcast: true},
		{line: "/team team-1 respond req-1 3 approve looks good", op: "team_request_respond", teamID: "team-1", decision: string(teams.RequestApproved)},
		{line: "/team team-1 shutdown member-1", op: "team_shutdown_request", teamID: "team-1"},
		{line: "/team team-1 spawn reader explore inspect changes --plan", op: "team_member_spawn", teamID: "team-1", runID: "parent", memberName: "reader", agentName: "explore", text: "inspect changes", planRequired: true},
		{line: "/team team-1 resume member-1", op: "team_member_resume", teamID: "team-1", runID: "parent", memberID: "member-1"},
		{line: "/team team-1 resume member-1 --accept-role-change", op: "team_member_resume", teamID: "team-1", runID: "parent", memberID: "member-1", acceptRoleChange: true},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			m := New("", t.TempDir())
			m.ActiveSession = "session"
			m.Pending, m.ActiveRunID, m.LastCursor = true, "parent", 17
			parentStream := &conversation.StreamClient{}
			m.stream = parentStream
			socket, requests := agentSocketFixture(t, conversation.ServerMsg{Type: tc.op})
			m.Socket = socket
			m.Composer.SetValue(tc.line)
			updated, command := m.submitComposer()
			got := updated.(Model)
			if command == nil || !got.Pending || got.ActiveRunID != "parent" || got.LastCursor != 17 || got.stream != parentStream {
				t.Fatalf("team command changed parent run state: pending=%v run=%q cursor=%d stream=%p", got.Pending, got.ActiveRunID, got.LastCursor, got.stream)
			}
			result := command().(resultMsg)
			if result.err != nil {
				t.Fatal(result.err)
			}
			request := agentFixtureRequest(t, requests)
			if request.Op != tc.op || request.SessionID != "session" || request.TeamID != tc.teamID || request.Run != nil {
				t.Fatalf("request=%+v", request)
			}
			if request.RunID != tc.runID {
				t.Fatalf("run ID=%q, want %q", request.RunID, tc.runID)
			}
			if request.CoordinatorOn != tc.coordinatorOn {
				t.Fatalf("coordinator mode=%v, want %v", request.CoordinatorOn, tc.coordinatorOn)
			}
			if tc.op != "team_create" && tc.op != "team_member_spawn" && tc.op != "team_member_resume" && request.RunID != "" {
				t.Fatalf("session operation borrowed run ID: %+v", request)
			}
			if tc.op == "team_member_spawn" && (request.TeamMemberName != tc.memberName || request.AgentName != tc.agentName || request.Text != tc.text || request.TeamPlanRequired != tc.planRequired) {
				t.Fatalf("member spawn request=%+v", request)
			}
			if tc.op == "team_member_resume" && request.TeamMemberID != tc.memberID {
				t.Fatalf("member resume request=%+v", request)
			}
			if request.TeamAcceptRoleChange != tc.acceptRoleChange {
				t.Fatalf("role change acceptance=%v, want %v", request.TeamAcceptRoleChange, tc.acceptRoleChange)
			}
			if tc.op == "team_send" {
				if request.Text != tc.text || request.TeamRecipient != tc.recipient || request.TeamBroadcast != tc.broadcast || request.TeamToken == "" {
					t.Fatalf("send request=%+v", request)
				}
			}
			if tc.op == "team_messages" && (request.AfterSeq != tc.after || request.Limit != int(tc.limit)) {
				t.Fatalf("message page=%+v", request)
			}
			if tc.op == "team_task_list" && request.Limit != int(tc.limit) {
				t.Fatalf("task page limit=%d, want %d", request.Limit, tc.limit)
			}
			if tc.op == "team_list" && request.Limit != int(tc.limit) {
				t.Fatalf("team page limit=%d, want %d", request.Limit, tc.limit)
			}
			if tc.op == "team_request_list" && request.Limit != int(tc.limit) {
				t.Fatalf("team request page limit=%d, want %d", request.Limit, tc.limit)
			}
			if tc.op == "team_task_create" && (request.TaskTitle == nil || *request.TaskTitle != tc.text) {
				t.Fatalf("task title=%v", request.TaskTitle)
			}
			if tc.op == "team_request_respond" && request.TeamDecision != tc.decision {
				t.Fatalf("decision=%q", request.TeamDecision)
			}
			updated, _ = got.handleResult(resultMsg{op: tc.op, sessionID: "session", msgs: []conversation.ServerMsg{{Type: tc.op}}})
			got = updated.(Model)
			if !got.Pending || got.ActiveRunID != "parent" || got.LastCursor != 17 || got.stream != parentStream {
				t.Fatal("one-shot team result ended the parent run")
			}
		})
	}
}

func TestTeamCommandsRejectInvalidUsageAndRequireRunForCreate(t *testing.T) {
	for _, line := range []string{"/teams", "/teams list 0", "/teams list 101", "/teams create", "/teams close", "/teams coordinator", "/teams coordinator yes", "/team", "/team team-1 requests list 0", "/team team-1 requests list 101", "/team team-1 tasks update task-1 0 status completed", "/team team-1 tasks list 101", "/team team-1 messages 0 999", "/team team-1 send all", "/team team-1 respond req-1 0 approve", "/team team-1 shutdown", "/team team-1 spawn reader explore inspect", "/team team-1 resume member-1"} {
		m := New("", t.TempDir())
		m.ActiveSession = "session"
		m.Composer.SetValue(line)
		updated, command := m.submitComposer()
		if command != nil || !strings.Contains(updated.(Model).Status, "用法") {
			t.Fatalf("invalid command accepted: %s status=%q", line, updated.(Model).Status)
		}
	}
	m := New("", t.TempDir())
	m.ActiveSession = "session"
	m.Composer.SetValue("/teams create squad")
	updated, command := m.submitComposer()
	if command != nil || !strings.Contains(updated.(Model).Status, "活动的 lead run") {
		t.Fatal("team create without active lead run was accepted")
	}
}
