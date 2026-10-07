package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"stable/internal/agent"
	"stable/internal/commands"
	"stable/internal/llm"
	"stable/internal/sessionlog"
	"stable/internal/skills"
)

// SkillGate is the conversation-side skill facade: it owns the service-side
// skills catalog, activates skills for both entries (slash command op and the
// load_skill tool), journals the skill events, and composes the per-session
// inventory text injected into every run. It implements execution.SkillProvider.
type SkillGate struct {
	mu      sync.Mutex
	service *Service
	catalog *skills.Catalog
	states  map[string]*sessionSkillState
}

// sessionSkillState is the per-session skill runtime state. It is restored
// lazily from the session log so a restarted service rebuilds the inventory
// snapshot, the already-notified skill names, and the activation list.
type sessionSkillState struct {
	inventorySent bool
	snapshot      []sessionlog.SkillInfo
	notified      map[string]bool
	activated     []string
}

// NewSkillGate builds the gate for one project. The service may be nil when
// the executor factory is built before conversation.Serve returns; call Bind
// once the service exists. Event appends require the bound service.
func NewSkillGate(service *Service, userDir, projectDir string) *SkillGate {
	return &SkillGate{
		service: service,
		catalog: skills.LoadCatalog(userDir, projectDir),
		states:  map[string]*sessionSkillState{},
	}
}

// Bind attaches the conversation service after construction and registers
// the gate as the service skill surface. Serve auto-binds deps.Skills, so
// production wiring does not call this directly.
func (g *SkillGate) Bind(service *Service) {
	g.mu.Lock()
	g.service = service
	g.mu.Unlock()
	service.skills = g
}

// LoadSkill implements execution.SkillProvider for the load_skill tool.

func (g *SkillGate) LoadSkill(ctx context.Context, sessionID, name, args string) (string, error) {
	if g.isFork(name) {
		prepared, err := g.prepareFork(sessionID, name, args, sessionlog.SkillEntryTool)
		if err != nil {
			return "", err
		}
		parent, ok := agent.ForkSkillParentRunFromContext(ctx)
		if !ok {
			return "", errors.New("fork skill parent run context is unavailable")
		}
		g.mu.Lock()
		service := g.service
		g.mu.Unlock()
		if service == nil {
			return "", errors.New("fork skill service is unavailable")
		}
		return service.executeForkSkill(ctx, parent, prepared)
	}
	return g.activate(sessionID, name, args, sessionlog.SkillEntryTool)
}

type preparedForkSkill struct {
	Name        string
	Source      string
	Entry       string
	Args        string
	Instruction string
	ContextMode ForkContextMode
}

func (g *SkillGate) isFork(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	skill, ok := g.catalog.Get(name)
	return ok && skill.Meta.IsFork()
}

func (g *SkillGate) prepareFork(sessionID, name, args, entry string) (preparedForkSkill, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stateLocked(sessionID)
	skill, ok := g.catalog.Get(name)
	if !ok {
		return preparedForkSkill{}, fmt.Errorf("unknown skill: %s (available: %s)", name, g.availableNamesLocked())
	}
	if !skill.Meta.IsFork() {
		return preparedForkSkill{}, fmt.Errorf("skill %s is not a fork skill", name)
	}
	full, err := g.catalog.GetFull(name)
	if err != nil {
		return preparedForkSkill{}, fmt.Errorf("技能 %s 正文读取失败: %w", name, err)
	}
	if len([]byte(full.PromptBody))+len([]byte(args)) > 64<<10 {
		return preparedForkSkill{}, fmt.Errorf("技能 %s 的正文与参数超过 64 KiB", name)
	}
	mode := ForkContextMode(full.Meta.ForkContext)
	switch mode {
	case ForkContextNone, ForkContextRecent, ForkContextFull:
	default:
		return preparedForkSkill{}, fmt.Errorf("技能 %s 的 fork_context 无效: %q", name, full.Meta.ForkContext)
	}
	instruction := commands.ExpandPrompt(full.PromptBody, args)
	if len([]byte(instruction)) > 64<<10 {
		return preparedForkSkill{}, fmt.Errorf("技能 %s 的正文与参数超过 64 KiB", name)
	}
	return preparedForkSkill{Name: name, Source: full.Source, Entry: entry, Args: args, Instruction: instruction, ContextMode: mode}, nil
}

func (g *SkillGate) recordForkInvocation(sessionID string, prepared preparedForkSkill, runID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.stateLocked(sessionID)
	args := redactRunCredential(prepared.Args, g.service.deps.ProviderCredential)
	invoked := sessionlog.SkillInvoked{Name: prepared.Name, Source: prepared.Source, Entry: prepared.Entry, Args: args, Mode: sessionlog.SkillModeFork, RunID: runID}
	if err := g.appendEventLocked(sessionID, sessionlog.EventSkillInvoked, invoked); err != nil {
		return fmt.Errorf("record skill invocation: %w", err)
	}
	state.activated = appendUniqueName(state.activated, prepared.Name)
	return nil
}

// activate handles inline skills, refuses fork skills that bypass the fork
// runner, reads the freshest body, renders it with the M06 argument semantics,
// journals skill_invoked, and records the activation for the session inventory.
func (g *SkillGate) activate(sessionID, name, args, entry string) (string, error) {
	g.mu.Lock()
	state := g.stateLocked(sessionID)
	skill, ok := g.catalog.Get(name)
	if !ok {
		available := g.availableNamesLocked()
		g.mu.Unlock()
		return "", fmt.Errorf("unknown skill: %s (available: %s)", name, available)
	}
	if skill.Meta.IsFork() {
		g.mu.Unlock()
		return "", fmt.Errorf("fork skill %s must use the fork execution path", name)
	}
	full, err := g.catalog.GetFull(name)
	if err != nil {
		g.mu.Unlock()
		return "", fmt.Errorf("技能 %s 正文读取失败: %w", name, err)
	}
	body := commands.ExpandPrompt(full.PromptBody, args)
	invoked := sessionlog.SkillInvoked{Name: name, Source: full.Source, Entry: entry, Args: args}
	if err := g.appendEventLocked(sessionID, sessionlog.EventSkillInvoked, invoked); err != nil {
		g.mu.Unlock()
		return "", fmt.Errorf("record skill invocation: %w", err)
	}
	state.activated = appendUniqueName(state.activated, name)
	g.mu.Unlock()
	return body, nil
}

// SkillInventory implements execution.SkillProvider. It journals the session
// inventory snapshot on the first run, picks up newly added skills as a
// one-shot delta, and composes the injection text: the stable skill list
// plus the activated-skill note that survives compaction.
func (g *SkillGate) SkillInventory(_ context.Context, sessionID string) (string, string, error) {
	g.mu.Lock()
	state := g.stateLocked(sessionID)
	if err := g.ensureInventoryLocked(sessionID, state); err != nil {
		g.mu.Unlock()
		return "", "", fmt.Errorf("record skill inventory: %w", err)
	}
	delta, added, err := g.refreshAndDiffLocked(sessionID, state)
	g.mu.Unlock()
	if err != nil {
		return "", "", fmt.Errorf("record skill delta: %w", err)
	}
	if len(added) > 0 {
		g.broadcastDelta(sessionID, added)
	}
	return inventoryText(state.snapshot, state.activated), delta, nil
}

// List returns the current catalog infos and the session's activated skill
// names for the /skills listing.
func (g *SkillGate) List(sessionID string) ([]sessionlog.SkillInfo, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	infos := g.catalogInfosLocked()
	activated := append([]string(nil), g.stateLocked(sessionID).activated...)
	return infos, activated
}

// Reload rescans the skill directories; the next run's inventory diff then
// surfaces skills that appeared since the last run as a delta. It reports
// the skill count before and after.
func (g *SkillGate) Reload() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	before := len(g.catalog.List())
	g.catalog.Reload()
	return before, len(g.catalog.List())
}

// Rejections reports the skipped or rejected skill entries of the last scan.
func (g *SkillGate) Rejections() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.catalog.Rejections()
}

// stateLocked returns the per-session state, restoring it from the session
// log on first touch so restarts rebuild snapshot, notified set and
// activations exactly as they were.
func (g *SkillGate) stateLocked(sessionID string) *sessionSkillState {
	if state, ok := g.states[sessionID]; ok {
		return state
	}
	state := g.restore(sessionID)
	g.states[sessionID] = state
	return state
}

// restore replays the three skill event types of one session. It runs under
// g.mu and only reads the log, so no event mutex is taken.
func (g *SkillGate) restore(sessionID string) *sessionSkillState {
	state := &sessionSkillState{notified: map[string]bool{}}
	svc := g.service
	if svc == nil {
		return state
	}
	replay, err := sessionlog.Replay(svc.deps.ProjectRoot, sessionID)
	if err != nil {
		return state
	}
	for _, event := range replay.Events {
		switch event.Type {
		case sessionlog.EventSkillInventory:
			var inv sessionlog.SkillInventory
			if decodeSessionData(event.Data, &inv) != nil {
				continue
			}
			state.inventorySent = true
			state.snapshot = append([]sessionlog.SkillInfo(nil), inv.Skills...)
			for _, info := range inv.Skills {
				state.notified[info.Name] = true
			}
		case sessionlog.EventSkillDelta:
			var delta sessionlog.SkillDelta
			if decodeSessionData(event.Data, &delta) != nil {
				continue
			}
			for _, info := range delta.Added {
				state.notified[info.Name] = true
				state.snapshot = upsertSkillInfo(state.snapshot, info)
			}
		case sessionlog.EventSkillInvoked:
			var invoked sessionlog.SkillInvoked
			if decodeSessionData(event.Data, &invoked) != nil || invoked.Name == "" {
				continue
			}
			state.activated = appendUniqueName(state.activated, invoked.Name)
		}
	}
	return state
}

// ensureInventoryLocked journals the one-time skill_inventory snapshot of the
// session from the current catalog state.
func (g *SkillGate) ensureInventoryLocked(sessionID string, state *sessionSkillState) error {
	if state.inventorySent {
		return nil
	}
	infos := g.catalogInfosLocked()
	if err := g.appendEventLocked(sessionID, sessionlog.EventSkillInventory, sessionlog.SkillInventory{Skills: infos}); err != nil {
		return err
	}
	state.inventorySent = true
	state.snapshot = append([]sessionlog.SkillInfo(nil), infos...)
	for _, info := range infos {
		state.notified[info.Name] = true
	}
	return nil
}

// refreshAndDiffLocked rescans the catalog when the directories changed and
// journals the skills this session has never been told about as one
// skill_delta event. The diff runs on every call — not only on mtime
// changes — so skills added while the service was down are announced on the
// first run after restart; for a session whose inventory was just journaled
// the diff is empty because ensureInventory marked every name notified.
func (g *SkillGate) refreshAndDiffLocked(sessionID string, state *sessionSkillState) (string, []sessionlog.SkillInfo, error) {
	if g.catalog.NeedsReload() {
		g.catalog.Reload()
	}
	var added []sessionlog.SkillInfo
	for _, info := range g.catalogInfosLocked() {
		if !state.notified[info.Name] {
			added = append(added, info)
		}
	}
	if len(added) == 0 {
		return "", nil, nil
	}
	if err := g.appendEventLocked(sessionID, sessionlog.EventSkillDelta, sessionlog.SkillDelta{Added: added}); err != nil {
		return "", nil, err
	}
	for _, info := range added {
		state.notified[info.Name] = true
		state.snapshot = upsertSkillInfo(state.snapshot, info)
	}
	return deltaReminderText(added), added, nil
}

// appendEventLocked journals one skill event under the service event mutex,
// keeping a single-writer discipline with the other session log appends.
func (g *SkillGate) appendEventLocked(sessionID string, typ string, data any) error {
	svc := g.service
	if svc == nil {
		return errors.New("skill gate is not bound to a session service")
	}
	svc.eventMu.Lock()
	_, err := sessionlog.Append(svc.deps.ProjectRoot, sessionID, typ, data)
	svc.eventMu.Unlock()
	return err
}

func (g *SkillGate) catalogInfosLocked() []sessionlog.SkillInfo {
	list := g.catalog.List()
	infos := make([]sessionlog.SkillInfo, 0, len(list))
	for _, skill := range list {
		infos = append(infos, sessionlog.SkillInfo{
			Name:        skill.Meta.Name,
			Description: skill.Meta.Description,
			WhenToUse:   skill.Meta.WhenToUse,
			Source:      skill.Source,
		})
	}
	return infos
}

func (g *SkillGate) availableNamesLocked() string {
	list := g.catalog.List()
	names := make([]string, 0, len(list))
	for _, skill := range list {
		names = append(names, skill.Meta.Name)
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// broadcastDelta pushes the skill delta to the session clients once it is
// journaled. It must be called without holding g.mu.
func (g *SkillGate) broadcastDelta(sessionID string, added []sessionlog.SkillInfo) {
	g.mu.Lock()
	svc := g.service
	g.mu.Unlock()
	if svc == nil {
		return
	}
	names := make([]string, 0, len(added))
	for _, info := range added {
		names = append(names, info.Name)
	}
	svc.broadcast(ServerMsg{Type: "skill_report", SkillReport: &SkillReport{Kind: SkillReportDelta, SessionID: sessionID, Added: names}})
}

// inventoryText composes the per-run skill inventory injection. An empty
// catalog yields an empty text so runs without skills inject nothing. The
// activated note keeps the skill names perceivable after compaction; the
// bodies are re-readable through load_skill.
func inventoryText(snapshot []sessionlog.SkillInfo, activated []string) string {
	if len(snapshot) == 0 && len(activated) == 0 {
		return ""
	}
	var b strings.Builder
	if len(snapshot) > 0 {
		b.WriteString("## 可用技能\n\n")
		b.WriteString("以下技能在本会话可用,可经斜杠命令 /<名称> 或 load_skill 工具调用:\n")
		for _, info := range snapshot {
			b.WriteString("- " + info.Name)
			if info.Description != "" {
				b.WriteString(" — " + info.Description)
			}
			if info.WhenToUse != "" {
				b.WriteString("(适用场景: " + info.WhenToUse + ")")
			}
			b.WriteString("\n")
		}
	}
	if len(activated) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## 已激活技能\n\n")
		b.WriteString(strings.Join(activated, "、") + " 的正文已在会话早期提供;如该内容已被压缩,可重新调用 load_skill 获取最新正文。\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// deltaReminderText is the one-shot reminder injected on the run that first
// sees the newly added skills.
func deltaReminderText(added []sessionlog.SkillInfo) string {
	names := make([]string, 0, len(added))
	for _, info := range added {
		names = append(names, info.Name)
	}
	return "以下技能在会话期间新增可用: " + strings.Join(names, "、") + " — 可经 /<名称> 或 load_skill 调用。"
}

func upsertSkillInfo(snapshot []sessionlog.SkillInfo, info sessionlog.SkillInfo) []sessionlog.SkillInfo {
	for i := range snapshot {
		if snapshot[i].Name == info.Name {
			snapshot[i] = info
			return snapshot
		}
	}
	return append(snapshot, info)
}

func appendUniqueName(names []string, name string) []string {
	for _, existing := range names {
		if existing == name {
			return names
		}
	}
	return append(names, name)
}

// invokeSkill serves the skill_invoke op: activate through the slash entry,
// then start a run with the rendered body as the user message — the same
// shape a KindPrompt command submission takes, so the body lands in the
// session log as history and the model reads it this run.
func (s *Service) invokeSkill(ctx context.Context, c ClientMsg, updates chan ServerMsg) error {
	if s.skills == nil {
		return errors.New("技能通道不可用")
	}
	if s.skills.isFork(c.SkillName) {
		prepared, err := s.skills.prepareFork(c.SessionID, c.SkillName, c.SkillArgs, sessionlog.SkillEntrySlash)
		if err != nil {
			return err
		}
		return s.startForkSkillRun(ctx, c.SessionID, prepared, updates)
	}
	body, err := s.skills.activate(c.SessionID, c.SkillName, c.SkillArgs, sessionlog.SkillEntrySlash)
	if err != nil {
		return err
	}
	request := agent.ExecutionRequest{
		Work:     agent.WorkRef{Kind: agent.WorkSession, SessionID: c.SessionID},
		Intent:   body,
		Messages: []llm.Message{{Role: "user", Content: body}},
	}
	if c.RunID != "" {
		request.RunID = c.RunID
	}
	return s.startRun(ctx, ClientMsg{Op: "run_start", SessionID: c.SessionID, Run: &request}, updates)
}

// reloadSkills serves the skill_reload op: rescan the directories and report
// the skill counts before and after.
func (s *Service) reloadSkills() ([]ServerMsg, error) {
	if s.skills == nil {
		return nil, errors.New("技能通道不可用")
	}
	before, after := s.skills.Reload()
	return []ServerMsg{{Type: "skill_report", SkillReport: &SkillReport{Kind: SkillReportReload, Before: before, After: after}}}, nil
}

// listSkills serves the skill_list op: the current catalog infos plus the
// session's activated skill names for the /skills listing.
func (s *Service) listSkills(c ClientMsg) ([]ServerMsg, error) {
	if s.skills == nil {
		return nil, errors.New("技能通道不可用")
	}
	infos, activated := s.skills.List(c.SessionID)
	return []ServerMsg{{Type: "skill_list", Skills: infos, SkillActivated: activated}}, nil
}
