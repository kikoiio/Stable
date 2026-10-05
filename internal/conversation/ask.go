package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"stable/internal/decision"
	"stable/internal/execution"
	"stable/internal/goalrun"
	"stable/internal/sessionlog"
	"stable/internal/todo"
)

// AskAdapter adapts the conversation service to execution.QuestionSink: an
// ask_user tool call appends an M05 pending_question event, pushes it to the
// session clients as a "questions" message, and blocks — polling the session
// log — until a reply event consumes the question. The factory call sites
// wire it with execution.WithQuestionSink / the deps literal.
//
// Replies are one free text for the whole form (a dialog submission or a
// /reply line), so the returned AskResponse always carries the free-text
// shape: Answers holds exactly one entry with the raw reply text and
// FreeText is true. The structured questions themselves are persisted with
// the ask_user tool call input; the question event only carries the readable
// prompt rendering.
type AskAdapter struct {
	// PollEvery is the reply poll interval; zero defaults to 500ms, matching
	// the approval wait.
	PollEvery time.Duration

	mu      sync.Mutex
	service *Service
}

// NewAskAdapter builds the question sink for the executor factory. The
// service may be nil when the factory is built before conversation.Serve
// returns; call Bind once the service exists.
func NewAskAdapter(service *Service) *AskAdapter {
	return &AskAdapter{service: service}
}

// Bind attaches the conversation service after construction. The executor
// factory is built before Serve starts listening, so the call sites bind the
// service right after Serve returns — runs cannot reach a tool call before
// that, and the binding is mutex-guarded so the hand-off is race-free.
func (a *AskAdapter) Bind(service *Service) {
	a.mu.Lock()
	a.service = service
	a.mu.Unlock()
}

// current returns the bound service, or nil before Bind.
func (a *AskAdapter) current() *Service {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.service
}

// pollInterval resolves the reply poll interval.
func (a *AskAdapter) pollInterval() time.Duration {
	if a.PollEvery > 0 {
		return a.PollEvery
	}
	return 500 * time.Millisecond
}

// Ask implements execution.QuestionSink. Validation failures are errors; a
// cancelled context returns ctx.Err() while the question stays pending in
// the session log (the leftover-question lifecycle is handled by
// replyQuestion, which queues late replies as user messages).
func (a *AskAdapter) Ask(ctx context.Context, req execution.AskRequest) (execution.AskResponse, error) {
	svc := a.current()
	if svc == nil {
		return execution.AskResponse{}, errors.New("question sink is not bound to a session service")
	}
	if strings.TrimSpace(req.SessionID) == "" {
		return execution.AskResponse{}, errors.New("ask requires a session id")
	}
	if len(req.Questions) == 0 {
		return execution.AskResponse{}, errors.New("ask requires at least one question")
	}
	root, err := svc.boundProjectRoot()
	if err != nil {
		return execution.AskResponse{}, err
	}
	// Register the waiter before the question exists so a reply racing with
	// the append is never mistaken for a queued message.
	svc.enterAskWait(req.SessionID)
	defer svc.exitAskWait(req.SessionID)
	id := goalrun.RandomID("q")
	question := sessionlog.PendingQuestion{
		QuestionID: id,
		WorkRef:    req.WorkRef,
		SessionID:  req.SessionID,
		RunID:      req.RunID,
		Prompt:     redactProviderCredential(renderAskPrompt(req.Questions), svc.deps.ChatProvider, svc.deps.Provider),
		CreatedAt:  time.Now().UTC(),
		Status:     sessionlog.QuestionPending,
	}
	svc.eventMu.Lock()
	stored, err := sessionlog.Append(svc.deps.ProjectRoot, req.SessionID, sessionlog.EventQuestion, question)
	svc.eventMu.Unlock()
	if err != nil {
		return execution.AskResponse{}, fmt.Errorf("record question: %w", err)
	}
	svc.pushQuestions(req.SessionID, []sessionlog.PendingQuestion{question})

	ticker := time.NewTicker(a.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return execution.AskResponse{}, ctx.Err()
		case <-ticker.C:
		}
		replay, err := sessionlog.ReplayAfter(root, req.SessionID, stored.Seq)
		if err != nil {
			continue
		}
		for _, event := range replay.Events {
			if event.Type != sessionlog.EventReply {
				continue
			}
			var reply sessionlog.QuestionReply
			if decodeSessionData(event.Data, &reply) != nil || reply.QuestionID != id {
				continue
			}
			return execution.AskResponse{FreeText: true, Answers: [][]string{{reply.ReplyText}}}, nil
		}
	}
}

// renderAskPrompt is the human-readable transcript form of the structured
// ask_user questions; the session log projection renders it verbatim.
func renderAskPrompt(questions []execution.QuestionSpec) string {
	var b strings.Builder
	for i, question := range questions {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s] %s", question.Header, question.Question)
		for _, option := range question.Options {
			if strings.TrimSpace(option.Description) != "" {
				fmt.Fprintf(&b, "\n  - %s：%s", option.Label, option.Description)
			} else {
				fmt.Fprintf(&b, "\n  - %s", option.Label)
			}
		}
		if question.MultiSelect {
			b.WriteString("\n  (可多选)")
		}
	}
	return b.String()
}

// enterAskWait, exitAskWait and askWaiters track the sessions with a run
// blocked inside Ask. replyQuestion uses the count to decide between an
// immediate answer and queueing the reply as a user message. The counters
// live for the service lifetime and start empty after a restart, which
// matches the plan-state semantics: leftover dialogs answer into the queue.
func (s *Service) enterAskWait(sessionID string) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if s.askWaiters == nil {
		s.askWaiters = map[string]int{}
	}
	s.askWaiters[sessionID]++
}

func (s *Service) exitAskWait(sessionID string) {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	if s.askWaiters[sessionID] <= 1 {
		delete(s.askWaiters, sessionID)
		return
	}
	s.askWaiters[sessionID]--
}

// askWaiterCount reports how many runs are blocked in Ask for the session.
func (s *Service) askWaiterCount(sessionID string) int {
	s.askMu.Lock()
	defer s.askMu.Unlock()
	return s.askWaiters[sessionID]
}

// pushQuestions fans a questions update out to the clients bound to the
// session and records delivery so the poll loop never re-pushes the same
// question while it stays pending. Undelivered updates (no bound client or a
// full channel) stay unmarked and are retried by the poll loop.
func (s *Service) pushQuestions(sessionID string, questions []sessionlog.PendingQuestion) {
	if len(questions) == 0 {
		return
	}
	msg := ServerMsg{Type: "questions", Questions: questions}
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if s.notifiedQuestions == nil {
			s.notifiedQuestions = map[string]bool{}
		}
		for _, question := range questions {
			s.notifiedQuestions[question.QuestionID] = true
		}
	}
}

// TodoProvider adapts the conversation service to execution.TodoProvider: it
// serves one todo.TaskList per session, journals every change as a full
// todo_update snapshot event, and pushes the new snapshot to the session
// clients as a "todo" message. The factory call sites wire it through the
// ToolExecutorDeps literal.
type TodoProvider struct {
	mu        sync.Mutex
	service   *Service
	lists     map[string]*todo.TaskList
	revisions map[string]int
}

// NewTodoProvider builds the task list provider for the executor factory.
// The service may be nil when the factory is built before
// conversation.Serve returns; call Bind once the service exists.
func NewTodoProvider(service *Service) *TodoProvider {
	return &TodoProvider{service: service, lists: map[string]*todo.TaskList{}, revisions: map[string]int{}}
}

// Bind attaches the conversation service after construction; see
// AskAdapter.Bind.
func (p *TodoProvider) Bind(service *Service) {
	p.mu.Lock()
	p.service = service
	p.mu.Unlock()
}

// For implements execution.TodoProvider. The list is created lazily per
// session and cached for the service lifetime; the revision counter seeds
// from the session log so updates stay strictly increasing across restarts.
// A nil return means no list can be served for the session.
func (p *TodoProvider) For(sessionID string) *todo.TaskList {
	p.mu.Lock()
	defer p.mu.Unlock()
	if list, ok := p.lists[sessionID]; ok {
		return list
	}
	if p.service == nil {
		return nil
	}
	svc := p.service
	list := todo.NewTaskList(svc.deps.ProjectRoot, sessionID, func(tasks []todo.Task) error {
		return p.recordTodoUpdate(sessionID, tasks)
	})
	if credentials := svc.providerCredentials(); len(credentials) > 0 {
		list.SetCredentials(credentials)
	}
	p.lists[sessionID] = list
	p.revisions[sessionID] = maxTodoRevision(svc.deps.ProjectRoot, sessionID)
	return list
}

// recordTodoUpdate journals one full task-list snapshot and pushes it to the
// session clients. It runs under the task list mutex, so the revisions of a
// session are serialized.
func (p *TodoProvider) recordTodoUpdate(sessionID string, tasks []todo.Task) error {
	p.mu.Lock()
	revision := p.revisions[sessionID] + 1
	p.revisions[sessionID] = revision
	svc := p.service
	p.mu.Unlock()
	if svc == nil {
		return errors.New("todo provider is not bound to a session service")
	}
	snapshots := make([]sessionlog.TaskSnapshot, 0, len(tasks))
	for _, task := range tasks {
		snapshots = append(snapshots, todoTaskSnapshot(task))
	}
	svc.eventMu.Lock()
	_, err := sessionlog.Append(svc.deps.ProjectRoot, sessionID, sessionlog.EventTodo, sessionlog.TodoUpdate{Revision: revision, Tasks: snapshots})
	svc.eventMu.Unlock()
	if err != nil {
		p.mu.Lock()
		if p.revisions[sessionID] == revision {
			// The log refused the snapshot: the next change retries with the
			// same revision instead of growing the gap.
			p.revisions[sessionID] = revision - 1
		}
		p.mu.Unlock()
		return fmt.Errorf("record todo update: %w", err)
	}
	svc.pushTodo(sessionID, snapshots)
	return nil
}

// todoTaskSnapshot converts a todo task into its session log projection,
// copying the reference fields so later list mutations cannot change a
// journaled snapshot.
func todoTaskSnapshot(task todo.Task) sessionlog.TaskSnapshot {
	snapshot := sessionlog.TaskSnapshot{
		ID:          task.ID,
		Subject:     task.Subject,
		Description: task.Description,
		ActiveForm:  task.ActiveForm,
		Status:      string(task.Status),
		Owner:       task.Owner,
		Metadata:    task.Metadata,
	}
	if len(task.Blocks) > 0 {
		snapshot.Blocks = append([]string(nil), task.Blocks...)
	}
	if len(task.BlockedBy) > 0 {
		snapshot.BlockedBy = append([]string(nil), task.BlockedBy...)
	}
	return snapshot
}

// maxTodoRevision returns the highest todo revision already recorded in the
// session log, so a restarted service continues the revision sequence.
func maxTodoRevision(root, sessionID string) int {
	replay, err := sessionlog.Replay(root, sessionID)
	if err != nil {
		return 0
	}
	revision := 0
	for _, event := range replay.Events {
		if event.Type != sessionlog.EventTodo {
			continue
		}
		var update sessionlog.TodoUpdate
		if decodeSessionData(event.Data, &update) == nil && update.Revision > revision {
			revision = update.Revision
		}
	}
	return revision
}

// providerCredentials collects the provider secrets the service knows about,
// mirroring the sources redactProviderCredential strips from user text.
func (s *Service) providerCredentials() []string {
	var credentials []string
	if s.deps.ProviderCredential != "" {
		credentials = append(credentials, s.deps.ProviderCredential)
	}
	for _, provider := range []any{s.deps.ChatProvider, s.deps.Provider} {
		if p, ok := provider.(*decision.HTTPProvider); ok && p != nil && p.Config.APIKey != "" {
			credentials = append(credentials, p.Config.APIKey)
		}
	}
	return credentials
}

// pushTodo fans a task snapshot out to the clients bound to the session.
// Todo updates are always new events, so unlike questions they need no
// delivery dedup.
func (s *Service) pushTodo(sessionID string, tasks []sessionlog.TaskSnapshot) {
	msg := ServerMsg{Type: "todo", Tasks: tasks}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, sub := range s.clients {
		if sub.sessionID != sessionID {
			continue
		}
		select {
		case ch <- msg:
		default:
		}
	}
}
