package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/client"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/goalrun"
	"stable/internal/prompt"
	"stable/internal/sessioncontext"
	"stable/internal/sessionlog"
)

func jsonDecoder(r interface{ Read([]byte) (int, error) }) *json.Decoder {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec
}

// handle executes one client operation and returns the messages every
// connected client should see.
func (s *Service) handle(ctx context.Context, c ClientMsg) ([]ServerMsg, error) {
	switch c.Op {
	case "session_list":
		root, err := s.trustedSessionRoot(c.ProjectRoot)
		if err != nil {
			return nil, err
		}
		sessions, err := sessionlog.List(root)
		if err != nil {
			return nil, err
		}
		goals, err := s.deps.Store.ListGoals(ctx)
		if err != nil {
			return nil, err
		}
		goals, err = s.withEvidenceSummaries(ctx, goals)
		if err != nil {
			return nil, err
		}
		return []ServerMsg{{Type: "sessions", Sessions: sessions, Goals: goals}}, nil
	case "session_create":
		root, err := s.trustedSessionRoot(c.ProjectRoot)
		if err != nil {
			return nil, err
		}
		info, err := sessionlog.Create(root, "")
		if err != nil {
			return nil, err
		}
		// Plan mode is session runtime state: a fresh session starts in the
		// default mode with no plan file.
		s.initPlanState(info.ID)
		return []ServerMsg{{Type: "session", Session: &info}}, nil
	case "session_load":
		root, err := s.trustedSessionRoot(c.ProjectRoot)
		if err != nil {
			return nil, err
		}
		replay, err := sessionlog.Replay(root, c.SessionID)
		if err != nil {
			return nil, err
		}
		for i := range replay.Events {
			if replay.Events[i].Type != sessionlog.EventProposal {
				continue
			}
			var p core.CriteriaProposal
			raw, _ := json.Marshal(replay.Events[i].Data)
			if e := json.Unmarshal(raw, &p); e != nil {
				return nil, fmt.Errorf("decode proposal reference in session: %w", e)
			}
			current, e := s.deps.Store.GetProposal(ctx, p.ID)
			if e != nil {
				return nil, fmt.Errorf("restore proposal %s from SQLite: %w", p.ID, e)
			}
			if current.SessionID != c.SessionID {
				return nil, fmt.Errorf("proposal %s is attributed to a different session", p.ID)
			}
			replay.Events[i].Data = current
		}
		if _, err = sessionlog.Append(root, c.SessionID, sessionlog.EventActivity, map[string]string{"action": "loaded"}); err != nil {
			return nil, err
		}
		goals, err := s.deps.Store.ListGoals(ctx)
		if err != nil {
			return nil, err
		}
		goals, err = s.withEvidenceSummaries(ctx, goals)
		if err != nil {
			return nil, err
		}
		plan := s.PlanStateOf(c.SessionID)
		return []ServerMsg{{Type: "transcript", Transcript: &replay, Goals: goals, Plan: &plan}}, nil
	case "plan_mode":
		return s.planMode(c)
	case "plan_resolve":
		return s.planResolve(c)
	case "skill_reload":
		return s.reloadSkills()
	case "skill_list":
		return s.listSkills(c)
	case "hooks_list":
		return s.listHooks()
	case "hooks_reload":
		return s.reloadHooks(c)
	case "mcp_list":
		return s.listMCP(c)
	case "mcp_reload":
		return s.reloadMCP(c)
	case "memory_list":
		return s.listMemory(ctx, c.SessionID)
	case "memory_delete":
		return s.deleteMemory(ctx, c.SessionID, c.MemoryScope, c.MemoryEntry)
	case "memory_clear":
		return s.clearMemory(ctx, c.SessionID, c.MemoryScope)
	case "history":
		history, err := s.deps.Store.ListMessages(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ServerMsg, 0, len(history))
		for i := range history {
			out = append(out, ServerMsg{Type: "message", Message: &history[i]})
		}
		return out, nil
	case "status":
		goals, err := s.deps.Store.ListGoals(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]ServerMsg, 0, len(goals))
		for i := range goals {
			if s.deps.Refresher != nil {
				refreshed, refreshErr := s.deps.Refresher.Refresh(ctx, goals[i].ID)
				if refreshErr != nil {
					return nil, fmt.Errorf("refresh dependencies for goal %s: %w", goals[i].ID, refreshErr)
				}
				goals[i] = refreshed.Snapshot.Goal
			} else {
				snapshot, getErr := s.deps.Store.GetGoalSnapshot(ctx, goals[i].ID)
				if getErr != nil {
					return nil, getErr
				}
				goals[i] = snapshot.Goal
				goals[i].EvidenceSummary = evidenceSummary(snapshot.Evidence)
			}
			out = append(out, ServerMsg{Type: "goal_update", Goal: &goals[i]})
		}
		s.mu.Lock()
		for i := range goals {
			s.statuses[goals[i].ID] = goals[i].Status
		}
		s.mu.Unlock()
		return out, nil
	case "say":
		return s.say(ctx, c, core.MessageKindText, core.EventKindUserMessage)
	case "chat":
		if c.SessionID != "" {
			return s.sessionChat(ctx, c)
		}
		return s.chat(ctx, c)
	case "reply":
		if c.QuestionID != "" {
			return s.replyQuestion(ctx, c)
		}
		return s.say(ctx, c, core.MessageKindReply, core.EventKindHumanReply)
	case "create_goal":
		return s.createGoal(ctx, c)
	case "confirm":
		return s.confirm(ctx, c)
	case "reject":
		return s.reject(ctx, c)
	}
	return nil, fmt.Errorf("unknown op %q", c.Op)
}

func (s *Service) withEvidenceSummaries(ctx context.Context, goals []core.Goal) ([]core.Goal, error) {
	for i := range goals {
		snap, err := s.deps.Store.GetGoalSnapshot(ctx, goals[i].ID)
		if err != nil {
			return nil, err
		}
		goals[i] = snap.Goal
		goals[i].EvidenceSummary = evidenceSummary(snap.Evidence)
	}
	return goals, nil
}
func evidenceSummary(evidence []core.Evidence) string {
	if len(evidence) == 0 {
		return "尚无验证证据"
	}
	counts := map[string]int{}
	for _, e := range evidence {
		counts[e.Result]++
	}
	return fmt.Sprintf("证据 %d 项：通过 %d，失败 %d，过期 %d", len(evidence), counts["pass"], counts["fail"], counts["stale"])
}

// redactProviderCredential prevents accidentally pasted provider keys from
// being persisted in the append-only session transcript.
func redactProviderCredential(text string, providers ...any) string {
	for _, provider := range providers {
		p, ok := provider.(*decision.HTTPProvider)
		if !ok || p == nil || p.Config.APIKey == "" {
			continue
		}
		text = redactRunCredential(text, p.Config.APIKey)
	}
	return text
}

func (s *Service) chat(ctx context.Context, c ClientMsg) ([]ServerMsg, error) {
	if s.deps.ChatProvider == nil {
		return nil, errors.New("chat model provider not configured; run stable config check")
	}
	user, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
		ID: goalrun.RandomID("msg"), Role: core.MessageRoleUser, Kind: core.MessageKindText, Text: c.Text, Ref: "chat",
	})
	if err != nil {
		return nil, err
	}
	out := []ServerMsg{{Type: "message", Message: &user}}
	history, err := s.deps.Store.ListMessages(ctx)
	if err != nil {
		return out, err
	}
	turns := []decision.ChatMessage{{Role: "system", Content: "你是 Stable 的聊天助手。使用用户的语言直接回答问题。目标执行只能通过 /goal、/confirm、/say、/reply 等明确命令触发；不要声称已经执行命令、读取文件或修改设计。对于状态问题，建议使用 /status。"}}
	for _, m := range history {
		if m.Ref != "chat" || m.GoalID != "" || m.Kind != core.MessageKindText || (m.Role != core.MessageRoleUser && m.Role != core.MessageRoleAgent) {
			continue
		}
		role := "assistant"
		if m.Role == core.MessageRoleUser {
			role = "user"
		}
		turns = append(turns, decision.ChatMessage{Role: role, Content: m.Text})
	}
	if len(turns) > 21 {
		turns = append(turns[:1], turns[len(turns)-20:]...)
	}
	if len(turns) > 1 && turns[1].Role == "assistant" {
		turns = append(turns[:1], turns[2:]...)
	}
	answer, err := s.deps.ChatProvider.GenerateChat(ctx, turns)
	if err != nil {
		return out, fmt.Errorf("chat response failed: %w", err)
	}
	agent, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
		ID: goalrun.RandomID("msg"), Role: core.MessageRoleAgent, Kind: core.MessageKindText, Text: answer, Ref: "chat",
	})
	if err != nil {
		return out, err
	}
	return append(out, ServerMsg{Type: "message", Message: &agent}), nil
}

func sessionRoot(requested string) (string, error) {
	if requested == "" {
		return "", errors.New("project root is required")
	}
	a, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", err
	}
	a, err = filepath.Abs(a)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(a)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", errors.New("project root is not a directory")
	}
	return a, nil
}

func (s *Service) trustedSessionRoot(requested string) (string, error) {
	// Small in-process unit services may omit ProjectRoot; production servers
	// always bind it at construction and therefore take the exact-match path.
	if s.deps.ProjectRoot == "" {
		return sessionRoot(requested)
	}
	configured, err := sessionRoot(s.deps.ProjectRoot)
	if err != nil {
		return "", fmt.Errorf("configured project root: %w", err)
	}
	provided, err := sessionRoot(requested)
	if err != nil {
		return "", err
	}
	if configured != provided {
		return "", errors.New("project root does not match the server-bound root")
	}
	return configured, nil
}

func (s *Service) sessionChat(ctx context.Context, c ClientMsg) ([]ServerMsg, error) {
	if s.deps.ChatProvider == nil {
		return nil, errors.New("chat model provider not configured; run stable config check")
	}
	root, err := s.trustedSessionRoot(c.ProjectRoot)
	if err != nil {
		return nil, err
	}
	if _, err = sessionlog.Replay(root, c.SessionID); err != nil {
		return nil, err
	}
	text := redactProviderCredential(c.Text, s.deps.ChatProvider, s.deps.Provider)
	_, err = sessionlog.Append(root, c.SessionID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Kind: "text", Text: text})
	if err != nil {
		return nil, err
	}
	if _, err = sessionlog.Append(root, c.SessionID, sessionlog.EventActivity, map[string]string{"action": "message"}); err != nil {
		return nil, err
	}
	replay, err := sessionlog.Replay(root, c.SessionID)
	if err != nil {
		return nil, err
	}
	goalContext := ""
	if c.Goal != "" {
		snap, e := s.deps.Store.GetGoalSnapshot(ctx, c.Goal)
		if e != nil {
			return nil, e
		}
		goalContext = fmt.Sprintf("ID: %s\nObjective: %s\nStatus: %s\nReason: %s\nEvidence: %s", snap.Goal.ID, snap.Goal.Objective, snap.Goal.Status, snap.Goal.Reason, evidenceSummary(snap.Evidence))
	}
	projection := sessionlog.Project(replay)
	manager, fellBack := sessioncontext.NewManager(s.deps.ContextWindowTokens, s.deps.ChatProvider)
	if fellBack {
		log.Printf("invalid context_window_tokens %d; using default %d", s.deps.ContextWindowTokens, sessioncontext.DefaultWindowTokens)
	}
	prepared, err := manager.Prepare(ctx, projection, prompt.SystemPrefix(goalContext))
	if err != nil {
		return nil, err
	}
	if prepared.Compacted && prepared.Boundary != nil {
		// The conversation service is the only session log writer: persist
		// the single boundary before sending, or fail the turn visibly.
		if _, err = sessionlog.Append(root, c.SessionID, sessionlog.EventBoundary, *prepared.Boundary); err != nil {
			return nil, fmt.Errorf("persist compaction boundary: %w", err)
		}
	}
	answer, err := s.deps.ChatProvider.GenerateChat(ctx, prepared.Messages)
	if err != nil {
		return []ServerMsg{{Type: "message", Message: nil}}, fmt.Errorf("chat response failed: %w", err)
	}
	_, err = sessionlog.Append(root, c.SessionID, sessionlog.EventMessage, sessionlog.Message{Role: "assistant", Kind: "text", Text: answer})
	if err != nil {
		return nil, err
	}
	replay, err = sessionlog.Replay(root, c.SessionID)
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "transcript", Transcript: &replay}}, nil
}

// say persists a user message for the focused goal and wakes the workflow, so
// the text is consumed at the next decision round.
func (s *Service) say(ctx context.Context, c ClientMsg, kind, eventKind string) ([]ServerMsg, error) {
	if err := s.deps.Store.EnsureGoal(ctx, c.Goal); err != nil {
		return nil, err
	}
	msg, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
		ID: goalrun.RandomID("msg"), GoalID: c.Goal, Role: core.MessageRoleUser, Kind: kind, Text: c.Text,
	})
	if err != nil {
		return nil, err
	}
	event := core.Event{ID: goalrun.RandomID("evt"), GoalID: c.Goal, Kind: eventKind,
		Payload: mustJSON(map[string]string{"message_id": msg.ID})}
	e, inserted, err := s.deps.Store.InsertEventIfAbsent(ctx, event)
	if err != nil {
		return nil, err
	}
	// Signal failure leaves the event queued; the worker re-signals it on startup.
	_ = s.signal(ctx, c.Goal, e.ID, inserted)
	return []ServerMsg{{Type: "message", Message: &msg}}, nil
}

// createGoal transpiles the request; the goal is only created after an
// explicit confirm of the proposed criteria.
func (s *Service) createGoal(ctx context.Context, c ClientMsg) ([]ServerMsg, error) {
	description := redactProviderCredential(c.Text, s.deps.ChatProvider, s.deps.Provider)
	var sessionPath string
	if c.SessionID != "" {
		root, e := s.trustedSessionRoot(c.ProjectRoot)
		if e != nil {
			return nil, e
		}
		if _, e = sessionlog.Replay(root, c.SessionID); e != nil {
			return nil, e
		}
		sessionPath = root
		if _, e = sessionlog.Append(root, c.SessionID, sessionlog.EventMessage, sessionlog.Message{Role: "user", Kind: "goal_request", Text: description}); e != nil {
			return nil, e
		}
	}
	if s.deps.Provider == nil {
		return nil, errors.New("model provider not configured; run stable config check")
	}
	res, err := decision.Transpile(ctx, s.deps.Provider, description)
	if err != nil {
		return nil, fmt.Errorf("criteria transpilation failed: %w", err)
	}
	if res.Status == "reject" {
		msg, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
			ID: goalrun.RandomID("msg"), Role: core.MessageRoleAgent, Kind: core.MessageKindText,
			Text: "无法为该描述定义可验证的验收标准：" + res.Reason,
		})
		if err != nil {
			return nil, err
		}
		if sessionPath != "" {
			if _, e := sessionlog.Append(sessionPath, c.SessionID, sessionlog.EventMessage, sessionlog.Message{Role: "assistant", Kind: "text", Text: msg.Text}); e != nil {
				return nil, e
			}
		}
		return []ServerMsg{{Type: "message", Message: &msg}}, nil
	}
	// Until the goal exists, proposal rows must stay session-level: they
	// reference the goals table.
	goalID := ""
	if s.deps.Store.EnsureGoal(ctx, c.Goal) == nil {
		goalID = c.Goal
	}
	proposal := core.CriteriaProposal{ID: goalrun.RandomID("prop"), GoalID: goalID,
		Status: core.ProposalPending, Criteria: res.Criteria, RawText: description, SessionID: c.SessionID}
	saved, err := s.deps.Store.InsertProposal(ctx, proposal)
	if err != nil {
		return nil, err
	}
	if c.SessionID != "" {
		if _, e := sessionlog.Append(sessionPath, c.SessionID, sessionlog.EventProposal, saved); e != nil {
			return nil, fmt.Errorf("proposal saved but session reference could not be written: %w", e)
		}
	}
	payload := mustJSON(saved)
	msg, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
		ID: goalrun.RandomID("msg"), GoalID: goalID, Role: core.MessageRoleSystem, Kind: core.MessageKindCriteriaProposal,
		Text: "验收标准提案（回复 /confirm " + saved.ID + " 生效）：", Payload: payload, Ref: saved.ID,
	})
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "message", Message: &msg}, {Type: "proposal", Proposal: &saved}}, nil
}

func (s *Service) confirm(ctx context.Context, c ClientMsg) ([]ServerMsg, error) {
	proposal, err := s.deps.Store.GetProposal(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	if c.SessionID != "" && proposal.SessionID != c.SessionID {
		return nil, errors.New("proposal does not belong to the active session")
	}
	if proposal.GoalID != "" {
		// Mid-run criteria change: one store transaction confirms the proposal,
		// bumps the criteria revision, marks the goal pending_reverification,
		// invalidates old evidence, and persists the wake event.
		conf, err := s.deps.Store.ConfirmGoalCriteria(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		msg, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
			ID: goalrun.RandomID("msg"), GoalID: conf.Goal.ID, Role: core.MessageRoleSystem, Kind: core.MessageKindCriteriaConfirm,
			Text: fmt.Sprintf("验收标准已更新（revision %d），目标待复核", conf.Goal.CriteriaRevision), Payload: mustJSON(conf.Proposal),
		})
		if err != nil {
			return nil, err
		}
		// The transaction already committed: a wake failure leaves the event
		// persisted for the worker to replay on its next start.
		if err = goalrun.WakeGoal(ctx, s.deps.Temporal, conf.Goal.ID, conf.Event.ID); err == nil {
			_ = s.deps.Store.SetEventStatus(ctx, conf.Event.ID, "signaled")
		}
		return []ServerMsg{{Type: "message", Message: &msg}, {Type: "goal_update", Goal: &conf.Goal}}, nil
	}
	// Creation flow: the confirmed proposal becomes a new goal. The objective is
	// the user's original description.
	if proposal, err = s.deps.Store.SetProposalStatus(ctx, c.ID, core.ProposalConfirmed); err != nil {
		return nil, err
	}
	id := c.Goal
	if id == "" {
		id = goalrun.RandomID("goal")
	}
	spec := goalrun.Spec{ID: id, Objective: proposal.RawText, Criteria: proposal.Criteria, SourceSessionID: proposal.SessionID}
	goal, err := goalrun.Create(ctx, s.deps.Store, s.deps.RunRoot, s.deps.Temporal, s.deps.ProjectRoot, spec)
	if err != nil {
		return nil, err
	}
	if err = s.deps.Store.AttachProposalGoal(ctx, proposal.ID, goal.ID); err != nil {
		return nil, err
	}
	if err = s.deps.Store.AttachMessagesToGoal(ctx, proposal.ID, goal.ID); err != nil {
		return nil, err
	}
	msg, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
		ID: goalrun.RandomID("msg"), GoalID: goal.ID, Role: core.MessageRoleSystem, Kind: core.MessageKindCriteriaConfirm,
		Text: fmt.Sprintf("目标 %s 已创建并开始运行", goal.ID), Payload: mustJSON(proposal),
	})
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "message", Message: &msg}, {Type: "goal_update", Goal: &goal}}, nil
}

func (s *Service) reject(ctx context.Context, c ClientMsg) ([]ServerMsg, error) {
	if c.SessionID != "" {
		proposal, err := s.deps.Store.GetProposal(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		if proposal.SessionID != c.SessionID {
			return nil, errors.New("proposal does not belong to the active session")
		}
	}
	proposal, err := s.deps.Store.SetProposalStatus(ctx, c.ID, core.ProposalRejected)
	if err != nil {
		return nil, err
	}
	msg, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
		ID: goalrun.RandomID("msg"), GoalID: proposal.GoalID, Role: core.MessageRoleSystem, Kind: core.MessageKindText,
		Text: fmt.Sprintf("提案 %s 已拒绝，可重新描述验收要求", proposal.ID),
	})
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "message", Message: &msg}}, nil
}

// planMode toggles the plan mode of the session and reports the new runtime
// state. The first toggle into plan mode creates the session plan file; every
// toggle records a plan_mode event with reason user_toggle.
func (s *Service) planMode(c ClientMsg) ([]ServerMsg, error) {
	next := sessionlog.PlanModePlan
	if s.PlanStateOf(c.SessionID).Mode == sessionlog.PlanModePlan {
		next = sessionlog.PlanModeDefault
	}
	state, err := s.SetPlanMode(c.SessionID, next, sessionlog.PlanModeReasonUserToggle)
	if err != nil {
		return nil, err
	}
	return []ServerMsg{{Type: "plan_state", PlanState: &state}}, nil
}

// planResolve answers the session's pending plan approval and reports the
// resulting plan state plus the ordinary user message that was inserted into
// the session context.
func (s *Service) planResolve(c ClientMsg) ([]ServerMsg, error) {
	feedback := redactProviderCredential(c.Text, s.deps.ChatProvider, s.deps.Provider)
	_, state, text, err := s.ResolvePlanApproval(c.SessionID, c.ApprovalChoice, feedback)
	if err != nil {
		return nil, err
	}
	msg := core.SessionMessage{ID: goalrun.RandomID("msg"), Role: core.MessageRoleUser, Kind: core.MessageKindText, Text: text, CreatedAt: time.Now().UTC()}
	return []ServerMsg{{Type: "plan_state", PlanState: &state}, {Type: "message", Message: &msg}}, nil
}

// signal wakes the workflow; on any Temporal failure the event stays queued
// and the worker re-signals it on startup.
func (s *Service) signal(ctx context.Context, goalID, eventID string, inserted bool) error {
	connection, err := client.Dial(client.Options{HostPort: s.deps.Temporal})
	if err != nil {
		return err
	}
	defer connection.Close()
	if err = connection.SignalWorkflow(ctx, goalID, "", core.GoalEventSignal, eventID); err != nil {
		return err
	}
	if inserted {
		return s.deps.Store.SetEventStatus(ctx, eventID, "signaled")
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}
