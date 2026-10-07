package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"stable/internal/platform/secfile"
)

const (
	MaxConsolidationInputBytes = 2 << 20
	backgroundTimeout          = 30 * time.Second
	backgroundQueueSize        = 32
)

type BackgroundEvent struct {
	Action    string
	State     string
	Count     int
	Reason    string
	SessionID string
	At        time.Time
}

type Options struct {
	ProjectRoot   string
	UserConfigDir string
	StateDir      string
	Model         ChatModel
	Selector      Selector
	Processor     Processor
	OnEvent       func(projectRoot string, event BackgroundEvent)
	Now           func() time.Time
}

type Service struct {
	projectRoot   string
	userConfigDir string
	stateRoot     secfile.Root
	selector      Selector
	processor     Processor
	onEvent       func(string, BackgroundEvent)
	now           func() time.Time
	queue         chan RunCompletion
	closeMu       sync.Mutex
	closed        bool
	worker        sync.WaitGroup
	opMu          sync.Mutex
}

func NewManager(options Options) (*Service, error) {
	if strings.TrimSpace(options.ProjectRoot) == "" || strings.TrimSpace(options.UserConfigDir) == "" || strings.TrimSpace(options.StateDir) == "" {
		return nil, errors.New("memory manager requires project, user config, and state roots")
	}
	projectRoot, err := filepath.Abs(options.ProjectRoot)
	if err != nil {
		return nil, err
	}
	projectRoot = filepath.Clean(projectRoot)
	if _, err := secfile.OpenRoot(projectRoot); err != nil {
		return nil, fmt.Errorf("open memory project root: %w", err)
	}
	userConfigDir, err := filepath.Abs(options.UserConfigDir)
	if err != nil {
		return nil, err
	}
	stateDir, err := filepath.Abs(options.StateDir)
	if err != nil {
		return nil, err
	}
	if err := secfile.MkdirAllPrivate(stateDir, 0700); err != nil {
		return nil, fmt.Errorf("prepare memory state directory: %w", err)
	}
	stateRoot, err := secfile.OpenRoot(stateDir)
	if err != nil {
		return nil, fmt.Errorf("open memory state directory: %w", err)
	}
	if err := stateRoot.MkdirAll("memory", 0700); err != nil {
		return nil, err
	}
	selector := options.Selector
	if selector == nil {
		selector = NewSelector(options.Model)
	}
	processor := options.Processor
	if processor == nil {
		processor = NewProcessor(options.Model)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{
		projectRoot:   projectRoot,
		userConfigDir: filepath.Clean(userConfigDir),
		stateRoot:     stateRoot,
		selector:      selector,
		processor:     processor,
		onEvent:       options.OnEvent,
		now:           now,
		queue:         make(chan RunCompletion, backgroundQueueSize),
	}
	s.worker.Add(1)
	go s.runWorker()
	return s, nil
}

func (s *Service) Close(ctx context.Context) error {
	s.closeMu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.closeMu.Unlock()
	done := make(chan struct{})
	go func() {
		s.worker.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) PrepareRun(ctx context.Context, projectRoot, workDir, sessionID, query string) (RunMemoryContext, error) {
	if err := s.checkProjectRoot(projectRoot); err != nil {
		return RunMemoryContext{}, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	store, err := s.newStore()
	if err != nil {
		return RunMemoryContext{}, err
	}
	userInstructionDir := filepath.Join(s.userConfigDir, "stable")
	sources, issues := DiscoverInstructions(s.projectRoot, workDir, userInstructionDir)
	result := RunMemoryContext{InstructionText: renderInstructionSources(sources), Issues: issues}
	state, err := s.loadState()
	if err != nil {
		result.Issues = append(result.Issues, LoadIssue{Path: "memory state", Reason: safeError(err)})
	} else {
		result.ExtractedThrough = state.SessionCursors[sessionID]
	}
	userIndex, userTruncated, userIssues, err := store.Index(ScopeUser)
	if err != nil {
		result.Issues = append(result.Issues, LoadIssue{Path: "user memory index", Reason: safeError(err)})
	} else {
		result.UserIndex, result.UserTruncated = userIndex, userTruncated
		result.Issues = append(result.Issues, userIssues...)
	}
	projectIndex, projectTruncated, projectIssues, err := store.Index(ScopeProject)
	if err != nil {
		result.Issues = append(result.Issues, LoadIssue{Path: "project memory index", Reason: safeError(err)})
	} else {
		result.ProjectIndex, result.ProjectTruncated = projectIndex, projectTruncated
		result.Issues = append(result.Issues, projectIssues...)
	}
	headers, headerIssues, err := store.List()
	if err != nil {
		result.Issues = append(result.Issues, LoadIssue{Path: "memory entries", Reason: safeError(err)})
		return result, nil
	}
	result.Issues = append(result.Issues, headerIssues...)
	refs, err := s.selector.Select(ctx, query, headers)
	if err != nil {
		result.Issues = append(result.Issues, LoadIssue{Path: "memory selector", Reason: safeError(err)})
		return result, nil
	}
	for _, ref := range refs {
		entry, err := store.Read(ref.Scope, ref.Filename)
		if err != nil {
			result.Issues = append(result.Issues, LoadIssue{Path: ref.Filename, Reason: safeError(err)})
			continue
		}
		result.Selected = append(result.Selected, entry)
	}
	return result, nil
}

func (s *Service) List(ctx context.Context, projectRoot string) ([]MemoryHeader, error) {
	if err := s.checkProjectRoot(projectRoot); err != nil {
		return nil, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	store, err := s.newStore()
	if err != nil {
		return nil, err
	}
	headers, issues, err := store.List()
	if err != nil {
		return headers, err
	}
	if len(issues) > 0 {
		return headers, fmt.Errorf("%d invalid memory entries were skipped", len(issues))
	}
	return headers, nil
}

func (s *Service) Read(ctx context.Context, projectRoot string, scope MemoryScope, filename string) (MemoryEntry, error) {
	if err := s.checkProjectRoot(projectRoot); err != nil {
		return MemoryEntry{}, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	store, err := s.newStore()
	if err != nil {
		return MemoryEntry{}, err
	}
	return store.Read(scope, filename)
}

func (s *Service) Save(ctx context.Context, projectRoot string, change MemoryChange) error {
	if err := s.checkProjectRoot(projectRoot); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	store, err := s.newStore()
	if err != nil {
		return err
	}
	if change.Action == ActionDelete {
		return s.deleteByName(store, change)
	}
	_, err = store.Save(change)
	return err
}

func (s *Service) Delete(ctx context.Context, projectRoot string, scope MemoryScope, filename string) error {
	if err := s.checkProjectRoot(projectRoot); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	store, err := s.newStore()
	if err != nil {
		return err
	}
	return store.Delete(scope, filename)
}

func (s *Service) Clear(ctx context.Context, projectRoot string, scope MemoryScope) (int, error) {
	if err := s.checkProjectRoot(projectRoot); err != nil {
		return 0, err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	store, err := s.newStore()
	if err != nil {
		return 0, err
	}
	return store.Clear(scope)
}

func (s *Service) CompleteRun(_ context.Context, completion RunCompletion) {
	if strings.TrimSpace(completion.SessionID) == "" {
		s.emit(s.projectRoot, BackgroundEvent{Action: "extract", State: "failed", Reason: "session id is missing"})
		return
	}
	if err := s.checkProjectRoot(completion.ProjectRoot); err != nil {
		s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Reason: safeError(err)})
		return
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- completion:
	default:
		s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "skipped", Reason: "background queue is full"})
	}
}

func (s *Service) MaybeConsolidate(ctx context.Context, projectRoot string) error {
	if err := s.checkProjectRoot(projectRoot); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.consolidate(ctx, "")
}

func (s *Service) runWorker() {
	defer s.worker.Done()
	for completion := range s.queue {
		ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
		s.processCompletion(ctx, completion)
		cancel()
	}
}

func (s *Service) processCompletion(ctx context.Context, completion RunCompletion) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	state, err := s.loadState()
	if err != nil {
		s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Reason: safeError(err)})
		return
	}
	if state.SessionCursors == nil {
		state.SessionCursors = make(map[string]uint64)
	}
	if state.SessionsSinceConsolidation == nil {
		state.SessionsSinceConsolidation = make(map[string]bool)
	}
	state.SessionsSinceConsolidation[completion.SessionID] = true
	store, err := s.newStore()
	if err != nil {
		s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Reason: safeError(err)})
		return
	}
	count := 0
	switch {
	case completion.MainAgentWroteMemory:
		// A successful host-tool save already captured the run's memory change.
	case len(completion.Messages) == 0:
		// There is no new text to extract.
	default:
		headers, _, err := store.List()
		if err != nil {
			s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Reason: safeError(err)})
			return
		}
		changes, err := s.processor.Extract(ctx, ExtractionInput{WorkKind: completion.WorkKind, Messages: completion.Messages, Existing: headers})
		if err != nil {
			s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Reason: safeError(err)})
			return
		}
		changes, err = validateChanges(changes, completion.WorkKind)
		if err != nil {
			s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Reason: safeError(err)})
			return
		}
		count, err = s.applyChanges(store, changes)
		if err != nil {
			s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Count: count, Reason: safeError(err)})
			return
		}
	}
	if completion.ThroughSeq > state.SessionCursors[completion.SessionID] {
		state.SessionCursors[completion.SessionID] = completion.ThroughSeq
	}
	if err := s.saveState(state); err != nil {
		s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: "failed", Count: count, Reason: safeError(err)})
		return
	}
	stateName := "success"
	if completion.MainAgentWroteMemory || len(completion.Messages) == 0 {
		stateName = "skipped"
	}
	s.emitFor(completion.SessionID, BackgroundEvent{Action: "extract", State: stateName, Count: count})
	if err := s.consolidate(ctx, completion.SessionID); err != nil {
		s.emitFor(completion.SessionID, BackgroundEvent{Action: "consolidate", State: "failed", Reason: safeError(err)})
	}
}

func (s *Service) consolidate(ctx context.Context, sessionID string) error {
	state, err := s.loadState()
	if err != nil {
		return err
	}
	if state.SessionsSinceConsolidation == nil {
		state.SessionsSinceConsolidation = make(map[string]bool)
	}
	now := s.now().UTC()
	if !state.LastConsolidatedAt.IsZero() && now.Sub(state.LastConsolidatedAt) < 24*time.Hour {
		s.emitFor(sessionID, BackgroundEvent{Action: "consolidate", State: "skipped", Reason: "24-hour interval has not elapsed"})
		return nil
	}
	if len(state.SessionsSinceConsolidation) < 5 {
		s.emitFor(sessionID, BackgroundEvent{Action: "consolidate", State: "skipped", Reason: "fewer than five active sessions"})
		return nil
	}
	store, err := s.newStore()
	if err != nil {
		return err
	}
	input, err := s.consolidationInput(store, len(state.SessionsSinceConsolidation))
	if err != nil {
		return err
	}
	changes, err := s.processor.Consolidate(ctx, input)
	if err != nil {
		return err
	}
	changes, err = validateChanges(changes, "consolidation")
	if err != nil {
		return err
	}
	count, err := s.applyChanges(store, changes)
	if err != nil {
		return err
	}
	state.LastConsolidatedAt = now
	state.SessionsSinceConsolidation = make(map[string]bool)
	if err := s.saveState(state); err != nil {
		return err
	}
	s.emitFor(sessionID, BackgroundEvent{Action: "consolidate", State: "success", Count: count})
	return nil
}

func (s *Service) consolidationInput(store *Store, activeSessions int) (ConsolidationInput, error) {
	input := ConsolidationInput{ActiveSessions: activeSessions}
	var consumed int
	for _, scope := range []MemoryScope{ScopeUser, ScopeProject} {
		index, truncated, _, err := store.Index(scope)
		if err != nil {
			return ConsolidationInput{}, err
		}
		input.Truncated = input.Truncated || truncated
		if scope == ScopeUser {
			input.UserIndex = index
		} else {
			input.ProjectIndex = index
		}
		headers, _, err := store.Headers(scope)
		if err != nil {
			return ConsolidationInput{}, err
		}
		for _, header := range headers {
			if consumed >= MaxConsolidationInputBytes {
				input.Truncated = true
				break
			}
			entry, err := store.Read(scope, header.Filename)
			if err != nil {
				input.Truncated = true
				continue
			}
			if consumed+len(entry.Body) > MaxConsolidationInputBytes {
				remaining := MaxConsolidationInputBytes - consumed
				if remaining <= 0 {
					input.Truncated = true
					break
				}
				entry.Body = entry.Body[:remaining]
				input.Truncated = true
			}
			consumed += len(entry.Body)
			if scope == ScopeUser {
				input.UserEntries = append(input.UserEntries, entry)
			} else {
				input.ProjectEntries = append(input.ProjectEntries, entry)
			}
		}
	}
	return input, nil
}

func (s *Service) applyChanges(store *Store, changes []MemoryChange) (int, error) {
	count := 0
	for _, change := range changes {
		if change.Action == ActionDelete {
			headers, _, err := store.Headers(change.Scope)
			if err != nil {
				return count, err
			}
			for _, header := range headers {
				if header.Type == change.Type && header.Name == change.Name {
					if err := store.Delete(change.Scope, header.Filename); err != nil {
						return count, err
					}
					count++
					break
				}
			}
			continue
		}
		if _, err := store.Save(change); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Service) deleteByName(store *Store, change MemoryChange) error {
	headers, _, err := store.Headers(change.Scope)
	if err != nil {
		return err
	}
	for _, header := range headers {
		if header.Type == change.Type && header.Name == strings.TrimSpace(change.Name) {
			return store.Delete(change.Scope, header.Filename)
		}
	}
	return os.ErrNotExist
}

func (s *Service) newStore() (*Store, error) {
	return NewStore(s.projectRoot, s.userConfigDir)
}

func (s *Service) checkProjectRoot(projectRoot string) error {
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		return err
	}
	if filepath.Clean(abs) != s.projectRoot {
		return errors.New("memory manager is bound to a different project root")
	}
	return nil
}

func (s *Service) stateFilename() string {
	hash := sha256.Sum256([]byte(s.projectRoot))
	return "memory/" + hex.EncodeToString(hash[:]) + ".json"
}

func (s *Service) loadState() (WorkerState, error) {
	state := WorkerState{SessionCursors: make(map[string]uint64), SessionsSinceConsolidation: make(map[string]bool)}
	f, err := s.stateRoot.Open(s.stateFilename())
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return WorkerState{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return WorkerState{}, err
	}
	if len(data) > 1<<20 {
		return WorkerState{}, errors.New("memory state exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return WorkerState{}, fmt.Errorf("invalid memory state: %w", err)
	}
	if state.SessionCursors == nil {
		state.SessionCursors = make(map[string]uint64)
	}
	if state.SessionsSinceConsolidation == nil {
		state.SessionsSinceConsolidation = make(map[string]bool)
	}
	return state, nil
}

func (s *Service) saveState(state WorkerState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.stateRoot.WriteFileAtomic(s.stateFilename(), data, 0600)
}

func (s *Service) emit(projectRoot string, event BackgroundEvent) {
	if s.onEvent == nil {
		return
	}
	if event.At.IsZero() {
		event.At = s.now().UTC()
	}
	if len(event.Reason) > 256 {
		event.Reason = event.Reason[:256]
	}
	s.onEvent(projectRoot, event)
}

func (s *Service) emitFor(sessionID string, event BackgroundEvent) {
	event.SessionID = sessionID
	s.emit(s.projectRoot, event)
}

func safeError(err error) string {
	if err == nil {
		return "unknown error"
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 256 {
		message = message[:256]
	}
	return message
}

func renderInstructionSources(sources []InstructionSource) string {
	var b strings.Builder
	for _, source := range sources {
		fmt.Fprintf(&b, "\n--- Instructions: %s (priority %d) ---\n%s\n", source.Path, source.Priority, source.Content)
	}
	return b.String()
}
