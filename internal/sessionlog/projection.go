package sessionlog

// ItemKind describes one projected transcript entry.
type ItemKind string

const (
	ItemMessage        ItemKind = "message"
	ItemToolCall       ItemKind = "tool_call"
	ItemToolResult     ItemKind = "tool_result"
	ItemSummary        ItemKind = "summary"
	ItemRunTerminal    ItemKind = "run_terminal"
	ItemSnapshot       ItemKind = "snapshot"
	ItemRewind         ItemKind = "rewind"
	ItemQuestion       ItemKind = "question"
	ItemReply          ItemKind = "reply"
	ItemPlanMode       ItemKind = "plan_mode"
	ItemPlanApproval   ItemKind = "plan_approval"
	ItemTodo           ItemKind = "todo_update"
	ItemSkillInventory ItemKind = "skill_inventory"
	ItemSkillDelta     ItemKind = "skill_delta"
	ItemSkillInvoked   ItemKind = "skill_invoked"
	ItemHookFired      ItemKind = "hook_fired"
	ItemHookReload     ItemKind = "hook_reload"
)

// Item is one entry of the unified session projection. The pointer field
// matching Kind is set; the rest are nil.
type Item struct {
	Seq            uint64
	Kind           ItemKind
	Message        *Message
	Call           *ToolCall
	Result         *ToolResult
	Summary        *Boundary
	Run            *RunEvent
	Snapshot       *SnapshotRef
	Rewind         *RewindRecord
	Question       *PendingQuestion
	Reply          *QuestionReply
	PlanMode       *PlanMode
	Approval       *PlanApprovalRecord
	Todo           *TodoUpdate
	SkillInventory *SkillInventory
	SkillDelta     *SkillDelta
	SkillInvoked   *SkillInvoked
	HookFired      *HookFired
	HookReload     *HookReload
	// Matched reports whether a tool call has its result inside the same
	// projected tail. An unmatched call stays visible for audit but must
	// not become the recoverable tail of a normal conversation.
	Matched bool
}

// Projection is the single view both the user-facing transcript and the
// agent context are built from, so the two never diverge.
type Projection struct {
	Session SessionInfo
	Items   []Item
}

// Project folds a replayed transcript into the unified projection. The
// latest compaction boundary replaces the events it covers with a summary
// item; events after the covered range stay in place and in order. Source
// events are never modified, so the raw log remains auditable.
func Project(t Transcript) Projection {
	out := Projection{Session: t.Session, Items: []Item{}}
	covered, boundarySeq := coveredSeqs(t.Events)
	matched := map[string]bool{}
	for _, e := range t.Events {
		if e.Seq == boundarySeq {
			continue
		}
		if covered[e.Seq] {
			continue
		}
		if e.Type == EventToolResult {
			var result ToolResult
			if decodeData(e.Data, &result) == nil {
				matched[result.CallID] = true
			}
		}
	}
	for _, e := range t.Events {
		if covered[e.Seq] {
			continue
		}
		switch e.Type {
		case EventMessage:
			var m Message
			if decodeData(e.Data, &m) == nil {
				msg := m
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemMessage, Message: &msg, Matched: true})
			}
		case EventToolCall:
			var call ToolCall
			if decodeData(e.Data, &call) == nil {
				c := call
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemToolCall, Call: &c, Matched: matched[call.CallID]})
			}
		case EventToolResult:
			var result ToolResult
			if decodeData(e.Data, &result) == nil {
				r := result
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemToolResult, Result: &r, Matched: true})
			}
		case EventBoundary:
			if e.Seq != boundarySeq {
				// Older boundaries are superseded by the latest one and
				// stay only in the raw log for audit.
				continue
			}
			var b Boundary
			if decodeData(e.Data, &b) == nil {
				summary := b
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemSummary, Summary: &summary, Matched: true})
			}
		case EventRunEvent:
			var run RunEvent
			if decodeData(e.Data, &run) == nil && run.Kind == "terminal" {
				r := run
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemRunTerminal, Run: &r, Matched: true})
			}
		case EventSnapshot:
			var snap SnapshotRef
			if decodeData(e.Data, &snap) == nil {
				s := snap
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemSnapshot, Snapshot: &s, Matched: true})
			}
		case EventRewind:
			var rewind RewindRecord
			if decodeData(e.Data, &rewind) == nil {
				r := rewind
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemRewind, Rewind: &r, Matched: true})
			}
		case EventQuestion:
			var question PendingQuestion
			if decodeData(e.Data, &question) == nil {
				q := question
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemQuestion, Question: &q, Matched: true})
			}
		case EventReply:
			var reply QuestionReply
			if decodeData(e.Data, &reply) == nil {
				r := reply
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemReply, Reply: &r, Matched: true})
			}
		case EventPlanMode:
			var mode PlanMode
			if decodeData(e.Data, &mode) == nil {
				m := mode
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemPlanMode, PlanMode: &m, Matched: true})
			}
		case EventPlanApproval:
			var record PlanApprovalRecord
			if decodeData(e.Data, &record) == nil {
				r := record
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemPlanApproval, Approval: &r, Matched: true})
			}
		case EventTodo:
			var update TodoUpdate
			if decodeData(e.Data, &update) == nil {
				u := update
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemTodo, Todo: &u, Matched: true})
			}
		case EventSkillInventory:
			var inv SkillInventory
			if decodeData(e.Data, &inv) == nil {
				s := inv
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemSkillInventory, SkillInventory: &s, Matched: true})
			}
		case EventSkillDelta:
			var delta SkillDelta
			if decodeData(e.Data, &delta) == nil {
				d := delta
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemSkillDelta, SkillDelta: &d, Matched: true})
			}
		case EventSkillInvoked:
			var invoked SkillInvoked
			if decodeData(e.Data, &invoked) == nil {
				i := invoked
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemSkillInvoked, SkillInvoked: &i, Matched: true})
			}
		case EventHookFired:
			var fired HookFired
			if decodeData(e.Data, &fired) == nil {
				h := fired
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemHookFired, HookFired: &h, Matched: true})
			}
		case EventHookReload:
			var reload HookReload
			if decodeData(e.Data, &reload) == nil {
				h := reload
				out.Items = append(out.Items, Item{Seq: e.Seq, Kind: ItemHookReload, HookReload: &h, Matched: true})
			}
		}
	}
	return out
}

// coveredSeqs returns the session sequence numbers replaced by the latest
// effective compaction boundary, plus that boundary's own sequence number.
// boundarySeq is zero when the log has no boundary.
func coveredSeqs(events []Event) (map[uint64]bool, uint64) {
	covered := map[uint64]bool{}
	latest := -1
	var boundary Boundary
	for i, e := range events {
		if e.Type != EventBoundary {
			continue
		}
		var b Boundary
		if decodeData(e.Data, &b) != nil {
			continue
		}
		latest = i
		boundary = b
	}
	if latest < 0 {
		return covered, 0
	}
	switch {
	case boundary.Scope == "":
		// Legacy boundary written by the old prompt.Compact: it carries no
		// scope, and the retained tail was re-appended after the boundary
		// with fresh sequence numbers. Covering only FromSeq..ToSeq would
		// leave the original tail visible next to its re-appended copies,
		// so everything before the boundary is replaced by the summary.
		for _, e := range events {
			if e.Seq < events[latest].Seq {
				covered[e.Seq] = true
			}
		}
	case boundary.EffectiveScope() == BoundaryScopeRun:
		for _, e := range events {
			if e.Type != EventRunEvent {
				continue
			}
			var run RunEvent
			if decodeData(e.Data, &run) != nil || run.RunID != boundary.RunID {
				continue
			}
			if run.RunSeq >= boundary.FromSeq && run.RunSeq <= boundary.ToSeq {
				covered[e.Seq] = true
			}
		}
	default:
		for seq := boundary.FromSeq; seq <= boundary.ToSeq; seq++ {
			covered[seq] = true
		}
	}
	return covered, events[latest].Seq
}

// CoveredSeqs exposes the coverage of the latest effective boundary so
// consumers outside this package (run message rebuilds) apply exactly the
// same substitution as the unified projection.
func CoveredSeqs(events []Event) (map[uint64]bool, uint64) {
	return coveredSeqs(events)
}
