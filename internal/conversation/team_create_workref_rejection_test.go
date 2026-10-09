package conversation

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestCreateTeamRejectsForgedWorkRefBeforeSideEffects(t *testing.T) {
	root := t.TempDir()
	service, owner := teamServiceFixture(t, root, "create-workref-owner-run")
	team, err := service.CreateTeam(context.Background(), owner, "owner-team")
	if err != nil {
		t.Fatal(err)
	}

	scheduler := &teamScheduler{
		ready:           []string{"preexisting-ready"},
		readySet:        map[string]bool{"preexisting-ready": true},
		readyGeneration: map[string]uint64{"preexisting-ready": 7},
		active:          map[string]context.CancelFunc{"preexisting-turn": func() {}},
	}
	service.teamScheduler = scheduler

	beforeProjection, err := sessionlog.ReplayTeams(root, owner.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHistory, err := sessionlog.TeamHistory(root, owner.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := sessionlog.Replay(root, owner.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	forged := owner
	forged.Work = agent.WorkRef{
		Kind: agent.WorkGoal, SessionID: owner.Work.SessionID,
		GoalID: "forged-goal", WorkItemID: "forged-work-item",
	}
	if created, createErr := service.CreateTeam(context.Background(), forged, "forged-team"); !errors.Is(createErr, teams.ErrPermission) || created.ID != "" {
		t.Fatalf("CreateTeam with forged WorkRef = %+v, %v; want empty result and ErrPermission", created, createErr)
	}

	afterProjection, err := sessionlog.ReplayTeams(root, owner.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	afterHistory, err := sessionlog.TeamHistory(root, owner.Work.SessionID, team.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	afterSession, err := sessionlog.Replay(root, owner.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterProjection, beforeProjection) || !reflect.DeepEqual(afterHistory, beforeHistory) || !reflect.DeepEqual(afterSession.Events, beforeSession.Events) {
		t.Fatalf("rejected forged WorkRef changed durable facts: projectionEqual=%v historyEqual=%v sessionEvents=%d/%d", reflect.DeepEqual(afterProjection, beforeProjection), reflect.DeepEqual(afterHistory, beforeHistory), len(afterSession.Events), len(beforeSession.Events))
	}

	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if !reflect.DeepEqual(scheduler.ready, []string{"preexisting-ready"}) || len(scheduler.active) != 1 || !scheduler.readySet["preexisting-ready"] || scheduler.readyGeneration["preexisting-ready"] != 7 {
		t.Fatalf("rejected forged WorkRef changed scheduler state: ready=%v active=%d readySet=%v generations=%v", scheduler.ready, len(scheduler.active), scheduler.readySet, scheduler.readyGeneration)
	}
}
