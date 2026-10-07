package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/agentcatalog"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

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

type teamScheduler struct {
	service   *Service
	submitter agent.CommittedTaskSubmitter
	mu        sync.Mutex
	drainMu   sync.Mutex
	active    map[string]context.CancelFunc
	roles     map[string]agentcatalog.Definition
	waiting   []func() error
	wake      chan struct{}
	done      chan struct{}
	closed    bool
}

func newTeamScheduler(service *Service) *teamScheduler {
	scheduler := &teamScheduler{service: service, active: map[string]context.CancelFunc{}, roles: map[string]agentcatalog.Definition{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if service != nil {
		scheduler.submitter, _ = service.deps.Delegator.(agent.CommittedTaskSubmitter)
		if scheduler.submitter != nil {
			go scheduler.capacityLoop(service.lifeCtx)
		}
	}
	return scheduler
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
			s.drainCapacityQueue()
		case <-s.wake:
			s.drainCapacityQueue()
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
	tools := definition.EffectiveTools()
	if len(tools) == 0 {
		return teams.Member{}, errors.New("team member role has no permitted inspection tools")
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
	roleHash := sha256.Sum256([]byte(definition.Instruction))
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
	if parent.ProviderName == "" {
		parent.ProviderName = s.deps.ProviderName
	}
	if definition.MaxTurns > 0 && definition.MaxTurns < parent.Budget.MaxToolRounds {
		parent.Budget.MaxToolRounds = definition.MaxTurns
	}
	if member.Budget.RemainingDuration() < parent.Budget.MaxDuration {
		parent.Budget.MaxDuration = member.Budget.RemainingDuration()
	}
	parent.ToolSchemas, err = agent.TeamMemberToolSchemas(s.deps.ToolSchemas, tools)
	if err != nil {
		return teams.Member{}, err
	}
	parent.ExecutorFactory, err = agent.NewTeamMemberExecutorFactory(s.deps.ForkExecutorFactory, tools, s.executeTeamMemberTool)
	if err != nil {
		return teams.Member{}, err
	}
	input, err := agent.BuildTeamTurnTask(turnID, definition.Instruction+"\n\nAssigned task:\n"+spawn.Instruction, agent.TeamTurnInput{
		Identity: *parent.TeamTurn,
	})
	if err != nil {
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
			if err := s.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turnID, child, result, &runSeq); err != nil {
				return err
			}
			durableResult = result
			durableTerminal = true
			return nil
		},
	}
	accepted, err := s.submitTeamMember(ctx, root, scope, team, member, request, parent, input, &startedAt, &runSeq, &durableTerminal, &durableResult, false, nil, false)
	if err != nil {
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
	if s == nil || s.teamScheduler == nil || s.teamScheduler.submitter == nil {
		return teams.Member{}, errors.New("team member scheduler is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return teams.Member{}, err
	}
	if request.TeamUser || request.TeamTurn != nil || request.RunID == "" || teams.ValidateText(originCallID, 256, true) != nil {
		return teams.Member{}, teams.ErrPermission
	}
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil || !actor.Lead {
		return teams.Member{}, teams.ErrPermission
	}
	if err := validateTeamLeadAuthority(request, scope); err != nil {
		return teams.Member{}, err
	}
	team, projection, err := s.teamForOperation(root, scope, teamID, actor)
	if err != nil {
		return teams.Member{}, err
	}
	member, ok := projection.Members[memberID]
	if !ok || member.TeamID != team.ID || (member.Status != teams.MemberIdle && member.Status != teams.MemberInterrupted) {
		return teams.Member{}, teams.ErrPermission
	}
	if member.PlanRequired && !member.PlanApproved {
		return teams.Member{}, errors.New("team member requires approved plan before it can resume")
	}
	if err := member.Budget.CanAccept(); err != nil {
		return teams.Member{}, err
	}
	definition, err := s.teamMemberRoleSnapshot(member)
	if err != nil {
		return teams.Member{}, err
	}
	turnID, err := sessionlog.NewID()
	if err != nil {
		return teams.Member{}, err
	}
	identity := agent.TeamTurnIdentity{TeamID: team.ID, MemberID: member.ID, TurnID: turnID, MemberName: member.Name}
	messages, handoffs := pendingTeamMessageBatch(projection, team.ID, member.ID)
	tasks, err := resumableMemberTasks(projection, team.ID, member.ID)
	if err != nil {
		return teams.Member{}, err
	}
	task, err := agent.BuildTeamTurnTask(turnID, definition.Instruction+"\n\nContinue the prior assigned work. Use the prior summary, unfinished assigned tasks, and any newly delivered messages as reference data.", agent.TeamTurnInput{
		Identity: identity,
		Summary:  member.Summary,
		Messages: messages,
		Tasks:    tasks,
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
	if parent.ProviderName == "" {
		parent.ProviderName = s.deps.ProviderName
	}
	if definition.MaxTurns > 0 && definition.MaxTurns < parent.Budget.MaxToolRounds {
		parent.Budget.MaxToolRounds = definition.MaxTurns
	}
	if remaining := member.Budget.RemainingDuration(); remaining < parent.Budget.MaxDuration {
		parent.Budget.MaxDuration = remaining
	}
	parent.ToolSchemas, err = agent.TeamMemberToolSchemas(s.deps.ToolSchemas, member.Tools)
	if err != nil {
		return teams.Member{}, err
	}
	parent.ExecutorFactory, err = agent.NewTeamMemberExecutorFactory(s.deps.ForkExecutorFactory, member.Tools, s.executeTeamMemberTool)
	if err != nil {
		return teams.Member{}, err
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
			if err := s.persistTeamChildFinish(root, request.Work.SessionID, team.ID, member.ID, turnID, child, result, &runSeq); err != nil {
				return err
			}
			durableResult, durableTerminal = result, true
			return nil
		},
	}
	accepted, err := s.submitTeamMember(ctx, root, scope, team, member, request, parent, task, &startedAt, &runSeq, &durableTerminal, &durableResult, true, handoffs, false)
	if errors.Is(err, agent.ErrDelegationQueueFull) {
		waiting, waitErr := s.markMemberWaitingCapacity(root, request.Work.SessionID, team.ID, member)
		if waitErr != nil {
			return teams.Member{}, err
		}
		member = waiting
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
			_, retryErr := s.submitTeamMember(s.lifeCtx, root, scope, team, member, queuedRequest, parent, task, &startedAt, &runSeq, &durableTerminal, &durableResult, true, handoffs, true)
			if retryErr != nil && !errors.Is(retryErr, agent.ErrDelegationQueueFull) {
				_ = s.restoreCapacityWaiter(root, queuedRequest.Work.SessionID, team.ID, member.ID)
			}
			return retryErr
		}
		if enqueueErr := s.teamScheduler.enqueueCapacityResume(deferred); enqueueErr != nil {
			_ = s.restoreCapacityWaiter(root, request.Work.SessionID, team.ID, member.ID)
			return teams.Member{}, enqueueErr
		}
		return member, nil
	}
	if err != nil {
		return teams.Member{}, err
	}
	s.teamScheduler.mu.Lock()
	s.teamScheduler.roles[member.ID] = definition
	s.teamScheduler.mu.Unlock()
	return accepted, nil
}

func validateTeamLeadAuthority(request agent.ExecutionRequest, scope teams.Scope) error {
	var authority permission.Authority
	if json.Unmarshal(request.PermissionBounds, &authority) != nil || authority.RunID != request.RunID || authority.SessionID != request.Work.SessionID || authority.GoalID != request.Work.GoalID || authority.WorkItemID != request.Work.WorkItemID || authority.AllowedRoot != scope.ProjectRoot {
		return errors.New("team lead run authority is invalid")
	}
	return nil
}

func (s *Service) teamMemberRoleSnapshot(member teams.Member) (agentcatalog.Definition, error) {
	s.teamScheduler.mu.Lock()
	definition, ok := s.teamScheduler.roles[member.ID]
	s.teamScheduler.mu.Unlock()
	if !ok {
		if s.deps.Agents == nil {
			return agentcatalog.Definition{}, errors.New("original team member role snapshot is unavailable")
		}
		definition, ok = s.deps.Agents.Resolve(member.AgentName)
		if !ok {
			return agentcatalog.Definition{}, errors.New("original team member role is no longer available")
		}
	}
	hash := sha256.Sum256([]byte(definition.Instruction))
	if definition.Name != member.AgentName || hex.EncodeToString(hash[:]) != member.RoleHash || !sameStringList(definition.EffectiveTools(), member.Tools) || (definition.Model != "" && definition.Model != "inherit" && definition.Model != member.Model) {
		return agentcatalog.Definition{}, errors.New("original team member role changed; spawn a new member to use the updated role")
	}
	return definition, nil
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

func (s *Service) submitTeamMember(ctx context.Context, root string, scope teams.Scope, team teams.Team, member teams.Member, request agent.ExecutionRequest, parent agent.ParentRun, task agent.DelegationTask, startedAt *time.Time, runSeq *uint64, durableTerminal *bool, durableResult *agent.ChildRunResult, memberExists bool, handoffs []sessionlog.HandoffFact, waitingRetry bool) (teams.Member, error) {
	turnID := parent.TeamTurn.TurnID
	messageIDs := make([]string, 0, len(handoffs))
	for _, handoff := range handoffs {
		messageIDs = append(messageIDs, handoff.MessageID)
	}
	turn := sessionlog.TurnFact{ID: turnID, MemberID: member.ID, TaskID: task.ID, MessageIDs: messageIDs, OriginRunID: request.RunID, OriginCallID: parent.ToolCallID, Status: "intent"}
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
		if !waitingRetry && !s.activeLeadRun(request) {
			return teams.ErrPermission
		}
		current, currentProjection, checkErr := s.teamForOperation(root, scope, team.ID, teams.Actor{Lead: true})
		if checkErr != nil || current.Status != teams.TeamOpen {
			return teams.ErrPermission
		}
		currentMember, found := currentProjection.Members[member.ID]
		if memberExists {
			allowedStatus := currentMember.Status == teams.MemberIdle || currentMember.Status == teams.MemberInterrupted || waitingRetry && currentMember.Status == teams.MemberWaitingCapacity
			if !found || currentMember.Revision != member.Revision || currentMember.Status != member.Status || currentMember.TeamID != team.ID || !allowedStatus {
				return teams.ErrPermission
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
		if !memberExists {
			if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMemberAdded, ActorID: teams.Lead, ActorRunID: request.RunID, Member: &member}); err != nil {
				return err
			}
		}
		if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnIntent, ActorID: "service", ActorRunID: request.RunID, Turn: &turn}); err != nil {
			return err
		}
		accepted := turn
		accepted.Status = "queued"
		if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamTurnAccepted, ActorID: "service", ActorRunID: request.RunID, Turn: &accepted}); err != nil {
			return err
		}
		childInput = agent.ChildRunInput{TeamTurn: parent.TeamTurn, ParentRunID: request.RunID, BatchID: admission.BatchID, ChildRunID: admission.ChildRunID, Task: task, Work: parent.Work}
		if err := s.persistTeamChildQueuedLocked(root, scope, request.RunID, parent.ToolCallID, childInput, runSeq); err != nil {
			return err
		}
		for _, handoff := range handoffs {
			handoff.DestinationRunID = admission.ChildRunID
			handoff.DestinationTurnID = turnID
			if err := appendTeamFactLocked(root, scope.SessionID, team.ID, sessionlog.TeamEvent{Kind: sessionlog.TeamMessageHandoff, ActorID: "service", ActorRunID: request.RunID, Handoff: &handoff}); err != nil {
				return err
			}
		}
		admissionCommitted = true
		return nil
	})
	if err != nil {
		if admissionCommitted {
			if compensateErr := s.compensateUnpublishedTeamAdmission(root, scope.SessionID, team.ID, member.ID, turnID, childInput, runSeq, err); compensateErr != nil {
				return teams.Member{}, fmt.Errorf("team turn admission failed: %v; durable compensation failed: %w", err, compensateErr)
			}
		}
		return teams.Member{}, err
	}
	s.teamScheduler.mu.Lock()
	s.teamScheduler.active[turnID] = handle.Cancel
	closed = s.teamScheduler.closed
	s.teamScheduler.mu.Unlock()
	if closed {
		handle.Cancel()
	}
	go s.watchTeamMember(root, scope.SessionID, team.ID, member.ID, turnID, handle, startedAt, durableTerminal, durableResult)
	member.Status, member.RunID, member.TurnID = teams.MemberQueued, childRunID, turnID
	return member, nil
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
	if !ok || turn.Status == "aborted" || turnTerminalTeamStatus(turn.Status) {
		return nil
	}
	if turn.Status != "queued" || turn.RunID != child.ChildRunID {
		return teams.ErrPermission
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
	if !ok || member.TurnID != turnID || member.Status != teams.MemberQueued {
		return teams.ErrPermission
	}
	member.Status = teams.MemberInterrupted
	member.Revision++
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

func (s *Service) watchTeamMember(root, sessionID, teamID, memberID, turnID string, handle *agent.TaskHandle, startedAt *time.Time, durableTerminal *bool, durableResult *agent.ChildRunResult) {
	defer func() {
		s.teamScheduler.mu.Lock()
		delete(s.teamScheduler.active, turnID)
		s.teamScheduler.mu.Unlock()
	}()
	_, ok := <-handle.Results
	if !ok {
		// The durable child hook remains the authority for the outcome.
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
	projection, err = sessionlog.ReplayTeams(root, sessionID, teamID)
	if err != nil {
		return
	}
	member = projection.Members[memberID]
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
	if member.Status == teams.MemberIdle && member.PlanRequired && !member.PlanApproved {
		member.Status = teams.MemberAwaitingPlan
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
	if err != nil || !found || start.TeamID != child.TeamTurn.TeamID || start.TeamMemberID != memberID || start.TeamTurnID != turnID || start.OriginRunID != originRunID || start.OriginCallID != originCallID {
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
	start := sessionlog.RunStarted{TeamID: child.TeamTurn.TeamID, TeamMemberID: child.TeamTurn.MemberID, TeamTurnID: child.TeamTurn.TurnID, RunID: child.ChildRunID, WorkKind: scope.WorkKind, GoalID: scope.GoalID, WorkItemID: scope.WorkItemID, Intent: "team member turn", OriginRunID: originRunID, OriginCallID: originCallID}
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
