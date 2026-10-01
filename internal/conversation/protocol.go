package conversation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"stable/internal/core"
	"stable/internal/sessionlog"
)

// ClientMsg is one line of JSON sent from a chat client to the session service.
type ClientMsg struct {
	Op          string `json:"op"`             // session_list | session_create | session_load | chat | say | create_goal | confirm | reject | reply | history | status
	Goal        string `json:"goal,omitempty"` // focused goal (required for say/reply)
	Text        string `json:"text,omitempty"` // natural-language content
	ID          string `json:"id,omitempty"`   // proposal ID for confirm/reject
	ProjectRoot string `json:"project_root,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
}

// ServerMsg is one line of JSON pushed from the session service to clients.
type ServerMsg struct {
	Type       string                   `json:"type"` // message | proposal | goal_update | error | done
	Message    *core.SessionMessage     `json:"message,omitempty"`
	Proposal   *core.CriteriaProposal   `json:"proposal,omitempty"`
	Goal       *core.Goal               `json:"goal,omitempty"`
	Error      string                   `json:"error,omitempty"`
	Session    *sessionlog.SessionInfo  `json:"session,omitempty"`
	Sessions   []sessionlog.SessionInfo `json:"sessions,omitempty"`
	Transcript *sessionlog.Transcript   `json:"transcript,omitempty"`
	Goals      []core.Goal              `json:"goals,omitempty"`
}

func validOp(op string) bool {
	switch op {
	case "session_list", "session_create", "session_load", "chat", "say", "create_goal", "confirm", "reject", "reply", "history", "status":
		return true
	}
	return false
}

func decodeClient(r io.Reader) (ClientMsg, error) {
	var m ClientMsg
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("invalid message: %w", err)
	}
	if err := validateClient(m); err != nil {
		return m, err
	}
	return m, nil
}

func validateClient(m ClientMsg) error {
	if !validOp(m.Op) {
		return fmt.Errorf("unknown op %q", m.Op)
	}
	switch m.Op {
	case "session_list", "session_create":
		if m.ProjectRoot == "" {
			return fmt.Errorf("op %s requires project_root", m.Op)
		}
	case "session_load":
		if m.ProjectRoot == "" || m.SessionID == "" {
			return fmt.Errorf("op session_load requires project_root and session_id")
		}
	case "chat":
		if m.Text == "" {
			return fmt.Errorf("op chat requires text")
		}
	case "say", "reply":
		if m.Goal == "" || m.Text == "" {
			return fmt.Errorf("op %s requires goal and text", m.Op)
		}
	case "create_goal":
		if m.Text == "" {
			return fmt.Errorf("op create_goal requires text")
		}
	case "confirm", "reject":
		if m.ID == "" {
			return fmt.Errorf("op %s requires proposal ID", m.Op)
		}
	}
	return nil
}

func encodeServer(w io.Writer, m ServerMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	out := bufio.NewWriter(w)
	if _, err = out.Write(append(b, '\n')); err != nil {
		return err
	}
	return out.Flush()
}
