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
	"stable/internal/store"
	"stable/internal/teams"
)

func TestGoalTeamMessageSendRejectsAnotherWorkItemWithoutFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	goalRoot := filepath.Join(root, "goal-root")
	if err := os.MkdirAll(goalRoot, 0700); err != nil {
		t.Fatal(err)
	}
	session, err := sessionlog.Create(root, "goal team message send scope")
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if _, err := state.CreateGoal(context.Background(), coreGoal("goal-message-send-scope", goalRoot, session.ID)); err != nil {
		t.Fatal(err)
	}
	workA := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-message-send-scope", WorkItemID: "item-a"}
	workB := agent.WorkRef{Kind: agent.WorkGoal, SessionID: session.ID, GoalID: "goal-message-send-scope", WorkItemID: "item-b"}
	requestA := appendGoalScopeRun(t, root, "goal-message-send-item-a", workA)
	requestB := appendGoalScopeRun(t, root, "goal-message-send-item-b", workB)
	service := &Service{
		deps:       Deps{ProjectRoot: root, Store: state},
		activeRuns: map[string]string{requestA.RunID: session.ID, requestB.RunID: session.ID},
	}
	team, err := service.CreateTeam(t.Context(), requestA, "item-a-message-team")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMessageMember(t, service, requestA, team.ID, "message-member-a", "reader")

	beforeProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	const token = "cross-workitem-message-token"
	args := TeamSendRequest{TeamID: team.ID, Recipient: "message-member-a", Body: "message owned by item A", Token: token}
	for _, actor := range []struct {
		name    string
		request agent.ExecutionRequest
	}{
		{name: "valid run from another work item", request: requestB},
		{name: "forged work item on owner run", request: func() agent.ExecutionRequest {
			forged := requestA
			forged.Work.WorkItemID = workB.WorkItemID
			return forged
		}()},
	} {
		t.Run(actor.name, func(t *testing.T) {
			if message, sendErr := service.SendTeamMessage(t.Context(), actor.request, args); !errors.Is(sendErr, teams.ErrPermission) || message.ID != "" {
				t.Fatalf("cross-WorkItem send = %+v, %v; want empty result and ErrPermission", message, sendErr)
			}
			projection, replayErr := sessionlog.ReplayTeams(root, session.ID, team.ID)
			if replayErr != nil {
				t.Fatal(replayErr)
			}
			history, historyErr := sessionlog.TeamHistory(root, session.ID, team.ID, 0, teams.MaxPageSize)
			if historyErr != nil {
				t.Fatal(historyErr)
			}
			transcript, transcriptErr := sessionlog.Replay(root, session.ID)
			if transcriptErr != nil {
				t.Fatal(transcriptErr)
			}
			if !reflect.DeepEqual(projection, beforeProjection) || !reflect.DeepEqual(history, beforeHistory) || len(transcript.Events) != len(beforeTranscript.Events) {
				t.Fatalf("rejected cross-WorkItem send changed facts: projection-equal=%v history=%d/%d session-events=%d/%d", reflect.DeepEqual(projection, beforeProjection), len(history), len(beforeHistory), len(transcript.Events), len(beforeTranscript.Events))
			}
		})
	}

	accepted, err := service.SendTeamMessage(t.Context(), requestA, args)
	if err != nil {
		t.Fatalf("owner could not reuse rejected idempotency token: %v", err)
	}
	if accepted.ID == "" || accepted.TeamID != team.ID || accepted.SenderID != teams.Lead || len(accepted.Recipients) != 1 || accepted.Recipients[0] != "message-member-a" || accepted.Body != args.Body {
		t.Fatalf("authorized owner send = %+v", accepted)
	}
	afterProjection, err := sessionlog.ReplayTeams(root, session.ID, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTeam, afterTeam := beforeProjection.Teams[team.ID], afterProjection.Teams[team.ID]
	if afterTeam.Revision != beforeTeam.Revision+1 || afterTeam.Status != beforeTeam.Status || !afterTeam.Scope.Matches(beforeTeam.Scope) || !reflect.DeepEqual(beforeProjection.Members, afterProjection.Members) || len(afterProjection.Messages) != 1 || afterProjection.Messages[accepted.ID].ID != accepted.ID {
		t.Fatalf("authorized retry did not create exactly one message: before=%+v after=%+v", beforeProjection, afterProjection)
	}
	afterTranscript, err := sessionlog.Replay(root, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTranscript.Events) != len(beforeTranscript.Events)+1 {
		t.Fatalf("authorized retry appended %d session events, want 1", len(afterTranscript.Events)-len(beforeTranscript.Events))
	}
}
