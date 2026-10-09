package teams

import (
	"errors"
	"sort"
	"time"
)

const (
	MaxNameBytes            = 64
	MaxSessionTeams         = 2
	MaxServiceTeams         = 4
	MaxTeamMembers          = 8
	MaxServiceMembers       = 16
	MaxMemberTurns          = 16
	MaxMemberDuration       = 10 * time.Minute
	MaxTurnRounds           = 8
	MaxTurnDuration         = 3 * time.Minute
	MaxTurnOutputBytes      = 50000
	MaxSummaryBytes         = 8 * 1024
	MaxInputBytes           = 64 * 1024
	MaxErrorBytes           = 1024
	MaxMessageBytes         = 8 * 1024
	MaxRecipientPending     = 64
	MaxTeamPending          = 256
	MaxTeamPendingBytes     = 2 * 1024 * 1024
	MaxBatchMessages        = 8
	MaxBatchBytes           = 32 * 1024
	MaxTeamTasks            = 256
	MaxTaskTitleBytes       = 256
	MaxTaskDescriptionBytes = 4 * 1024
	MaxTaskDependencies     = 16
	MaxPlanBytes            = 8 * 1024
	MaxFeedbackBytes        = 2 * 1024
	RequestDuration         = 10 * time.Minute
	DefaultPageSize         = 20
	MaxPageSize             = 100
	MaxWaitDuration         = 30 * time.Second
)

var (
	ErrCapacity         = errors.New("team capacity exceeded")
	ErrBudgetExhausted  = errors.New("member lifetime budget exhausted")
	ErrPermission       = errors.New("team actor is not authorized")
	ErrRevisionConflict = errors.New("team task revision conflict")
	ErrNotFound         = errors.New("team task not found")
	ErrDependency       = errors.New("invalid team task dependency")
)

// Budget is reconstructed from accepted turns and terminal facts. Resuming a
// member must carry this value forward; it never resets the lifetime budget.
type Budget struct {
	AcceptedTurns int           `json:"accepted_turns"`
	Elapsed       time.Duration `json:"elapsed"`
}

func (b Budget) Validate() error {
	if b.AcceptedTurns < 0 || b.AcceptedTurns > MaxMemberTurns || b.Elapsed < 0 {
		return errors.New("invalid member budget accounting")
	}
	return nil
}

func (b Budget) CanAccept() error {
	if err := b.Validate(); err != nil {
		return err
	}
	if b.AcceptedTurns >= MaxMemberTurns || b.Elapsed >= MaxMemberDuration {
		return ErrBudgetExhausted
	}
	return nil
}

func (b Budget) RemainingDuration() time.Duration {
	if b.Elapsed >= MaxMemberDuration {
		return 0
	}
	if b.Elapsed < 0 {
		return 0
	}
	return min(MaxTurnDuration, MaxMemberDuration-b.Elapsed)
}

// CheckCapacity only checks prospective deltas; service serializes reservation
// and persistence. Callers cannot exceed service caps by creating another team.
func CheckCapacity(sessionTeams, serviceTeams, teamMembers, serviceMembers int, addTeam, addMember bool) error {
	if sessionTeams < 0 || serviceTeams < 0 || teamMembers < 0 || serviceMembers < 0 {
		return errors.New("invalid capacity accounting")
	}
	if sessionTeams > MaxSessionTeams || serviceTeams > MaxServiceTeams || teamMembers > MaxTeamMembers || serviceMembers > MaxServiceMembers {
		return ErrCapacity
	}
	if addTeam && (sessionTeams >= MaxSessionTeams || serviceTeams >= MaxServiceTeams) {
		return ErrCapacity
	}
	if addMember && (teamMembers >= MaxTeamMembers || serviceMembers >= MaxServiceMembers) {
		return ErrCapacity
	}
	return nil
}

func PageSize(limit int) int {
	if limit <= 0 {
		return DefaultPageSize
	}
	return min(limit, MaxPageSize)
}

// MembersPage returns a stable, bounded copy of a team's member history.
func MembersPage(members []Member, limit int) []Member {
	page, _ := MembersPageAfter(members, "", limit)
	return page
}

// MembersPageAfter returns a stable page after the previous visible member ID.
func MembersPageAfter(members []Member, afterMemberID string, limit int) ([]Member, error) {
	if afterMemberID != "" && ValidateID(afterMemberID) != nil {
		return nil, errors.New("team member cursor is invalid")
	}
	page := append([]Member(nil), members...)
	sort.Slice(page, func(i, j int) bool {
		if page[i].Name == page[j].Name {
			return page[i].ID < page[j].ID
		}
		return page[i].Name < page[j].Name
	})
	if afterMemberID != "" {
		cursorIndex := -1
		for i := range page {
			if page[i].ID == afterMemberID {
				cursorIndex = i
				break
			}
		}
		if cursorIndex < 0 {
			return nil, errors.New("team member cursor was not found")
		}
		page = page[cursorIndex+1:]
	}
	pageSize := PageSize(limit)
	if len(page) > pageSize {
		page = page[:pageSize]
	}
	return page, nil
}

func WaitDuration(wait time.Duration) time.Duration {
	if wait < 0 {
		return 0
	}
	return min(wait, MaxWaitDuration)
}

// PendingUsage counts deliveries, including each broadcast recipient. Payload
// bytes are also charged per delivery to prevent broadcast capacity bypasses.
type PendingUsage struct {
	Deliveries int
	Bytes      int
	Recipients map[string]int
}

func (p PendingUsage) CheckMessage(recipients []string, bodyBytes int) error {
	if bodyBytes < 1 || bodyBytes > MaxMessageBytes || p.Deliveries < 0 || p.Bytes < 0 {
		return errors.New("invalid message size or pending accounting")
	}
	if len(recipients) == 0 || len(recipients) > MaxTeamMembers+1 {
		return errors.New("invalid message recipients")
	}
	seen := make(map[string]bool, len(recipients))
	for _, id := range recipients {
		if id == "" || seen[id] {
			return errors.New("message recipients must be unique and nonempty")
		}
		seen[id] = true
		if p.Recipients[id] < 0 {
			return errors.New("invalid recipient pending accounting")
		}
		if p.Recipients[id] >= MaxRecipientPending {
			return ErrCapacity
		}
	}
	if p.Deliveries > MaxTeamPending || p.Bytes > MaxTeamPendingBytes || len(recipients) > MaxTeamPending-p.Deliveries || len(recipients)*bodyBytes > MaxTeamPendingBytes-p.Bytes {
		return ErrCapacity
	}
	return nil
}
