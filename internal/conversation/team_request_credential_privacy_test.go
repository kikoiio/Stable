package conversation

import (
	"os"
	"strings"
	"testing"

	"stable/internal/sessionlog"
	"stable/internal/teams"
)

const teamRequestCredentialSentinel = "sk-team-request-fixture-credential-0123456789"

func TestTeamRequestBodyAndFeedbackCredentialsAreRedacted(t *testing.T) {
	fixture := newTeamRequestSizeLimitFixture(t, true)
	fixture.service.deps.ProviderCredential = teamRequestCredentialSentinel

	planBody := "Inspect parser context before " + teamRequestCredentialSentinel + " and report entry points."
	plan, err := fixture.service.SubmitTeamPlan(t.Context(), fixture.childRequest, fixture.team.ID, planBody)
	if err != nil {
		t.Fatalf("submit plan: %v", err)
	}
	wantPlanBody := "Inspect parser context before [credential redacted] and report entry points."
	if plan.Body != wantPlanBody {
		t.Fatalf("returned plan body = %q, want %q", plan.Body, wantPlanBody)
	}

	shutdown, err := fixture.service.RequestTeamShutdown(t.Context(), fixture.request, fixture.team.ID, fixture.member.ID)
	if err != nil || shutdown.Status != teams.RequestPending {
		t.Fatalf("request shutdown = %+v, err=%v", shutdown, err)
	}
	feedback := "Keep the member active until the review finishes; credential " + teamRequestCredentialSentinel + " must stay private."
	responded, err := fixture.service.RespondTeamRequest(t.Context(), fixture.childRequest, fixture.team.ID, shutdown.ID, shutdown.Revision, string(teams.RequestDeferred), feedback)
	if err != nil {
		t.Fatalf("defer shutdown request: %v", err)
	}
	wantFeedback := "Keep the member active until the review finishes; credential [credential redacted] must stay private."
	if responded.Feedback != wantFeedback {
		t.Fatalf("returned feedback = %q, want %q", responded.Feedback, wantFeedback)
	}

	projection, err := sessionlog.ReplayTeams(fixture.root, fixture.request.Work.SessionID, fixture.team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := projection.Requests[plan.ID].Body; got != wantPlanBody {
		t.Fatalf("replayed plan body = %q, want %q", got, wantPlanBody)
	}
	if got := projection.Requests[shutdown.ID].Feedback; got != wantFeedback {
		t.Fatalf("replayed feedback = %q, want %q", got, wantFeedback)
	}

	path, err := sessionlog.SessionPath(fixture.root, fixture.request.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), teamRequestCredentialSentinel) {
		t.Fatal("provider credential was persisted in the raw session log")
	}
	for _, safeContext := range []string{"Inspect parser context before", "and report entry points.", "Keep the member active until the review finishes; credential", "must stay private."} {
		if !strings.Contains(string(raw), safeContext) {
			t.Errorf("raw session log lost safe context %q", safeContext)
		}
	}
	if strings.Count(string(raw), "[credential redacted]") < 2 {
		t.Fatal("raw session log did not retain both redaction markers")
	}
	fixture.runner.release <- struct{}{}
	waitForTeamMemberStatus(t, fixture.root, fixture.request.Work.SessionID, fixture.team.ID, fixture.member.ID, teams.MemberAwaitingPlan)
}
