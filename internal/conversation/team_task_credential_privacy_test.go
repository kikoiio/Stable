package conversation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

const teamTaskCredentialSentinel = "sk-team-task-fixture-credential-0123456789"

func TestTeamTaskTitleDescriptionCredentialRedaction(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	service, request := teamServiceFixture(t, root, "task-credential-redaction")
	service.deps.ProviderCredential = teamTaskCredentialSentinel
	team, err := service.CreateTeam(t.Context(), request, "task-privacy")
	if err != nil {
		t.Fatal(err)
	}

	createTitle := "Review parser context before " + teamTaskCredentialSentinel
	createDescription := "Preserve the safe explanation after " + teamTaskCredentialSentinel + " is removed."
	created, err := service.CreateTeamTask(t.Context(), request, team.ID, teams.Task{
		Title: createTitle, Description: createDescription,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	wantCreatedTitle := "Review parser context before [credential redacted]"
	wantCreatedDescription := "Preserve the safe explanation after [credential redacted] is removed."
	if created.Title != wantCreatedTitle || created.Description != wantCreatedDescription {
		t.Fatalf("created task content = (%q, %q), want (%q, %q)", created.Title, created.Description, wantCreatedTitle, wantCreatedDescription)
	}
	projection, err := sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Tasks[created.ID]; got.Title != wantCreatedTitle || got.Description != wantCreatedDescription {
		t.Fatalf("created task projection retained unexpected content: %+v", got)
	}

	updateTitle := "Confirm call sites after " + teamTaskCredentialSentinel
	updateDescription := "Keep this update context while hiding " + teamTaskCredentialSentinel + "."
	updated, err := service.UpdateTeamTask(t.Context(), request, team.ID, created.ID, created.Revision, teams.TaskPatch{
		Title:       &updateTitle,
		Description: &updateDescription,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	wantUpdatedTitle := "Confirm call sites after [credential redacted]"
	wantUpdatedDescription := "Keep this update context while hiding [credential redacted]."
	if updated.Title != wantUpdatedTitle || updated.Description != wantUpdatedDescription {
		t.Fatalf("updated task content = (%q, %q), want (%q, %q)", updated.Title, updated.Description, wantUpdatedTitle, wantUpdatedDescription)
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Tasks[created.ID]; got.Title != wantUpdatedTitle || got.Description != wantUpdatedDescription {
		t.Fatalf("updated task projection retained unexpected content: %+v", got)
	}

	path, err := sessionlog.SessionPath(root, request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	if strings.Contains(log, teamTaskCredentialSentinel) {
		t.Fatal("provider credential was persisted in the raw session log")
	}
	for _, safeContext := range []string{
		"Review parser context before", "Preserve the safe explanation after", "is removed.",
		"Confirm call sites after", "Keep this update context while hiding", "[credential redacted]",
	} {
		if !strings.Contains(log, safeContext) {
			t.Errorf("raw session log lost safe task context %q", safeContext)
		}
	}
	if strings.Count(log, "[credential redacted]") < 4 {
		t.Fatalf("raw session log did not retain all task redaction markers: count=%d", strings.Count(log, "[credential redacted]"))
	}

	// Validation runs on the redacted text too: replacing a short credential
	// can expand a field beyond the task title limit, which must remain invalid.
	service.deps.ProviderCredential = "x"
	tooLongAfterRedaction := strings.Repeat("a", teams.MaxTaskTitleBytes-1) + "x"
	if _, err := service.CreateTeamTask(t.Context(), request, team.ID, teams.Task{Title: tooLongAfterRedaction}); err == nil {
		t.Fatal("task title exceeding the byte limit after redaction was accepted")
	}
	projection, err = sessionlog.ReplayTeams(root, request.Work.SessionID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Tasks) != 1 || projection.Tasks[created.ID].Revision != updated.Revision {
		t.Fatalf("invalid post-redaction title changed task projection: %+v", projection.Tasks)
	}
}
