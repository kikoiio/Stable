package conversation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestCloseTeamRejectsForgedActorAndRunIdentityWithoutSideEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := ensureDir(root); err != nil {
		t.Fatal(err)
	}
	service, lead := teamServiceFixture(t, root, "close-actor-lead")
	team, err := service.CreateTeam(t.Context(), lead, "close-actor")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, lead, team.ID, "close-actor-member", "reader")
	if _, err := sessionlog.Append(root, lead.Work.SessionID, sessionlog.EventRunStarted, sessionlog.RunStarted{
		RunID: "close-actor-inactive", WorkKind: string(lead.Work.Kind), Intent: "persisted but inactive",
	}); err != nil {
		t.Fatal(err)
	}
	cancelCalls := 0
	service.teamScheduler = &teamScheduler{
		activeTeam: map[string]string{"turn-close-actor": team.ID},
		active:     map[string]context.CancelFunc{"turn-close-actor": func() { cancelCalls++ }},
	}

	forged := []struct {
		name    string
		request agent.ExecutionRequest
	}{
		{name: "empty RunID", request: agent.ExecutionRequest{Work: lead.Work}},
		{name: "forged RunID", request: agent.ExecutionRequest{RunID: "forged-close-run", Work: lead.Work}},
		{name: "persisted but inactive parent", request: agent.ExecutionRequest{RunID: "close-actor-inactive", Work: lead.Work}},
		{name: "caller-set user boolean", request: func() agent.ExecutionRequest { r := lead; r.TeamUser = true; return r }()},
	}
	for _, tc := range forged {
		t.Run(tc.name, func(t *testing.T) {
			beforeProjection, err := sessionlog.ReplayTeams(root, lead.Work.SessionID, team.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeHistory, err := sessionlog.TeamHistory(root, lead.Work.SessionID, team.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			beforeSession, err := sessionlog.Replay(root, lead.Work.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.CloseTeam(t.Context(), tc.request, team.ID); !errors.Is(err, teams.ErrPermission) {
				t.Fatalf("CloseTeam error = %v, want ErrPermission", err)
			}
			afterProjection, err := sessionlog.ReplayTeams(root, lead.Work.SessionID, team.ID)
			if err != nil {
				t.Fatal(err)
			}
			afterHistory, err := sessionlog.TeamHistory(root, lead.Work.SessionID, team.ID, 0, teams.MaxPageSize)
			if err != nil {
				t.Fatal(err)
			}
			afterSession, err := sessionlog.Replay(root, lead.Work.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterProjection, beforeProjection) || afterProjection.Teams[team.ID].Status != teams.TeamOpen || afterProjection.Members["close-actor-member"].Status != teams.MemberCreated {
				t.Fatalf("rejected close changed team/member projection: before=%+v after=%+v", beforeProjection, afterProjection)
			}
			if len(afterHistory) != len(beforeHistory) || len(afterSession.Events) != len(beforeSession.Events) {
				t.Fatalf("rejected close appended facts: history %d->%d, session events %d->%d", len(beforeHistory), len(afterHistory), len(beforeSession.Events), len(afterSession.Events))
			}
			if cancelCalls != 0 {
				t.Fatalf("rejected close canceled %d child runs", cancelCalls)
			}
		})
	}

	closed, err := service.CloseTeam(t.Context(), lead, team.ID)
	if err != nil || closed.Status != teams.TeamClosed {
		t.Fatalf("valid lead close = %+v, err=%v; want closed", closed, err)
	}
	projection, err := sessionlog.ReplayTeams(root, lead.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Members["close-actor-member"].Status; got != teams.MemberStopped {
		t.Fatalf("valid lead close left member %s, want stopped", got)
	}
	if cancelCalls != 1 {
		t.Fatalf("valid close cancellation calls = %d, want 1", cancelCalls)
	}
}

func TestTeamCloseSocketUsesServerDerivedUserIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := ensureDir(root); err != nil {
		t.Fatal(err)
	}
	service, lead := teamServiceFixture(t, root, "close-socket-lead")
	team, err := service.CreateTeam(t.Context(), lead, "socket-close")
	if err != nil {
		t.Fatal(err)
	}
	// The socket request supplies only session/team IDs; the service rebuilds
	// the user actor and exact WorkRef from persisted team facts.
	response, err := service.handleTeamRequest(t.Context(), ClientMsg{Op: "team_close", SessionID: lead.Work.SessionID, TeamID: team.ID})
	if err != nil || response.Team == nil || response.Team.Status != teams.TeamClosed {
		t.Fatalf("server-derived socket close = %+v, err=%v; want closed", response, err)
	}
}

func ensureDir(path string) error { return os.MkdirAll(path, 0700) }
