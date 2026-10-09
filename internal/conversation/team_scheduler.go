package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/execution"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
	"stable/internal/workspace"
)

var errTeamMemberAlreadyActive = errors.New("team member already has an active turn")

// TeamMemberSpawnRequest contains member configuration from a trusted lead
// operation. Actor, scope, member ID, turn ID and child run ID are service-owned.
type TeamMemberSpawnRequest struct {
	TeamID       string
	Name         string
	AgentName    string
	Instruction  string
	PlanRequired bool
	OriginCallID string
}

type teamParentGrant struct {
	Request       agent.ExecutionRequest
	Scope         teams.Scope
	TeamID        string
	MemberID      string
	OriginCallID  string
	PlanRequestID string
	Generation    uint64
}

type teamWorkspaceRun struct {
	manager *workspace.LifecycleService
	lease   workspace.WriterLease
	cancel  context.CancelFunc
	done    chan struct{}
}

type teamExpiryScope struct {
	root      string
	sessionID string
	teamID    string
}

type teamScheduler struct {
	service         *Service
	submitter       agent.CommittedTaskSubmitter
	appendHandoff   func(root, sessionID, teamID string, handoff sessionlog.HandoffFact) error
	mu              sync.Mutex
	drainMu         sync.Mutex
	active          map[string]context.CancelFunc
	activeOrigin    map[string]string
	activeMember    map[string]string
	activeTeam      map[string]string
	workspaceRuns   map[string]teamWorkspaceRun
	roles           map[string]agentcatalog.Definition
	waiting         []func() error
	grants          map[string]teamParentGrant
	grantGeneration map[string]uint64
	ready           []string
	readySet        map[string]bool
	readyGeneration map[string]uint64
	expiryScopes    map[teamExpiryScope]struct{}
	wake            chan struct{}
	expiryWake      chan struct{}
	done            chan struct{}
	closed          bool
}

func newTeamScheduler(service *Service) *teamScheduler {
	scheduler := &teamScheduler{
		service: service, active: map[string]context.CancelFunc{}, activeOrigin: map[string]string{}, activeMember: map[string]string{}, activeTeam: map[string]string{}, workspaceRuns: map[string]teamWorkspaceRun{},
		roles: map[string]agentcatalog.Definition{}, grants: map[string]teamParentGrant{}, grantGeneration: map[string]uint64{},
		readySet: map[string]bool{}, readyGeneration: map[string]uint64{}, expiryScopes: map[teamExpiryScope]struct{}{}, wake: make(chan struct{}, 1), expiryWake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if service != nil {
		scheduler.submitter, _ = service.deps.Delegator.(agent.CommittedTaskSubmitter)
		if scheduler.submitter != nil {
			go scheduler.capacityLoop(service.lifeCtx)
		}
		go func() {
			scheduler.seedExpiryScopes(service.deps.ProjectRoot)
			scheduler.expiryLoop(service.lifeCtx)
		}()
	}
	return scheduler
}

func (s *teamScheduler) seedExpiryScopes(root string) {
	if root == "" {
		return
	}
	sessions, err := sessionlog.List(root)
	if err != nil {
		return
	}
	for _, session := range sessions {
		projection, replayErr := sessionlog.ReplayTeams(root, session.ID)
		if replayErr != nil {
			continue
		}
		for teamID, team := range projection.Teams {
			for _, request := range projection.Requests {
				if request.TeamID == teamID && (request.Status == teams.RequestPending || request.Status == teams.RequestDeferred) {
					s.trackExpiryScope(root, team.Scope.SessionID, teamID)
					break
				}
			}
		}
	}
}

func (s *teamScheduler) trackExpiryScope(root, sessionID, teamID string) {
	if s == nil || root == "" || sessionlog.ValidateID(sessionID) != nil || teams.ValidateID(teamID) != nil {
		return
	}
	s.mu.Lock()
	s.expiryScopes[teamExpiryScope{root: root, sessionID: sessionID, teamID: teamID}] = struct{}{}
	s.mu.Unlock()
	select {
	case s.expiryWake <- struct{}{}:
	default:
	}
}

// expiryLoop is the single timer owner for durable team request deadlines.
// It tracks only teams touched by this service, then recomputes the nearest
// pending deadline whenever a request is created, answered, or expires.
func (s *teamScheduler) expiryLoop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	var timer *time.Timer
	for {
		next, scopes := s.nextTeamRequestExpiry()
		if next.IsZero() {
			select {
			case <-s.done:
				return
			case <-ctx.Done():
				return
			case <-s.expiryWake:
				continue
			}
		}
		delay := time.Until(next)
		if delay <= 0 {
			// A failed append is retried with a bounded pause instead of spinning.
			delay = time.Second
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.expiryWake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		for _, scope := range scopes {
			s.service.eventMu.Lock()
			err := s.service.expireDueTeamRequestsForTeam(scope.root, scope.sessionID, scope.teamID)
			s.service.eventMu.Unlock()
			if err != nil {
				// Revisit the same persisted deadline after the loop's retry pause.
				select {
				case s.expiryWake <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (s *teamScheduler) nextTeamRequestExpiry() (time.Time, []teamExpiryScope) {
	s.mu.Lock()
	scopes := make([]teamExpiryScope, 0, len(s.expiryScopes))
	for scope := range s.expiryScopes {
		scopes = append(scopes, scope)
	}
	s.mu.Unlock()
	var next time.Time
	for _, scope := range scopes {
		projection, err := sessionlog.ReplayTeams(scope.root, scope.sessionID, scope.teamID)
		if err != nil {
			continue
		}
		for _, request := range projection.Requests {
			if request.TeamID != scope.teamID || request.Status != teams.RequestPending && request.Status != teams.RequestDeferred {
				continue
			}
			if next.IsZero() || request.ExpiresAt.Before(next) {
				next = request.ExpiresAt
			}
		}
	}
	return next, scopes
}

// capacityLoop owns the single coalescing pool capacity subscription. It drains
// deferred explicit resumes in FIFO order and stops at the first still-full
// admission, avoiding polling and preventing younger turns from bypassing it.
func (s *teamScheduler) capacityLoop(ctx context.Context) {
	capacity := s.submitter.CapacityChanged()
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			return
		case _, ok := <-capacity:
			if !ok {
				return
			}
			s.drainScheduler()
		case <-s.wake:
			s.drainScheduler()
		}
	}
}

func (s *teamScheduler) enqueueCapacityResume(run func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("team member scheduler is closed")
	}
	// The pool queue is bounded at 32. Keep the deferred set no larger than
	// that shared admission bound; one waiting item represents one future slot.
	if len(s.waiting) >= 32 {
		return agent.ErrDelegationQueueFull
	}
	s.waiting = append(s.waiting, run)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *teamScheduler) drainCapacityQueue() {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	for {
		s.mu.Lock()
		if s.closed || len(s.waiting) == 0 {
			s.mu.Unlock()
			return
		}
		next := s.waiting[0]
		s.mu.Unlock()
		err := next()
		s.mu.Lock()
		if s.closed {
			s.waiting = nil
			s.mu.Unlock()
			return
		}
		if errors.Is(err, agent.ErrDelegationQueueFull) {
			s.mu.Unlock()
			return
		}
		// Remove only the head we attempted. The queue is FIFO and only this
		// goroutine drains it; enqueue may append concurrently.
		if len(s.waiting) > 0 {
			s.waiting = s.waiting[1:]
		}
		s.mu.Unlock()
	}
}

func (s *teamScheduler) drainScheduler() {
	s.drainCapacityQueue()
	s.drainReadyQueue()
}

func (s *teamScheduler) drainReadyQueue() {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	for {
		s.mu.Lock()
		if s.closed || len(s.ready) == 0 {
			s.mu.Unlock()
			return
		}
		memberID := s.ready[0]
		s.ready = s.ready[1:]
		s.readySet[memberID] = false
		readyGeneration := s.readyGeneration[memberID]
		grant, hasGrant := s.grants[memberID]
		s.mu.Unlock()
		if !hasGrant {
			continue
		}
		root := grant.Scope.ProjectRoot
		projection, err := sessionlog.ReplayTeams(root, grant.Scope.SessionID, grant.TeamID)
		if err != nil {
			s.clearReady(memberID, readyGeneration)
			continue
		}
		member, ok := projection.Members[memberID]
		if !ok || member.TeamID != grant.TeamID || projection.Teams[grant.TeamID].Status != teams.TeamOpen || member.Status.IsTerminal() || member.Status == teams.MemberAwaitingPlan {
			s.clearReady(memberID, readyGeneration)
			continue
		}
		if member.Status.HasTurn() || member.Status == teams.MemberWaitingCapacity {
			continue
		}
		_, err = s.service.resumeTeamMemberWithGrant(grant, false)
		if err != nil {
			s.clearReady(memberID, readyGeneration)
			continue
		}
		s.clearReady(memberID, readyGeneration)
	}
}

func (s *teamScheduler) clearReady(memberID string, generation uint64) {
	s.mu.Lock()
	if s.readyGeneration[memberID] == generation {
		delete(s.readyGeneration, memberID)
		s.readySet[memberID] = false
	}
	s.mu.Unlock()
}

func (s *teamScheduler) queueReady(memberID string, newSignal bool) {
	if s == nil || teams.ValidateID(memberID) != nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if newSignal {
		s.readyGeneration[memberID]++
	}
	if !s.readySet[memberID] {
		s.ready = append(s.ready, memberID)
		s.readySet[memberID] = true
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.mu.Unlock()
}

func cleanTeamParentRequest(request agent.ExecutionRequest) agent.ExecutionRequest {
	request.Intent = ""
	request.Messages = nil
	request.ToolSchemas = nil
	request.AllowedScope = nil
	request.ResourceBounds = nil
	request.TeamTurn = nil
	request.TeamUser = false
	request.TeamCoordinator = false
	request.AcceptTeamRoleChange = false
	request.PermissionBounds = append(json.RawMessage(nil), request.PermissionBounds...)
	return request
}

func (s *teamScheduler) rememberParent(request agent.ExecutionRequest, scope teams.Scope, teamID, memberID, callID string) uint64 {
	if s == nil || request.RunID == "" || request.TeamTurn != nil || request.TeamUser || scope.Validate() != nil || teams.ValidateID(teamID) != nil || teams.ValidateID(memberID) != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0
	}
	if prior, ok := s.grants[memberID]; ok && prior.Request.RunID == request.RunID && prior.TeamID == teamID && prior.Scope.Matches(scope) {
		// A second call from the same lead run can race an automatic wake (for
		// example plan approval followed immediately by explicit resume). Refresh
		// its request metadata without invalidating the already accepted child.
		prior.Request = cleanTeamParentRequest(request)
		prior.OriginCallID = callID
		s.grants[memberID] = prior
		return prior.Generation
	}
	s.grantGeneration[memberID]++
	grant := teamParentGrant{Request: cleanTeamParentRequest(request), Scope: scope, TeamID: teamID, MemberID: memberID, OriginCallID: callID, Generation: s.grantGeneration[memberID]}
	s.grants[memberID] = grant
	return grant.Generation
}

func (s *teamScheduler) currentParent(memberID string, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[memberID]
	return ok && grant.Generation == generation
}

func (s *Service) teamPlanTrigger(memberID, runID, teamID string, scope teams.Scope, grantGeneration uint64) string {
	if s == nil || s.teamScheduler == nil {
		return ""
	}
	s.teamScheduler.mu.Lock()
	defer s.teamScheduler.mu.Unlock()
	grant, ok := s.teamScheduler.grants[memberID]
	if !ok || grant.Request.RunID != runID || grant.TeamID != teamID || !grant.Scope.Matches(scope) || grantGeneration != 0 && grant.Generation != grantGeneration {
		return ""
	}
	return grant.PlanRequestID
}

func (s *teamScheduler) consumePlanTrigger(memberID, runID, teamID string, scope teams.Scope, grantGeneration uint64, requestID string) {
	if s == nil || requestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[memberID]
	if ok && grant.Request.RunID == runID && grant.TeamID == teamID && grant.Scope.Matches(scope) && (grantGeneration == 0 || grant.Generation == grantGeneration) && grant.PlanRequestID == requestID {
		grant.PlanRequestID = ""
		s.grants[memberID] = grant
	}
}

func (s *teamScheduler) invalidateMember(memberID string) context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grantGeneration[memberID]++
	delete(s.grants, memberID)
	s.readyGeneration[memberID]++
	s.readySet[memberID] = false
	for turnID, activeMember := range s.activeMember {
		if activeMember == memberID {
			return s.active[turnID]
		}
	}
	return nil
}

func (s *teamScheduler) invalidateTeam(teamID string) []context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	for memberID, grant := range s.grants {
		if grant.TeamID == teamID {
			s.grantGeneration[memberID]++
			delete(s.grants, memberID)
			s.readyGeneration[memberID]++
			s.readySet[memberID] = false
		}
	}
	var cancels []context.CancelFunc
	for turnID, activeTeam := range s.activeTeam {
		if activeTeam == teamID {
			if cancel := s.active[turnID]; cancel != nil {
				cancels = append(cancels, cancel)
			}
		}
	}
	return cancels
}

func (s *teamScheduler) invalidateParentRun(runID string) []context.CancelFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	for memberID, grant := range s.grants {
		if grant.Request.RunID == runID {
			s.grantGeneration[memberID]++
			delete(s.grants, memberID)
			s.readyGeneration[memberID]++
			s.readySet[memberID] = false
		}
	}
	var cancels []context.CancelFunc
	for turnID, originRunID := range s.activeOrigin {
		if originRunID == runID {
			if cancel := s.active[turnID]; cancel != nil {
				cancels = append(cancels, cancel)
			}
		}
	}
	return cancels
}

func (s *teamScheduler) signalFromLead(request agent.ExecutionRequest, scope teams.Scope, teamID, memberID, callID string) {
	if request.TeamTurn == nil && !request.TeamUser && request.RunID != "" {
		generation := s.rememberParent(request, scope, teamID, memberID, callID)
		if generation != 0 {
			s.mu.Lock()
			if grant, ok := s.grants[memberID]; ok && grant.Generation == generation {
				grant.PlanRequestID = ""
				s.grants[memberID] = grant
			}
			s.mu.Unlock()
		}
	}
	s.queueReady(memberID, true)
}

func (s *teamScheduler) signalPlanResponse(request agent.ExecutionRequest, scope teams.Scope, teamID, memberID, requestID string) {
	if request.TeamTurn == nil {
		generation := uint64(0)
		if request.RunID != "" && !request.TeamUser {
			generation = s.rememberParent(request, scope, teamID, memberID, requestID)
		} else if s != nil {
			s.mu.Lock()
			if grant, ok := s.grants[memberID]; ok && grant.TeamID == teamID && grant.Scope.Matches(scope) {
				generation = grant.Generation
			}
			s.mu.Unlock()
		}
		if generation != 0 {
			s.mu.Lock()
			if grant, ok := s.grants[memberID]; ok && grant.Generation == generation {
				grant.PlanRequestID = requestID
				s.grants[memberID] = grant
			}
			s.mu.Unlock()
		}
	}
	s.queueReady(memberID, true)
}

func (s *teamScheduler) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	s.waiting = nil
	s.ready = nil
	s.readySet = map[string]bool{}
	s.readyGeneration = map[string]uint64{}
	s.grants = map[string]teamParentGrant{}
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, cancel := range s.active {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// SpawnTeamMember persists one member and its first accepted turn atomically
// with a real shared-pool reservation. A full pool rejects the spawn without
// leaving a member or accepted-turn fact behind.
func (s *Service) SpawnTeamMember(ctx context.Context, request agent.ExecutionRequest, spawn TeamMemberSpawnRequest) (teams.Member, error) {
	if s == nil || s.teamScheduler == nil || s.teamScheduler.submitter == nil {
		return teams.Member{}, errors.New("team member scheduler is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return teams.Member{}, err
	}
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil || !actor.Lead {
		return teams.Member{}, teams.ErrPermission
	}
	if request.TeamTurn != nil || request.Work.SessionID == "" {
		return teams.Member{}, teams.ErrPermission
	}
	if teams.ValidateText(spawn.Instruction, teams.MaxInputBytes, true) != nil || teams.ValidateText(spawn.OriginCallID, 256, false) != nil {
		return teams.Member{}, errors.New("team member requires a bounded task instruction")
	}
	name, err := teams.NormalizeMemberName(spawn.Name)
	if err != nil {
		return teams.Member{}, err
	}
	roleName, err := teams.NormalizeName(spawn.AgentName)
	if err != nil {
		return teams.Member{}, err
	}
	if s.deps.Agents == nil || s.deps.ForkProvider == nil || s.deps.ForkExecutorFactory == nil {
		return teams.Member{}, errors.New("team member role or read-only execution dependencies are unavailable")
	}
	definition, ok := s.deps.Agents.Resolve(roleName)
	if !ok {
		return teams.Member{}, errors.New("unknown team member role")
	}
	if definition.Isolation == "worktree" && (!request.TeamCoordinator || request.TeamCoordinatorTeamID != spawn.TeamID) {
		return teams.Member{}, teams.ErrPermission
	}
	tools := definition.EffectiveTools()
	if len(tools) == 0 {
		return teams.Member{}, errors.New("team member role has no permitted tools")
	}
	team, projection, err := s.teamForOperation(root, scope, spawn.TeamID, actor)
	if err != nil {
		return teams.Member{}, err
	}
	for _, prior := range projection.Members {
		if prior.TeamID == team.ID && prior.Name == name && !prior.Status.IsTerminal() {
			return teams.Member{}, errors.New("team member name is already active")
		}
	}
	_, serviceMembers, err := serviceTeamCapacity(root)
	if err != nil {
		return teams.Member{}, err
	}
	teamMembers := 0
	for _, prior := range projection.Members {
		if prior.TeamID == team.ID && !prior.Status.IsTerminal() {
			teamMembers++
		}
	}
	if err := teams.CheckCapacity(0, 0, teamMembers, serviceMembers, false, true); err != nil {
		return teams.Member{}, err
	}
	var authority permission.Authority
	if json.Unmarshal(request.PermissionBounds, &authority) != nil || authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID || authority.AllowedRoot == "" {
		return teams.Member{}, errors.New("team lead run authority is invalid")
	}
	if authority.AllowedRoot != team.Scope.ProjectRoot {
		return teams.Member{}, teams.ErrPermission
	}
	memberID, err := sessionlog.NewID()
	if err != nil {
		return teams.Member{}, err
	}
	turnID, err := sessionlog.NewID()
	if err != nil {
		return teams.Member{}, err
	}
	roleHash := teamRoleHash(definition)
	member := teams.Member{
		ID: memberID, TeamID: team.ID, Name: name, AgentName: definition.Name,
		RoleHash: hex.EncodeToString(roleHash[:]),
		Tools:    append([]string(nil), tools...), Status: teams.MemberCreated,
		PlanRequired: spawn.PlanRequired, Revision: 1,
	}
	model := definition.Model
	if model == "" || model == "inherit" {
		model = request.Model
	}
	if model == "" {
		model = s.deps.Model
	}
	if model == "" {
		return teams.Member{}, errors.New("team member model is unavailable")
	}
	member.Model = model
	parent := agent.ParentRun{
		TeamTurn: &agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turnID, MemberName: member.Name},
		RunID:    request.RunID, ToolCallID: spawn.OriginCallID, Work: request.Work,
		ProjectRoot: authority.AllowedRoot, PermissionBounds: append(json.RawMessage(nil), request.PermissionBounds...),
		Provider: s.deps.ForkProvider, ProviderName: request.ProviderName, Model: model,
		Budget: agent.DefaultDelegationLimits(), RoleInstruction: definition.Instruction,
	}
	var memberWorkspace *workspace.LifecycleService
	var writerLease *workspace.WriterLease
	workspaceCreated := false
	if definition.Isolation == "worktree" {
		parent, memberWorkspace, writerLease, workspaceCreated, err = s.bindTeamMemberWorkspace(ctx, request, team.Scope, member, parent, tools)
		if err != nil {
			return teams.Member{}, err
		}
		member.WorkspaceID = writerLease.WorkspaceID
	} else if member.WorkspaceID != "" {
		return teams.Member{}, errors.New("team member worktree isolation cannot be removed after workspace creation")
	}
	if parent.ProviderName == "" {
		parent.ProviderName = s.deps.ProviderName
	}
	if definition.MaxTurns > 0 && definition.MaxTurns < parent.Budget.MaxToolRounds {
		parent.Budget.MaxToolRounds = definition.MaxTurns
	}
	if member.Budget.RemainingDuration() < parent.Budget.MaxDuration {
		parent.Budget.MaxDuration = member.Budget.RemainingDuration()
	}
	if definition.Isolation != "worktree" {
		parent.ToolSchemas, err = agent.TeamMemberToolSchemas(s.deps.ToolSchemas, tools)
		if err != nil {
			return teams.Member{}, err
		}
		parent.ExecutorFactory, err = agent.NewTeamMemberExecutorFactory(s.deps.ForkExecutorFactory, tools, s.executeTeamMemberTool)
		if err != nil {
			return teams.Member{}, err
		}
	}
	input, err := agent.BuildTeamTurnTask(turnID, definition.Instruction+"\n\nAssigned task:\n"+spawn.Instruction, agent.TeamTurnInput{
		Identity: *parent.TeamTurn,
	})
	if err != nil {
		if writerLease != nil {
			cleanupTeamMemberWorkspace(memberWorkspace, *writerLease, workspaceCreated)
		}
		return teams.Member{}, err
	}
	input.Name = member.Name
	startedAt := time.Time{}
	runSeq := uint64(0)
	durableTerminal := false
	durableResult := agent.ChildRunResult{}
	var lifecycleMu sync.Mutex
	parent.TeamLifecycle = &agent.TeamRunLifecycle{
		Started: func(child agent.ChildRunInput) error {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			if err := s.persistTeamChildStart(root, team.Scope, member.ID, turnID, request.RunID, spawn.OriginCallID, child, &runSeq); err != nil {
				return err
			}
			startedAt = time.Now()
			return nil
		},
		Finished: func(child agent.ChildRunInput, result agent.ChildRunResult) error {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			if writerLease != nil {
				if _, releaseErr := memberWorkspace.ReleaseCompletedWriter(context.Background(), *writerLease); releaseErr != nil {
					result.Status = agent.DelegationInterrupted
					result.Error = "team member workspace writer could not be safely released: " + releaseErr.Error()
				}
			}
			if err := s.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turnID, child, result, &runSeq); err != nil {
				return err
			}
			durableResult = result
			durableTerminal = true
			return nil
		},
	}
	accepted, err := s.submitTeamMember(ctx, root, scope, team, member, request, parent, input, &startedAt, &runSeq, &durableTerminal, &durableResult, false, nil, false, 0, "", memberWorkspace, writerLease)
	if err != nil {
		if writerLease != nil {
			cleanupTeamMemberWorkspaceUnlessReferenced(root, request.Work.SessionID, team.ID, member.ID, memberWorkspace, *writerLease, workspaceCreated)
		}
		return teams.Member{}, err
	}
	s.teamScheduler.mu.Lock()
	s.teamScheduler.roles[member.ID] = definition
	s.teamScheduler.mu.Unlock()
	return accepted, nil
}

// ResumeTeamMember creates one explicit follow-up turn for an idle or
// interrupted member. It reuses the pinned role identity, carries only the
// member summary, assigned unfinished tasks and one bounded pending-message
// batch, and persists accepted handoffs before the reserved pool slot is made
// visible to a worker.
func (s *Service) ResumeTeamMember(ctx context.Context, request agent.ExecutionRequest, teamID, memberID, originCallID string) (teams.Member, error) {
	return s.resumeTeamMember(ctx, request, teamID, memberID, originCallID, 0)
}

func (s *Service) resumeTeamMemberWithGrant(grant teamParentGrant, acceptRoleChange bool) (teams.Member, error) {
	if s == nil || s.teamScheduler == nil || !s.teamScheduler.currentParent(grant.MemberID, grant.Generation) {
		return teams.Member{}, teams.ErrPermission
	}
	request := grant.Request
	request.AcceptTeamRoleChange = acceptRoleChange
	return s.resumeTeamMember(s.lifeCtx, request, grant.TeamID, grant.MemberID, grant.OriginCallID, grant.Generation)
}

func (s *Service) resumeTeamMember(ctx context.Context, request agent.ExecutionRequest, teamID, memberID, originCallID string, grantGeneration uint64) (teams.Member, error) {
	if s == nil || s.teamScheduler == nil || s.teamScheduler.submitter == nil {
		return teams.Member{}, errors.New("team member scheduler is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return teams.Member{}, err
	}
	if request.TeamUser || request.TeamTurn != nil || request.RunID == "" || teams.ValidateText(originCallID, 256, true) != nil {
		return teams.Member{}, teams.ErrPermission
	}
	var root string
	var scope teams.Scope
	var actor teams.Actor
	var err error
	if grantGeneration == 0 {
		root, scope, actor, err = s.teamOperationScope(ctx, request)
		if err != nil || !actor.Lead {
			return teams.Member{}, teams.ErrPermission
		}
	} else {
		root, scope, err = s.teamScope(ctx, request)
		if err != nil || !s.teamScheduler.currentParent(memberID, grantGeneration) {
			return teams.Member{}, teams.ErrPermission
		}
		actor = teams.Actor{Lead: true}
	}
	if err := validateTeamLeadAuthority(request, scope); err != nil {
		return teams.Member{}, err
	}
	s.eventMu.Lock()
	if err := s.reconcileExpiredPlanMembers(root, scope.SessionID, teamID); err != nil {
		s.eventMu.Unlock()
		return teams.Member{}, err
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	s.eventMu.Unlock()
	if err != nil {
		return teams.Member{}, err
	}
	member, ok := projection.Members[memberID]
	if !ok || member.TeamID != team.ID {
		return teams.Member{}, teams.ErrPermission
	}
	planRequestID := s.teamPlanTrigger(member.ID, request.RunID, team.ID, scope, grantGeneration)
	if planRequestID != "" && member.TurnID != "" {
		if turn, exists := projection.Turns[member.TurnID]; exists && turn.MemberID == member.ID && turn.PlanRequestID == planRequestID && turn.Status != "intent" && turn.Status != "aborted" {
			// A plan response is a one-shot scheduler trigger. If its follow-up
			// already ran before an explicit resume arrives, the request has been
			// satisfied even though its turn is now terminal.
			s.teamScheduler.consumePlanTrigger(member.ID, request.RunID, team.ID, scope, grantGeneration, planRequestID)
			return member, nil
		}
	}
	// A persisted worktree member can write on every resumed turn. Keep the
	// same server-side team binding required for its initial spawn; callers
	// cannot turn an ordinary lead run into workspace writer authority.
	if grantGeneration == 0 && member.WorkspaceID != "" && (!request.TeamCoordinator || request.TeamCoordinatorTeamID != team.ID) {
		return teams.Member{}, teams.ErrPermission
	}
	if turn, active := acceptedTeamTurnForMember(projection, member.ID); active && !member.Status.IsTerminal() && member.Status != teams.MemberStopping {
		member.Status = teams.MemberQueued
		if turn.Status == "running" {
			member.Status = teams.MemberRunning
		}
		member.RunID, member.TurnID = turn.RunID, turn.ID
		return member, nil
	}
	// A message or approved plan may already have queued the member by the time
	// an explicit resume reaches the service. Treat that request as satisfied.
	if (member.Status == teams.MemberQueued || member.Status == teams.MemberRunning) && hasAcceptedCurrentTeamTurn(projection, member) {
		return member, nil
	}
	if member.Status != teams.MemberIdle && member.Status != teams.MemberInterrupted {
		return teams.Member{}, teams.ErrPermission
	}
	planRevisionAllowed := false
	for _, planRequest := range projection.Requests {
		if planRequest.TeamID == team.ID && planRequest.MemberID == member.ID && planRequest.Type == teams.RequestPlan && (planRequest.Status == teams.RequestRejected || planRequest.Status == teams.RequestExpired) {
			planRevisionAllowed = true
			break
		}
	}
	if member.PlanRequired && !member.PlanApproved && !planRevisionAllowed {
		return teams.Member{}, errors.New("team member requires approved plan before it can resume")
	}
	if err := member.Budget.CanAccept(); err != nil {
		return teams.Member{}, err
	}
	if grantGeneration == 0 {
		grantGeneration = s.teamScheduler.rememberParent(request, scope, team.ID, member.ID, originCallID)
		if grantGeneration == 0 {
			return teams.Member{}, teams.ErrPermission
		}
	}
	s.teamScheduler.mu.Lock()
	readyGeneration := s.teamScheduler.readyGeneration[memberID]
	s.teamScheduler.mu.Unlock()
	definition, err := s.teamMemberRoleSnapshot(member, request.AcceptTeamRoleChange)
	if err != nil {
		return teams.Member{}, err
	}
	roleHash := teamRoleHash(definition)
	member.RoleHash = hex.EncodeToString(roleHash[:])
	member.Tools = definition.EffectiveTools()
	if definition.Model != "" && definition.Model != "inherit" {
		member.Model = definition.Model
	}
	turnID, err := sessionlog.NewID()
	if err != nil {
		return teams.Member{}, err
	}
	identity := agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turnID, MemberName: member.Name}
	messages, handoffs := pendingTeamMessageBatch(projection, team.ID, member.ID)
	planFeedback := latestRejectedPlanFeedback(projection, team.ID, member.ID)
	tasks, err := resumableMemberTasks(projection, team.ID, member.ID)
	if err != nil {
		return teams.Member{}, err
	}
	task, err := agent.BuildTeamTurnTask(turnID, definition.Instruction+"\n\nContinue the prior assigned work. Use the prior summary, unfinished assigned tasks, and any newly delivered messages as reference data.", agent.TeamTurnInput{
		Identity:     identity,
		Summary:      member.Summary,
		PlanFeedback: planFeedback,
		Messages:     messages,
		Tasks:        tasks,
	})
	if err != nil {
		return teams.Member{}, err
	}
	task.Name = member.Name
	parent := agent.ParentRun{
		TeamTurn: &identity, RunID: request.RunID, ToolCallID: originCallID, Work: request.Work,
		ProjectRoot: scope.ProjectRoot, PermissionBounds: append(json.RawMessage(nil), request.PermissionBounds...),
		Provider: s.deps.ForkProvider, ProviderName: request.ProviderName, Model: member.Model,
		Budget: agent.DefaultDelegationLimits(), RoleInstruction: definition.Instruction,
	}
	var memberWorkspace *workspace.LifecycleService
	var writerLease *workspace.WriterLease
	workspaceCreated := false
	if definition.Isolation == "worktree" {
		parent, memberWorkspace, writerLease, workspaceCreated, err = s.bindTeamMemberWorkspace(ctx, request, team.Scope, member, parent, member.Tools)
		if err != nil {
			return teams.Member{}, err
		}
		member.WorkspaceID = writerLease.WorkspaceID
	} else if member.WorkspaceID != "" {
		return teams.Member{}, errors.New("team member worktree isolation cannot be removed after workspace creation")
	}
	if parent.ProviderName == "" {
		parent.ProviderName = s.deps.ProviderName
	}
	if definition.MaxTurns > 0 && definition.MaxTurns < parent.Budget.MaxToolRounds {
		parent.Budget.MaxToolRounds = definition.MaxTurns
	}
	if remaining := member.Budget.RemainingDuration(); remaining < parent.Budget.MaxDuration {
		parent.Budget.MaxDuration = remaining
	}
	if definition.Isolation != "worktree" {
		parent.ToolSchemas, err = agent.TeamMemberToolSchemas(s.deps.ToolSchemas, member.Tools)
		if err != nil {
			return teams.Member{}, err
		}
		parent.ExecutorFactory, err = agent.NewTeamMemberExecutorFactory(s.deps.ForkExecutorFactory, member.Tools, s.executeTeamMemberTool)
		if err != nil {
			return teams.Member{}, err
		}
	}
	startedAt := time.Time{}
	runSeq := uint64(0)
	durableTerminal := false
	durableResult := agent.ChildRunResult{}
	var lifecycleMu sync.Mutex
	parent.TeamLifecycle = &agent.TeamRunLifecycle{
		Started: func(child agent.ChildRunInput) error {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			if err := s.persistTeamChildStart(root, team.Scope, member.ID, turnID, request.RunID, originCallID, child, &runSeq); err != nil {
				return err
			}
			startedAt = time.Now()
			return nil
		},
		Finished: func(child agent.ChildRunInput, result agent.ChildRunResult) error {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			if writerLease != nil {
				if _, releaseErr := memberWorkspace.ReleaseCompletedWriter(context.Background(), *writerLease); releaseErr != nil {
					result.Status = agent.DelegationInterrupted
					result.Error = "team member workspace writer could not be safely released: " + releaseErr.Error()
				}
			}
			if err := s.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turnID, child, result, &runSeq); err != nil {
				return err
			}
			durableResult, durableTerminal = result, true
			return nil
		},
	}
	accepted, err := s.submitTeamMember(ctx, root, scope, team, member, request, parent, task, &startedAt, &runSeq, &durableTerminal, &durableResult, true, handoffs, false, grantGeneration, planRequestID, memberWorkspace, writerLease)
	if errors.Is(err, agent.ErrDelegationQueueFull) {
		waiting, waitErr := s.markMemberWaitingCapacity(root, request.Work.SessionID, team.ID, member)
		if waitErr != nil {
			if writerLease != nil {
				cleanupTeamMemberWorkspaceUnlessReferenced(root, request.Work.SessionID, team.ID, member.ID, memberWorkspace, *writerLease, workspaceCreated)
			}
			return teams.Member{}, err
		}
		member = waiting
		if writerLease != nil {
			if _, releaseErr := memberWorkspace.ReleaseCompletedWriter(context.Background(), *writerLease); releaseErr != nil {
				_ = s.restoreCapacityWaiter(root, request.Work.SessionID, team.ID, member.ID)
				return teams.Member{}, releaseErr
			}
			writerLease = nil
		}
		s.teamScheduler.mu.Lock()
		s.teamScheduler.roles[member.ID] = definition
		s.teamScheduler.mu.Unlock()
		queuedRequest := request
		queuedRequest.Intent = ""
		queuedRequest.Messages = nil
		queuedRequest.AllowedScope = nil
		queuedRequest.ResourceBounds = nil
		queuedRequest.ToolSchemas = nil
		deferred := func() error {
			if !s.teamScheduler.currentParent(member.ID, grantGeneration) {
				_ = s.restoreCapacityWaiter(root, queuedRequest.Work.SessionID, team.ID, member.ID)
				return nil
			}
			if definition.Isolation == "worktree" {
				parent, memberWorkspace, writerLease, workspaceCreated, err = s.bindTeamMemberWorkspace(s.lifeCtx, queuedRequest, team.Scope, member, parent, member.Tools)
				if err != nil {
					_ = s.restoreCapacityWaiter(root, queuedRequest.Work.SessionID, team.ID, member.ID)
					return err
				}
			}
			_, retryErr := s.submitTeamMember(s.lifeCtx, root, scope, team, member, queuedRequest, parent, task, &startedAt, &runSeq, &durableTerminal, &durableResult, true, handoffs, true, grantGeneration, planRequestID, memberWorkspace, writerLease)
			if errors.Is(retryErr, agent.ErrDelegationQueueFull) {
				// Keep the durable capacity-waiting state while this FIFO head
				// remains queued. A later pool-capacity event retries it; changing
				// it to interrupted here exposes a false terminal-looking state.
				if writerLease != nil {
					_, _ = memberWorkspace.ReleaseCompletedWriter(context.Background(), *writerLease)
					writerLease = nil
				}
				return retryErr
			}
			if retryErr != nil {
				_ = s.restoreCapacityWaiter(root, queuedRequest.Work.SessionID, team.ID, member.ID)
				if writerLease != nil {
					cleanupTeamMemberWorkspaceUnlessReferenced(root, queuedRequest.Work.SessionID, team.ID, member.ID, memberWorkspace, *writerLease, workspaceCreated)
				}
			}
			return retryErr
		}
		if enqueueErr := s.teamScheduler.enqueueCapacityResume(deferred); enqueueErr != nil {
			_ = s.restoreCapacityWaiter(root, request.Work.SessionID, team.ID, member.ID)
			if writerLease != nil {
				cleanupTeamMemberWorkspaceUnlessReferenced(root, request.Work.SessionID, team.ID, member.ID, memberWorkspace, *writerLease, workspaceCreated)
			}
			return teams.Member{}, enqueueErr
		}
		s.teamScheduler.clearReady(memberID, readyGeneration)
		return member, nil
	}
	if err != nil {
		if writerLease != nil {
			cleanupTeamMemberWorkspaceUnlessReferenced(root, request.Work.SessionID, team.ID, member.ID, memberWorkspace, *writerLease, workspaceCreated)
		}
		return teams.Member{}, err
	}
	s.teamScheduler.mu.Lock()
	s.teamScheduler.roles[member.ID] = definition
	s.teamScheduler.mu.Unlock()
	s.teamScheduler.clearReady(memberID, readyGeneration)
	return accepted, nil
}

func validateTeamLeadAuthority(request agent.ExecutionRequest, scope teams.Scope) error {
	var authority permission.Authority
	if json.Unmarshal(request.PermissionBounds, &authority) != nil || authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID || authority.AllowedRoot != scope.ProjectRoot {
		return errors.New("team lead run authority is invalid")
	}
	return nil
}

func (s *Service) teamMemberRoleSnapshot(member teams.Member, acceptChange bool) (agentcatalog.Definition, error) {
	s.teamScheduler.mu.Lock()
	definition, ok := s.teamScheduler.roles[member.ID]
	s.teamScheduler.mu.Unlock()
	pinned := definition
	hasPinned := ok
	if s.deps.Agents != nil {
		if current, found := s.deps.Agents.Resolve(member.AgentName); found {
			definition, ok = current, true
		}
	}
	if !ok {
		return agentcatalog.Definition{}, errors.New("original team member role is no longer available")
	}
	if hasPinned {
		pinnedHash := teamRoleHash(pinned)
		if hex.EncodeToString(pinnedHash[:]) == member.RoleHash {
			definition = pinned
		}
	}
	hash := teamRoleHash(definition)
	changed := hex.EncodeToString(hash[:]) != member.RoleHash || !sameStringList(definition.EffectiveTools(), member.Tools) || (definition.Model != "" && definition.Model != "inherit" && definition.Model != member.Model)
	if definition.Name != member.AgentName || len(definition.EffectiveTools()) == 0 {
		return agentcatalog.Definition{}, errors.New("original team member role is unavailable or no longer has permitted tools")
	}
	if changed && !acceptChange {
		model := definition.Model
		if model == "" || model == "inherit" {
			model = member.Model
		}
		changes := []string{"role definition fingerprint"}
		if model != member.Model {
			changes = append(changes, fmt.Sprintf("model %s -> %s", member.Model, model))
		}
		if !sameStringList(definition.EffectiveTools(), member.Tools) {
			changes = append(changes, fmt.Sprintf("tools %v -> %v", member.Tools, definition.EffectiveTools()))
		}
		return agentcatalog.Definition{}, fmt.Errorf("team member role changed (%s); role instructions are not persisted, inspect the current %q definition and resume with --accept-role-change to accept it", strings.Join(changes, "; "), member.AgentName)
	}
	return definition, nil
}

func teamRoleHash(definition agentcatalog.Definition) [32]byte {
	encoded, _ := json.Marshal(struct {
		Name        string   `json:"name"`
		Instruction string   `json:"instruction"`
		Model       string   `json:"model"`
		Tools       []string `json:"tools"`
		MaxTurns    int      `json:"max_turns"`
		Isolation   string   `json:"isolation"`
	}{definition.Name, definition.Instruction, definition.Model, definition.EffectiveTools(), definition.MaxTurns, definition.Isolation})
	return sha256.Sum256(encoded)
}

func sameStringList(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func pendingTeamMessageBatch(projection sessionlog.TeamProjection, teamID, memberID string) ([]teams.Message, []sessionlog.HandoffFact) {
	latest := map[string]sessionlog.HandoffFact{}
	for _, handoff := range projection.Handoffs {
		if handoff.RecipientID == memberID && handoff.MessageID != "" {
			latest[handoff.MessageID] = handoff
		}
	}
	messages := make([]teams.Message, 0)
	for _, message := range projection.Messages {
		if message.TeamID == teamID && containsString(message.Recipients, memberID) {
			handoff, delivered := latest[message.ID]
			if delivered {
				prior := projection.Turns[handoff.DestinationTurnID]
				if prior.Status != "interrupted" {
					continue
				}
			}
			messages = append(messages, message)
		}
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].Seq < messages[j].Seq })
	selected := make([]teams.Message, 0, teams.MaxBatchMessages)
	selectedHandoffs := make([]sessionlog.HandoffFact, 0, teams.MaxBatchMessages)
	usedBytes := 0
	for _, message := range messages {
		if len(selected) >= teams.MaxBatchMessages || usedBytes+len(message.Body) > teams.MaxBatchBytes {
			break
		}
		selected = append(selected, message)
		usedBytes += len(message.Body)
		handoff := sessionlog.HandoffFact{MessageID: message.ID, RecipientID: memberID}
		if prior, delivered := latest[message.ID]; delivered {
			handoff.RetryOfTurnID = prior.DestinationTurnID
		}
		selectedHandoffs = append(selectedHandoffs, handoff)
	}
	return selected, selectedHandoffs
}

func latestRejectedPlanFeedback(projection sessionlog.TeamProjection, teamID, memberID string) string {
	var latest *teams.Request
	for _, request := range projection.Requests {
		if request.TeamID != teamID || request.MemberID != memberID || request.Type != teams.RequestPlan || request.Status != teams.RequestRejected {
			continue
		}
		if latest == nil || request.ExpiresAt.After(latest.ExpiresAt) || request.ExpiresAt.Equal(latest.ExpiresAt) && request.ID > latest.ID {
			copy := request
			latest = &copy
		}
	}
	if latest == nil {
		return ""
	}
	return latest.Feedback
}

func resumableMemberTasks(projection sessionlog.TeamProjection, teamID, memberID string) ([]teams.Task, error) {
	graph, err := teamTaskGraph(projection, teamID)
	if err != nil {
		return nil, err
	}
	out := make([]teams.Task, 0)
	for _, task := range graph.List() {
		if task.Assignee == memberID && task.Status != teams.TaskCompleted {
			out = append(out, task)
		}
	}
	return out, nil
}

func (s *Service) submitTeamMember(ctx context.Context, root string, scope teams.Scope, team teams.Team, member teams.Member, request agent.ExecutionRequest, parent agent.ParentRun, task agent.DelegationTask, startedAt *time.Time, runSeq *uint64, durableTerminal *bool, durableResult *agent.ChildRunResult, memberExists bool, handoffs []sessionlog.HandoffFact, waitingRetry bool, grantGeneration uint64, planRequestID string, memberWorkspace *workspace.LifecycleService, writerLease *workspace.WriterLease) (teams.Member, error) {
	turnID := parent.TeamTurn.TurnID
	messageIDs := make([]string, 0, len(handoffs))
	for _, handoff := range handoffs {
		messageIDs = append(messageIDs, handoff.MessageID)
	}
	turn := sessionlog.TurnFact{ID: turnID, MemberID: member.ID, TaskID: task.ID, MessageIDs: messageIDs, OriginRunID: request.RunID, OriginCallID: parent.ToolCallID, PlanRequestID: planRequestID, Status: "intent"}
	if writerLease != nil {
		turn.WorkspaceID, turn.WorkspaceGeneration = writerLease.WorkspaceID, writerLease.Generation
	}
	poolCtx := s.lifeCtx
	if poolCtx == nil {
		poolCtx = context.Background()
	}
	s.teamScheduler.mu.Lock()
	closed := s.teamScheduler.closed
	s.teamScheduler.mu.Unlock()
	if closed {
		return teams.Member{}, errors.New("team member scheduler is closed")
	}
	childRunID := ""
	admissionCommitted := false
	var childInput agent.ChildRunInput
	handle, err := s.teamScheduler.submitter.SubmitTaskCommitted(poolCtx, parent, task, func(admission agent.TaskAdmission) error {
		s.eventMu.Lock()
		defer s.eventMu.Unlock()
		s.teamScheduler.mu.Lock()
		defer s.teamScheduler.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.teamScheduler.closed {
			return errors.New("team member scheduler is closed")
		}
		if grantGeneration == 0 && !waitingRetry && !s.activeLeadRun(request) {
			return teams.ErrPermission
		}
		if grantGeneration != 0 {
			grant, ok := s.teamScheduler.grants[member.ID]
			if !ok || grant.Generation != grantGeneration || grant.TeamID != team.ID || !grant.Scope.Matches(scope) || grant.Request.RunID != request.RunID {
				return teams.ErrPermission
			}
		}
		current, currentProjection, checkErr := s.teamForOperation(root, scope, team.ID, teams.Actor{Lead: true})
		if checkErr != nil || current.Status != teams.TeamOpen {
			return teams.ErrPermission
		}
		currentMember, found := currentProjection.Members[member.ID]
		if memberExists {
			if turn, active := acceptedTeamTurnForMember(currentProjection, member.ID); found && active && currentMember.TeamID == team.ID && !currentMember.Status.IsTerminal() && currentMember.Status != teams.MemberStopping {
				// Automatic scheduling and an explicit resume can race. Once the
				// member already owns an accepted turn, the later resume is satisfied.
				member = currentMember
				member.Status = teams.MemberQueued
				if turn.Status == "running" {
					member.Status = teams.MemberRunning
				}
				member.RunID, member.TurnID = turn.RunID, turn.ID
				return errTeamMemberAlreadyActive
			}
			if found && waitingRetry && currentMember.TeamID == team.ID && currentMember.Status == teams.MemberInterrupted && member.Status == teams.MemberWaitingCapacity && currentMember.RunID == member.RunID && currentMember.TurnID == member.TurnID {
				// A previous capacity retry may have failed its admission check and
				// restored this exact waiter to interrupted. Refresh only that same
				// prior turn identity; grant validation above still fences stop/close.
				member = currentMember
			}
			allowedStatus := currentMember.Status == teams.MemberIdle || currentMember.Status == teams.MemberInterrupted || waitingRetry && currentMember.Status == teams.MemberWaitingCapacity
			if !found || currentMember.Revision != member.Revision || currentMember.Status != member.Status || currentMember.TeamID != team.ID || !allowedStatus {
				return teams.ErrPermission
			}
			roleChanged := currentMember.RoleHash != member.RoleHash || currentMember.Model != member.Model || !sameStringList(currentMember.Tools, member.Tools)
			if roleChanged {
				if !request.AcceptTeamRoleChange || (currentMember.Status != teams.MemberIdle && currentMember.Status != teams.MemberInterrupted && currentMember.Status != teams.MemberWaitingCapacity) {
					return teams.ErrPermission
				}
				member.Status = currentMember.Status
				member.RunID, member.TurnID = currentMember.RunID, currentMember.TurnID
				member.Revision = currentMember.Revision + 1
				if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
					return err
				}
			} else {
				member = currentMember
			}
		} else {
			if found {
				return teams.ErrPermission
			}
			_, globalMembers, capacityErr := serviceTeamCapacity(root)
			if capacityErr != nil {
				return capacityErr
			}
			currentTeamMembers := 0
			for _, existing := range currentProjection.Members {
				if existing.TeamID == team.ID && !existing.Status.IsTerminal() {
					currentTeamMembers++
				}
			}
			if capacityErr = teams.CheckCapacity(0, 0, currentTeamMembers, globalMembers, false, true); capacityErr != nil {
				return capacityErr
			}
		}
		turn.RunID = admission.ChildRunID
		childRunID = admission.ChildRunID
		turn.Status = "intent"
		childInput = agent.ChildRunInput{TeamTurn: parent.TeamTurn, ParentRunID: request.RunID, BatchID: admission.BatchID, ChildRunID: admission.ChildRunID, WorkspaceID: parent.WorkspaceID, WorkspaceGeneration: parent.WorkspaceGeneration, Task: task, Work: parent.Work}
		if !memberExists {
			if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
				return err
			}
			admissionCommitted = true
		}
		if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
			return err
		}
		admissionCommitted = true
		accepted := turn
		accepted.Status = "queued"
		if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
			return err
		}
		if err := s.persistTeamChildQueuedLocked(root, scope, request.RunID, parent.ToolCallID, childInput, runSeq); err != nil {
			return err
		}
		// The reservation is still private to the pool until this callback
		// returns. If the following handoff append fails, compensate the durable
		// accepted turn so it cannot be mistaken for work that may run.
		for _, handoff := range handoffs {
			handoff.DestinationRunID = admission.ChildRunID
			handoff.DestinationTurnID = turnID
			var appendErr error
			if s.teamScheduler.appendHandoff != nil {
				appendErr = s.teamScheduler.appendHandoff(root, scope.SessionID, team.ID, handoff)
			} else {
				appendErr = appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMessageHandoff, ActorID: "service", ActorRunID: request.RunID, Handoff: &handoff})
			}
			if appendErr != nil {
				return appendErr
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errTeamMemberAlreadyActive) {
			return member, nil
		}
		if admissionCommitted {
			if compensateErr := s.compensateUnpublishedTeamAdmission(root, scope.SessionID, team.ID, member.ID, turnID, childInput, runSeq, err); compensateErr != nil {
				return teams.Member{}, fmt.Errorf("team turn admission failed: %v; durable compensation failed: %w", err, compensateErr)
			}
		}
		return teams.Member{}, err
	}
	s.teamScheduler.mu.Lock()
	s.teamScheduler.active[turnID] = handle.Cancel
	s.teamScheduler.activeOrigin[turnID] = request.RunID
	s.teamScheduler.activeMember[turnID] = member.ID
	s.teamScheduler.activeTeam[turnID] = team.ID
	var workspaceRunDone chan struct{}
	if writerLease != nil {
		workspaceRunDone = make(chan struct{})
		s.teamScheduler.workspaceRuns[writerLease.RunID] = teamWorkspaceRun{manager: memberWorkspace, lease: *writerLease, cancel: handle.Cancel, done: workspaceRunDone}
	}
	closed = s.teamScheduler.closed
	grantCurrent := grantGeneration == 0
	if grantGeneration != 0 {
		grant, ok := s.teamScheduler.grants[member.ID]
		grantCurrent = ok && grant.Generation == grantGeneration
	}
	s.teamScheduler.mu.Unlock()
	if closed || !grantCurrent {
		handle.Cancel()
	}
	go s.watchTeamMember(root, scope.SessionID, team.ID, member.ID, turnID, handle, startedAt, durableTerminal, durableResult, memberWorkspace, writerLease, workspaceRunDone)
	member.Status, member.RunID, member.TurnID = teams.MemberQueued, childRunID, turnID
	return member, nil
}

func hasAcceptedCurrentTeamTurn(projection sessionlog.TeamProjection, member teams.Member) bool {
	turn, ok := projection.Turns[member.TurnID]
	return member.TurnID != "" && ok && turn.MemberID == member.ID && (turn.Status == "queued" || turn.Status == "running")
}

func acceptedTeamTurnForMember(projection sessionlog.TeamProjection, memberID string) (sessionlog.TurnFact, bool) {
	for _, turn := range projection.Turns {
		if turn.MemberID == memberID && (turn.Status == "queued" || turn.Status == "running") {
			return turn, true
		}
	}
	return sessionlog.TurnFact{}, false
}

// prepareTeamMemberWorkspace materializes or reuses the member's service-owned
// worktree and acquires its writer lease before the turn becomes visible to the
// shared child pool. The child RunID is generated by the service before any
// queue or event locks are taken.
func (s *Service) bindTeamMemberWorkspace(ctx context.Context, request agent.ExecutionRequest, teamScope teams.Scope, member teams.Member, parent agent.ParentRun, roleTools []string) (agent.ParentRun, *workspace.LifecycleService, *workspace.WriterLease, bool, error) {
	childRunID, err := sessionlog.NewID()
	if err != nil {
		return parent, nil, nil, false, err
	}
	parent.ChildRunID = childRunID
	manager, lease, created, err := s.prepareTeamMemberWorkspace(ctx, request, teamScope, member, childRunID)
	if err != nil {
		return parent, nil, nil, false, err
	}
	parent.WorkspaceID, parent.WorkspaceGeneration = lease.WorkspaceID, lease.Generation
	parent.ProjectRoot = lease.Authority.AllowedRoot
	parent.PermissionBounds, err = json.Marshal(lease.Authority)
	if err == nil {
		parent.ExecutorFactory = execution.WorkspaceWriterExecutorFactory(s.deps.ForkExecutorFactory, *lease, manager)
		parent.ToolSchemas, err = worktreeTeamMemberSchemas(s.deps.ToolSchemas, roleTools)
	}
	if err == nil {
		parent.ExecutorFactory, err = agent.NewWorktreeTeamMemberExecutorFactory(parent.ExecutorFactory, roleTools, s.executeTeamMemberTool)
	}
	if err != nil {
		cleanupTeamMemberWorkspace(manager, *lease, created)
		return parent, nil, nil, false, err
	}
	return parent, manager, lease, created, nil
}

func (s *Service) prepareTeamMemberWorkspace(ctx context.Context, request agent.ExecutionRequest, teamScope teams.Scope, member teams.Member, childRunID string) (*workspace.LifecycleService, *workspace.WriterLease, bool, error) {
	var authority permission.Authority
	if s.deps.WorkspaceStateRoot == "" || json.Unmarshal(request.PermissionBounds, &authority) != nil || authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID || authority.AllowedRoot != teamScope.ProjectRoot || authority.Mode == permission.ModePlan {
		return nil, nil, false, workspace.ErrOwnership
	}
	projectRoot, scope, err := s.workspaceScope(ctx, ClientMsg{SessionID: request.Work.SessionID, WorkKind: string(request.Work.Kind), GoalID: request.Work.GoalID, WorkItemID: request.Work.WorkItemID})
	if err != nil || projectRoot != teamScope.ProjectRoot {
		return nil, nil, false, workspace.ErrOwnership
	}
	scope.Authority = authority
	scope.OriginRunID = request.RunID
	scope.OriginTaskID = member.ID
	if err := scope.ValidateAuthority(); err != nil {
		return nil, nil, false, workspace.ErrOwnership
	}
	manager, err := s.workspaceService(projectRoot)
	if err != nil {
		return nil, nil, false, err
	}
	created := false
	workspaceID := member.WorkspaceID
	if workspaceID == "" {
		snapshot, createErr := manager.Create(ctx, scope, member.Name)
		if createErr != nil {
			return nil, nil, false, createErr
		}
		workspaceID, created = snapshot.ID, true
	} else {
		if _, getErr := manager.Get(ctx, scope, workspaceID); getErr != nil {
			return nil, nil, false, getErr
		}
	}
	lease, err := manager.AcquireWriter(ctx, scope, workspaceID, childRunID)
	if err != nil {
		if created {
			_, _ = manager.RemoveClean(context.Background(), scope, workspaceID)
		}
		return nil, nil, false, err
	}
	return manager, &lease, created, nil
}

func worktreeTeamMemberSchemas(serviceSchemas []llm.ToolSchema, roleTools []string) ([]llm.ToolSchema, error) {
	memberTools, err := agent.TeamMemberToolSchemas(serviceSchemas, nil)
	if err != nil {
		return nil, err
	}
	schemas := append(execution.WorkspaceWriterToolSchemas(), memberTools...)
	return agent.WorktreeTeamMemberToolSchemas(schemas, roleTools)
}

func cleanupTeamMemberWorkspace(manager *workspace.LifecycleService, lease workspace.WriterLease, created bool) {
	if manager == nil {
		return
	}
	if _, err := manager.ReleaseCompletedWriter(context.Background(), lease); err != nil {
		return
	}
	if created {
		_, _ = manager.RemoveClean(context.Background(), lease.Scope, lease.WorkspaceID)
	}
}

func cleanupTeamMemberWorkspaceUnlessReferenced(root, sessionID, teamID, memberID string, manager *workspace.LifecycleService, lease workspace.WriterLease, created bool) {
	if manager == nil {
		return
	}
	if created {
		projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
		if err == nil {
			if member, ok := projection.Members[memberID]; ok && member.WorkspaceID == lease.WorkspaceID {
				_, _ = manager.ReleaseCompletedWriter(context.Background(), lease)
				return
			}
		}
	}
	cleanupTeamMemberWorkspace(manager, lease, created)
}

// compensateUnpublishedTeamAdmission closes a durable accepted turn when the
// pool failed after the callback committed its facts but before publishing the
// work item. Startup recovery can finish this sequence if a write fails midway.
func (s *Service) compensateUnpublishedTeamAdmission(root, sessionID, teamID, memberID, turnID string, child agent.ChildRunInput, runSeq *uint64, cause error) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	turn, ok := projection.Turns[turnID]
	if !ok {
		member, exists := projection.Members[memberID]
		if exists && member.Status == teams.MemberCreated {
			member.Status = teams.MemberInterrupted
			member.Revision++
			return appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", Member: &member})
		}
		return nil
	}
	if turn.Status == "aborted" || turnTerminalTeamStatus(turn.Status) {
		return nil
	}
	if turn.Status == "intent" {
		turn.Status = "aborted"
		if err := appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAborted, ActorID: "service", ActorRunID: turn.OriginRunID, Turn: &turn}); err != nil {
			return err
		}
		return nil
	}
	if turn.Status != "queued" || turn.RunID != child.ChildRunID {
		return teams.ErrPermission
	}
	if _, found, startErr := sessionlog.FindRunStart(root, sessionID, child.ChildRunID); startErr != nil {
		return startErr
	} else if !found {
		// Recovery will reconcile a queued turn whose RunStarted append did not
		// complete; keep any attached workspace reference intact for that repair.
		return nil
	}
	reason := "shared pool could not publish the accepted turn"
	causeText := truncateDelegationText(redactRunCredential(cause.Error(), s.deps.ProviderCredential), teams.MaxErrorBytes-len(reason)-2)
	if err := s.persistTeamChildFinishLocked(root, sessionID, teamID, memberID, turnID, child, agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: reason + ": " + causeText}, runSeq); err != nil {
		return err
	}
	projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	turn = projection.Turns[turnID]
	turn.Status, turn.Error = string(agent.DelegationInterrupted), reason
	if err := appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: turn.OriginRunID, Turn: &turn}); err != nil {
		return err
	}
	projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	member, ok := projection.Members[memberID]
	if !ok || member.TeamID != teamID || member.Status.IsTerminal() || member.Status == teams.MemberStopping {
		return teams.ErrPermission
	}
	member.Status = teams.MemberInterrupted
	member.Revision++
	member.RunID, member.TurnID = child.ChildRunID, turnID
	if err := appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: turn.OriginRunID, Member: &member}); err != nil {
		return err
	}
	projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
	if err == nil && projection.Teams[teamID].Status == teams.TeamClosing {
		_, err = s.closeTeamIfIdleLocked(root, sessionID, teamID)
	}
	return err
}

func (s *Service) markMemberWaitingCapacity(root, sessionID, teamID string, expected teams.Member) (teams.Member, error) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return teams.Member{}, err
	}
	member, ok := projection.Members[expected.ID]
	if !ok || member.Revision != expected.Revision || member.Status != expected.Status || member.Status != teams.MemberIdle && member.Status != teams.MemberInterrupted {
		return teams.Member{}, teams.ErrPermission
	}
	member.RoleHash, member.Model = expected.RoleHash, expected.Model
	member.Tools = append([]string(nil), expected.Tools...)
	if member.WorkspaceID == "" && expected.WorkspaceID != "" {
		member.WorkspaceID = expected.WorkspaceID
	}
	member.Status = teams.MemberWaitingCapacity
	member.Revision++
	if err := appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member}); err != nil {
		return teams.Member{}, err
	}
	return member, nil
}

func (s *Service) restoreCapacityWaiter(root, sessionID, teamID, memberID string) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	member, ok := projection.Members[memberID]
	if !ok || member.Status != teams.MemberWaitingCapacity {
		return nil
	}
	if projection.Teams[teamID].Status == teams.TeamClosing {
		member.Status = teams.MemberStopped
	} else {
		member.Status = teams.MemberInterrupted
	}
	member.Revision++
	return appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: member.OriginRunID, Member: &member})
}

func (s *Service) watchTeamMember(root, sessionID, teamID, memberID, turnID string, handle *agent.TaskHandle, startedAt *time.Time, durableTerminal *bool, durableResult *agent.ChildRunResult, memberWorkspace *workspace.LifecycleService, writerLease *workspace.WriterLease, workspaceRunDone chan struct{}) {
	defer func() {
		s.teamScheduler.mu.Lock()
		delete(s.teamScheduler.active, turnID)
		delete(s.teamScheduler.activeOrigin, turnID)
		delete(s.teamScheduler.activeMember, turnID)
		delete(s.teamScheduler.activeTeam, turnID)
		if writerLease != nil {
			delete(s.teamScheduler.workspaceRuns, writerLease.RunID)
		}
		if workspaceRunDone != nil {
			close(workspaceRunDone)
		}
		s.teamScheduler.mu.Unlock()
	}()
	_, ok := <-handle.Results
	if !ok {
		// The durable child hook remains the authority for the outcome.
	}
	if memberWorkspace != nil && writerLease != nil {
		if _, releaseErr := memberWorkspace.ReleaseCompletedWriter(context.Background(), *writerLease); releaseErr != nil {
			*durableTerminal = false
			*durableResult = agent.ChildRunResult{Status: agent.DelegationInterrupted, Error: "team member workspace writer could not be safely released: " + releaseErr.Error()}
		}
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return
	}
	turn, exists := projection.Turns[turnID]
	if !exists || turnTerminalTeamStatus(turn.Status) {
		return
	}
	if !*durableTerminal {
		// Child run durability is written by the lifecycle callback. If it
		// failed, leave the accepted turn for startup recovery to mark interrupted.
		return
	}
	elapsed := time.Duration(0)
	if !startedAt.IsZero() {
		elapsed = time.Since(*startedAt)
	}
	member := projection.Members[memberID]
	if remaining := member.Budget.RemainingDuration(); elapsed > remaining {
		elapsed = remaining
	}
	status := string(durableResult.Status)
	terminal := turn
	terminal.Status, terminal.Elapsed = status, elapsed
	terminal.Summary = truncateDelegationText(redactRunCredential(durableResult.Summary, s.deps.ProviderCredential), teams.MaxSummaryBytes)
	terminal.Error = truncateDelegationText(redactRunCredential(durableResult.Error, s.deps.ProviderCredential), teams.MaxErrorBytes)
	if appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnTerminal, ActorID: "service", ActorRunID: turn.OriginRunID, Turn: &terminal}) != nil {
		return
	}
	// Request expiry is an independent durable fact. If its append fails, keep
	// committing the child terminal/member state; the single expiry timer retries
	// the request and reconciles an awaiting-plan member after the write recovers.
	_ = s.expireDueTeamRequestsForMember(root, sessionID, teamID, memberID)
	projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return
	}
	member = projection.Members[memberID]
	planExpired := false
	pendingPlan := false
	for _, request := range projection.Requests {
		if request.TeamID != teamID || request.MemberID != memberID || request.Type != teams.RequestPlan {
			continue
		}
		switch request.Status {
		case teams.RequestExpired:
			planExpired = true
		case teams.RequestPending, teams.RequestDeferred:
			pendingPlan = true
		}
	}
	if member.Status == teams.MemberStopping {
		member.Status = teams.MemberStopped
	} else {
		switch status {
		case "succeeded":
			member.Status = teams.MemberIdle
		case "failed", "canceled", "interrupted":
			member.Status = teams.MemberInterrupted
		default:
			member.Status = teams.MemberInterrupted
		}
	}
	if member.Status != teams.MemberStopped && member.PlanRequired && !member.PlanApproved {
		if member.Status == teams.MemberIdle && planExpired && !pendingPlan {
			member.Status = teams.MemberIdle
		} else if member.Status == teams.MemberIdle {
			member.Status = teams.MemberAwaitingPlan
		}
	}
	if member.Budget.CanAccept() != nil && member.Status != teams.MemberStopped {
		member.Status = teams.MemberBudgetExhausted
	}
	member.Revision++
	_ = appendTeamFactLocked(root, sessionID, teamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: turn.OriginRunID, Member: &member})
	projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
	if err == nil && projection.Teams[teamID].Status == teams.TeamClosing {
		_, _ = s.closeTeamIfIdleLocked(root, sessionID, teamID)
	}
}

func (s *Service) persistTeamChildStart(root string, scope teams.Scope, memberID, turnID, originRunID, originCallID string, child agent.ChildRunInput, runSeq *uint64) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if child.TeamTurn == nil {
		return teams.ErrPermission
	}
	projection, err := sessionlog.ReplayTeams(root, scope.SessionID, child.TeamTurn.TeamID)
	if err != nil {
		return err
	}
	turn, ok := projection.Turns[turnID]
	if !ok || turn.RunID != child.ChildRunID || turn.Status != "queued" || turn.MemberID != memberID {
		return teams.ErrPermission
	}
	start, found, err := sessionlog.FindRunStart(root, scope.SessionID, child.ChildRunID)
	if err != nil || !found || start.TeamID != child.TeamTurn.TeamID || start.TeamMemberID != memberID || start.TeamTurnID != turnID || start.WorkspaceID != child.WorkspaceID || start.WorkspaceGeneration != child.WorkspaceGeneration || start.OriginRunID != originRunID || start.OriginCallID != originCallID {
		return teams.ErrPermission
	}
	if *runSeq != 1 {
		return errors.New("team child run is missing its durable queued fact")
	}
	if err := s.appendTeamChildRunEventLocked(root, scope.SessionID, child, runSeq, "running", "", ""); err != nil {
		return err
	}
	projection, err = sessionlog.ReplayTeams(root, scope.SessionID, child.TeamTurn.TeamID)
	if err != nil {
		return err
	}
	member := projection.Members[memberID]
	member.Status = teams.MemberRunning
	member.Revision++
	return appendTeamFactLocked(root, scope.SessionID, child.TeamTurn.TeamID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberState, ActorID: "service", ActorRunID: originRunID, Member: &member})
}

func (s *Service) persistTeamChildQueuedLocked(root string, scope teams.Scope, originRunID, originCallID string, child agent.ChildRunInput, runSeq *uint64) error {
	if child.TeamTurn == nil {
		return teams.ErrPermission
	}
	start := sessionlog.RunStarted{TeamID: child.TeamTurn.TeamID, TeamMemberID: child.TeamTurn.MemberID, TeamTurnID: child.TeamTurn.TurnID, RunID: child.ChildRunID, WorkKind: scope.WorkKind, GoalID: scope.GoalID, WorkItemID: scope.WorkItemID, Intent: "team member turn", WorkspaceID: child.WorkspaceID, WorkspaceGeneration: child.WorkspaceGeneration, OriginRunID: originRunID, OriginCallID: originCallID}
	if _, err := sessionlog.Append(root, scope.SessionID, sessionlog.EventRunStarted, start); err != nil {
		return err
	}
	*runSeq = 0
	return s.appendTeamChildRunEventLocked(root, scope.SessionID, child, runSeq, "queued", "", "")
}

func (s *Service) persistTeamChildFinish(root, sessionID, teamID, memberID, turnID string, child agent.ChildRunInput, result agent.ChildRunResult, runSeq *uint64) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.persistTeamChildFinishLocked(root, sessionID, teamID, memberID, turnID, child, result, runSeq)
}

// persistTeamChildFinishLocked writes terminal run facts when the caller
// already holds eventMu for a larger atomic team transition.
func (s *Service) persistTeamChildFinishLocked(root, sessionID, teamID, memberID, turnID string, child agent.ChildRunInput, result agent.ChildRunResult, runSeq *uint64) error {
	status := string(result.Status)
	if !teamTerminalStatus(status) {
		if result.Error != "" {
			status = "failed"
		} else {
			status = "succeeded"
		}
	}
	summary := redactRunCredential(result.Summary, s.deps.ProviderCredential)
	errorText := redactRunCredential(result.Error, s.deps.ProviderCredential)
	summary = truncateDelegationText(summary, teams.MaxSummaryBytes)
	errorText = truncateDelegationText(errorText, teams.MaxErrorBytes)
	delegationStatus := status
	if err := s.appendTeamChildRunEventLocked(root, sessionID, child, runSeq, delegationStatus, summary, errorText); err != nil {
		return err
	}
	runStatus := map[string]string{"succeeded": "completed", "failed": "failed", "canceled": "cancelled", "interrupted": "interrupted"}[status]
	return s.appendTeamChildTerminalLocked(root, sessionID, child, runSeq, runStatus, errorText)
}

func (s *Service) appendTeamChildRunEventLocked(root, sessionID string, child agent.ChildRunInput, runSeq *uint64, status, summary, errorText string) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	*runSeq = *runSeq + 1
	payload := sessionlog.AgentTaskDelegation{SessionID: sessionID, BatchID: child.BatchID, TaskID: child.Task.ID, TaskName: child.Task.Name, Status: status, Summary: summary, Error: errorText, UpdatedAt: time.Now().UTC()}
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: id, RunID: child.ChildRunID, SessionID: sessionID, RunSeq: *runSeq, At: time.Now().UTC(), Kind: string(agent.EventDelegation), Payload: payload})
	return err
}

func (s *Service) appendTeamChildTerminalLocked(root, sessionID string, child agent.ChildRunInput, runSeq *uint64, status, reason string) error {
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	*runSeq = *runSeq + 1
	payload := map[string]string{"status": status, "reason": reason}
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventRunEvent, sessionlog.RunEvent{ID: id, RunID: child.ChildRunID, SessionID: sessionID, RunSeq: *runSeq, At: time.Now().UTC(), Kind: string(agent.EventTerminal), Payload: payload})
	return err
}

func appendTeamFactLocked(root, sessionID, teamID string, fact sessionlog.TeamEvent) error {
	projection, err := sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return err
	}
	team, ok := projection.Teams[teamID]
	if !ok {
		return teams.ErrNotFound
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return err
	}
	fact.ID, fact.TeamID, fact.SessionID = id, teamID, sessionID
	fact.Revision = team.Revision + 1
	_, err = sessionlog.Append(root, sessionID, sessionlog.EventTeam, fact)
	return err
}

func teamTerminalStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "canceled", "interrupted":
		return true
	default:
		return false
	}
}

func turnTerminalTeamStatus(status string) bool { return teamTerminalStatus(status) }

func (s *Service) executeTeamMemberTool(ctx context.Context, request agent.ExecutionRequest, call llm.ToolUse) (agent.ToolOutcome, error) {
	return s.ExecuteTeamTool(ctx, request, call)
}
