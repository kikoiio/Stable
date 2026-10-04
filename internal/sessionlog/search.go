package sessionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	// DefaultSearchLimit bounds the total number of returned hits.
	DefaultSearchLimit = 50
	// SearchSnippetRadius is the number of runes kept on each side of a
	// match inside a snippet.
	SearchSnippetRadius = 60
)

// SearchHit is one bounded match inside a session.
type SearchHit struct {
	Session SessionInfo
	Seq     uint64 // 0 for title hits
	Field   string // "title" or "event"
	Snippet string
}

// SearchError names a session whose log could not be scanned. Corrupt logs
// are reported, never silently skipped.
type SearchError struct {
	SessionID string
	Err       string
}

// SearchResult groups bounded hits with per-session scan failures.
type SearchResult struct {
	Hits    []SearchHit
	Corrupt []SearchError
}

// Search scans every session log for query, matching session titles and
// event text (messages, tool calls and results, boundary summaries). It
// streams one session at a time and never builds a secondary index, so
// results always reflect the append-only logs themselves.
func Search(root, query string, limit int) (SearchResult, error) {
	out := SearchResult{Hits: []SearchHit{}, Corrupt: []SearchError{}}
	query = strings.TrimSpace(query)
	if query == "" {
		return out, nil
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	dir, err := Prepare(root)
	if err != nil {
		return out, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, err
	}
	lower := strings.ToLower(query)
	for _, entry := range entries {
		if entry.IsDir() || len(entry.Name()) != 38 || entry.Name()[32:] != ".jsonl" {
			continue
		}
		id := entry.Name()[:32]
		if ValidateID(id) != nil {
			continue
		}
		replay, e := Replay(root, id)
		if e != nil {
			out.Corrupt = append(out.Corrupt, SearchError{SessionID: id, Err: e.Error()})
			continue
		}
		info := replay.Session
		if len(replay.Events) > 0 {
			info.UpdatedAt = replay.Events[len(replay.Events)-1].At
		}
		var hits []SearchHit
		if snippet, ok := matchSnippet(info.Title, lower); ok {
			hits = append(hits, SearchHit{Session: info, Field: "title", Snippet: snippet})
		}
		for _, event := range replay.Events {
			text := searchableText(event)
			if text == "" {
				continue
			}
			if snippet, ok := matchSnippet(text, lower); ok {
				hits = append(hits, SearchHit{Session: info, Seq: event.Seq, Field: "event", Snippet: snippet})
			}
		}
		out.Hits = append(out.Hits, hits...)
	}
	// Order by recent session activity first, then by event sequence, so the
	// picker shows the freshest sessions on top. List uses the same order.
	for i := 0; i < len(out.Hits); i++ {
		for j := i + 1; j < len(out.Hits); j++ {
			a, b := out.Hits[i], out.Hits[j]
			if b.Session.UpdatedAt.After(a.Session.UpdatedAt) ||
				(b.Session.UpdatedAt.Equal(a.Session.UpdatedAt) && b.Seq < a.Seq) {
				out.Hits[i], out.Hits[j] = out.Hits[j], out.Hits[i]
			}
		}
	}
	if len(out.Hits) > limit {
		out.Hits = out.Hits[:limit]
	}
	return out, nil
}

// searchableText extracts the user-visible text of one event.
func searchableText(e Event) string {
	switch e.Type {
	case EventMessage:
		var m Message
		if decodeData(e.Data, &m) == nil {
			return m.Text
		}
	case EventToolCall:
		var call ToolCall
		if decodeData(e.Data, &call) == nil {
			input, _ := json.Marshal(call.Input)
			return fmt.Sprintf("%s %s", call.Name, input)
		}
	case EventToolResult:
		var result ToolResult
		if decodeData(e.Data, &result) == nil {
			value, _ := json.Marshal(result.Result)
			return fmt.Sprintf("%s %s", result.Error, value)
		}
	case EventBoundary:
		var b Boundary
		if decodeData(e.Data, &b) == nil {
			return b.Summary
		}
	case EventQuestion:
		var q PendingQuestion
		if decodeData(e.Data, &q) == nil {
			return q.Prompt
		}
	case EventReply:
		var r QuestionReply
		if decodeData(e.Data, &r) == nil {
			return r.ReplyText
		}
	}
	return ""
}

// matchSnippet returns a bounded snippet around the first case-insensitive
// match of lower (an already lowercased query) inside text.
func matchSnippet(text, lower string) (string, bool) {
	index := strings.Index(strings.ToLower(text), lower)
	if index < 0 {
		return "", false
	}
	runes := []rune(text)
	at := len([]rune(text[:index]))
	start := at - SearchSnippetRadius
	if start < 0 {
		start = 0
	}
	end := at + len([]rune(lower)) + SearchSnippetRadius
	if end > len(runes) {
		end = len(runes)
	}
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet, true
}
