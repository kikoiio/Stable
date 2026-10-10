package sessionlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"stable/internal/teams"
)

// walkTeamLog reads one envelope at a time. It never retains a Transcript or
// provider payload history. Team-specific folds perform their own validation.
func walkTeamLog(root, sessionID string, visit func(Event) error) error {
	path, err := SessionPath(root, sessionID)
	if err != nil {
		return err
	}
	if err = checkTrailingNewline(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 8<<20)
	seq := uint64(1)
	for s.Scan() {
		var event Event
		if err := json.Unmarshal(s.Bytes(), &event); err != nil {
			return fmt.Errorf("session log corrupt at seq %d: %w", seq, err)
		}
		if event.SchemaVersion != SchemaVersion || event.SessionID != sessionID || event.Seq != seq || event.At.IsZero() || event.At.Location() != time.UTC {
			return fmt.Errorf("session log invalid envelope at seq %d", seq)
		}
		if seq == 1 {
			var info SessionInfo
			if event.Type != EventSessionCreated || decodeData(event.Data, &info) != nil || info.ID != sessionID || info.CreatedAt.IsZero() {
				return errors.New("session log invalid creation")
			}
		}
		if err := visit(event); err != nil {
			return fmt.Errorf("team fold at seq %d: %w", seq, err)
		}
		seq++
	}
	if err := s.Err(); err != nil {
		return err
	}
	if seq == 1 {
		return errors.New("session log is empty")
	}
	return nil
}

// ReplayTeams folds raw facts without loading transcript history. With no ID
// filter it returns open/closing teams; explicit IDs include closed history.
// Delivered message bodies are removed once no active/interrupted turn needs
// them. Use TeamHistory to display delivered mail or closed history pages.
func ReplayTeams(root, sessionID string, teamIDs ...string) (TeamProjection, error) {
	selected := map[string]bool{}
	for _, id := range teamIDs {
		if teams.ValidateID(id) != nil {
			return TeamProjection{}, errors.New("invalid requested team ID")
		}
		selected[id] = true
	}
	if len(teamIDs) == 0 {
		if err := walkTeamLog(root, sessionID, func(event Event) error {
			if event.Type != EventTeam {
				return nil
			}
			var fact TeamEvent
			if err := decodeTeamData(event.Data, &fact); err != nil {
				return err
			}
			if fact.SessionID != sessionID {
				return errors.New("team fact belongs to another session")
			}
			if fact.Kind == TeamCreated {
				selected[fact.TeamID] = true
			}
			if fact.Kind == TeamClosed {
				delete(selected, fact.TeamID)
			}
			return nil
		}); err != nil {
			return TeamProjection{}, err
		}
	}
	// Only referenced runs participate in the fold. An unrelated long parent
	// transcript contributes no run, tool or provider data to the team view.
	refs := map[string]bool{}
	if err := walkTeamLog(root, sessionID, func(event Event) error {
		if event.Type != EventTeam {
			return nil
		}
		var fact TeamEvent
		if err := decodeTeamData(event.Data, &fact); err != nil {
			return err
		}
		if !selected[fact.TeamID] {
			return nil
		}
		if fact.ActorRunID != "" {
			refs[fact.ActorRunID] = true
		}
		if fact.Team != nil && fact.Team.CreatorRunID != "" {
			refs[fact.Team.CreatorRunID] = true
		}
		if fact.Turn != nil {
			refs[fact.Turn.RunID] = true
			if fact.Turn.OriginRunID != "" {
				refs[fact.Turn.OriginRunID] = true
			}
		}
		if fact.Handoff != nil {
			refs[fact.Handoff.DestinationRunID] = true
		}
		return nil
	}); err != nil {
		return TeamProjection{}, err
	}
	st := newTeamScan()
	err := walkTeamLog(root, sessionID, func(event Event) error {
		switch event.Type {
		case EventTeam:
			var fact TeamEvent
			if err := decodeTeamData(event.Data, &fact); err != nil {
				return err
			}
			if !selected[fact.TeamID] {
				return nil
			}
		case EventRunStarted:
			var run RunStarted
			if decodeData(event.Data, &run) != nil {
				return errors.New("invalid run source")
			}
			if !refs[run.RunID] {
				return nil
			}
		case EventRunEvent:
			var run RunEvent
			if decodeData(event.Data, &run) != nil {
				return errors.New("invalid run event")
			}
			if !refs[run.RunID] {
				return nil
			}
		default:
			return nil
		}
		if err := st.observe(sessionID, event); err != nil {
			return err
		}
		st.pruneDeliveredMessages()
		return nil
	})
	if err != nil {
		return TeamProjection{}, err
	}
	for id := range selected {
		if _, ok := st.Teams[id]; !ok {
			return TeamProjection{}, teams.ErrNotFound
		}
	}
	return st.TeamProjection, nil
}

// TeamIDs returns each team whose creation fact is present, including closed
// history. It scans the log incrementally so listing team metadata does not
// materialize the session transcript.
func TeamIDs(root, sessionID string) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	err := walkTeamLog(root, sessionID, func(event Event) error {
		if event.Type != EventTeam {
			return nil
		}
		var fact TeamEvent
		if err := decodeTeamData(event.Data, &fact); err != nil {
			return err
		}
		if fact.SessionID != sessionID {
			return errors.New("team fact belongs to another session")
		}
		if fact.Kind == TeamCreated && !seen[fact.TeamID] {
			seen[fact.TeamID] = true
			ids = append(ids, fact.TeamID)
		}
		return nil
	})
	return ids, err
}

// FindRunStart returns one persisted run attribution without retaining the
// complete parent transcript or provider payload history.
func FindRunStart(root, sessionID, runID string) (RunStarted, bool, error) {
	var found RunStarted
	matched := false
	err := walkTeamLog(root, sessionID, func(event Event) error {
		if event.Type != EventRunStarted {
			return nil
		}
		var run RunStarted
		if err := decodeData(event.Data, &run); err != nil {
			return errors.New("invalid persisted run attribution")
		}
		if run.RunID == runID {
			if matched {
				return errors.New("duplicate persisted run attribution")
			}
			found, matched = run, true
		}
		return nil
	})
	return found, matched, err
}

func (st *teamScan) pruneDeliveredMessages() {
	for id, message := range st.Messages {
		needed := false
		for _, turn := range st.Turns {
			if containsID(turn.MessageIDs, id) && (turn.Status == "interrupted" || turn.Status != "aborted" && !turnTerminal(turn.Status)) {
				needed = true
				break
			}
		}
		if needed {
			continue
		}
		for _, recipient := range message.Recipients {
			delivered := false
			for _, h := range st.Handoffs {
				if h.MessageID != id || h.RecipientID != recipient {
					continue
				}
				if recipient == teams.Lead {
					_, delivered = st.runs[h.DestinationRunID]
				} else {
					delivered = st.Turns[h.DestinationTurnID].Status != "intent" && st.Turns[h.DestinationTurnID].Status != "aborted"
				}
				if delivered {
					break
				}
			}
			if !delivered {
				needed = true
				break
			}
		}
		if !needed {
			delete(st.Messages, id)
		}
	}
	kept := st.Handoffs[:0]
	for _, h := range st.Handoffs {
		if h.MessageID == "" {
			kept = append(kept, h)
			continue
		}
		if _, ok := st.Messages[h.MessageID]; ok {
			kept = append(kept, h)
		}
	}
	st.Handoffs = kept
}

// TeamHistory returns at most a bounded page of raw collaboration facts.
// afterSeq is the last returned session cursor, not a revision or run cursor.
func TeamHistory(root, sessionID, teamID string, afterSeq uint64, limit int) ([]Event, error) {
	if teams.ValidateID(teamID) != nil {
		return nil, errors.New("invalid team history ID")
	}
	limit = teams.PageSize(limit)
	out := make([]Event, 0, limit)
	found := false
	err := walkTeamLog(root, sessionID, func(event Event) error {
		if event.Type != EventTeam {
			return nil
		}
		var fact TeamEvent
		if err := decodeTeamData(event.Data, &fact); err != nil {
			return err
		}
		if fact.TeamID != teamID {
			return nil
		}
		if fact.SessionID != sessionID {
			return errors.New("history fact belongs to another session")
		}
		found = true
		if event.Seq > afterSeq && len(out) < limit {
			out = append(out, event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, teams.ErrNotFound
	}
	return out, nil
}

// LookupTeamMutation supports durable token idempotence without retaining an
// unbounded token/result cache in the service. A caller compares ArgsDigest
// before returning the original typed operation result.
func LookupTeamMutation(root, sessionID, teamID, actorID, token string) (TeamEvent, bool, error) {
	if !validTeamIDs(teamID) || actorID == "" || token == "" || teams.ValidateText(token, 256, true) != nil {
		return TeamEvent{}, false, errors.New("invalid team mutation lookup")
	}
	var found TeamEvent
	ok := false
	err := walkTeamLog(root, sessionID, func(event Event) error {
		if event.Type != EventTeam {
			return nil
		}
		var fact TeamEvent
		if err := decodeTeamData(event.Data, &fact); err != nil {
			return err
		}
		if fact.TeamID != teamID || fact.ActorID != actorID || fact.Token != token {
			return nil
		}
		if fact.SessionID != sessionID {
			return errors.New("mutation belongs to another session")
		}
		if ok {
			return errors.New("duplicate durable team mutation token")
		}
		if fact.Message != nil {
			message := *fact.Message
			message.Seq = event.Seq
			fact.Message = &message
		}
		found = fact
		ok = true
		return nil
	})
	return found, ok, err
}
