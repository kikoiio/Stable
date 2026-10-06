package candidate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"stable/internal/platform/secfile"
)

type TransactionKind string

const (
	TransactionAcceptance TransactionKind = "acceptance"
	TransactionRewind     TransactionKind = "rewind"
)

type TransactionPhase string

const (
	PhasePrepared        TransactionPhase = "prepared"
	PhaseOldSaved        TransactionPhase = "old_saved"
	PhaseTargetInstalled TransactionPhase = "target_installed"
	PhaseSwapped         TransactionPhase = "swapped"
	PhaseFinalized       TransactionPhase = "finalized"
	PhaseBlocked         TransactionPhase = "blocked"
)

type DirectoryTransaction struct {
	ID             string
	Kind           TransactionKind
	CurrentRoot    string
	IncomingRoot   string
	RollbackRoot   string
	ExpectedDigest string
	TargetDigest   string
	ServiceRoot    string
	Mode           string
}

type RecoveryState string

const (
	RecoveryOld          RecoveryState = "old"
	RecoveryNew          RecoveryState = "new"
	RecoveryIntermediate RecoveryState = "intermediate"
	RecoveryBlocked      RecoveryState = "blocked"
)

type TransactionJournal interface {
	Advance(context.Context, string, TransactionPhase, TransactionPhase, string) error
}

type TransactionCoordinator struct{}

func NewTransactionCoordinator() *TransactionCoordinator { return &TransactionCoordinator{} }

func (c *TransactionCoordinator) Apply(ctx context.Context, tx DirectoryTransaction, journal TransactionJournal) error {
	if err := validateTransaction(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if journal == nil {
		return errors.New("transaction journal is required")
	}
	if tx.Mode == "" {
		tx.Mode = secfile.TransactionMode()
	}
	if tx.Mode == "unsupported" {
		return secfile.ErrUnsupported
	}
	if err := verifyTransactionRoots(tx); err != nil {
		return err
	}
	if err := advance(ctx, journal, tx.ID, PhasePrepared, PhasePrepared, ""); err != nil {
		return err
	}
	if tx.Mode == "atomic-exchange" {
		if err := secfile.ExchangeDirectories(tx.CurrentRoot, tx.IncomingRoot); err != nil {
			return err
		}
	} else {
		if tx.RollbackRoot == "" {
			return errors.New("journaled transaction requires rollback root")
		}
		if err := prepareSibling(tx.CurrentRoot, tx.RollbackRoot); err != nil {
			return err
		}
		if err := secfile.MoveDirectory(tx.CurrentRoot, tx.RollbackRoot, false); err != nil {
			return err
		}
		if err := journal.Advance(ctx, tx.ID, PhasePrepared, PhaseOldSaved, ""); err != nil {
			return err
		}
		if err := secfile.MoveDirectory(tx.IncomingRoot, tx.CurrentRoot, false); err != nil {
			return err
		}
		if err := journal.Advance(ctx, tx.ID, PhaseOldSaved, PhaseTargetInstalled, ""); err != nil {
			return err
		}
		if err := secfile.MoveDirectory(tx.RollbackRoot, tx.IncomingRoot, false); err != nil {
			return err
		}
	}
	if err := verifyDigest(tx.CurrentRoot, tx.TargetDigest); err != nil {
		return err
	}
	return journal.Advance(ctx, tx.ID, phaseBeforeSwapped(tx), PhaseSwapped, "")
}

func (c *TransactionCoordinator) Inspect(_ context.Context, tx DirectoryTransaction, phase TransactionPhase) (RecoveryState, error) {
	if err := validateTransaction(tx); err != nil {
		return RecoveryBlocked, err
	}
	current, currentErr := digestState(tx.CurrentRoot)
	_, incomingErr := digestState(tx.IncomingRoot)
	_, rollbackErr := digestState(tx.RollbackRoot)
	if currentErr == nil && current == tx.TargetDigest {
		return RecoveryNew, nil
	}
	if currentErr == nil && current == tx.ExpectedDigest && (phase == PhasePrepared || phase == PhaseOldSaved) {
		return RecoveryOld, nil
	}
	if phase == PhaseOldSaved && incomingErr == nil && rollbackErr == nil {
		return RecoveryIntermediate, nil
	}
	if phase == PhaseTargetInstalled && currentErr == nil && incomingErr != nil && rollbackErr == nil {
		return RecoveryIntermediate, nil
	}
	if incomingErr == nil || rollbackErr == nil {
		return RecoveryIntermediate, nil
	}
	return RecoveryBlocked, fmt.Errorf("transaction phase %s has no recognizable old or new root", phase)
}

func (c *TransactionCoordinator) Recover(ctx context.Context, tx DirectoryTransaction, phase TransactionPhase, journal TransactionJournal) error {
	state, err := c.Inspect(ctx, tx, phase)
	if err != nil {
		return err
	}
	switch state {
	case RecoveryOld:
		if tx.IncomingRoot != "" {
			_ = os.RemoveAll(tx.IncomingRoot)
		}
		return journal.Advance(ctx, tx.ID, phase, PhaseFinalized, "old version remains")
	case RecoveryNew:
		if err := verifyDigest(tx.CurrentRoot, tx.TargetDigest); err != nil {
			return err
		}
		return journal.Advance(ctx, tx.ID, phase, PhaseSwapped, "new version installed")
	case RecoveryIntermediate:
		if tx.Mode != "journaled-move" {
			return fmt.Errorf("atomic transaction %s has an unrecognized intermediate state", tx.ID)
		}
		if phase == PhaseOldSaved {
			if err := secfile.MoveDirectory(tx.IncomingRoot, tx.CurrentRoot, false); err != nil {
				return err
			}
			if err := journal.Advance(ctx, tx.ID, PhaseOldSaved, PhaseTargetInstalled, "recovered target installation"); err != nil {
				return err
			}
			if err := secfile.MoveDirectory(tx.RollbackRoot, tx.IncomingRoot, false); err != nil {
				return err
			}
			return journal.Advance(ctx, tx.ID, PhaseTargetInstalled, PhaseSwapped, "recovered old root placement")
		}
		if phase == PhaseTargetInstalled {
			if err := secfile.MoveDirectory(tx.RollbackRoot, tx.IncomingRoot, false); err != nil {
				return err
			}
			return journal.Advance(ctx, tx.ID, PhaseTargetInstalled, PhaseSwapped, "recovered old root placement")
		}
		return fmt.Errorf("transaction %s intermediate phase %s cannot continue", tx.ID, phase)
	default:
		return fmt.Errorf("transaction %s recovery is blocked", tx.ID)
	}
}

func (c *TransactionCoordinator) Cleanup(_ context.Context, tx DirectoryTransaction) error {
	for _, path := range []string{tx.RollbackRoot, tx.IncomingRoot} {
		if path == "" {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

// RestoreServiceRoot moves the private runtime subtree from the spent
// candidate/rollback root back into the installed formal root after an
// acceptance exchange. It is a no-op when no service subtree exists.
func RestoreServiceRoot(formalRoot, candidateRoot string) error {
	source := filepath.Join(candidateRoot, ".stable")
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return secfile.ErrUnsafePath
	}
	return secfile.MoveDirectory(source, filepath.Join(formalRoot, ".stable"), false)
}

func validateTransaction(tx DirectoryTransaction) error {
	if tx.ID == "" || tx.CurrentRoot == "" || tx.IncomingRoot == "" || tx.ExpectedDigest == "" || tx.TargetDigest == "" {
		return errors.New("transaction identity and digests are required")
	}
	if filepath.Clean(tx.CurrentRoot) == filepath.Clean(tx.IncomingRoot) {
		return errors.New("transaction roots must be distinct")
	}
	if tx.Kind != TransactionAcceptance && tx.Kind != TransactionRewind {
		return errors.New("unsupported transaction kind")
	}
	return nil
}

func verifyTransactionRoots(tx DirectoryTransaction) error {
	current, err := secfile.OpenRoot(tx.CurrentRoot)
	if err != nil {
		return err
	}
	incoming, err := secfile.OpenRoot(tx.IncomingRoot)
	if err != nil {
		return err
	}
	if err = secfile.SameVolume(current.Path(), incoming.Path()); err != nil {
		return err
	}
	if err = current.Revalidate(); err != nil {
		return err
	}
	if err = incoming.Revalidate(); err != nil {
		return err
	}
	if err = verifyDigest(current.Path(), tx.ExpectedDigest); err != nil {
		return fmt.Errorf("current root digest mismatch: %w", err)
	}
	return verifyDigest(incoming.Path(), tx.TargetDigest)
}

func verifyDigest(root, expected string) error {
	_, got, err := BuildManifest(root)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("digest mismatch: got %s want %s", got, expected)
	}
	return nil
}

func digestState(root string) (string, error) {
	if root == "" {
		return "", os.ErrNotExist
	}
	_, digest, err := BuildManifest(root)
	return digest, err
}

func prepareSibling(current, rollback string) error {
	if filepath.Dir(filepath.Clean(current)) != filepath.Dir(filepath.Clean(rollback)) {
		return secfile.ErrDifferentDevice
	}
	if _, err := os.Lstat(rollback); err == nil {
		return os.ErrExist
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func phaseBeforeSwapped(tx DirectoryTransaction) TransactionPhase {
	if tx.Mode == "atomic-exchange" {
		return PhasePrepared
	}
	return PhaseTargetInstalled
}
func advance(ctx context.Context, j TransactionJournal, id string, from, to TransactionPhase, reason string) error {
	if from == to {
		return nil
	}
	return j.Advance(ctx, id, from, to, reason)
}
