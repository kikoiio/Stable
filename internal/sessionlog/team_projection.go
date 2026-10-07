package sessionlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"

	"stable/internal/teams"
)

type teamScan struct {
	TeamProjection
	runs          map[string]RunStarted
	runStatus     map[string]string
	runDelegation map[string]AgentTaskDelegation
	ids           map[string]bool
	tokens        map[string]bool
	turnTeam      map[string]string
	runSeq        map[string]uint64
	runEventIDs   map[string]bool
}

func newTeamScan() *teamScan {
	return &teamScan{TeamProjection: TeamProjection{Teams: map[string]teams.Team{}, Members: map[string]teams.Member{}, Turns: map[string]TurnFact{}, Messages: map[string]teams.Message{}, Tasks: map[string]teams.Task{}, Requests: map[string]teams.Request{}, LastSeq: map[string]uint64{}, TurnTerminalSeq: map[string]uint64{}}, runs: map[string]RunStarted{}, runStatus: map[string]string{}, runDelegation: map[string]AgentTaskDelegation{}, ids: map[string]bool{}, tokens: map[string]bool{}, turnTeam: map[string]string{}, runSeq: map[string]uint64{}, runEventIDs: map[string]bool{}}
}

func decodeTeamData(data any, target any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(raw) > 64<<10 {
		return errors.New("team event exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("team event contains trailing data")
	}
	return nil
}

func ProjectTeams(t Transcript) (TeamProjection, error) {
	st := newTeamScan()
	for _, e := range t.Events {
		if e.SessionID != "" && e.SessionID != t.Session.ID {
			return TeamProjection{}, errors.New("team fact belongs to another session")
		}
		if err := st.observe(t.Session.ID, e); err != nil {
			return TeamProjection{}, fmt.Errorf("team fact at seq %d: %w", e.Seq, err)
		}
	}
	return st.TeamProjection, nil
}
func scanTeams(sessionID string, events []Event) (*teamScan, error) {
	st := newTeamScan()
	for _, e := range events {
		if err := st.observe(sessionID, e); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (st *teamScan) checkActor(e TeamEvent, team teams.Team) error {
	if e.ActorID == "service" {
		switch e.Kind {
		case TeamMemberState, TeamTurnIntent, TeamTurnAccepted, TeamTurnAborted, TeamTurnTerminal, TeamRequestExpired, TeamRequestResponded, TeamClosing, TeamClosed, TeamMessageHandoff, TeamLeadHandoff:
			return nil
		default:
			return errors.New("service actor cannot perform this team operation")
		}
	}
	if e.ActorID == teams.Lead {
		if e.ActorRunID != "" {
			run, ok := st.runs[e.ActorRunID]
			if !ok || run.TeamID != "" || !teamWorkMatches(team.Scope, run) {
				return errors.New("team lead run does not own this work")
			}
		}
		return nil
	}
	member, ok := st.Members[e.ActorID]
	if !ok || member.TeamID != e.TeamID || member.Status.IsTerminal() {
		return errors.New("team actor is not an active member")
	}
	run, ok := st.runs[e.ActorRunID]
	if !ok || run.TeamID != e.TeamID || run.TeamMemberID != e.ActorID || st.runStatus[run.RunID] != "" {
		return errors.New("member actor requires its registered active turn run")
	}
	switch e.Kind {
	case TeamMessageSent, TeamTaskCreated, TeamTaskUpdated, TeamRequestCreated, TeamRequestResponded:
		return nil
	default:
		return errors.New("member actor cannot perform lead operations")
	}
}
func teamWorkMatches(scope teams.Scope, run RunStarted) bool {
	return scope.WorkKind == run.WorkKind && scope.GoalID == run.GoalID && scope.WorkItemID == run.WorkItemID
}
func validTeamIDs(ids ...string) bool {
	for _, id := range ids {
		if teams.ValidateID(id) != nil {
			return false
		}
	}
	return true
}
func sameTurnIdentity(a, b TurnFact) bool {
	a.Status, b.Status = "", ""
	a.Elapsed, b.Elapsed = 0, 0
	a.Summary, b.Summary = "", ""
	a.Error, b.Error = "", ""
	return reflect.DeepEqual(a, b)
}
func turnTerminal(status string) bool { return agentTaskTerminal(status) }

func (st *teamScan) checkEvent(sessionID string, e TeamEvent, at time.Time) error {
	if !validTeamIDs(e.ID, e.TeamID) || e.SessionID != sessionID || e.Revision == 0 || e.ActorID == "" {
		return errors.New("team event identity/session/revision/actor is invalid")
	}
	if st.ids[e.ID] {
		return errors.New("duplicate team event ID")
	}
	if teams.ValidateText(e.OperationID, 256, false) != nil || teams.ValidateText(e.Token, 256, false) != nil || teams.ValidateText(e.ArgsDigest, 256, false) != nil {
		return errors.New("team operation identity exceeds bounds")
	}
	if e.Token != "" {
		if e.ArgsDigest == "" {
			return errors.New("team token requires args digest")
		}
		if st.tokens[e.TeamID+"/"+e.ActorID+"/"+e.Token] {
			return errors.New("duplicate team mutation token")
		}
	}
	count := 0
	for _, present := range []bool{e.Team != nil, e.Member != nil, e.Message != nil, e.Task != nil, e.Request != nil, e.Turn != nil, e.Handoff != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return errors.New("team event must contain exactly one typed payload")
	}
	team, exists := st.Teams[e.TeamID]
	if e.Kind == TeamCreated {
		if exists || e.Team == nil || e.Revision != 1 || e.ActorID != teams.Lead {
			return errors.New("team must start with a unique lead creation fact")
		}
		t := e.Team
		name, err := teams.NormalizeName(t.Name)
		if err != nil || name != t.Name || t.ID != e.TeamID || t.Scope.SessionID != sessionID || t.Scope.Validate() != nil || t.Status != teams.TeamOpen || t.Revision != 1 || t.CreatedAt.IsZero() {
			return errors.New("invalid team creation metadata")
		}
		if t.CreatorRunID != e.ActorRunID {
			return errors.New("team creator run does not match actor")
		}
		if e.ActorRunID != "" {
			run, ok := st.runs[e.ActorRunID]
			if !ok || run.TeamID != "" || !teamWorkMatches(t.Scope, run) {
				return errors.New("team creator does not own the work")
			}
		}
		open := 0
		for _, prior := range st.Teams {
			if prior.Status != teams.TeamClosed {
				if prior.Name == t.Name {
					return errors.New("duplicate active team name")
				}
				open++
			}
		}
		if open >= teams.MaxSessionTeams {
			return teams.ErrCapacity
		}
		return nil
	}
	if !exists || e.Revision != team.Revision+1 || team.Status == teams.TeamClosed {
		return errors.New("team event requires an open history and next exact revision")
	}
	if err := st.checkActor(e, team); err != nil {
		return err
	}
	if team.Status == teams.TeamClosing {
		switch e.Kind {
		case TeamClosed, TeamMemberState, TeamTurnAborted, TeamTurnTerminal, TeamRequestResponded, TeamRequestExpired:
		default:
			return errors.New("closing team rejects new collaboration writes")
		}
	}
	switch e.Kind {
	case TeamClosing, TeamClosed:
		if e.Team == nil || (e.ActorID != teams.Lead && e.ActorID != "service") {
			return teams.ErrPermission
		}
		next := *e.Team
		previous := team
		next.Status, previous.Status = "", ""
		next.Revision, previous.Revision = 0, 0
		if !reflect.DeepEqual(next, previous) {
			return errors.New("team identity/scope cannot change")
		}
		want := teams.TeamClosing
		if e.Kind == TeamClosed {
			want = teams.TeamClosed
		}
		if e.Team.Status != want || e.Team.Revision != e.Revision || teams.ValidateTeamTransition(team.Status, want) != nil || team.Status == want {
			return errors.New("invalid or duplicate team close transition")
		}
		if want == teams.TeamClosed {
			for _, m := range st.Members {
				if m.TeamID == e.TeamID && !m.Status.IsTerminal() {
					return errors.New("team closed before every member exited")
				}
			}
		}
	case TeamMemberAdded:
		if e.Member == nil || e.ActorID != teams.Lead {
			return teams.ErrPermission
		}
		m := e.Member
		name, err := teams.NormalizeMemberName(m.Name)
		role, roleErr := teams.NormalizeName(m.AgentName)
		if !validTeamIDs(m.ID) || m.ID == teams.Lead || m.ID == "service" || m.TeamID != e.TeamID || err != nil || name != m.Name || roleErr != nil || role != m.AgentName || m.Status != teams.MemberCreated || m.Revision != 1 || m.TurnID != "" || m.RunID != "" || m.Budget.AcceptedTurns != 0 || m.Budget.Elapsed != 0 {
			return errors.New("invalid member creation metadata")
		}
		if err := checkMemberText(*m); err != nil {
			return err
		}
		if _, dup := st.Members[m.ID]; dup {
			return errors.New("duplicate member ID")
		}
		count := 0
		for _, prior := range st.Members {
			if prior.TeamID == e.TeamID && !prior.Status.IsTerminal() {
				count++
				if prior.Name == m.Name {
					return errors.New("duplicate active member name")
				}
			}
		}
		if count >= teams.MaxTeamMembers {
			return teams.ErrCapacity
		}
	case TeamMemberState:
		if e.Member == nil || (e.ActorID != teams.Lead && e.ActorID != "service") {
			return teams.ErrPermission
		}
		m := *e.Member
		prior, ok := st.Members[m.ID]
		if !ok || prior.TeamID != e.TeamID || m.TeamID != e.TeamID || m.Revision != prior.Revision+1 {
			return errors.New("member state requires exact member revision")
		}
		if m.Name != prior.Name || m.AgentName != prior.AgentName || m.PlanRequired != prior.PlanRequired {
			return errors.New("member identity cannot change")
		}
		if err := checkMemberText(m); err != nil {
			return err
		}
		if teams.ValidateMemberTransition(prior.Status, m.Status) != nil || m.Budget != prior.Budget {
			return errors.New("member status/budget does not match durable turns")
		}
		if prior.Status.HasTurn() && !m.Status.HasTurn() {
			turn, ok := st.Turns[prior.TurnID]
			if !ok || !turnTerminal(turn.Status) {
				return errors.New("member cannot leave an accepted turn before its terminal fact")
			}
		}
		if m.Status == teams.MemberRunning && st.runDelegation[m.RunID].Status != "running" {
			return errors.New("running member requires actual running delegation")
		}
		if m.PlanApproved != prior.PlanApproved {
			approved := false
			for _, request := range st.Requests {
				if request.MemberID == m.ID && request.Type == teams.RequestPlan && request.Status == teams.RequestApproved {
					approved = true
				}
			}
			if !m.PlanApproved || !approved {
				return errors.New("plan approval requires its durable approved request")
			}
		}
		if m.RoleHash != prior.RoleHash || m.Model != prior.Model || !reflect.DeepEqual(m.Tools, prior.Tools) {
			if e.ActorID != teams.Lead || prior.Status != teams.MemberInterrupted {
				return errors.New("role metadata changes require explicit interrupted-member resume")
			}
		}
		if m.Status == teams.MemberRunning || m.Status == teams.MemberQueued || m.Status == teams.MemberStopping {
			turn, ok := st.Turns[m.TurnID]
			if !ok || st.turnTeam[turn.ID] != e.TeamID || turn.MemberID != m.ID || turn.RunID != m.RunID || turnTerminal(turn.Status) || turn.Status == "intent" || turn.Status == "aborted" {
				return errors.New("active member state requires an accepted turn")
			}
		}
		if m.Status.IsTerminal() {
			for _, turn := range st.Turns {
				if turn.MemberID == m.ID && turn.Status != "aborted" && !turnTerminal(turn.Status) {
					return errors.New("member terminal before active turn exited")
				}
			}
		}
	case TeamTurnIntent, TeamTurnAccepted, TeamTurnAborted, TeamTurnTerminal:
		return st.checkTurn(e)
	case TeamMessageSent:
		if e.Message == nil {
			return errors.New("message event needs message payload")
		}
		m := *e.Message
		if !validTeamIDs(m.ID) || m.TeamID != e.TeamID || m.SenderID != e.ActorID || m.CreatedAt.IsZero() || m.Seq != 0 || teams.ValidateText(m.Body, teams.MaxMessageBytes, true) != nil {
			return errors.New("invalid message identity/sender/body")
		}
		if _, dup := st.Messages[m.ID]; dup {
			return errors.New("duplicate message ID")
		}
		usage := st.pendingUsage(e.TeamID)
		if err := usage.CheckMessage(m.Recipients, len(m.Body)); err != nil {
			return err
		}
		for _, recipient := range m.Recipients {
			if recipient == teams.Lead {
				continue
			}
			member, ok := st.Members[recipient]
			if !ok || member.TeamID != e.TeamID || member.Status.IsTerminal() {
				return errors.New("message recipient is unknown or closed")
			}
		}
	case TeamMessageHandoff, TeamLeadHandoff:
		return st.checkHandoff(e)
	case TeamTaskCreated, TeamTaskUpdated:
		return st.checkTask(e)
	case TeamRequestCreated, TeamRequestResponded, TeamRequestExpired:
		return st.checkRequest(e, at)
	default:
		return errors.New("unknown team event kind")
	}
	return nil
}
func checkMemberText(m teams.Member) error {
	if teams.ValidateText(m.Model, 256, true) != nil || teams.ValidateText(m.RoleHash, 128, true) != nil || teams.ValidateText(m.Summary, teams.MaxSummaryBytes, false) != nil || teams.ValidateText(m.Error, teams.MaxErrorBytes, false) != nil || m.Budget.Validate() != nil {
		return errors.New("member text or budget exceeds bounds")
	}
	seen := map[string]bool{}
	for _, name := range m.Tools {
		if (name != "read_file" && name != "grep" && name != "glob") || seen[name] {
			return errors.New("member role tools must be unique read-only inspection tools")
		}
		seen[name] = true
	}
	return nil
}
func (st *teamScan) checkTurn(e TeamEvent) error {
	if e.Turn == nil || (e.ActorID != teams.Lead && e.ActorID != "service") {
		return teams.ErrPermission
	}
	t := *e.Turn
	m, ok := st.Members[t.MemberID]
	if !ok || m.TeamID != e.TeamID || !validTeamIDs(t.ID, t.MemberID, t.RunID, t.TaskID) || teams.ValidateText(t.Summary, teams.MaxSummaryBytes, false) != nil || teams.ValidateText(t.Error, teams.MaxErrorBytes, false) != nil || t.Elapsed < 0 || t.Elapsed > teams.MaxTurnDuration {
		return errors.New("invalid team turn metadata")
	}
	if len(t.MessageIDs) > teams.MaxBatchMessages {
		return teams.ErrCapacity
	}
	seen := map[string]bool{}
	bytes := 0
	for _, id := range t.MessageIDs {
		message, ok := st.Messages[id]
		if !ok || message.TeamID != e.TeamID || seen[id] || !containsID(message.Recipients, t.MemberID) {
			return errors.New("turn batch contains unknown, duplicate or wrong-recipient message")
		}
		seen[id] = true
		bytes += len(message.Body)
	}
	if bytes > teams.MaxBatchBytes {
		return teams.ErrCapacity
	}
	if t.OriginRunID != "" {
		run, ok := st.runs[t.OriginRunID]
		if !ok || run.TeamID != "" || !teamWorkMatches(st.Teams[e.TeamID].Scope, run) {
			return errors.New("turn origin run does not own the same work")
		}
	}
	if teams.ValidateText(t.OriginCallID, 256, false) != nil || t.OriginCallID != "" && t.OriginRunID == "" {
		return errors.New("turn origin call requires bounded audit ID and origin run")
	}
	prior, exists := st.Turns[t.ID]
	switch e.Kind {
	case TeamTurnIntent:
		if exists || m.Status.IsTerminal() || t.Status != "intent" || t.Elapsed != 0 || t.Summary != "" || t.Error != "" {
			return errors.New("turn intent must be unique and nonterminal")
		}
		if m.Budget.CanAccept() != nil {
			return teams.ErrBudgetExhausted
		}
		for _, h := range st.Handoffs {
			if h.RecipientID == t.MemberID && containsID(t.MessageIDs, h.MessageID) && st.Turns[h.DestinationTurnID].Status != "interrupted" {
				return errors.New("turn batch reuses a delivered message without interrupted retry")
			}
		}
		for _, active := range st.Turns {
			if active.RunID == t.RunID || active.TaskID == t.TaskID {
				return errors.New("team turn run/task identity is already reserved")
			}
			if active.MemberID == t.MemberID && active.Status != "aborted" && !turnTerminal(active.Status) {
				return errors.New("member already has an active turn")
			}
		}
	case TeamTurnAccepted:
		if !exists || prior.Status != "intent" || t.Status != "queued" || !sameTurnIdentity(prior, t) || t.Elapsed != 0 || t.Summary != "" || t.Error != "" || m.Budget.CanAccept() != nil {
			return errors.New("accepted turn requires its unique intent and remaining budget")
		}
	case TeamTurnAborted:
		if !exists || prior.Status != "intent" || t.Status != "aborted" || !sameTurnIdentity(prior, t) || t.Elapsed != 0 {
			return errors.New("only an unaccepted intent can be aborted")
		}
	case TeamTurnTerminal:
		if !exists || st.turnTeam[t.ID] != e.TeamID || !sameTurnIdentity(prior, t) || prior.Status == "intent" || prior.Status == "aborted" || turnTerminal(prior.Status) || !turnTerminal(t.Status) {
			return errors.New("turn terminal must end one accepted turn once")
		}
		if st.runStatus[t.RunID] != taskRunStatus(t.Status) {
			return errors.New("turn terminal contradicts or precedes durable run outcome")
		}
		if t.Elapsed > m.Budget.RemainingDuration() {
			return errors.New("terminal exceeds remaining member lifetime duration")
		}
	}
	return nil
}
func containsID(ids []string, id string) bool {
	for _, value := range ids {
		if value == id {
			return true
		}
	}
	return false
}

func validateTeamTaskView(existing []teams.Task, next teams.Task, kind string, actor teams.Actor) error {
	if len(next.Blocks) != 0 {
		return errors.New("task blocks is derived, not a writable fact")
	}
	canonical := append([]string(nil), next.BlockedBy...)
	sort.Strings(canonical)
	for i, id := range canonical {
		if id != next.BlockedBy[i] || (i > 0 && id == canonical[i-1]) {
			return errors.New("task dependencies must be canonical")
		}
	}
	graph, err := teams.LoadTaskGraph(next.TeamID, existing)
	if err != nil {
		return err
	}
	if kind == TeamTaskCreated {
		if next.Revision != 1 || next.CreatedBy != actor.MemberID {
			return errors.New("task creation revision/creator mismatch")
		}
		_, err = graph.Create(next, actor)
	} else {
		old, ok := graph.GetCanonical(next.ID)
		if !ok || next.Revision != old.Revision+1 || next.CreatedBy != old.CreatedBy {
			return teams.ErrRevisionConflict
		}
		_, err = graph.Update(next.ID, old.Revision, teams.TaskPatch{Title: &next.Title, Description: &next.Description, Status: &next.Status, Assignee: &next.Assignee, BlockedBy: &next.BlockedBy}, actor)
	}
	return err
}

func (st *teamScan) checkStarted(start RunStarted) error {
	count := 0
	for _, id := range []string{start.TeamID, start.TeamMemberID, start.TeamTurnID} {
		if id != "" {
			count++
		}
	}
	if count != 0 && count != 3 {
		return errors.New("team source fields must be present together")
	}
	if count == 3 {
		if !validTeamIDs(start.TeamID, start.TeamMemberID, start.TeamTurnID, start.RunID) || start.AgentTaskID != "" || start.AgentName != "" || start.ForkSkill != "" || start.ForkEntry != "" {
			return errors.New("team run source is invalid or conflicts with another source")
		}
		team, ok := st.Teams[start.TeamID]
		turn, exists := st.Turns[start.TeamTurnID]
		if !ok || !exists || st.turnTeam[turn.ID] != team.ID || turn.MemberID != start.TeamMemberID || turn.RunID != start.RunID || turn.Status != "queued" || !teamWorkMatches(team.Scope, start) || turn.OriginRunID != start.OriginRunID || turn.OriginCallID != start.OriginCallID {
			return errors.New("team run requires its exact accepted turn and work owner")
		}
	}
	for teamID := range st.Teams {
		for _, h := range st.Handoffs {
			if h.RecipientID == teams.Lead && h.DestinationRunID == start.RunID {
				belongs := false
				if h.MessageID != "" {
					belongs = st.Messages[h.MessageID].TeamID == teamID
				}
				if h.TerminalSeq != 0 {
					for turnID, seq := range st.TurnTerminalSeq {
						if seq == h.TerminalSeq && st.turnTeam[turnID] == teamID {
							belongs = true
						}
					}
				}
				if belongs && (count != 0 || !teamWorkMatches(st.Teams[teamID].Scope, start)) {
					return errors.New("lead handoff destination does not own same work")
				}
				for _, prior := range st.Handoffs {
					if prior.RecipientID == teams.Lead && prior.MessageID == h.MessageID && prior.TerminalSeq == h.TerminalSeq && prior.DestinationRunID != start.RunID {
						if _, delivered := st.runs[prior.DestinationRunID]; delivered {
							return errors.New("lead handoff was already delivered to another run")
						}
					}
				}
			}
		}
	}
	return nil
}

func (st *teamScan) checkRunEvent(sessionID string, event RunEvent) error {
	start, ok := st.runs[event.RunID]
	if !ok || start.TeamID == "" {
		return nil
	}
	if event.SessionID != sessionID || st.runStatus[event.RunID] != "" {
		return errors.New("team run event follows terminal or has wrong session")
	}
	turn := st.Turns[start.TeamTurnID]
	prior := st.runDelegation[event.RunID]
	if event.Kind == "delegation_event" {
		var d AgentTaskDelegation
		if decodeTeamData(event.Payload, &d) != nil {
			return errors.New("invalid team delegation payload")
		}
		if d.TaskID != turn.TaskID || !validTeamIDs(d.BatchID) || d.SessionID != "" && d.SessionID != sessionID || teams.ValidateText(d.TaskName, 256, true) != nil || teams.ValidateText(d.Stage, 256, false) != nil || teams.ValidateText(d.Summary, teams.MaxSummaryBytes, false) != nil || teams.ValidateText(d.Error, teams.MaxErrorBytes, false) != nil || d.UpdatedAt.IsZero() {
			return errors.New("team delegation owner/text mismatch")
		}
		if prior.BatchID != "" && prior.BatchID != d.BatchID || turnTerminal(prior.Status) {
			return errors.New("team delegation batch changed or already terminal")
		}
		switch d.Status {
		case "queued":
			if prior.Status != "" {
				return errors.New("duplicate team queued fact")
			}
		case "running":
			if prior.Status != "queued" && prior.Status != "running" {
				return errors.New("team running fact requires queued")
			}
		case "succeeded", "failed", "canceled", "interrupted":
		default:
			return errors.New("invalid team delegation lifecycle")
		}
	}
	if event.Kind == "terminal" {
		var t struct {
			Status string `json:"status"`
			Reason string `json:"reason,omitempty"`
		}
		if decodeData(event.Payload, &t) != nil {
			return errors.New("invalid team run terminal")
		}
		if !turnTerminal(prior.Status) || taskRunStatus(prior.Status) != t.Status {
			return errors.New("team run terminal requires matching delegation terminal")
		}
	}
	return nil
}

func (st *teamScan) observe(sessionID string, e Event) error {
	if e.SessionID != "" && e.SessionID != sessionID {
		return errors.New("team event belongs to another session")
	}
	switch e.Type {
	case EventRunStarted:
		var start RunStarted
		if decodeData(e.Data, &start) != nil {
			return errors.New("invalid team run source")
		}
		if _, dup := st.runs[start.RunID]; dup {
			return errors.New("duplicate team run ID")
		}
		if err := st.checkStarted(start); err != nil {
			return err
		}
		st.runs[start.RunID] = start
	case EventRunEvent:
		var r RunEvent
		if decodeData(e.Data, &r) != nil {
			return errors.New("invalid team run event")
		}
		if r.ID == "" || r.At.IsZero() || r.SessionID != sessionID || st.runs[r.RunID].RunID == "" || r.RunSeq != st.runSeq[r.RunID]+1 || st.runEventIDs[r.ID] {
			return errors.New("invalid team fold run identity or sequence")
		}
		if err := st.checkRunEvent(sessionID, r); err != nil {
			return err
		}
		st.runSeq[r.RunID] = r.RunSeq
		st.runEventIDs[r.ID] = true
		if r.Kind == "terminal" {
			var t struct {
				Status string `json:"status"`
			}
			_ = decodeData(r.Payload, &t)
			st.runStatus[r.RunID] = t.Status
		}
		if r.Kind == "delegation_event" && st.runs[r.RunID].TeamID != "" {
			var d AgentTaskDelegation
			_ = decodeData(r.Payload, &d)
			st.runDelegation[r.RunID] = d
		}
	case EventTeam:
		var fact TeamEvent
		if err := decodeTeamData(e.Data, &fact); err != nil {
			return err
		}
		if err := st.checkEvent(sessionID, fact, e.At); err != nil {
			return err
		}
		st.ids[fact.ID] = true
		if fact.Token != "" {
			st.tokens[fact.TeamID+"/"+fact.ActorID+"/"+fact.Token] = true
		}
		switch fact.Kind {
		case TeamCreated, TeamClosing, TeamClosed:
			st.Teams[fact.TeamID] = *fact.Team
		case TeamMemberAdded, TeamMemberState:
			st.Members[fact.Member.ID] = *fact.Member
		case TeamTurnIntent, TeamTurnAccepted, TeamTurnAborted, TeamTurnTerminal:
			t := *fact.Turn
			st.Turns[t.ID] = t
			st.turnTeam[t.ID] = fact.TeamID
			m := st.Members[t.MemberID]
			if fact.Kind == TeamTurnAccepted {
				m.Budget.AcceptedTurns++
				m.TurnID, m.RunID, m.OriginRunID = t.ID, t.RunID, t.OriginRunID
				m.Status = teams.MemberQueued
			}
			if fact.Kind == TeamTurnTerminal {
				m.Budget.Elapsed += t.Elapsed
				m.Summary, m.Error = t.Summary, t.Error
				st.TurnTerminalSeq[t.ID] = e.Seq
			}
			st.Members[m.ID] = m
		case TeamMessageSent:
			m := *fact.Message
			m.Seq = e.Seq
			st.Messages[m.ID] = m
		case TeamTaskCreated, TeamTaskUpdated:
			st.Tasks[fact.Task.ID] = *fact.Task
		case TeamRequestCreated, TeamRequestResponded, TeamRequestExpired:
			st.Requests[fact.Request.ID] = *fact.Request
		case TeamMessageHandoff, TeamLeadHandoff:
			st.Handoffs = append(st.Handoffs, *fact.Handoff)
		}
		team := st.Teams[fact.TeamID]
		team.Revision = fact.Revision
		st.Teams[fact.TeamID] = team
		st.LastSeq[fact.TeamID] = e.Seq
	}
	return nil
}

func (st *teamScan) pendingUsage(teamID string) teams.PendingUsage {
	usage := teams.PendingUsage{Recipients: map[string]int{}}
	for _, message := range st.Messages {
		if message.TeamID != teamID {
			continue
		}
		for _, recipient := range message.Recipients {
			delivered := false
			for _, h := range st.Handoffs {
				if h.MessageID == message.ID && h.RecipientID == recipient {
					if recipient == teams.Lead {
						_, delivered = st.runs[h.DestinationRunID]
					} else {
						turn := st.Turns[h.DestinationTurnID]
						delivered = turn.Status != "intent" && turn.Status != "aborted"
					}
				}
			}
			if !delivered {
				usage.Deliveries++
				usage.Bytes += len(message.Body)
				usage.Recipients[recipient]++
			}
		}
	}
	return usage
}
func (st *teamScan) checkHandoff(e TeamEvent) error {
	if e.Handoff == nil || e.ActorID != teams.Lead && e.ActorID != "service" {
		return teams.ErrPermission
	}
	h := *e.Handoff
	if !validTeamIDs(h.DestinationRunID) || h.MessageID == "" && h.TerminalSeq == 0 {
		return errors.New("handoff requires source and destination")
	}
	if h.MessageID != "" && h.TerminalSeq != 0 {
		return errors.New("handoff must reference exactly one message or terminal fact")
	}
	if h.MessageID != "" {
		m, ok := st.Messages[h.MessageID]
		if !ok || m.TeamID != e.TeamID || !containsID(m.Recipients, h.RecipientID) {
			return errors.New("handoff message/recipient mismatch")
		}
	}
	if e.Kind == TeamLeadHandoff {
		if h.RecipientID != teams.Lead || h.DestinationTurnID != "" || h.RetryOfTurnID != "" {
			return errors.New("lead handoff cannot name a member turn")
		}
		if run, ok := st.runs[h.DestinationRunID]; ok && (run.TeamID != "" || !teamWorkMatches(st.Teams[e.TeamID].Scope, run)) {
			return errors.New("lead handoff destination has different work")
		}
		if h.TerminalSeq != 0 {
			found := false
			for id, seq := range st.TurnTerminalSeq {
				if seq == h.TerminalSeq && st.turnTeam[id] == e.TeamID {
					found = true
				}
			}
			if !found {
				return errors.New("lead handoff terminal seq does not exist")
			}
		}
	} else {
		turn, ok := st.Turns[h.DestinationTurnID]
		if !ok || st.turnTeam[turn.ID] != e.TeamID || turn.MemberID != h.RecipientID || turn.RunID != h.DestinationRunID || turn.Status == "intent" || turn.Status == "aborted" || h.TerminalSeq != 0 || !containsID(turn.MessageIDs, h.MessageID) {
			return errors.New("member handoff requires matching accepted message batch")
		}
		if st.runDelegation[turn.RunID].Status == "" {
			return errors.New("member handoff requires actual queued run fact")
		}
		if h.RetryOfTurnID != "" {
			old, ok := st.Turns[h.RetryOfTurnID]
			if !ok || old.MemberID != turn.MemberID || old.Status != "interrupted" || !containsID(old.MessageIDs, h.MessageID) {
				return errors.New("message retry must reference interrupted same-member turn")
			}
		}
	}
	lastMemberTurn := ""
	for _, prior := range st.Handoffs {
		if prior.MessageID != h.MessageID || prior.RecipientID != h.RecipientID || prior.TerminalSeq != h.TerminalSeq {
			continue
		}
		if prior.DestinationRunID == h.DestinationRunID {
			return errors.New("duplicate team handoff")
		}
		if e.Kind == TeamLeadHandoff {
			if _, delivered := st.runs[prior.DestinationRunID]; delivered {
				return errors.New("lead notice was already delivered")
			}
		} else {
			lastMemberTurn = prior.DestinationTurnID
		}
	}
	if e.Kind == TeamMessageHandoff && lastMemberTurn != "" && h.RetryOfTurnID != lastMemberTurn {
		return errors.New("member message retry must follow the latest interrupted handoff")
	}
	return nil
}
func (st *teamScan) checkTask(e TeamEvent) error {
	if e.Task == nil || e.Task.TeamID != e.TeamID {
		return errors.New("task payload does not match team")
	}
	actor := teams.Actor{MemberID: e.ActorID, Lead: e.ActorID == teams.Lead}
	// Domain graph validates the full prospective graph. Restore already
	// validated canonical facts directly through its dedicated constructor.
	var existing []teams.Task
	for _, task := range st.Tasks {
		if task.TeamID == e.TeamID {
			existing = append(existing, task)
		}
	}
	sort.Slice(existing, func(i, j int) bool { return existing[i].ID < existing[j].ID })
	if e.Task.Assignee != "" {
		m, ok := st.Members[e.Task.Assignee]
		if !ok || m.TeamID != e.TeamID || m.Status.IsTerminal() {
			return teams.ErrPermission
		}
	}
	if err := validateTeamTaskView(existing, *e.Task, e.Kind, actor); err != nil {
		return err
	}
	return nil
}
func (st *teamScan) checkRequest(e TeamEvent, at time.Time) error {
	if e.Request == nil {
		return errors.New("request event needs request payload")
	}
	r := *e.Request
	m, ok := st.Members[r.MemberID]
	if !validTeamIDs(r.ID, r.MemberID) || !ok || m.TeamID != e.TeamID || r.TeamID != e.TeamID || teams.ValidateText(r.Body, teams.MaxPlanBytes, false) != nil || teams.ValidateText(r.Feedback, teams.MaxFeedbackBytes, false) != nil {
		return errors.New("request metadata exceeds bounds or ownership")
	}
	if r.Type != teams.RequestPlan && r.Type != teams.RequestShutdown {
		return errors.New("unknown team request type")
	}
	if r.Type == teams.RequestPlan {
		if r.RequesterID != r.MemberID || r.ResponderID != teams.Lead || r.Body == "" {
			return errors.New("plan request requires member requester and lead responder")
		}
	} else if r.RequesterID != teams.Lead || r.ResponderID != r.MemberID {
		return errors.New("shutdown request requires lead requester and member responder")
	}
	prior, exists := st.Requests[r.ID]
	if e.Kind == TeamRequestCreated {
		if exists || r.Revision != 1 || r.Status != teams.RequestPending || r.RequesterID != e.ActorID || r.ExpiresAt.IsZero() || !r.ExpiresAt.After(at) || r.ExpiresAt.Sub(at) > teams.RequestDuration {
			return errors.New("invalid new team request lifecycle")
		}
		for _, other := range st.Requests {
			if other.MemberID == r.MemberID && other.Type == r.Type && (other.Status == teams.RequestPending || other.Status == teams.RequestDeferred) {
				return errors.New("member already has pending request of this type")
			}
		}
		return nil
	}
	if !exists || r.Revision != prior.Revision+1 || prior.Status != teams.RequestPending && prior.Status != teams.RequestDeferred {
		return errors.New("request response requires one unresolved exact revision")
	}
	a, b := r, prior
	a.Status, b.Status = "", ""
	a.Feedback, b.Feedback = "", ""
	a.Revision, b.Revision = 0, 0
	if !reflect.DeepEqual(a, b) {
		return errors.New("request identity or protocol cannot change")
	}
	if e.Kind == TeamRequestExpired {
		if e.ActorID != "service" || r.Status != teams.RequestExpired || at.Before(r.ExpiresAt) {
			return errors.New("request cannot expire before deadline")
		}
		return nil
	}
	if e.ActorID != r.ResponderID && e.ActorID != "service" || !at.Before(r.ExpiresAt) {
		return errors.New("wrong or expired request responder")
	}
	if r.Status != teams.RequestApproved && r.Status != teams.RequestRejected && !(r.Type == teams.RequestShutdown && r.Status == teams.RequestDeferred) {
		return errors.New("invalid request response decision")
	}
	if e.ActorID == "service" && (r.Type != teams.RequestShutdown || r.Status != teams.RequestApproved || m.Status != teams.MemberIdle && m.Status != teams.MemberCreated) {
		return errors.New("automatic response only approves idle member shutdown")
	}
	return nil
}
