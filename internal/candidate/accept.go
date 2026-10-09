package candidate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"stable/internal/platform/secfile"
)

type AcceptanceMode string

const (
	AcceptNormal AcceptanceMode = "normal"
	AcceptForce  AcceptanceMode = "force"
)

type AcceptanceDecision struct {
	ID                string         `json:"id"`
	UserID            string         `json:"user_id"`
	CandidateID       string         `json:"candidate_id"`
	CandidateDigest   string         `json:"candidate_digest"`
	PreviewDigest     string         `json:"preview_digest"`
	FormalDigest      string         `json:"formal_digest"`
	Mode              AcceptanceMode `json:"mode"`
	ConfirmedFindings []string       `json:"confirmed_findings,omitempty"`
}

type Receipt struct {
	ID           string    `json:"id"`
	DecisionID   string    `json:"decision_id"`
	CandidateID  string    `json:"candidate_id"`
	FormalDigest string    `json:"formal_digest"`
	AcceptedAt   time.Time `json:"accepted_at"`
}

type AcceptanceStore interface {
	CheckAcceptance(context.Context, AcceptanceDecision) (bool, Receipt, bool, error)
	SaveAcceptanceDecision(context.Context, AcceptanceDecision) (bool, error)
	FindAcceptanceReceipt(context.Context, string) (Receipt, bool, error)
	SetAcceptancePhase(context.Context, string, string, string, string) error
	FinalizeAcceptance(context.Context, AcceptanceDecision, Receipt, string, string) error
}

type acceptanceTransactionInfo interface {
	AcceptanceTransaction(context.Context, string) (string, string, error)
}

type protectedMetadataStore interface {
	SaveProtectedMetadata(context.Context, string, []ProtectedMetadataFact) error
	LoadProtectedMetadata(context.Context, string) ([]ProtectedMetadataFact, error)
}

type acceptanceRootIdentityStore interface {
	SaveAcceptanceRootIdentities(context.Context, string, string, string) error
}

type acceptanceJournal struct{ store AcceptanceStore }

func (j acceptanceJournal) Advance(ctx context.Context, id string, from, to TransactionPhase, reason string) error {
	return j.store.SetAcceptancePhase(ctx, id, string(from), string(to), reason)
}

func AcceptCandidate(ctx context.Context, c Candidate, review Review, decision AcceptanceDecision, goalID, actionID string, store AcceptanceStore, now time.Time) (Receipt, error) {
	if store == nil {
		return Receipt{}, errors.New("acceptance journal is unavailable")
	}
	unlock := lockProjectTransaction(c.FormalRoot)
	defer unlock()
	exists, existingReceipt, hasReceipt, err := store.CheckAcceptance(ctx, decision)
	if err != nil {
		return Receipt{}, err
	}
	if exists {
		if hasReceipt {
			return existingReceipt, nil
		}
		return Receipt{}, errors.New("acceptance is pending reconciliation")
	}
	if err = validateAcceptance(ctx, c, review, decision); err != nil {
		return Receipt{}, err
	}
	var metadataFacts []ProtectedMetadataFact
	var expectedRootIdentity, targetRootIdentity string
	if c.ManifestPolicy == ManifestPolicyProject {
		if err = ValidateProtectedMetadata(c.FormalRoot, c.CandidateRoot); err != nil {
			return Receipt{}, err
		}
		metadataFacts, err = CaptureProtectedMetadata(c.FormalRoot)
		if err != nil {
			return Receipt{}, err
		}
		expectedRootIdentity, err = CaptureRootIdentity(c.FormalRoot)
		if err != nil {
			return Receipt{}, err
		}
		targetRootIdentity, err = CaptureRootIdentity(c.CandidateRoot)
		if err != nil {
			return Receipt{}, err
		}
		if _, ok := store.(acceptanceRootIdentityStore); !ok {
			return Receipt{}, errors.New("transaction root identity journal is unavailable")
		}
		if _, ok := store.(protectedMetadataStore); !ok {
			return Receipt{}, errors.New("protected metadata journal is unavailable")
		}
	}
	created, err := store.SaveAcceptanceDecision(ctx, decision)
	if err != nil {
		return Receipt{}, err
	}
	if !created {
		if receipt, ok, findErr := store.FindAcceptanceReceipt(ctx, decision.ID); findErr != nil {
			return Receipt{}, findErr
		} else if ok {
			return receipt, nil
		}
		return Receipt{}, errors.New("acceptance is pending reconciliation")
	}
	if c.ManifestPolicy == ManifestPolicyProject {
		if err = store.(acceptanceRootIdentityStore).SaveAcceptanceRootIdentities(ctx, decision.ID, expectedRootIdentity, targetRootIdentity); err != nil {
			_ = store.SetAcceptancePhase(ctx, decision.ID, "prepared", "blocked", err.Error())
			return Receipt{}, err
		}
		metadataJournal := store.(protectedMetadataStore)
		if err = metadataJournal.SaveProtectedMetadata(ctx, decision.ID, metadataFacts); err != nil {
			_ = store.SetAcceptancePhase(ctx, decision.ID, "prepared", "blocked", err.Error())
			return Receipt{}, fmt.Errorf("protected metadata journal failed: %w", err)
		}
	}
	_, formalDigest, formalErr := BuildManifestForPolicy(c.FormalRoot, c.ManifestPolicy)
	_, candidateDigest, candidateErr := BuildManifestForPolicy(c.CandidateRoot, c.ManifestPolicy)
	if formalErr != nil || candidateErr != nil || formalDigest != decision.FormalDigest || candidateDigest != decision.CandidateDigest {
		reason := "project changed immediately before atomic exchange; review again"
		if formalErr != nil {
			reason = formalErr.Error()
		} else if candidateErr != nil {
			reason = candidateErr.Error()
		}
		_ = store.SetAcceptancePhase(ctx, decision.ID, "prepared", "blocked", reason)
		return Receipt{}, errors.New(reason)
	}
	mode := secfile.TransactionMode()
	rollback := filepath.Join(filepath.Dir(filepath.Clean(c.CandidateRoot)), ".stable-accept-rollback-"+decision.ID)
	if infoStore, ok := store.(acceptanceTransactionInfo); ok {
		if storedMode, storedRollback, infoErr := infoStore.AcceptanceTransaction(ctx, decision.ID); infoErr == nil {
			if storedMode != "" {
				mode = storedMode
			}
			if storedRollback != "" {
				rollback = storedRollback
			}
		}
	}
	tx := DirectoryTransaction{ID: decision.ID, Kind: TransactionAcceptance, ManifestPolicy: c.ManifestPolicy, ProtectedMetadata: metadataFacts, CurrentRoot: c.FormalRoot, IncomingRoot: c.CandidateRoot, RollbackRoot: rollback, ExpectedRootIdentity: expectedRootIdentity, TargetRootIdentity: targetRootIdentity, ExpectedDigest: decision.FormalDigest, TargetDigest: decision.CandidateDigest, ServiceRoot: filepath.Join(c.FormalRoot, ".stable"), Mode: mode}
	if err = NewTransactionCoordinator().applyLocked(ctx, tx, acceptanceJournal{store: store}); err != nil {
		_ = store.SetAcceptancePhase(ctx, decision.ID, "prepared", "blocked", err.Error())
		return Receipt{}, err
	}
	// Candidates never carry the .stable service subtree (see BuildManifest),
	// so the exchange moved the live session logs into the spent candidate
	// directory. Move them back before anyone appends to the transcript. A
	// crash before this move leaves the logs under the candidate root, where
	// acceptance recovery can still find them.
	var moveErr error
	if c.ManifestPolicy == ManifestPolicyProject {
		moveErr = RestoreProtectedMetadataFacts(c.FormalRoot, c.CandidateRoot, metadataFacts)
	} else {
		moveErr = RestoreServiceRoot(c.FormalRoot, c.CandidateRoot)
	}
	if moveErr != nil {
		_ = store.SetAcceptancePhase(ctx, decision.ID, "swapped", "blocked", moveErr.Error())
		return Receipt{}, fmt.Errorf("project exchanged; session state restore required: %w", moveErr)
	}
	if state, err := NewTransactionCoordinator().Inspect(ctx, tx, PhaseSwapped); err != nil || state != RecoveryNew {
		if err == nil {
			err = errors.New("accepted roots changed before finalization")
		}
		_ = store.SetAcceptancePhase(ctx, decision.ID, "swapped", "blocked", err.Error())
		return Receipt{}, err
	}
	_, acceptedDigest, err := BuildManifestForPolicy(c.FormalRoot, c.ManifestPolicy)
	if err != nil {
		return Receipt{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	receipt := Receipt{ID: "receipt-" + decision.ID, DecisionID: decision.ID, CandidateID: c.ID, FormalDigest: acceptedDigest, AcceptedAt: now.UTC()}
	if err = store.FinalizeAcceptance(ctx, decision, receipt, goalID, actionID); err != nil {
		return Receipt{}, fmt.Errorf("project exchanged; acceptance recovery required: %w", err)
	}
	return receipt, nil
}

func validateAcceptance(ctx context.Context, c Candidate, review Review, decision AcceptanceDecision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Status != "reviewed" && c.Status != "frozen" {
		return fmt.Errorf("candidate state %q cannot be accepted", c.Status)
	}
	candidatePolicy, err := normalizeManifestPolicy(c.ManifestPolicy)
	if err != nil {
		return err
	}
	if candidatePolicy == ManifestPolicyLegacy {
		if err := RejectLegacyGitMetadata(c.FormalRoot, c.CandidateRoot); err != nil {
			return err
		}
	}
	reviewPolicy, err := normalizeManifestPolicy(review.ManifestPolicy)
	if err != nil || reviewPolicy != candidatePolicy {
		return errors.New("review manifest policy does not match candidate")
	}
	if decision.ID == "" || decision.UserID == "" || decision.CandidateID != c.ID {
		return errors.New("acceptance decision is missing trusted identity or candidate")
	}
	if review.CandidateID != c.ID || decision.PreviewDigest != review.Digest || decision.CandidateDigest != review.CandidateDigest || decision.FormalDigest != review.FormalDigest {
		return errors.New("acceptance decision does not match the reviewed preview")
	}
	computedReviewDigest, err := ComputeReviewDigest(review)
	if err != nil || computedReviewDigest != review.Digest {
		return errors.New("persisted review content does not match its digest")
	}
	_, formalDigest, err := BuildManifestForPolicy(c.FormalRoot, c.ManifestPolicy)
	if err != nil {
		return err
	}
	if formalDigest != review.FormalDigest {
		return errors.New("formal project changed after preview; review again")
	}
	_, candidateDigest, err := BuildManifestForPolicy(c.CandidateRoot, c.ManifestPolicy)
	if err != nil {
		return err
	}
	if candidateDigest != review.CandidateDigest {
		return errors.New("candidate changed after preview; review again")
	}
	if decision.Mode == AcceptNormal {
		for _, finding := range review.Findings {
			if finding.Result != FindingPass {
				return fmt.Errorf("normal acceptance blocked by %s (%s)", finding.ID, finding.Result)
			}
		}
	} else if decision.Mode == AcceptForce {
		confirmed := map[string]bool{}
		for _, id := range decision.ConfirmedFindings {
			confirmed[id] = true
		}
		for _, finding := range review.Findings {
			if finding.Result != FindingPass && !confirmed[finding.ID] {
				return fmt.Errorf("force acceptance requires explicit confirmation of %s", finding.ID)
			}
		}
	} else {
		return errors.New("unknown acceptance mode")
	}
	confirmed := map[string]bool{}
	for _, id := range decision.ConfirmedFindings {
		confirmed[id] = true
	}
	for id := range confirmed {
		found := false
		for _, f := range review.Findings {
			if f.ID == id && f.Result != FindingPass {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("force confirmation %q does not match a failed or unavailable finding", id)
		}
	}
	return nil
}

// ExchangeProjectDir atomically swaps two project directories on one Linux
// filesystem. It refuses cross-device or symlink roots; callers must persist
// an application journal before invoking it and reconcile that journal after
// restart.
func ExchangeProjectDir(formalRoot, candidateRoot string) error {
	formalRoot, err := filepath.Abs(formalRoot)
	if err != nil {
		return err
	}
	candidateRoot, err = filepath.Abs(candidateRoot)
	if err != nil {
		return err
	}
	if err = secfile.ExchangeDirectories(formalRoot, candidateRoot); err != nil {
		if errors.Is(err, secfile.ErrUnsafePath) {
			return ErrUnsafePath
		}
		if errors.Is(err, secfile.ErrDifferentDevice) {
			return errors.New("formal and candidate directories are on different filesystems")
		}
		return err
	}
	return nil
}
