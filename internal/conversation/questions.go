package conversation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"stable/internal/sessionlog"
)

// listQuestions returns every question recorded in the session, with the
// status advanced to replied when a matching reply event exists. Unknown
// sessions fail closed through replay.
func (s *Service) listQuestions(c ClientMsg) ([]sessionlog.PendingQuestion, error) {
	root, err := s.boundProjectRoot()
	if err != nil {
		return nil, err
	}
	replay, err := sessionlog.Replay(root, c.SessionID)
	if err != nil {
		return nil, err
	}
	answered := map[string]bool{}
	for _, e := range replay.Events {
		if e.Type != sessionlog.EventReply {
			continue
		}
		var r sessionlog.QuestionReply
		if decodeSessionData(e.Data, &r) == nil {
			answered[r.QuestionID] = true
		}
	}
	questions := []sessionlog.PendingQuestion{}
	for _, e := range replay.Events {
		if e.Type != sessionlog.EventQuestion {
			continue
		}
		var q sessionlog.PendingQuestion
		if decodeSessionData(e.Data, &q) != nil {
			continue
		}
		if answered[q.QuestionID] {
			q.Status = sessionlog.QuestionReplied
		}
		questions = append(questions, q)
	}
	return questions, nil
}

// replyQuestion atomically answers one pending question in the same session.
// The session log validates ownership and single-answer semantics at append
// time, so a duplicate, completed, or unknown question is always refused,
// never silently accepted.
func (s *Service) replyQuestion(ctx context.Context, c ClientMsg) ([]ServerMsg, error) {
	root, err := s.boundProjectRoot()
	if err != nil {
		return nil, err
	}
	replay, err := sessionlog.Replay(root, c.SessionID)
	if err != nil {
		return nil, err
	}
	var question *sessionlog.PendingQuestion
	answered := map[string]bool{}
	for _, e := range replay.Events {
		switch e.Type {
		case sessionlog.EventQuestion:
			var q sessionlog.PendingQuestion
			if decodeSessionData(e.Data, &q) == nil && q.QuestionID == c.QuestionID {
				copy := q
				question = &copy
			}
		case sessionlog.EventReply:
			var r sessionlog.QuestionReply
			if decodeSessionData(e.Data, &r) == nil {
				answered[r.QuestionID] = true
			}
		}
	}
	if question == nil {
		return nil, fmt.Errorf("question %s does not exist in this session", c.QuestionID)
	}
	if question.SessionID != c.SessionID {
		return nil, errors.New("question belongs to a different session")
	}
	if answered[c.QuestionID] {
		return nil, fmt.Errorf("question %s is already answered", c.QuestionID)
	}
	reply := sessionlog.QuestionReply{
		QuestionID: c.QuestionID,
		ReplyText:  redactProviderCredential(c.Text, s.deps.ChatProvider, s.deps.Provider),
		RepliedAt:  time.Now().UTC(),
	}
	s.eventMu.Lock()
	_, err = sessionlog.Append(root, c.SessionID, sessionlog.EventReply, reply)
	s.eventMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("record reply: %w", err)
	}
	if s.askWaiterCount(c.SessionID) == 0 {
		// No run is waiting for this answer: the question outlived its run
		// (cancel, crash, completion). Queue the reply as an ordinary user
		// message so the next run picks it up from the conversation history,
		// matching the /say semantics.
		message := sessionlog.Message{Role: "user", Kind: "text", Text: reply.ReplyText}
		s.eventMu.Lock()
		_, err = sessionlog.Append(root, c.SessionID, sessionlog.EventMessage, message)
		s.eventMu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("queue reply as message: %w", err)
		}
	}
	return []ServerMsg{{Type: "reply", Reply: &reply}}, nil
}
