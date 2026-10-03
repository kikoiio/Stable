package sessionlog

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

var locks sync.Map

func lockFor(path string) *sync.Mutex {
	v, _ := locks.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func Append(root, id, typ string, data any) (Event, error) {
	path, err := SessionPath(root, id)
	if err != nil {
		return Event{}, err
	}
	if typ == "" {
		return Event{}, errors.New("event type is required")
	}
	switch typ {
	case EventSessionCreated, EventActivity, EventMessage, EventProposal, EventToolCall, EventToolResult, EventBoundary, EventRunStarted, EventRunEvent:
	default:
		return Event{}, fmt.Errorf("unknown event type %q", typ)
	}
	if _, err = Prepare(root); err != nil {
		return Event{}, err
	}
	mu := lockFor(path)
	mu.Lock()
	defer mu.Unlock()
	var seq uint64
	_, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return Event{}, statErr
	}
	if os.IsNotExist(statErr) {
		if typ != EventSessionCreated {
			return Event{}, errors.New("the first session event must be session_created")
		}
		var info SessionInfo
		raw, _ := json.Marshal(data)
		if json.Unmarshal(raw, &info) != nil || info.ID != id || info.CreatedAt.IsZero() {
			return Event{}, errors.New("session creation metadata does not match file identity")
		}
	} else {
		if typ == EventSessionCreated {
			return Event{}, errors.New("session already has a creation event")
		}
		replay, e := replayFile(path, id)
		if e != nil {
			return Event{}, e
		}
		if len(replay.Events) > 0 {
			seq = replay.Events[len(replay.Events)-1].Seq
		}
		if typ == EventRunStarted || typ == EventRunEvent {
			if err := validateRunAppend(id, typ, data, replay.Events); err != nil {
				return Event{}, err
			}
		}
		if typ == EventToolResult {
			var result ToolResult
			raw, _ := json.Marshal(data)
			if json.Unmarshal(raw, &result) != nil || result.CallID == "" {
				return Event{}, errors.New("tool result requires call_id")
			}
			found := false
			for _, event := range replay.Events {
				if event.Type == EventToolCall {
					var call ToolCall
					b, _ := json.Marshal(event.Data)
					_ = json.Unmarshal(b, &call)
					if call.CallID == result.CallID {
						found = true
					}
				}
				if event.Type == EventToolResult {
					var prior ToolResult
					b, _ := json.Marshal(event.Data)
					_ = json.Unmarshal(b, &prior)
					if prior.CallID == result.CallID {
						found = false
					}
				}
			}
			if !found {
				return Event{}, errors.New("tool result has no unmatched call")
			}
		}
		if typ == EventToolCall {
			var call ToolCall
			raw, _ := json.Marshal(data)
			if json.Unmarshal(raw, &call) != nil || call.CallID == "" {
				return Event{}, errors.New("tool call requires call_id")
			}
			outstanding := false
			for _, event := range replay.Events {
				if event.Type == EventToolCall {
					var old ToolCall
					b, _ := json.Marshal(event.Data)
					_ = json.Unmarshal(b, &old)
					if old.CallID == call.CallID {
						outstanding = true
					}
				}
				if event.Type == EventToolResult {
					var result ToolResult
					b, _ := json.Marshal(event.Data)
					_ = json.Unmarshal(b, &result)
					if result.CallID == call.CallID {
						outstanding = false
					}
				}
			}
			if outstanding {
				return Event{}, errors.New("duplicate pending tool call id")
			}
		}
	}
	e := Event{SchemaVersion: SchemaVersion, SessionID: id, Seq: seq + 1, At: time.Now().UTC(), Type: typ, Data: data}
	b, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return Event{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return Event{}, err
	}
	oldSize := info.Size()
	n, err := f.Write(b)
	if err != nil {
		_ = f.Truncate(oldSize)
		return Event{}, err
	}
	if n != len(b) {
		_ = f.Truncate(oldSize)
		return Event{}, io.ErrShortWrite
	}
	if err = f.Sync(); err != nil {
		return Event{}, err
	}
	return e, nil
}

func Replay(root, id string) (Transcript, error) {
	path, err := SessionPath(root, id)
	if err != nil {
		return Transcript{}, err
	}
	return replayFile(path, id)
}

func ReplayAfter(root, id string, afterSeq uint64) (Transcript, error) {
	transcript, err := Replay(root, id)
	if err != nil {
		return Transcript{}, err
	}
	filtered := transcript.Events[:0]
	for _, event := range transcript.Events {
		if event.Seq > afterSeq {
			filtered = append(filtered, event)
		}
	}
	transcript.Events = filtered
	return transcript, nil
}

func validateRunAppend(sessionID, typ string, data any, events []Event) error {
	starts := map[string]RunStarted{}
	lastSeq := map[string]uint64{}
	terminal := map[string]bool{}
	for _, event := range events {
		switch event.Type {
		case EventRunStarted:
			var started RunStarted
			if decodeData(event.Data, &started) == nil {
				starts[started.RunID] = started
			}
		case EventRunEvent:
			var runEvent RunEvent
			if decodeData(event.Data, &runEvent) == nil {
				lastSeq[runEvent.RunID] = runEvent.RunSeq
				if runEvent.Kind == "terminal" {
					terminal[runEvent.RunID] = true
				}
			}
		}
	}
	if typ == EventRunStarted {
		var started RunStarted
		if err := decodeData(data, &started); err != nil || started.RunID == "" || started.Intent == "" {
			return errors.New("run_started requires run_id and intent")
		}
		if starts[started.RunID].RunID != "" {
			return errors.New("run ID already exists in session")
		}
		switch started.WorkKind {
		case "session":
			if started.GoalID != "" || started.WorkItemID != "" {
				return errors.New("session run cannot include goal IDs")
			}
		case "goal":
			if started.GoalID == "" || started.WorkItemID == "" {
				return errors.New("goal run requires goal and work item IDs")
			}
		default:
			return errors.New("run has invalid work kind")
		}
		return nil
	}
	var runEvent RunEvent
	if err := decodeData(data, &runEvent); err != nil || runEvent.ID == "" || runEvent.RunID == "" || runEvent.RunSeq == 0 || runEvent.Kind == "" || runEvent.At.IsZero() {
		return errors.New("run_event requires event ID, run ID, sequence, kind, and time")
	}
	if runEvent.SessionID != sessionID {
		return errors.New("run_event session does not match log")
	}
	if starts[runEvent.RunID].RunID == "" {
		return errors.New("run_event has no run_started event")
	}
	if terminal[runEvent.RunID] {
		return errors.New("run_event follows terminal event")
	}
	if runEvent.RunSeq != lastSeq[runEvent.RunID]+1 {
		return errors.New("run_event sequence is not monotonic")
	}
	if runEvent.Kind == "terminal" {
		var result struct {
			Status string `json:"status"`
		}
		if err := decodeData(runEvent.Payload, &result); err != nil {
			return errors.New("terminal event has invalid payload")
		}
		switch result.Status {
		case "completed", "cancelled", "failed", "awaiting_tools":
		default:
			return errors.New("terminal event has invalid status")
		}
	}
	for _, event := range events {
		if event.Type != EventRunEvent {
			continue
		}
		var previous RunEvent
		if decodeData(event.Data, &previous) == nil && previous.ID == runEvent.ID {
			return errors.New("duplicate run event ID")
		}
	}
	return nil
}

func decodeData(data any, target any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func replayFile(path, id string) (Transcript, error) {
	if err := checkTrailingNewline(path); err != nil {
		return Transcript{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Transcript{}, err
	}
	defer f.Close()
	out := Transcript{Events: []Event{}}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 8<<20)
	var expected uint64 = 1
	openCalls := map[string]bool{}
	runStarts := map[string]RunStarted{}
	runSeq := map[string]uint64{}
	runTerminal := map[string]bool{}
	runEventIDs := map[string]bool{}
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		var e Event
		if err = json.Unmarshal(line, &e); err != nil {
			return out, fmt.Errorf("session log corrupt at seq %d: %w", expected, err)
		}
		if e.SchemaVersion != SchemaVersion || e.SessionID != id || e.Seq != expected || e.At.IsZero() || e.At.Location() != time.UTC {
			return out, fmt.Errorf("session log invalid envelope at seq %d", expected)
		}
		switch e.Type {
		case EventSessionCreated, EventActivity, EventMessage, EventProposal, EventToolCall, EventToolResult, EventBoundary, EventRunStarted, EventRunEvent:
		default:
			return out, fmt.Errorf("session log has unknown event type %q at seq %d", e.Type, e.Seq)
		}
		switch e.Type {
		case EventToolCall:
			var call ToolCall
			raw, _ := json.Marshal(e.Data)
			if json.Unmarshal(raw, &call) != nil || call.CallID == "" || openCalls[call.CallID] {
				return out, fmt.Errorf("session log invalid tool call at seq %d", e.Seq)
			}
			openCalls[call.CallID] = true
		case EventToolResult:
			var result ToolResult
			raw, _ := json.Marshal(e.Data)
			if json.Unmarshal(raw, &result) != nil || result.CallID == "" || !openCalls[result.CallID] {
				return out, fmt.Errorf("session log has unmatched tool result at seq %d", e.Seq)
			}
			openCalls[result.CallID] = false
		case EventBoundary:
			var boundary Boundary
			raw, _ := json.Marshal(e.Data)
			if json.Unmarshal(raw, &boundary) != nil || boundary.FromSeq == 0 || boundary.ToSeq < boundary.FromSeq || boundary.Summary == "" {
				return out, fmt.Errorf("session log has invalid compaction boundary at seq %d", e.Seq)
			}
		case EventRunStarted:
			var started RunStarted
			if decodeData(e.Data, &started) != nil || started.RunID == "" || started.Intent == "" || runStarts[started.RunID].RunID != "" {
				return out, fmt.Errorf("session log has invalid run_started at seq %d", e.Seq)
			}
			switch started.WorkKind {
			case "session":
				if started.GoalID != "" || started.WorkItemID != "" {
					return out, fmt.Errorf("session log has invalid session run at seq %d", e.Seq)
				}
			case "goal":
				if started.GoalID == "" || started.WorkItemID == "" {
					return out, fmt.Errorf("session log has invalid goal run at seq %d", e.Seq)
				}
			default:
				return out, fmt.Errorf("session log has invalid run kind at seq %d", e.Seq)
			}
			runStarts[started.RunID] = started
		case EventRunEvent:
			var runEvent RunEvent
			if decodeData(e.Data, &runEvent) != nil || runEvent.ID == "" || runEvent.RunID == "" || runEvent.SessionID != id || runEvent.RunSeq == 0 || runEvent.Kind == "" || runEvent.At.IsZero() {
				return out, fmt.Errorf("session log has invalid run_event at seq %d", e.Seq)
			}
			if runStarts[runEvent.RunID].RunID == "" || runTerminal[runEvent.RunID] || runEvent.RunSeq != runSeq[runEvent.RunID]+1 || runEventIDs[runEvent.ID] {
				return out, fmt.Errorf("session log has invalid run event order at seq %d", e.Seq)
			}
			if runEvent.Kind == "terminal" {
				var result struct {
					Status string `json:"status"`
				}
				if decodeData(runEvent.Payload, &result) != nil {
					return out, fmt.Errorf("session log has invalid terminal at seq %d", e.Seq)
				}
				switch result.Status {
				case "completed", "cancelled", "failed", "awaiting_tools":
				default:
					return out, fmt.Errorf("session log has invalid terminal status at seq %d", e.Seq)
				}
				runTerminal[runEvent.RunID] = true
			}
			runSeq[runEvent.RunID] = runEvent.RunSeq
			runEventIDs[runEvent.ID] = true
		}
		out.Events = append(out.Events, e)
		expected++
	}
	if err = s.Err(); err != nil {
		return out, err
	}
	if len(out.Events) == 0 {
		return out, errors.New("session log is empty")
	}
	if out.Events[0].Type != EventSessionCreated {
		return out, errors.New("session log is missing creation event")
	}
	_ = json.Unmarshal(marshalData(out.Events[0].Data), &out.Session)
	if out.Session.ID != id || out.Session.CreatedAt.IsZero() {
		return out, errors.New("session creation metadata does not match file identity")
	}
	return out, nil
}

func marshalData(v any) []byte { b, _ := json.Marshal(v); return b }
func checkTrailingNewline(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("session log is empty")
	}
	last := make([]byte, 1)
	if _, err = f.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] != '\n' {
		return errors.New("session log has incomplete final line")
	}
	return nil
}
