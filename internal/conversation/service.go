package conversation

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"stable/internal/agent"
	"stable/internal/candidate"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/llm"
	"stable/internal/permission"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

type Deps struct {
	Store              *store.Store
	Provider           decision.StructuredProvider
	ChatProvider       decision.ChatProvider
	Runner             agent.Runner
	ExecutorFactory    agent.ExecutorFactory
	ToolSchemas        []llm.ToolSchema
	ProviderName       string
	Model              string
	RunnerError        string
	CandidateCheckers  []candidate.Checker
	PermissionService  *permission.PermissionService
	ProviderCredential string
	Temporal           string
	ProjectRoot        string
	RunRoot            string
	SocketPath         string
	// ContextWindowTokens overrides the model context window used for
	// compaction; zero resolves to the sessioncontext default.
	ContextWindowTokens int
	// Snapshots is the candidate snapshot store used by snapshot_list and
	// snapshot_rewind; nil makes both report snapshots as unavailable.
	Snapshots *candidate.SnapshotStore
	Refresher core.DependencyRefresher
	PollEvery time.Duration // goal status poll interval; 0 defaults to 2s
}

// Service is the persistent chat session: it owns the unix socket, fans out
// messages to every connected client, and translates user input into goal
// events. Client connections are thin terminals; all state lives here and in
// the store.
type Service struct {
	deps              Deps
	ln                net.Listener
	mu                sync.Mutex
	clients           map[chan ServerMsg]*clientSubscription
	statuses          map[string]core.GoalStatus
	activeRuns        map[string]string
	notifiedApprovals map[string]bool
	eventMu           sync.Mutex
	// askMu guards the per-session count of runs blocked inside the question
	// adapter (see AskAdapter) and the poll-loop delivery dedup state below.
	askMu sync.Mutex
	// askWaiters counts, per session, the in-flight AskAdapter waits so
	// replyQuestion can tell a live waiting run from a leftover question.
	askWaiters map[string]int
	// notifiedQuestions and notifiedPlanApprovals remember which pending
	// dialog was already delivered to a session client, so the poll loop
	// pushes each pending question and plan approval once instead of
	// re-sending the full set every tick. Entries are pruned when the
	// underlying request is no longer pending.
	notifiedQuestions     map[string]bool
	notifiedPlanApprovals map[string]bool
	// planMu guards the plan mode runtime state below. Plan state and plan
	// approvals are service-lifetime memory: a restart drops sessions back to
	// the default mode and clears pending dialogs (the session log keeps the
	// transitions auditable).
	planMu        sync.Mutex
	planStates    map[string]*PlanState
	planApprovals map[string]*PlanApproval
}

type clientSubscription struct {
	ch            chan ServerMsg
	sessionID     string
	runID         string
	pendingCursor uint64
}

func Serve(ctx context.Context, deps Deps) (*Service, error) {
	if deps.PollEvery <= 0 {
		deps.PollEvery = 2 * time.Second
	}
	if err := os.Remove(deps.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", deps.SocketPath)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(deps.SocketPath, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	s := &Service{deps: deps, ln: ln, clients: map[chan ServerMsg]*clientSubscription{}, statuses: map[string]core.GoalStatus{}, activeRuns: map[string]string{}, notifiedApprovals: map[string]bool{}}
	go s.pollGoals(ctx)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go s.acceptLoop(ctx)
	return s, nil
}

func (s *Service) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				continue
			}
			return
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Service) Close() error { return s.ln.Close() }

func (s *Service) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	updates := make(chan ServerMsg, 64)
	sub := &clientSubscription{ch: updates}
	s.mu.Lock()
	s.clients[updates] = sub
	s.mu.Unlock()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for msg := range updates {
			if err := encodeServer(conn, msg); err != nil {
				return
			}
		}
	}()

	s.readLoop(ctx, conn, updates)
	s.mu.Lock()
	delete(s.clients, updates)
	s.mu.Unlock()
	close(updates) // unblocks the writer
	<-writerDone
}

func (s *Service) readLoop(ctx context.Context, conn net.Conn, updates chan ServerMsg) error {
	dec := jsonDecoder(conn)
	for {
		var c ClientMsg
		if err := dec.Decode(&c); err != nil {
			return err
		}
		if err := validateClient(c); err != nil {
			updates <- ServerMsg{Type: "error", Error: err.Error()}
			updates <- ServerMsg{Type: "done"}
			continue
		}
		switch c.Op {
		case "run_start":
			if err := s.startRun(ctx, c, updates); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			}
			continue
		case "run_subscribe":
			if err := s.subscribeRun(ctx, c, updates); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			}
			continue
		case "run_cancel":
			if err := s.cancelRun(c, updates); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "done", RunID: c.RunID}
			}
			continue
		case "review_get":
			review, err := s.reviewCandidate(ctx, c.CandidateID, c.SessionID)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "review", Review: &review}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "review_accept":
			receipt, err := s.acceptReviewedCandidate(ctx, c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "acceptance", Receipt: &receipt}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "approval_list":
			approvals, err := s.pendingApprovals(ctx, c.SessionID)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				prompts := make([]permission.ApprovalPrompt, 0, len(approvals))
				for _, approval := range approvals {
					prompts = append(prompts, permission.Prompt(approval))
				}
				updates <- ServerMsg{Type: "approvals", Approvals: prompts}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "approval_resolve":
			decision, err := s.resolveApproval(ctx, c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "permission_decision", Decision: &decision}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "approval_cancel":
			if err := s.cancelApproval(ctx, c); err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "approval_cancelled", RunID: c.RunID}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "session_search":
			result, err := s.searchSessions(c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "search", Search: &result}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "snapshot_list":
			snapshots, err := s.listSnapshots(c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "snapshots", Snapshots: snapshots}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "snapshot_rewind":
			record, err := s.rewindSnapshot(ctx, c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "rewind", Rewind: &record}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "question_list":
			questions, err := s.listQuestions(c)
			if err != nil {
				updates <- ServerMsg{Type: "error", Error: err.Error()}
			} else {
				updates <- ServerMsg{Type: "questions", Questions: questions}
			}
			updates <- ServerMsg{Type: "done"}
			continue
		case "session_load":
			s.mu.Lock()
			if sub := s.clients[updates]; sub != nil {
				sub.sessionID, sub.runID = c.SessionID, ""
			}
			s.mu.Unlock()
		}
		msgs, err := s.handle(ctx, c)
		for i := range msgs {
			s.broadcast(msgs[i])
		}
		if err != nil {
			updates <- ServerMsg{Type: "error", Error: err.Error()}
		}
		// Queue completion after this client's broadcast messages so one-shot
		// clients can distinguish a finished request from a slow model response.
		updates <- ServerMsg{Type: "done"}
	}
}

func (s *Service) broadcast(msg ServerMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.clients {
		select {
		case ch <- msg:
		default: // drop for slow clients; history replay covers reconnects
		}
	}
}

// pollGoals pushes goal status changes to connected clients so terminals see
// progress without running goal status themselves.
func (s *Service) pollGoals(ctx context.Context) {
	tick := time.NewTicker(s.deps.PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.deliverPermissionWakes(ctx)
		s.pushPendingApprovals(ctx)
		s.pushPendingQuestions()
		s.pushPendingPlanApprovals()
		if s.deps.Store == nil {
			continue
		}
		goals, err := s.deps.Store.ListGoals(ctx)
		if err != nil {
			continue
		}
		s.mu.Lock()
		for i := range goals {
			g := goals[i]
			if prev, ok := s.statuses[g.ID]; !ok || prev != g.Status {
				s.statuses[g.ID] = g.Status
				s.mu.Unlock()
				s.broadcast(ServerMsg{Type: "goal_update", Goal: &g})
				s.mu.Lock()
			}
		}
		s.mu.Unlock()
	}
}

func (s *Service) pushPendingApprovals(ctx context.Context) {
	if s.deps.Store == nil {
		return
	}
	s.mu.Lock()
	seen := map[string]bool{}
	for _, sub := range s.clients {
		if sub.sessionID != "" {
			seen[sub.sessionID] = true
		}
	}
	s.mu.Unlock()
	for sessionID := range seen {
		requests, err := s.pendingApprovals(ctx, sessionID)
		if err != nil {
			continue
		}
		for _, request := range requests {
			s.pushApproval(ctx, request.ID, sessionID)
		}
	}
}

// pushPendingQuestions mirrors pushPendingApprovals for the ask_user
// lifecycle: sessions with bound clients get their still-pending questions
// pushed once, tracked by question id so the poll never re-sends the same
// dialog. Delivery is re-attempted while the send fails, and the dedup
// entries are dropped once the question is answered.
func (s *Service) pushPendingQuestions() {
	s.mu.Lock()
	seen := map[string]bool{}
	for _, sub := range s.clients {
		if sub.sessionID != "" {
			seen[sub.sessionID] = true
		}
	}
	s.mu.Unlock()
	for sessionID := range seen {
		questions, err := s.listQuestions(ClientMsg{SessionID: sessionID})
		if err != nil {
			continue
		}
		pending := map[string]bool{}
		s.mu.Lock()
		var fresh []sessionlog.PendingQuestion
		for _, question := range questions {
			if question.Status == sessionlog.QuestionReplied {
				continue
			}
			pending[question.QuestionID] = true
			if !s.notifiedQuestions[question.QuestionID] {
				fresh = append(fresh, question)
			}
		}
		for id := range s.notifiedQuestions {
			if !pending[id] {
				delete(s.notifiedQuestions, id)
			}
		}
		s.mu.Unlock()
		if len(fresh) > 0 {
			s.pushQuestions(sessionID, fresh)
		}
	}
}

// pushPendingPlanApprovals mirrors pushPendingQuestions for the plan approval
// lifecycle. The pending set is service-lifetime memory (a restart clears
// pending dialogs), matching the PlanApprovalService; the broadcast reuses
// the plan_approvals message shape.
func (s *Service) pushPendingPlanApprovals() {
	s.mu.Lock()
	seen := map[string]bool{}
	for _, sub := range s.clients {
		if sub.sessionID != "" {
			seen[sub.sessionID] = true
		}
	}
	s.mu.Unlock()
	for sessionID := range seen {
		s.planMu.Lock()
		pending := map[string]bool{}
		var refs []PlanApprovalRef
		for _, approval := range s.planApprovals {
			if approval == nil || approval.SessionID != sessionID || approval.Status != sessionlog.PlanApprovalSubmitted {
				continue
			}
			pending[approval.ID] = true
			refs = append(refs, planApprovalRef(*approval))
		}
		s.planMu.Unlock()
		if len(refs) == 0 {
			s.mu.Lock()
			for id := range s.notifiedPlanApprovals {
				delete(s.notifiedPlanApprovals, id)
			}
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		var fresh []PlanApprovalRef
		for _, ref := range refs {
			if !s.notifiedPlanApprovals[ref.ID] {
				fresh = append(fresh, ref)
			}
		}
		s.mu.Unlock()
		if len(fresh) == 0 {
			continue
		}
		msg := ServerMsg{Type: "plan_approvals", PlanApprovals: fresh}
		s.mu.Lock()
		delivered := false
		for ch, sub := range s.clients {
			if sub.sessionID != sessionID {
				continue
			}
			select {
			case ch <- msg:
				delivered = true
			default:
			}
		}
		if delivered {
			if s.notifiedPlanApprovals == nil {
				s.notifiedPlanApprovals = map[string]bool{}
			}
			for _, ref := range fresh {
				s.notifiedPlanApprovals[ref.ID] = true
			}
		}
		s.mu.Unlock()
	}
}
