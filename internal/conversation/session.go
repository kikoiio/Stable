package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.temporal.io/sdk/client"
	"stable/internal/core"
	"stable/internal/decision"
	"stable/internal/goalrun"
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
			out = append(out, ServerMsg{Type: "goal_update", Goal: &goals[i]})
		}
		return out, nil
	case "say":
		return s.say(ctx, c, core.MessageKindText, core.EventKindUserMessage)
	case "reply":
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
	if s.deps.Provider == nil {
		return nil, errors.New("model provider not configured; run stable config check")
	}
	res, err := decision.Transpile(ctx, s.deps.Provider, c.Text)
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
		return []ServerMsg{{Type: "message", Message: &msg}}, nil
	}
	// Until the goal exists, proposal rows must stay session-level: they
	// reference the goals table.
	goalID := ""
	if s.deps.Store.EnsureGoal(ctx, c.Goal) == nil {
		goalID = c.Goal
	}
	proposal := core.CriteriaProposal{ID: goalrun.RandomID("prop"), GoalID: goalID,
		Status: core.ProposalPending, Criteria: res.Criteria, RawText: c.Text}
	saved, err := s.deps.Store.InsertProposal(ctx, proposal)
	if err != nil {
		return nil, err
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
	proposal, err := s.deps.Store.SetProposalStatus(ctx, c.ID, core.ProposalConfirmed)
	if err != nil {
		return nil, err
	}
	var out []ServerMsg
	if proposal.GoalID != "" {
		// Mid-run criteria change: new criteria take effect for the running goal.
		rev, err := s.deps.Store.UpdateGoalCriteria(ctx, proposal.GoalID, proposal.Criteria)
		if err != nil {
			return nil, err
		}
		msg, err := s.deps.Store.InsertMessage(ctx, core.SessionMessage{
			ID: goalrun.RandomID("msg"), GoalID: proposal.GoalID, Role: core.MessageRoleSystem, Kind: core.MessageKindCriteriaConfirm,
			Text: fmt.Sprintf("验收标准已更新（revision %d）", rev), Payload: mustJSON(proposal),
		})
		if err != nil {
			return nil, err
		}
		event := core.Event{ID: goalrun.RandomID("evt"), GoalID: proposal.GoalID, Kind: core.EventKindCriteriaUpdate,
			Payload: mustJSON(map[string]any{"proposal_id": proposal.ID, "criteria_revision": rev})}
		e, inserted, err := s.deps.Store.InsertEventIfAbsent(ctx, event)
		if err != nil {
			return nil, err
		}
		if err = s.signal(ctx, proposal.GoalID, e.ID, inserted); err != nil {
			return append(out, ServerMsg{Type: "message", Message: &msg}), nil
		}
		return append(out, ServerMsg{Type: "message", Message: &msg}), nil
	}
	// Creation flow: the confirmed proposal becomes a new goal. The objective is
	// the user's original description.
	id := c.Goal
	if id == "" {
		id = goalrun.RandomID("goal")
	}
	spec := goalrun.Spec{ID: id, Objective: proposal.RawText, Criteria: proposal.Criteria}
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
