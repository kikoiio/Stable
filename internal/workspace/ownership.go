package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"stable/internal/platform/proc"
	"stable/internal/platform/secfile"
)

const journalVersion = 1
const maxJournalBytes = 2 << 20

const unknownCreatingRootIdentityReason = "workspace create was interrupted before root identity was persisted; the unknown root is retained and lifecycle changes are blocked"

// RootIdentity identifies a physical directory, separately from stable project
// identity. Directory replacement must never let a journal adopt a new root.
type RootIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type Operation struct {
	ID         string               `json:"id"`
	Kind       string               `json:"kind"`
	Phase      string               `json:"phase"` // intent, complete, blocked
	Generation uint64               `json:"generation"`
	UpdatedAt  time.Time            `json:"updated_at"`
	Quarantine string               `json:"quarantine,omitempty"`
	Process    *proc.TrackedProcess `json:"process,omitempty"`
}

type Record struct {
	Version            int             `json:"version"`
	Scope              Scope           `json:"scope"`
	Snapshot           Snapshot        `json:"snapshot"`
	RootIdentity       RootIdentity    `json:"root_identity"`
	FormalRootIdentity RootIdentity    `json:"-"`
	Operation          Operation       `json:"operation"`
	UsedBytes          int64           `json:"used_bytes"`
	Resolution         *UserResolution `json:"resolution,omitempty"`
	Discard            *UserDiscard    `json:"discard,omitempty"`
}

// OwnershipStore persists resource intent separately from public session facts.
// A per-project journal survives creation failures and removal of the checkout.
type OwnershipStore struct {
	mu              sync.Mutex
	layout          *Layout
	journalIdentity secfile.Root
}

func (s *OwnershipStore) Close() error {
	if s == nil {
		return nil
	}
	return s.journalIdentity.Close()
}

func NewOwnershipStore(layout *Layout) (*OwnershipStore, error) {
	if layout == nil {
		return nil, ErrOwnership
	}
	if err := makePrivateDirectory(filepath.Join(layout.projectRoot(), ".journals")); err != nil {
		return nil, err
	}
	if _, err := layout.Paths("probe"); err != nil {
		return nil, err
	}
	identity, err := secfile.OpenRoot(filepath.Join(layout.projectRoot(), ".journals"))
	if err != nil {
		return nil, err
	}
	return &OwnershipStore{layout: layout, journalIdentity: identity}, nil
}

func (s *OwnershipStore) Create(ctx context.Context, scope Scope, id, label, operationID string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if err := scope.Validate(); err != nil {
		return Record{}, err
	}
	if scope.ProjectID != s.layout.ProjectID() || !ValidID(id) || !ValidID(operationID) {
		return Record{}, ErrOwnership
	}
	if err := ValidateLabel(label); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.load(id, false); err == nil {
		return Record{}, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}
	paths, err := s.layout.Paths(id)
	if err != nil {
		return Record{}, err
	}
	// Refuse an unowned preexisting root before persisting a create intent.
	if _, err := os.Lstat(paths.Root); err == nil {
		return Record{}, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, err
	}
	record := Record{Version: journalVersion, Scope: scope, Snapshot: Snapshot{ID: id, Label: label, SessionID: scope.SessionID, State: StateCreating, Generation: 1}, Operation: Operation{ID: operationID, Kind: "create", Phase: "intent", Generation: 1, UpdatedAt: time.Now().UTC()}}
	if err := s.save(record); err != nil {
		return Record{}, err
	}
	if err := ctx.Err(); err != nil {
		return record, err
	}
	paths, err = s.layout.Allocate(id)
	if err != nil {
		return record, err
	}
	info, err := os.Lstat(paths.Root)
	if err != nil {
		return record, err
	}
	record.RootIdentity, err = rootIdentity(info)
	if err != nil {
		return record, err
	}
	if err := s.save(record); err != nil {
		return record, err
	}
	return record, nil
}

func (s *OwnershipStore) Load(ctx context.Context, scope Scope, id string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if err := scope.Validate(); err != nil {
		return Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.load(id, true)
	if err != nil {
		return Record{}, err
	}
	if !record.Scope.SameOwner(scope) {
		return Record{}, ErrOwnership
	}
	return record, nil
}

// Records returns live resource records, capped at the approved workspace
// limit. Removed history is visited without retaining it in memory.
func (s *OwnershipStore) Records(ctx context.Context) ([]Record, error) {
	var records []Record
	err := s.Scan(ctx, func(record Record) error {
		if record.Snapshot.State == StateRemoved {
			return nil
		}
		if len(records) >= DefaultLimits().MaxWorkspaces {
			return ErrQuota
		}
		records = append(records, record)
		return nil
	})
	return records, err
}

// Scan streams journal facts for startup reconciliation. The callback must
// not call this store while its ownership lock is held.
func (s *OwnershipStore) Scan(ctx context.Context, visit func(Record) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	journalRoot, _, err := s.openJournal()
	if err != nil {
		return err
	}
	defer journalRoot.Close()
	dir, err := journalRoot.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(64)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			id := entry.Name()[:len(entry.Name())-len(".json")]
			record, err := s.load(id, false)
			if err != nil {
				return err
			}
			if err := visit(record); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return s.journalIdentity.Revalidate()
}

// Save expects the current generation. Physical identity is immutable; only
// the creating intent can acquire an identity after the directory is allocated.
func (s *OwnershipStore) Save(ctx context.Context, scope Scope, record Record, expectedGeneration uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := s.load(record.Snapshot.ID, false)
	if err != nil {
		return err
	}
	if !previous.Scope.SameOwner(scope) || !record.Scope.SameOwner(previous.Scope) || previous.Snapshot.Generation != expectedGeneration || record.Snapshot.Generation < expectedGeneration || record.Snapshot.Generation-expectedGeneration > 1 || !validTransition(previous.Snapshot.State, record.Snapshot.State) {
		return ErrOwnership
	}
	if previous.RootIdentity != record.RootIdentity || record.Scope.OriginRunID != previous.Scope.OriginRunID || record.Scope.OriginTaskID != previous.Scope.OriginTaskID || record.Snapshot.Label != previous.Snapshot.Label {
		return ErrOwnership
	}
	if record.Snapshot.State == StateInterrupted && record.RootIdentity.Inode == 0 {
		paths, err := s.layout.Paths(record.Snapshot.ID)
		if err != nil {
			return err
		}
		if hasUnknownCreatingRootIdentity(record) {
			if previous.Snapshot.State != StateCreating && !hasUnknownCreatingRootIdentity(previous) {
				return ErrOwnership
			}
			if record.Operation.Kind != "create" || record.Operation.Phase != "blocked" {
				return ErrOwnership
			}
			if _, err := os.Lstat(paths.Root); err != nil {
				return err
			}
		} else if _, err := os.Lstat(paths.Root); err == nil {
			return ErrOwnership
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if record.Snapshot.State != StateRemoved {
		if err := s.verifyRoot(record); err != nil {
			return err
		}
	} else {
		paths, err := s.layout.Paths(record.Snapshot.ID)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(paths.Root); err == nil {
			return errors.New("workspace cannot be marked removed while its root exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.save(record)
}

func validateRecord(record Record) error {
	if record.Version != journalVersion || record.Scope.Validate() != nil || !ValidID(record.Snapshot.ID) || record.Snapshot.SessionID != record.Scope.SessionID || !record.Snapshot.State.Valid() || record.Snapshot.Generation == 0 || ValidateLabel(record.Snapshot.Label) != nil || record.UsedBytes < 0 {
		return ErrOwnership
	}
	if !ValidID(record.Operation.ID) || !ValidID(record.Operation.Kind) || record.Operation.Generation != record.Snapshot.Generation {
		return ErrOwnership
	}
	if record.Operation.Quarantine != "" && (record.Operation.Kind != "remove" || !strings.HasPrefix(record.Operation.Quarantine, ".remove-") || !ValidID(strings.TrimPrefix(record.Operation.Quarantine, ".remove-"))) {
		return ErrOwnership
	}
	if record.Snapshot.Error == unknownCreatingRootIdentityReason && !hasUnknownCreatingRootIdentity(record) {
		return ErrOwnership
	}
	if process := record.Operation.Process; process != nil {
		if process.PID <= 0 || process.ProcessGroup != process.PID || process.StartTimeTicks == 0 || len(process.Token) != 64 || !ValidID(process.WorkspaceID) || process.WorkspaceID != record.Snapshot.ID || !ValidID(process.RunID) || process.RunID != record.Snapshot.WriterRunID || process.Generation != record.Snapshot.Generation || (record.Snapshot.State != StateWriting && record.Snapshot.State != StateStopping && record.Snapshot.State != StateBlocked && record.Snapshot.State != StateInterrupted) {
			return ErrOwnership
		}
	}
	switch record.Operation.Phase {
	case "intent", "complete", "blocked":
	default:
		return ErrOwnership
	}
	if record.RootIdentity.Inode == 0 && record.Snapshot.State != StateCreating && record.Snapshot.State != StateBlocked && record.Snapshot.State != StateInterrupted && record.Snapshot.State != StateRemoving && record.Snapshot.State != StateRemoved {
		return ErrOwnership
	}
	if len(record.Snapshot.Summary) > 8<<10 || len(record.Snapshot.Error) > 1024 || len(record.Snapshot.Conflicts) > 100 {
		return ErrQuota
	}
	for _, digest := range []string{record.Snapshot.BaselineDigest, record.Snapshot.FormalDigest, record.Snapshot.WorkspaceDigest} {
		if digest != "" && !validDigest(digest) {
			return ErrOwnership
		}
	}
	if !utf8.ValidString(record.Snapshot.Summary) || !utf8.ValidString(record.Snapshot.Error) || record.Snapshot.ChangedFiles < 0 || record.Snapshot.ConflictCount < 0 || record.Snapshot.ConflictCount < len(record.Snapshot.Conflicts) {
		return ErrOwnership
	}
	for _, path := range record.Snapshot.Conflicts {
		if clean, err := CleanRelative(path); err != nil || clean != path || ProtectedRoot(path) {
			return ErrUnsafePath
		}
	}
	if err := validateUserDecisions(record); err != nil {
		return err
	}
	return nil
}

func validTransition(from, to State) bool {
	if from == to {
		return true
	}
	if from == StateRemoved {
		return false
	}
	if to == StateBlocked || to == StateInterrupted {
		return true
	}
	switch from {
	case StateCreating:
		return to == StateReady || to == StateRemoving
	case StateReady, StateKept, StateExported:
		return to == StateWriting || to == StateExporting || to == StateRemoving || to == StateKept
	case StateWriting:
		return to == StateStopping || to == StateKept
	case StateStopping:
		return to == StateKept
	case StateExporting:
		return to == StateExported || to == StateKept
	case StateRemoving:
		return to == StateRemoved
	case StateBlocked, StateInterrupted:
		return to == StateKept || to == StateReady || to == StateRemoving || to == StateStopping
	}
	return false
}

func (s *OwnershipStore) openJournal() (*os.Root, os.FileInfo, error) {
	if err := s.journalIdentity.Revalidate(); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(s.layout.projectRoot(), ".journals")
	root, identity, err := openVerifiedRoot(path)
	if err != nil {
		return nil, nil, err
	}
	if err := s.journalIdentity.Revalidate(); err != nil {
		root.Close()
		return nil, nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(identity, current) {
		root.Close()
		return nil, nil, ErrOwnership
	}
	return root, identity, nil
}

func (s *OwnershipStore) load(id string, verify bool) (Record, error) {
	paths, err := s.layout.Paths(id)
	if err != nil {
		return Record{}, err
	}
	root, _, err := s.openJournal()
	if err != nil {
		return Record{}, err
	}
	defer root.Close()
	info, err := root.Lstat(filepath.Base(paths.Journal))
	if err != nil {
		return Record{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxJournalBytes {
		return Record{}, ErrOwnership
	}
	if err := validatePrivateFile(info, paths.Journal); err != nil {
		return Record{}, err
	}
	file, err := openRegular(root, filepath.Base(paths.Journal), info)
	if err != nil {
		return Record{}, err
	}
	defer file.Close()
	var record Record
	decoder := json.NewDecoder(io.LimitReader(file, maxJournalBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Record{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Record{}, ErrOwnership
	}
	if err := validateRecord(record); err != nil {
		return Record{}, err
	}
	if record.Scope.ProjectID != s.layout.ProjectID() || record.Snapshot.ID != id {
		return Record{}, ErrOwnership
	}
	if verify && record.Snapshot.State != StateRemoved && !hasUnknownCreatingRootIdentity(record) {
		if err := s.verifyRoot(record); err != nil {
			return Record{}, err
		}
	}
	return record, nil
}

func hasUnknownCreatingRootIdentity(record Record) bool {
	return record.Snapshot.Error == unknownCreatingRootIdentityReason &&
		record.Snapshot.State == StateInterrupted && record.RootIdentity == (RootIdentity{}) &&
		record.Operation.Kind == "create" && record.Operation.Phase == "blocked" && record.Operation.Process == nil
}

func validatePrivateFile(info os.FileInfo, path string) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrOwnership
	}
	// Check ownership and actual private mode while retaining the path needed
	// for Windows ACL checks. Hardlink validation is done by openRegular.
	ok, err := secfile.IsPrivatePath(path)
	if err != nil {
		return err
	}
	if !ok {
		return ErrOwnership
	}
	return nil
}

func (s *OwnershipStore) verifyRoot(record Record) error {
	if record.RootIdentity.Inode == 0 {
		return ErrOwnership
	}
	paths, err := s.layout.Paths(record.Snapshot.ID)
	if err != nil {
		return err
	}
	if err := validateAncestors(paths.Root, true); err != nil {
		return err
	}
	info, err := os.Lstat(paths.Root)
	if err != nil {
		return err
	}
	identity, err := rootIdentity(info)
	if err != nil {
		return err
	}
	if identity != record.RootIdentity {
		return ErrOwnership
	}
	return nil
}

func (s *OwnershipStore) save(record Record) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	paths, err := s.layout.Paths(record.Snapshot.ID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > maxJournalBytes {
		return ErrQuota
	}
	root, identity, err := s.openJournal()
	if err != nil {
		return err
	}
	defer root.Close()
	tempID, err := NewID()
	if err != nil {
		return err
	}
	temp := ".intent-" + tempID
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := revalidateRoot(filepath.Dir(paths.Journal), identity); err != nil {
		return err
	}
	if err := root.Rename(temp, filepath.Base(paths.Journal)); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("workspace journal renamed but directory sync failed: %w", err)
	}
	return revalidateRoot(filepath.Dir(paths.Journal), identity)
}
