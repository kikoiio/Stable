package conversation

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"stable/internal/agent"
	"stable/internal/memory"
	"stable/internal/sessionlog"
)

const memoryContextLimit = 128 << 10

func memoryQuery(request agent.ExecutionRequest) string {
	var parts []string
	if request.Intent != "" {
		parts = append(parts, request.Intent)
	}
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == "user" && strings.TrimSpace(request.Messages[i].Content) != "" {
			parts = append(parts, request.Messages[i].Content)
			break
		}
	}
	return strings.Join(parts, "\n")
}

func renderMemoryContext(memoryContext memory.RunMemoryContext) string {
	var b strings.Builder
	appendBoundedSection(&b, "Stable instructions", memoryContext.InstructionText)
	appendBoundedSection(&b, "User memory index", memoryContext.UserIndex)
	appendBoundedSection(&b, "Project memory index", memoryContext.ProjectIndex)
	for _, entry := range memoryContext.Selected {
		title := "Selected " + string(entry.Scope) + " memory: " + entry.Name
		if !entry.UpdatedAt.IsZero() {
			title += " (updated " + entry.UpdatedAt.UTC().Format(time.RFC3339) + ")"
			if time.Since(entry.UpdatedAt) > 24*time.Hour {
				title += " [older than 24 hours]"
			}
		}
		appendBoundedSection(&b, title, entry.Body)
	}
	if len(memoryContext.Issues) > 0 {
		var issues []string
		for _, issue := range memoryContext.Issues {
			issues = append(issues, issue.Path+": "+issue.Reason)
		}
		appendBoundedSection(&b, "Memory context warnings", strings.Join(issues, "\n"))
	}
	return strings.TrimSpace(b.String())
}

func appendBoundedSection(b *strings.Builder, title, content string) {
	if content == "" || b.Len() >= memoryContextLimit {
		return
	}
	remaining := memoryContextLimit - b.Len()
	prefix := "\n<" + title + ">\n"
	if len(prefix) >= remaining {
		return
	}
	b.WriteString(prefix)
	suffix := "\n</" + title + ">\n"
	remaining = memoryContextLimit - b.Len() - len(suffix)
	if remaining <= 0 {
		return
	}
	if len(content) > remaining {
		content = content[:remaining]
		for !utf8.ValidString(content) {
			content = content[:len(content)-1]
		}
	}
	b.WriteString(content)
	b.WriteString(suffix)
}

func (s *Service) completeMemoryRun(request agent.ExecutionRequest, cursor uint64) {
	replay, err := sessionlog.Replay(s.deps.ProjectRoot, request.Work.SessionID)
	if err != nil {
		return
	}
	completion := memory.RunCompletion{
		ProjectRoot: s.memory.root,
		SessionID:   request.Work.SessionID,
		RunID:       request.RunID,
		WorkKind:    string(request.Work.Kind),
		CompletedAt: time.Now().UTC(),
	}
	for _, event := range replay.Events {
		if event.Seq > completion.ThroughSeq {
			completion.ThroughSeq = event.Seq
		}
		if event.Type == sessionlog.EventMemoryAction {
			var action sessionlog.MemoryActionRecord
			if decodeSessionData(event.Data, &action) == nil && action.RunID == request.RunID && action.Operation == "save" && action.State == "success" {
				completion.MainAgentWroteMemory = true
			}
		}
		if event.Seq <= cursor || event.Type != sessionlog.EventMessage {
			continue
		}
		var message sessionlog.Message
		if decodeSessionData(event.Data, &message) != nil || message.Role != "user" || strings.TrimSpace(message.Text) == "" {
			continue
		}
		switch request.Work.Kind {
		case agent.WorkSession:
			if message.Kind == "text" {
				completion.Messages = append(completion.Messages, memory.ConversationText{Seq: event.Seq, Kind: "session_text", Text: message.Text})
			}
		case agent.WorkGoal:
			if message.Kind == "goal_say" || message.Kind == "goal_reply" {
				completion.Messages = append(completion.Messages, memory.ConversationText{Seq: event.Seq, Kind: "goal_reply", Text: message.Text})
			}
		}
	}
	if completion.ThroughSeq == 0 {
		return
	}
	s.memory.manager.CompleteRun(context.Background(), completion)
}
