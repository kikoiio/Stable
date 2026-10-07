package conversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"stable/internal/agent"
	"stable/internal/hooks"
	"stable/internal/sessionlog"
)

const maxHookNotifications = 20

type queuedNotice struct {
	ID     string
	Output string
}

// HookGate is the conversation-side hooks facade: it loads the two-level
// config, fires lifecycle hooks, journals hook events, and queues outputs
// for the next run. It implements execution.HookRunner.
type HookGate struct {
	mu          sync.Mutex
	service     *Service
	serviceCtx  context.Context
	serviceStop context.CancelFunc
	userPath    string
	projectPath string
	loaded      []hooks.Hook
	rejections  []string
	modTimes    map[string]time.Time
	onceFired   map[string]map[string]bool
	queue       map[string][]queuedNotice
	asyncAgents map[string]map[uint64]context.CancelFunc
	asyncSeq    uint64
	closing     bool
}

func NewHookGate(service *Service, userPath, projectPath string) *HookGate {
	return &HookGate{
		service:     service,
		userPath:    userPath,
		projectPath: projectPath,
		modTimes:    map[string]time.Time{},
		onceFired:   map[string]map[string]bool{},
		queue:       map[string][]queuedNotice{},
	}
}

func (g *HookGate) Bind(service *Service) {
	base := service.lifeCtx
	if base == nil {
		base = context.Background()
	}
	serviceCtx, stop := context.WithCancel(base)
	g.mu.Lock()
	if g.serviceStop != nil {
		g.serviceStop()
	}
	g.service = service
	g.serviceCtx, g.serviceStop = serviceCtx, stop
	g.mu.Unlock()
	service.hooks = g
}

func (g *HookGate) Close() {
	g.mu.Lock()
	stop := g.serviceStop
	g.serviceStop = nil
	g.serviceCtx = nil
	g.closing = true
	var cancelAgents []context.CancelFunc
	for _, tasks := range g.asyncAgents {
		for _, cancel := range tasks {
			cancelAgents = append(cancelAgents, cancel)
		}
	}
	g.mu.Unlock()
	for _, cancel := range cancelAgents {
		cancel()
	}
	if stop != nil {
		stop()
	}
}

func (g *HookGate) serviceContext() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.serviceCtx != nil {
		return g.serviceCtx
	}
	return context.Background()
}

func (g *HookGate) ensureLoaded() {
	changed := len(g.modTimes) == 0
	for _, path := range []string{g.userPath, g.projectPath} {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				changed = true
			} else if _, ok := g.modTimes[path]; ok {
				changed = true
			}
			continue
		}
		if prev, ok := g.modTimes[path]; !ok || !info.ModTime().Equal(prev) {
			changed = true
		}
	}
	if !changed {
		return
	}
	result := hooks.LoadFiles(g.userPath, g.projectPath)
	g.loaded = result.Hooks
	g.rejections = result.Rejections
	g.modTimes = map[string]time.Time{}
	for _, path := range []string{g.userPath, g.projectPath} {
		if info, err := os.Stat(path); err == nil {
			g.modTimes[path] = info.ModTime()
		}
	}
}

func (g *HookGate) Reload() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	before := len(g.loaded)
	g.modTimes = map[string]time.Time{}
	g.ensureLoaded()
	return before, len(g.loaded)
}

func (g *HookGate) List() ([]HookSummary, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLoaded()
	out := make([]HookSummary, 0, len(g.loaded))
	for _, h := range g.loaded {
		out = append(out, HookSummary{ID: h.ID, Event: string(h.Event), Action: h.Action.Type, Source: h.Source, Reject: h.Reject, Once: h.Once, Async: h.Async})
	}
	return out, append([]string(nil), g.rejections...)
}

func (g *HookGate) Rejections() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ensureLoaded()
	return append([]string(nil), g.rejections...)
}

func (g *HookGate) DrainNotifications(sessionID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	notices := g.queue[sessionID]
	delete(g.queue, sessionID)
	if len(notices) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range notices {
		fmt.Fprintf(&b, "<hook-notification id=%q>\n%s\n</hook-notification>\n", n.ID, n.Output)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (g *HookGate) PreToolUse(sessionID, toolName string, args map[string]any) (bool, string, string) {
	return g.PreToolUseRun(context.Background(), agent.ParentRun{Work: agent.WorkRef{SessionID: sessionID}}, sessionID, toolName, args)
}

func (g *HookGate) PreToolUseRun(ctx context.Context, parent agent.ParentRun, sessionID, toolName string, args map[string]any) (bool, string, string) {
	hookCtx := hooks.Context{Event: hooks.EventPreToolUse, ToolName: toolName, ToolArgs: args, FilePath: filePathFromArgs(args)}
	rejected, hookID, message := g.fire(ctx, parent, sessionID, hookCtx, parent.RunID, true)
	return rejected, hookID, message
}

func (g *HookGate) PostToolUse(sessionID, toolName string, args map[string]any, result string) {
	g.PostToolUseRun(context.Background(), agent.ParentRun{Work: agent.WorkRef{SessionID: sessionID}}, sessionID, toolName, args, result)
}

func (g *HookGate) PostToolUseRun(ctx context.Context, parent agent.ParentRun, sessionID, toolName string, args map[string]any, result string) {
	g.fire(ctx, parent, sessionID, hooks.Context{Event: hooks.EventPostToolUse, ToolName: toolName, ToolArgs: args, FilePath: filePathFromArgs(args), Message: result}, parent.RunID, false)
}

func (g *HookGate) RunStart(sessionID, runID, intent string) {
	g.RunStartRun(context.Background(), agent.ParentRun{RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}}, sessionID, intent)
}

func (g *HookGate) RunStartRun(ctx context.Context, parent agent.ParentRun, sessionID, intent string) {
	g.fire(ctx, parent, sessionID, hooks.Context{Event: hooks.EventRunStart, Message: intent}, parent.RunID, false)
}

func (g *HookGate) RunEnd(sessionID, runID, status, message string) {
	g.RunEndRun(context.Background(), agent.ParentRun{RunID: runID, Work: agent.WorkRef{Kind: agent.WorkSession, SessionID: sessionID}}, sessionID, status, message)
}

func (g *HookGate) RunEndRun(ctx context.Context, parent agent.ParentRun, sessionID, status, message string) {
	parent.Deadline = time.Time{}
	if status == string(agent.RunCancelled) {
		g.cancelAsyncAgents(parent.RunID)
	}
	g.fire(ctx, parent, sessionID, hooks.Context{Event: hooks.EventRunEnd, Message: message}, parent.RunID, false)
}

func (g *HookGate) fire(ctx context.Context, parent agent.ParentRun, sessionID string, hookCtx hooks.Context, runID string, stopOnReject bool) (bool, string, string) {
	g.mu.Lock()
	g.ensureLoaded()
	list := append([]hooks.Hook(nil), g.loaded...)
	g.mu.Unlock()
	var firstReject *hooks.Result
	for _, h := range list {
		if h.Event != hookCtx.Event || !hooks.EvaluateCondition(h.If, hookCtx) {
			continue
		}
		g.mu.Lock()
		if h.Once {
			if g.onceFired[sessionID] == nil {
				g.onceFired[sessionID] = map[string]bool{}
			}
			if g.onceFired[sessionID][h.ID] {
				g.mu.Unlock()
				continue
			}
			g.onceFired[sessionID][h.ID] = true
		}
		g.mu.Unlock()
		if h.Async && !stopOnReject {
			if strings.EqualFold(h.Action.Type, "agent") {
				g.startAsyncAgent(ctx, parent, sessionID, h, hookCtx, runID)
			} else {
				go g.runOne(ctx, parent, sessionID, h, hookCtx, runID)
			}
			continue
		}
		result := g.runOne(ctx, parent, sessionID, h, hookCtx, runID)
		if stopOnReject && result.Rejected && firstReject == nil {
			copy := result
			firstReject = &copy
			break
		}
	}
	if firstReject != nil {
		msg := firstReject.Output
		if msg == "" {
			msg = "blocked by hook " + firstReject.HookID
		}
		return true, firstReject.HookID, msg
	}
	return false, "", ""
}

// Async agent children use the service lifetime so a normally completed run
// does not cancel them. RunEndRun cancels only children of canceled parents.
func (g *HookGate) startAsyncAgent(_ context.Context, parent agent.ParentRun, sessionID string, h hooks.Hook, hookCtx hooks.Context, runID string) {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return
	}
	base := g.serviceCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	g.asyncSeq++
	id := g.asyncSeq
	if g.asyncAgents == nil {
		g.asyncAgents = map[string]map[uint64]context.CancelFunc{}
	}
	if g.asyncAgents[parent.RunID] == nil {
		g.asyncAgents[parent.RunID] = map[uint64]context.CancelFunc{}
	}
	g.asyncAgents[parent.RunID][id] = cancel
	g.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			g.mu.Lock()
			delete(g.asyncAgents[parent.RunID], id)
			if len(g.asyncAgents[parent.RunID]) == 0 {
				delete(g.asyncAgents, parent.RunID)
			}
			g.mu.Unlock()
		}()
		g.runOne(ctx, parent, sessionID, h, hookCtx, runID)
	}()
}

func (g *HookGate) cancelAsyncAgents(runID string) {
	g.mu.Lock()
	tasks := g.asyncAgents[runID]
	delete(g.asyncAgents, runID)
	g.mu.Unlock()
	for _, cancel := range tasks {
		cancel()
	}
}

func (g *HookGate) runOne(ctx context.Context, parent agent.ParentRun, sessionID string, h hooks.Hook, hookCtx hooks.Context, runID string) hooks.Result {
	var result hooks.Result
	if strings.EqualFold(h.Action.Type, "agent") {
		result = g.runAgent(ctx, parent, h, hookCtx)
	} else {
		result = hooks.FireOne(h, hookCtx)
	}
	output := result.Output
	if g.service != nil {
		output = redactRunCredential(output, g.service.deps.ProviderCredential)
	}
	if utf8.RuneCountInString(output) > sessionlog.MaxHookOutput {
		runes := []rune(output)
		output = string(runes[:sessionlog.MaxHookOutput])
	}
	result.Output = output
	g.journal(sessionID, h, result, runID)
	if output != "" {
		g.enqueue(sessionID, h.ID, output)
	}
	return result
}

func (g *HookGate) journal(sessionID string, h hooks.Hook, result hooks.Result, runID string) {
	svc := g.service
	if svc == nil || sessionID == "" {
		return
	}
	data := sessionlog.HookFired{
		HookID: h.ID, Event: string(h.Event), Action: h.Action.Type, Source: h.Source,
		Success: result.Success, Rejected: result.Rejected, TimedOut: result.TimedOut,
		Output: result.Output, RunID: runID, ChildRunID: result.ChildRunID,
	}
	svc.eventMu.Lock()
	_, _ = sessionlog.Append(svc.deps.ProjectRoot, sessionID, sessionlog.EventHookFired, data)
	svc.eventMu.Unlock()
}

func (g *HookGate) enqueue(sessionID, id, output string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.queue[sessionID]
	if len(q) >= maxHookNotifications {
		output = "（更早通知已丢弃）\n" + output
		q = q[1:]
	}
	g.queue[sessionID] = append(q, queuedNotice{ID: id, Output: output})
}

func filePathFromArgs(args map[string]any) string {
	if args == nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "file"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func (s *Service) listHooks() ([]ServerMsg, error) {
	if s.hooks == nil {
		return nil, errors.New("hooks 通道不可用")
	}
	hooks, rejections := s.hooks.List()
	return []ServerMsg{{Type: "hook_list", HookList: &HookListMsg{Hooks: hooks, Rejections: rejections}}}, nil
}

func (s *Service) reloadHooks(c ClientMsg) ([]ServerMsg, error) {
	if s.hooks == nil {
		return nil, errors.New("hooks 通道不可用")
	}
	before, after := s.hooks.Reload()
	if c.SessionID != "" {
		s.eventMu.Lock()
		_, _ = sessionlog.Append(s.deps.ProjectRoot, c.SessionID, sessionlog.EventHookReload, sessionlog.HookReload{Before: before, After: after})
		s.eventMu.Unlock()
	}
	return []ServerMsg{{Type: "hook_report", HookReport: &HookReportMsg{Before: before, After: after}}}, nil
}
