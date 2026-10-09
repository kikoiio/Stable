package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

func TestSpawnTeamMemberRejectsServiceCapacityWithoutPersistingFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	serviceA, requestA := teamServiceFixture(t, root, "global-capacity-run-a")
	serviceB, requestB := teamServiceFixture(t, root, "global-capacity-run-b")
	teamA, err := serviceA.CreateTeam(t.Context(), requestA, "global-capacity-a")
	if err != nil {
		t.Fatal(err)
	}
	teamB, err := serviceA.CreateTeam(t.Context(), requestA, "global-capacity-b")
	if err != nil {
		t.Fatal(err)
	}
	teamC, err := serviceB.CreateTeam(t.Context(), requestB, "global-capacity-c")
	if err != nil {
		t.Fatal(err)
	}
	newMemberID := func() string {
		t.Helper()
		id, err := sessionlog.NewID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := 0; i < teams.MaxTeamMembers; i++ {
		addTeamMessageMember(t, serviceA, requestA, teamA.ID, "global-a-"+newMemberID(), fmt.Sprintf("reader-a-%d", i))
	}
	for i := 0; i < teams.MaxServiceMembers-teams.MaxTeamMembers-teams.MaxTeamMembers/2; i++ {
		addTeamMessageMember(t, serviceA, requestA, teamB.ID, "global-b-"+newMemberID(), fmt.Sprintf("reader-b-%d", i))
	}
	teamCMembers := teams.MaxTeamMembers / 2
	for i := 0; i < teamCMembers; i++ {
		addTeamMessageMember(t, serviceB, requestB, teamC.ID, "global-c-"+newMemberID(), fmt.Sprintf("reader-c-%d", i))
	}
	if got := teams.MaxTeamMembers + (teams.MaxServiceMembers - teams.MaxTeamMembers - teams.MaxTeamMembers/2) + teamCMembers; got != teams.MaxServiceMembers {
		t.Fatalf("fixture members=%d, want service capacity %d", got, teams.MaxServiceMembers)
	}

	requestB.PermissionBounds, err = json.Marshal(permission.Authority{RunID: requestB.RunID, SessionID: requestB.Work.SessionID, AllowedRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	serviceB.activeRequests = map[string]agent.ExecutionRequest{requestB.RunID: requestB}
	serviceB.deps.Model = "model-v1"
	role := agentcatalog.Definition{Name: "explore", Instruction: "Inspect the assigned area.", Model: "inherit", Tools: []string{"read_file"}, MaxTurns: 3}
	runner := &gatedTeamChildRunner{inputs: make(chan agent.ChildRunInput, 1), release: make(chan struct{}, 1)}
	pool, err := agent.NewPoolDelegator(agent.DefaultDelegationLimits(), runner, nil)
	if err != nil {
		t.Fatal(err)
	}
	serviceB.deps.Agents = fixedTeamRoleCatalog{definition: role}
	serviceB.deps.Delegator = pool
	serviceB.deps.ForkProvider = forkSkillFixtureProvider{}
	serviceB.deps.ForkExecutorFactory = forkSkillFixtureExecutorFactory{}
	serviceB.deps.ForkToolSchemas = []llm.ToolSchema{{Name: "read_file"}}
	serviceB.lifeCtx = context.Background()
	serviceB.teamScheduler = newTeamScheduler(serviceB)
	t.Cleanup(func() {
		serviceB.teamScheduler.close()
		pool.Close()
	})

	before, err := sessionlog.ReplayTeams(root, requestB.Work.SessionID, teamC.ID)
	if err != nil {
		t.Fatal(err)
	}
	historyBefore, err := sessionlog.TeamHistory(root, requestB.Work.SessionID, teamC.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceB.SpawnTeamMember(t.Context(), requestB, TeamMemberSpawnRequest{
		TeamID: teamC.ID, Name: "service-capacity-overflow", AgentName: role.Name,
		Instruction: "Global member capacity must reject this member.", OriginCallID: "service-member-capacity-overflow",
	}); !errors.Is(err, teams.ErrCapacity) {
		t.Fatalf("spawn beyond service member limit = %v, want ErrCapacity", err)
	}
	if got := runner.childCount(); got != 0 {
		t.Fatalf("rejected service-capacity member started %d child runs", got)
	}
	after, err := sessionlog.ReplayTeams(root, requestB.Work.SessionID, teamC.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected service-capacity member changed team projection: before=%+v after=%+v", before, after)
	}
	historyAfter, err := sessionlog.TeamHistory(root, requestB.Work.SessionID, teamC.ID, 0, teams.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(historyAfter) != len(historyBefore) {
		t.Fatalf("rejected service-capacity member appended facts: before=%d after=%d", len(historyBefore), len(historyAfter))
	}
}

func TestCreateTeamRejectsServiceCapacityWithoutPersistingFacts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	serviceA, requestA := teamServiceFixture(t, root, "global-team-capacity-run-a")
	serviceB, requestB := teamServiceFixture(t, root, "global-team-capacity-run-b")
	serviceC, requestC := teamServiceFixture(t, root, "global-team-capacity-run-c")
	for i := 0; i < teams.MaxServiceTeams; i++ {
		service, request := serviceA, requestA
		if i >= teams.MaxSessionTeams {
			service, request = serviceB, requestB
		}
		if _, err := service.CreateTeam(t.Context(), request, fmt.Sprintf("global-team-%d", i)); err != nil {
			t.Fatalf("create service-capacity team %d: %v", i+1, err)
		}
	}
	before, err := sessionlog.Replay(root, requestC.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTeams, err := serviceC.ListTeams(t.Context(), requestC)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeTeams) != 0 {
		t.Fatalf("new session unexpectedly has teams before overflow attempt: %+v", beforeTeams)
	}
	if _, err := serviceC.CreateTeam(t.Context(), requestC, "global-team-overflow"); !errors.Is(err, teams.ErrCapacity) {
		t.Fatalf("create beyond service team limit = %v, want ErrCapacity", err)
	}
	after, err := sessionlog.Replay(root, requestC.Work.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	afterTeams, err := serviceC.ListTeams(t.Context(), requestC)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events) || len(afterTeams) != 0 {
		t.Fatalf("rejected service-capacity create changed target session: events %d -> %d, teams=%+v", len(before.Events), len(after.Events), afterTeams)
	}
}
