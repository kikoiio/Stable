package conversation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestListTeamRequestsClampsPageAndOrdersHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "team-request-query-bounds")
	team, err := service.CreateTeam(context.Background(), request, "request-query")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, request, team.ID, "member-query", "reader")
	team, err = service.GetTeam(context.Background(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Create and resolve 101 typed shutdown requests through the normal
	// sessionlog validators. This leaves realistic historical rows without
	// waiting for expiry or starting a member runner.
	const total = teams.MaxPageSize + 1
	baseExpiry := time.Now().UTC().Add(5 * time.Minute)
	for i := 0; i < total; i++ {
		pending, createErr := service.createTeamRequestUntil(
			root, team, request.RunID, teams.Lead, "member-query", teams.RequestShutdown, "",
			baseExpiry.Add(time.Duration(i)*time.Millisecond),
		)
		if createErr != nil {
			t.Fatalf("create request %d: %v", i, createErr)
		}
		team.Revision++
		pending.Status = teams.RequestApproved
		pending.Revision++
		if appendErr := service.appendTeamRequest(root, team, request.RunID, "service", sessionlog.TeamRequestResponded, pending); appendErr != nil {
			t.Fatalf("resolve request %d: %v", i, appendErr)
		}
		team.Revision++
	}

	defaultPage, err := service.ListTeamRequests(context.Background(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultPage) != teams.DefaultPageSize {
		t.Fatalf("default request page size=%d, want %d", len(defaultPage), teams.DefaultPageSize)
	}
	assertTeamRequestPageOrderedUnique(t, defaultPage)

	maximumPage, err := service.ListTeamRequests(context.Background(), request, team.ID, teams.MaxPageSize+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(maximumPage) != teams.MaxPageSize {
		t.Fatalf("oversized request page size=%d, want capped at %d", len(maximumPage), teams.MaxPageSize)
	}
	assertTeamRequestPageOrderedUnique(t, maximumPage)

	repeatedPage, err := service.ListTeamRequests(context.Background(), request, team.ID, teams.MaxPageSize+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(repeatedPage) != len(maximumPage) {
		t.Fatalf("repeated request page length=%d, want %d", len(repeatedPage), len(maximumPage))
	}
	for i := range maximumPage {
		if repeatedPage[i].ID != maximumPage[i].ID {
			t.Fatalf("request ordering changed at %d: first=%q repeated=%q", i, maximumPage[i].ID, repeatedPage[i].ID)
		}
	}
}

func assertTeamRequestPageOrderedUnique(t *testing.T, requests []teams.Request) {
	t.Helper()
	seen := make(map[string]struct{}, len(requests))
	for i, request := range requests {
		if request.ID == "" {
			t.Fatalf("request %d has an empty ID", i)
		}
		if _, duplicate := seen[request.ID]; duplicate {
			t.Fatalf("request page contains duplicate ID %q", request.ID)
		}
		seen[request.ID] = struct{}{}
		if i > 0 {
			previous := requests[i-1]
			if previous.ExpiresAt.After(request.ExpiresAt) || previous.ExpiresAt.Equal(request.ExpiresAt) && previous.ID >= request.ID {
				t.Fatalf("request page is not ordered by expiry then ID at %d: previous=%+v current=%+v", i, previous, request)
			}
		}
	}
}
