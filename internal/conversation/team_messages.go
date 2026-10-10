package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"stable/internal/agent"
	"stable/internal/sessionlog"
	"stable/internal/teams"
)

// TeamSendRequest has no sender or scope fields. The caller's persisted parent
// run supplies both. Recipient is a member ID or an unambiguous team-local name.
type TeamSendRequest struct {
	TeamID    string
	Recipient string
	Body      string
	Token     string
	Broadcast bool
}

// SendTeamMessage persists one message, including the entire broadcast snapshot,
// before returning success. Scheduling recipients happens after this commit.
func (s *Service) SendTeamMessage(ctx context.Context, request agent.ExecutionRequest, args TeamSendRequest) (teams.Message, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return teams.Message{}, err
	}
	if teams.ValidateID(args.TeamID) != nil || teams.ValidateText(args.Token, 256, true) != nil {
		return teams.Message{}, errors.New("team send requires a team ID and bounded idempotency token")
	}
	if err = teams.ValidateText(args.Body, teams.MaxMessageBytes, true); err != nil {
		return teams.Message{}, err
	}
	if args.Broadcast {
		if args.Recipient != "" {
			return teams.Message{}, errors.New("broadcast cannot specify a recipient")
		}
	} else if teams.ValidateID(args.Recipient) != nil {
		return teams.Message{}, errors.New("team send requires a team-local recipient")
	}
	// Hash the exact original input so a changed credential/redaction policy
	// cannot turn a different request into a successful idempotent retry.
	raw, err := json.Marshal(struct {
		TeamID, Recipient, Body string
		Broadcast               bool
	}{args.TeamID, args.Recipient, args.Body, args.Broadcast})
	if err != nil {
		return teams.Message{}, err
	}
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	body := redactRunCredential(args.Body, s.deps.ProviderCredential)
	if err = teams.ValidateText(body, teams.MaxMessageBytes, true); err != nil {
		return teams.Message{}, err
	}

	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if err = ctx.Err(); err != nil {
		return teams.Message{}, err
	}
	if _, currentScope, currentActor, scopeErr := s.teamOperationScope(ctx, request); scopeErr != nil || !currentScope.Matches(scope) || currentActor != actor {
		return teams.Message{}, teams.ErrPermission
	}
	projection, err := sessionlog.ReplayTeams(root, scope.SessionID, args.TeamID)
	if err != nil {
		return teams.Message{}, err
	}
	team, ok := projection.Teams[args.TeamID]
	if !ok || !team.Scope.Matches(scope) {
		return teams.Message{}, teams.ErrPermission
	}
	senderID := actorID(actor)
	prior, found, err := sessionlog.LookupTeamMutation(root, scope.SessionID, team.ID, senderID, args.Token)
	if err != nil {
		return teams.Message{}, err
	}
	if found {
		if prior.Kind != sessionlog.TeamMessageSent || prior.ArgsDigest != digest || prior.Message == nil {
			return teams.Message{}, errors.New("team mutation token was already used for different input")
		}
		// A retry returns the original snapshot even if recipients have stopped
		// or the team has closed since the successful commit.
		return *prior.Message, nil
	}
	if team.Status != teams.TeamOpen {
		return teams.Message{}, errors.New("team is not open for messages")
	}
	recipients, err := teamMessageRecipients(projection, team.ID, senderID, args.Recipient, args.Broadcast)
	if err != nil {
		return teams.Message{}, err
	}
	id, err := sessionlog.NewID()
	if err != nil {
		return teams.Message{}, err
	}
	eventID, err := sessionlog.NewID()
	if err != nil {
		return teams.Message{}, err
	}
	message := teams.Message{ID: id, TeamID: team.ID, SenderID: senderID, Recipients: recipients, Body: body, CreatedAt: time.Now().UTC()}
	fact := sessionlog.TeamEvent{ID: eventID, TeamID: team.ID, SessionID: scope.SessionID, Kind: sessionlog.TeamMessageSent, Revision: team.Revision + 1, ActorID: senderID, ActorRunID: request.RunID, OperationID: args.Token, Token: args.Token, ArgsDigest: digest, Message: &message}
	// Append's strict projection checks all recipient/team pending limits under
	// the session file lock. This also covers lead handoffs whose destination
	// run has not started; the public projection alone cannot prove delivery.
	event, err := sessionlog.Append(root, scope.SessionID, sessionlog.EventTeam, fact)
	if err != nil {
		return teams.Message{}, err
	}
	message.Seq = event.Seq
	if actor.Lead && s.teamScheduler != nil {
		for _, memberID := range recipients {
			s.teamScheduler.signalFromLead(request, scope, team.ID, memberID, args.Token)
		}
	}
	return message, nil
}

func teamMessageRecipients(projection sessionlog.TeamProjection, teamID, senderID, recipient string, broadcast bool) ([]string, error) {
	if !broadcast && recipient == teams.Lead {
		if senderID == teams.Lead {
			return nil, errors.New("lead cannot send a team message to itself")
		}
		return []string{teams.Lead}, nil
	}
	var matches []teams.Member
	for _, member := range projection.Members {
		if member.TeamID != teamID {
			continue
		}
		if broadcast {
			if member.ID != senderID && !member.Status.IsTerminal() && member.Status != teams.MemberStopping {
				matches = append(matches, member)
			}
		} else if member.ID == recipient || member.Name == recipient {
			matches = append(matches, member)
		}
	}
	if (!broadcast && len(matches) != 1) || len(matches) == 0 {
		return nil, errors.New("message recipient is unknown or ambiguous in this team")
	}
	recipients := make([]string, 0, len(matches))
	for _, member := range matches {
		if member.Status.IsTerminal() || member.Status == teams.MemberStopping {
			return nil, errors.New("message recipient is stopping or closed")
		}
		recipients = append(recipients, member.ID)
	}
	sort.Strings(recipients)
	return recipients, nil
}

// ListTeamMessages returns a bounded, scope-local page. Member runs can see
// only messages they sent or received; lead runs see the whole team stream.
func (s *Service) ListTeamMessages(ctx context.Context, request agent.ExecutionRequest, teamID string, after uint64, limit int) ([]teams.Message, error) {
	root, scope, actor, err := s.teamOperationScope(ctx, request)
	if err != nil {
		return nil, err
	}
	if _, _, err = s.teamForOperation(root, scope, teamID, actor); err != nil {
		return nil, err
	}
	projection, err := sessionlog.ReplayTeams(root, scope.SessionID, teamID)
	if err != nil {
		return nil, err
	}
	limit = teams.PageSize(limit)
	out := make([]teams.Message, 0, limit)
	for _, message := range projection.Messages {
		if message.TeamID != teamID || message.Seq <= after {
			continue
		}
		if !actor.Lead && message.SenderID != actor.MemberID && !containsString(message.Recipients, actor.MemberID) {
			continue
		}
		out = append(out, message)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
