package conversation

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestListTeamRequestsCursorPaginationReturnsAllRequestsOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "request-cursor-pagination")
	team, err := service.CreateTeam(context.Background(), request, "request-pages")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, request, team.ID, "member-request-pages", "reader")
	team, err = service.GetTeam(context.Background(), request, team.ID)
	if err != nil {
		t.Fatal(err)
	}

	const total = 237
	baseExpiry := time.Now().UTC().Add(5 * time.Minute)
	resolvedRequests := make([]teams.Request, 0, total)
	for i := 0; i < total; i++ {
		pending, createErr := service.createTeamRequestUntil(
			root, team, request.RunID, teams.Lead, "member-request-pages", teams.RequestShutdown, "",
			baseExpiry.Add(time.Duration(i%4)*time.Second),
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
		resolvedRequests = append(resolvedRequests, pending)
	}

	expected := append([]teams.Request(nil), resolvedRequests...)
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].ExpiresAt.Equal(expected[j].ExpiresAt) {
			return expected[i].ID < expected[j].ID
		}
		return expected[i].ExpiresAt.Before(expected[j].ExpiresAt)
	})

	cursor := ""
	offset := 0
	for pageIndex, wantSize := range []int{teams.MaxPageSize, teams.MaxPageSize, total - 2*teams.MaxPageSize} {
		page, listErr := service.ListTeamRequestsPage(context.Background(), request, team.ID, cursor, teams.MaxPageSize)
		if listErr != nil {
			t.Fatalf("list page %d after %q: %v", pageIndex, cursor, listErr)
		}
		if len(page) != wantSize {
			t.Fatalf("page %d returned %d requests, want %d", pageIndex, len(page), wantSize)
		}
		assertTeamRequestPageOrderedUnique(t, page)
		for i, item := range page {
			want := expected[offset+i]
			if item.ID != want.ID || !item.ExpiresAt.Equal(want.ExpiresAt) || item.Status != teams.RequestApproved {
				t.Fatalf("page %d item %d = %+v, want ID=%s expiry=%s approved", pageIndex, i, item, want.ID, want.ExpiresAt)
			}
		}
		cursor = page[len(page)-1].ID
		offset += len(page)
	}
	if offset != total {
		t.Fatalf("pagination returned %d requests, want %d", offset, total)
	}
	lastPage, err := service.ListTeamRequestsPage(context.Background(), request, team.ID, cursor, teams.MaxPageSize)
	if err != nil || len(lastPage) != 0 {
		t.Fatalf("page after final cursor %q = %d requests, err=%v; want empty", cursor, len(lastPage), err)
	}
}
