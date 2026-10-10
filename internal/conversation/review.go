package conversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"stable/internal/candidate"
	"stable/internal/sessionlog"
)

// We adapt the persistent store's record without exposing its implementation to clients.
func (s *Service) reviewCandidate(ctx context.Context, id, sessionID string) (candidate.Review, error) {
	if s.deps.Store == nil {
		return candidate.Review{}, errors.New("candidate store is unavailable")
	}
	record, err := s.deps.Store.GetCandidate(ctx, id)
	if err != nil {
		return candidate.Review{}, err
	}
	if err = s.verifyCandidateSession(ctx, record.GoalID, sessionID); err != nil {
		return candidate.Review{}, err
	}
	c := record.Candidate
	if c.Status != "ready" && c.Status != "reviewed" {
		return candidate.Review{}, fmt.Errorf("candidate %s is not ready for review", id)
	}
	c.Status = "frozen"
	if c.CandidateDigest == "" || c.BaselineDigest == "" {
		return candidate.Review{}, errors.New("candidate is missing baseline or candidate digest")
	}
	if _, digest, digestErr := candidate.BuildManifestForPolicy(c.CandidateRoot, c.ManifestPolicy); digestErr != nil {
		return candidate.Review{}, digestErr
	} else if digest != c.CandidateDigest {
		return candidate.Review{}, errors.New("candidate changed since it became ready; review again")
	}
	if _, digest, digestErr := candidate.BuildManifestForPolicy(c.FormalRoot, c.ManifestPolicy); digestErr != nil {
		return candidate.Review{}, digestErr
	} else if digest != c.BaselineDigest {
		return candidate.Review{}, errors.New("formal project changed since candidate baseline; review again")
	}
	checkers := append([]candidate.Checker(nil), s.deps.CandidateCheckers...)
	review, err := candidate.BuildReview(ctx, c, checkers)
	if err != nil {
		return candidate.Review{}, err
	}
	if len(checkers) == 0 {
		review.Findings = append(review.Findings, candidate.Finding{ID: "review-checks-unavailable", Checker: "candidate-review", Result: candidate.FindingUnavailable, Reason: "no independent candidate checker is configured"})
		review.Digest, err = candidate.ComputeReviewDigest(review)
		if err != nil {
			return candidate.Review{}, err
		}
	}
	if err = s.deps.Store.SaveCandidateReview(ctx, review); err != nil {
		return candidate.Review{}, err
	}
	if record.Candidate.Status == "ready" {
		if err = s.deps.Store.TransitionCandidate(ctx, id, "ready", "reviewed", review.CandidateDigest); err != nil {
			return candidate.Review{}, err
		}
	}
	return review, nil
}

func (s *Service) acceptReviewedCandidate(ctx context.Context, msg ClientMsg) (candidate.Receipt, error) {
	if s.deps.Store == nil {
		return candidate.Receipt{}, errors.New("candidate store is unavailable")
	}
	record, err := s.deps.Store.GetCandidate(ctx, msg.CandidateID)
	if err != nil {
		return candidate.Receipt{}, err
	}
	if err = s.verifyCandidateSession(ctx, record.GoalID, msg.SessionID); err != nil {
		return candidate.Receipt{}, err
	}
	review, err := s.deps.Store.GetCandidateReview(ctx, "review-"+msg.CandidateID)
	if err != nil {
		return candidate.Receipt{}, fmt.Errorf("candidate has no persisted review: %w", err)
	}
	mode := candidate.AcceptanceMode(msg.AcceptanceMode)
	decision := candidate.AcceptanceDecision{ID: msg.DecisionID, UserID: strconv.Itoa(os.Getuid()), CandidateID: msg.CandidateID, CandidateDigest: msg.CandidateDigest, PreviewDigest: msg.PreviewDigest, FormalDigest: msg.FormalDigest, Mode: mode, ConfirmedFindings: append([]string(nil), msg.Confirmed...)}
	return candidate.AcceptCandidate(ctx, record.Candidate, review, decision, record.GoalID, record.ActionID, s.deps.Store, time.Now().UTC())
}

func (s *Service) verifyCandidateSession(ctx context.Context, goalID, sessionID string) error {
	if sessionID == "" {
		return errors.New("candidate review requires an owning session")
	}
	if _, err := sessionlog.SessionPath(s.deps.ProjectRoot, sessionID); err != nil {
		return err
	}
	if strings.HasPrefix(goalID, "session-") {
		if goalID != "session-"+sessionID {
			return errors.New("candidate does not belong to this session")
		}
		return nil
	}
	goal, err := s.deps.Store.GetGoalSnapshot(ctx, goalID)
	if err != nil {
		return err
	}
	if goal.Goal.SourceSessionID == "" || goal.Goal.SourceSessionID != sessionID {
		return errors.New("candidate does not belong to this session")
	}
	return nil
}
