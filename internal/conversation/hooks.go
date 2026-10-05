package conversation

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	userPath    string
	projectPath string
	loaded      []hooks.Hook
	rejections  []string
	modTimes    map[string]time.Time
	onceFired   map[string]map[string]bool
	queue       map[string][]queuedNotice
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
	g.mu.Lock()
	g.service = service
	g.mu.Unlock()
	service.hooks = g
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
	ctx := hooks.Context{Event: hooks.EventPreToolUse, ToolName: toolName, ToolArgs: args, FilePath: filePathFromArgs(args)}
	rejected, hookID, message := g.fire(sessionID, ctx, "", true)
	return rejected, hookID, message
}

func (g *HookGate) PostToolUse(sessionID, toolName string, args map[string]any, result string) {
	g.fire(sessionID, hooks.Context{Event: hooks.EventPostToolUse, ToolName: toolName, ToolArgs: args, FilePath: filePathFromArgs(args), Message: result}, "", false)
}

func (g *HookGate) RunStart(sessionID, runID, intent string) {
	g.fire(sessionID, hooks.Context{Event: hooks.EventRunStart, Message: intent}, runID, false)
}

func (g *HookGate) RunEnd(sessionID, runID, status, message string) {
	g.fire(sessionID, hooks.Context{Event: hooks.EventRunEnd, Message: message}, runID, false)
}

func (g *HookGate) fire(sessionID string, ctx hooks.Context, runID string, stopOnReject bool) (bool, string, string) {
	g.mu.Lock()
	g.ensureLoaded()
	list := append([]hooks.Hook(nil), g.loaded...)
	g.mu.Unlock()
	var firstReject *hooks.Result
	for _, h := range list {
		if h.Event != ctx.Event || !hooks.EvaluateCondition(h.If, ctx) {
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
			go g.runOne(sessionID, h, ctx, runID)
			continue
		}
		result := g.runOne(sessionID, h, ctx, runID)
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

func (g *HookGate) runOne(sessionID string, h hooks.Hook, ctx hooks.Context, runID string) hooks.Result {
	result := hooks.FireOne(h, ctx)
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
		Success: result.Success, Rejected: result.Rejected, Output: result.Output, RunID: runID,
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
