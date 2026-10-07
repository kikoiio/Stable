package candidate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

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
	ID                string
	Kind              TransactionKind
	ManifestPolicy    string
	ProtectedMetadata []ProtectedMetadataFact
	CurrentRoot       string
	IncomingRoot      string
	RollbackRoot      string
	ExpectedDigest    string
	TargetDigest      string
	ServiceRoot       string
	Mode              string
}

type ProtectedMetadataFact struct {
	Name     string `json:"name"`
	Present  bool   `json:"present"`
	Type     string `json:"type,omitempty"`
	Identity string `json:"identity,omitempty"`
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
	if err := verifyDigestForPolicy(tx.CurrentRoot, tx.TargetDigest, tx.ManifestPolicy); err != nil {
		return err
	}
	return journal.Advance(ctx, tx.ID, phaseBeforeSwapped(tx), PhaseSwapped, "")
}

func (c *TransactionCoordinator) Inspect(_ context.Context, tx DirectoryTransaction, phase TransactionPhase) (RecoveryState, error) {
	if err := validateTransaction(tx); err != nil {
		return RecoveryBlocked, err
	}
	current, currentErr := digestStateForPolicy(tx.CurrentRoot, tx.ManifestPolicy)
	_, incomingErr := digestStateForPolicy(tx.IncomingRoot, tx.ManifestPolicy)
	_, rollbackErr := digestStateForPolicy(tx.RollbackRoot, tx.ManifestPolicy)
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
		if err := verifyDigestForPolicy(tx.CurrentRoot, tx.TargetDigest, tx.ManifestPolicy); err != nil {
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

// ValidateProtectedMetadata checks that project-v2 metadata is well-formed in
// the formal tree and absent from the incoming candidate. The former is moved
// back after the project exchange; the latter prevents candidate injection.
func ValidateProtectedMetadata(formalRoot, candidateRoot string) error {
	facts, err := CaptureProtectedMetadata(formalRoot)
	if err != nil {
		return err
	}
	return ValidateProtectedMetadataFacts(formalRoot, candidateRoot, facts)
}

func CaptureProtectedMetadata(formalRoot string) ([]ProtectedMetadataFact, error) {
	facts := make([]ProtectedMetadataFact, 0, 3)
	for _, name := range []string{".git", ".stable", ".mewcode"} {
		formalPath := filepath.Join(formalRoot, name)
		fact := ProtectedMetadataFact{Name: name}
		if info, err := os.Lstat(formalPath); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
				return nil, fmt.Errorf("protected metadata %s has an unsafe type: %w", name, secfile.ErrUnsafePath)
			}
			identity, err := fileIdentity(info)
			if err != nil {
				return nil, fmt.Errorf("cannot identify protected metadata %s: %w", name, err)
			}
			fact.Present = true
			fact.Identity = identity
			if info.IsDir() {
				fact.Type = "directory"
			} else {
				fact.Type = "file"
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

func ValidateProtectedMetadataFacts(formalRoot, candidateRoot string, facts []ProtectedMetadataFact) error {
	if len(facts) != 3 {
		return errors.New("protected metadata facts are incomplete")
	}
	for i, name := range []string{".git", ".stable", ".mewcode"} {
		fact := facts[i]
		if fact.Name != name || (fact.Present && (fact.Identity == "" || (fact.Type != "directory" && fact.Type != "file"))) {
			return errors.New("protected metadata facts are invalid")
		}
		formalPath := filepath.Join(formalRoot, name)
		info, err := os.Lstat(formalPath)
		if fact.Present {
			if err != nil {
				return fmt.Errorf("protected metadata %s disappeared: %w", name, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
				return fmt.Errorf("protected metadata %s has an unsafe type: %w", name, secfile.ErrUnsafePath)
			}
			if (fact.Type == "directory") != info.IsDir() {
				return fmt.Errorf("protected metadata %s type changed", name)
			}
			identity, idErr := fileIdentity(info)
			if idErr != nil || identity != fact.Identity {
				return fmt.Errorf("protected metadata %s identity changed", name)
			}
		} else if err == nil {
			return fmt.Errorf("unexpected formal metadata %s", name)
		} else if !os.IsNotExist(err) {
			return err
		}
		candidatePath := filepath.Join(candidateRoot, name)
		if _, err := os.Lstat(candidatePath); err == nil {
			return fmt.Errorf("candidate contains protected metadata %s: %w", name, secfile.ErrUnsafePath)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// RestoreProtectedMetadata is idempotent after directory exchange. A source
// entry may already have been moved during a previous recovery attempt; if
// both paths exist, the state is ambiguous and is left untouched.
func RestoreProtectedMetadata(formalRoot, spentCandidateRoot string) error {
	facts, err := CaptureProtectedMetadata(spentCandidateRoot)
	if err != nil {
		return err
	}
	return RestoreProtectedMetadataFacts(formalRoot, spentCandidateRoot, facts)
}

func RestoreProtectedMetadataFacts(formalRoot, spentCandidateRoot string, facts []ProtectedMetadataFact) error {
	if len(facts) != 3 {
		return errors.New("protected metadata facts are incomplete")
	}
	formal, err := secfile.OpenRoot(formalRoot)
	if err != nil {
		return err
	}
	spent, err := secfile.OpenRoot(spentCandidateRoot)
	if err != nil {
		return err
	}
	if err = secfile.SameVolume(formal.Path(), spent.Path()); err != nil {
		return err
	}
	formalRoot, spentCandidateRoot = formal.Path(), spent.Path()
	for _, name := range []string{".git", ".stable", ".mewcode"} {
		factIndex := map[string]int{".git": 0, ".stable": 1, ".mewcode": 2}[name]
		fact := facts[factIndex]
		if fact.Name != name || (fact.Present && (fact.Identity == "" || (fact.Type != "directory" && fact.Type != "file"))) {
			return errors.New("protected metadata facts are invalid")
		}
		if err := formal.Revalidate(); err != nil {
			return err
		}
		if err := spent.Revalidate(); err != nil {
			return err
		}
		source := filepath.Join(spentCandidateRoot, name)
		destination := filepath.Join(formalRoot, name)
		sourceInfo, sourceErr := os.Lstat(source)
		destinationInfo, destinationErr := os.Lstat(destination)
		if sourceErr != nil && !os.IsNotExist(sourceErr) {
			return sourceErr
		}
		if destinationErr != nil && !os.IsNotExist(destinationErr) {
			return destinationErr
		}
		if !fact.Present {
			if sourceErr == nil || destinationErr == nil {
				return fmt.Errorf("unexpected metadata appeared at %s", name)
			}
			continue
		}
		if os.IsNotExist(sourceErr) {
			if os.IsNotExist(destinationErr) {
				return fmt.Errorf("protected metadata %s is missing from both roots", name)
			}
			if destinationInfo.Mode()&os.ModeSymlink != 0 || (!destinationInfo.IsDir() && !destinationInfo.Mode().IsRegular()) {
				return fmt.Errorf("restored metadata %s has an unsafe type: %w", name, secfile.ErrUnsafePath)
			}
			if (fact.Type == "directory") != destinationInfo.IsDir() {
				return fmt.Errorf("restored metadata %s type does not match journal", name)
			}
			identity, err := fileIdentity(destinationInfo)
			if err != nil || identity != fact.Identity {
				return fmt.Errorf("restored metadata %s identity does not match journal", name)
			}
			continue
		}
		if destinationErr == nil {
			return fmt.Errorf("metadata restore conflict at %s: both source and destination exist", name)
		}
		if sourceInfo.Mode()&os.ModeSymlink != 0 || (!sourceInfo.IsDir() && !sourceInfo.Mode().IsRegular()) {
			return fmt.Errorf("saved metadata %s has an unsafe type: %w", name, secfile.ErrUnsafePath)
		}
		if (fact.Type == "directory") != sourceInfo.IsDir() {
			return fmt.Errorf("saved metadata %s type does not match journal", name)
		}
		identity, err := fileIdentity(sourceInfo)
		if err != nil || identity != fact.Identity {
			return fmt.Errorf("saved metadata %s identity does not match journal", name)
		}
		if sourceInfo.IsDir() {
			if err := secfile.MoveDirectory(source, destination, false); err != nil {
				return err
			}
		} else {
			if err := renameAndSync(source, destination); err != nil {
				return err
			}
		}
		if err := formal.Revalidate(); err != nil {
			return err
		}
		if err := spent.Revalidate(); err != nil {
			return err
		}
	}
	return nil
}

func renameAndSync(source, destination string) error {
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(source), filepath.Dir(destination)} {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func fileIdentity(info os.FileInfo) (string, error) {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return "", errors.New("filesystem identity is unavailable")
	}
	read := func(names ...string) (string, bool) {
		parts := make([]string, 0, len(names))
		for _, name := range names {
			field := value.FieldByName(name)
			if !field.IsValid() {
				return "", false
			}
			switch field.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				parts = append(parts, fmt.Sprint(field.Int()))
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				parts = append(parts, fmt.Sprint(field.Uint()))
			default:
				return "", false
			}
		}
		return strings.Join(parts, ":"), true
	}
	if identity, ok := read("Dev", "Ino"); ok {
		return "unix:" + identity, nil
	}
	if identity, ok := read("VolumeSerialNumber", "FileIndexHigh", "FileIndexLow"); ok {
		return "windows:" + identity, nil
	}
	return "", errors.New("filesystem identity is unsupported")
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
	if tx.Kind == TransactionAcceptance && tx.ManifestPolicy == ManifestPolicyProject {
		if err = ValidateProtectedMetadataFacts(current.Path(), incoming.Path(), tx.ProtectedMetadata); err != nil {
			return err
		}
	}
	if err = verifyDigestForPolicy(current.Path(), tx.ExpectedDigest, tx.ManifestPolicy); err != nil {
		return fmt.Errorf("current root digest mismatch: %w", err)
	}
	return verifyDigestForPolicy(incoming.Path(), tx.TargetDigest, tx.ManifestPolicy)
}

func verifyDigest(root, expected string) error {
	return verifyDigestForPolicy(root, expected, ManifestPolicyLegacy)
}

func verifyDigestForPolicy(root, expected, policy string) error {
	_, got, err := BuildManifestForPolicy(root, policy)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("digest mismatch: got %s want %s", got, expected)
	}
	return nil
}

func digestState(root string) (string, error) {
	return digestStateForPolicy(root, ManifestPolicyLegacy)
}

func digestStateForPolicy(root, policy string) (string, error) {
	if root == "" {
		return "", os.ErrNotExist
	}
	_, digest, err := BuildManifestForPolicy(root, policy)
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
