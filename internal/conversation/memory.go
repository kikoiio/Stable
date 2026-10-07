package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"stable/internal/execution"
	"stable/internal/memory"
	"stable/internal/sessionlog"
)

// MemoryGate binds the memory manager to the service's canonical project
// root. Session IDs are used for event attribution only.
type MemoryGate struct {
	manager memory.Manager
	root    string
	service *Service
}

func NewMemoryGate(manager memory.Manager, projectRoot string) *MemoryGate {
	return &MemoryGate{manager: manager, root: projectRoot}
}

func (g *MemoryGate) Bind(service *Service) {
	g.service = service
	service.memory = g
}

var _ execution.MemoryProvider = (*MemoryGate)(nil)

func (g *MemoryGate) List(ctx context.Context, sessionID string) ([]memory.MemoryHeader, error) {
	if g == nil || g.manager == nil {
		return nil, errors.New("记忆通道不可用")
	}
	return g.manager.List(ctx, g.root)
}

func (g *MemoryGate) Read(ctx context.Context, sessionID string, scope memory.MemoryScope, filename string) (memory.MemoryEntry, error) {
	if g == nil || g.manager == nil {
		return memory.MemoryEntry{}, errors.New("记忆通道不可用")
	}
	return g.manager.Read(ctx, g.root, scope, filename)
}

func (g *MemoryGate) Save(ctx context.Context, sessionID string, change memory.MemoryChange) error {
	if g == nil || g.manager == nil {
		return errors.New("记忆通道不可用")
	}
	return g.manager.Save(ctx, g.root, change)
}

func (g *MemoryGate) Delete(ctx context.Context, sessionID string, scope memory.MemoryScope, filename string) error {
	if g == nil || g.manager == nil {
		return errors.New("记忆通道不可用")
	}
	return g.manager.Delete(ctx, g.root, scope, filename)
}

func (s *Service) listMemory(ctx context.Context, sessionID string) ([]ServerMsg, error) {
	if s.memory == nil {
		return nil, errors.New("记忆通道不可用")
	}
	entries, err := s.memory.List(ctx, sessionID)
	s.appendMemoryAction(sessionID, "both", "list", "", err)
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "memory_list", MemoryEntries: entries}}, nil
}

func (s *Service) deleteMemory(ctx context.Context, sessionID, scope, filename string) ([]ServerMsg, error) {
	if s.memory == nil {
		return nil, errors.New("记忆通道不可用")
	}
	memScope := memory.MemoryScope(scope)
	err := s.memory.Delete(ctx, sessionID, memScope, filename)
	s.appendMemoryAction(sessionID, scope, "delete", filename, err)
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "memory_report", MemoryReport: &MemoryReportMsg{Operation: "delete", Scope: scope, Deleted: 1}}}, nil
}

func (s *Service) clearMemory(ctx context.Context, sessionID, requestedScope string) ([]ServerMsg, error) {
	if s.memory == nil || s.memory.manager == nil {
		return nil, errors.New("记忆通道不可用")
	}
	scope := requestedScope
	if scope == "" {
		scope = "project"
	}
	var scopes []memory.MemoryScope
	switch scope {
	case "project":
		scopes = []memory.MemoryScope{memory.ScopeProject}
	case "user":
		scopes = []memory.MemoryScope{memory.ScopeUser}
	case "all":
		scopes = []memory.MemoryScope{memory.ScopeUser, memory.ScopeProject}
	default:
		return nil, fmt.Errorf("invalid memory clear scope %q", scope)
	}
	deleted := 0
	var clearErr error
	for _, itemScope := range scopes {
		count, err := s.memory.manager.Clear(ctx, s.memory.root, itemScope)
		deleted += count
		if err != nil {
			clearErr = errors.Join(clearErr, err)
		}
	}
	s.appendMemoryAction(sessionID, scope, "clear", "", clearErr)
	report := MemoryReportMsg{Operation: "clear", Scope: scope, Deleted: deleted}
	if clearErr != nil {
		report.Error = clearErr.Error()
	}
	return []ServerMsg{{Type: "memory_report", MemoryReport: &report}}, nil
}

func (s *Service) appendMemoryAction(sessionID, scope, operation, entry string, err error) {
	if s.deps.ProjectRoot == "" || sessionID == "" {
		return
	}
	state := "success"
	if err != nil {
		state = "failure"
	}
	entry = strings.ToValidUTF8(entry, "�")
	if len([]rune(entry)) > sessionlog.MaxMemoryEventText {
		entry = string([]rune(entry)[:sessionlog.MaxMemoryEventText])
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	_, _ = sessionlog.Append(s.deps.ProjectRoot, sessionID, sessionlog.EventMemoryAction, sessionlog.MemoryActionRecord{
		Scope: scope, Entry: entry, Operation: operation, State: state, At: time.Now().UTC(),
	})
}
