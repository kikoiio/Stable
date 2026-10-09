package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"stable/internal/agent"
	"stable/internal/permission"
	"stable/internal/platform/proc"
)

// IdleGuard is supplied by the conversation/runtime layer. Workspace binding
// changes are rejected unless no run can still be using the current binding.
type IdleGuard interface {
	CanSwitchWorkspace(context.Context, Scope) error
}

// BindingAdmission serializes lifecycle binding changes with run admission.
// The returned release function must be called after the complete persisted
// operation, not immediately after its idle check.
type BindingAdmission interface {
	LockWorkspaceBinding(context.Context, Scope) (func(), error)
}

// WriterStopper must return only after the sandbox process tree has exited.
// The service retains the lease if this operation fails or times out.
type WriterStopper interface {
	StopWorkspaceWriter(context.Context, WriterLease) error
}

// WorkspaceExporter performs the separately owned three-way merge and
// candidate freeze. It is deliberately not implemented by this lifecycle
// manager because only the candidate coordinator may create candidates.
type WorkspaceExporter interface {
	ExportWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error)
}

type WorkspacePreviewer interface {
	PreviewWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error)
}

type ServiceDependencies struct {
	IdleGuard IdleGuard
	Stopper   WriterStopper
	Exporter  WorkspaceExporter
}

// LifecycleService coordinates durable workspace ownership and resource
// components. It does not receive client paths or let a model manufacture an
// authority. Binding files are private service facts, independent of process
// cwd and the active run's already-derived authority.
type LifecycleService struct {
	mu                  sync.Mutex
	layout              *Layout
	store               *OwnershipStore
	git                 *PrivateGit
	materializer        *Materializer
	budget              *Budget
	limits              Limits
	deps                ServiceDependencies
	writers             map[string]WriterLease
	bindings            *os.Root
	bindingInfo         os.FileInfo
	unknownRootIdentity bool
	closed              bool
}

var _ Service = (*LifecycleService)(nil)

func NewService(layout *Layout, limits Limits, deps ServiceDependencies) (*LifecycleService, error) {
	if layout == nil {
		return nil, ErrOwnership
	}
	limits = limits.Normalized()
	store, err := NewOwnershipStore(layout)
	if err != nil {
		_ = layout.Close()
		return nil, err
	}
	git, err := NewPrivateGit(layout, store, limits)
	if err != nil {
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	bindingsPath := filepath.Join(layout.projectRoot(), ".bindings")
	if err := makePrivateDirectory(bindingsPath); err != nil {
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	bindings, bindingInfo, err := openVerifiedRoot(bindingsPath)
	if err != nil {
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	service := &LifecycleService{layout: layout, store: store, git: git,
		materializer: NewMaterializer(limits), budget: NewBudget(limits), limits: limits,
		deps: deps, writers: make(map[string]WriterLease), bindings: bindings, bindingInfo: bindingInfo}
	records, err := store.Records(context.Background())
	if err != nil {
		bindings.Close()
		_ = service.materializer.Close(context.Background())
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	if err := service.recoverInterruptedOperations(context.Background(), records); err != nil {
		bindings.Close()
		_ = service.materializer.Close(context.Background())
		_ = store.Close()
		_ = layout.Close()
		return nil, fmt.Errorf("recover workspace operations: %w", err)
	}
	records, err = store.Records(context.Background())
	if err != nil {
		bindings.Close()
		_ = service.materializer.Close(context.Background())
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	if err := service.reconcileUsage(context.Background(), records); err != nil {
		bindings.Close()
		_ = service.materializer.Close(context.Background())
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	records, err = store.Records(context.Background())
	if err != nil {
		bindings.Close()
		_ = service.materializer.Close(context.Background())
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	if err := service.budget.Restore(records); err != nil && !errors.Is(err, ErrQuota) {
		bindings.Close()
		_ = service.materializer.Close(context.Background())
		_ = store.Close()
		_ = layout.Close()
		return nil, err
	}
	return service, nil
}

// reconcileUsage recomputes durable accounting from owned files after recovery.
// A missing or unsafe checkout is retained and blocked instead of silently
// trusting stale journal byte counts.
func (s *LifecycleService) reconcileUsage(ctx context.Context, records []Record) error {
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Snapshot.State == StateRemoved {
			continue
		}
		paths, err := s.layout.Paths(record.Snapshot.ID)
		if err != nil {
			return err
		}
		if hasUnknownCreatingRootIdentity(record) {
			s.mu.Lock()
			s.unknownRootIdentity = true
			s.mu.Unlock()
			// Never open or scan a root whose physical identity was not saved.
			// Reserve the whole project budget so this unknown resource cannot
			// enable writes that exceed the project limit.
			used := s.limits.MaxProjectBytes
			if used != record.UsedBytes {
				updated := record
				updated.UsedBytes = used
				updated.Snapshot.Cursor++
				updated.Operation.UpdatedAt = time.Now().UTC()
				if err := s.store.Save(ctx, record.Scope, updated, record.Snapshot.Generation); err != nil {
					return err
				}
			}
			continue
		}
		if record.Snapshot.State == StateInterrupted && record.RootIdentity.Inode == 0 {
			if _, err := os.Lstat(paths.Root); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				s.markBlocked(record.Scope, record, fmt.Errorf("workspace startup root identity failed: %w", err))
				continue
			}
		}
		if record.Snapshot.State == StateInterrupted {
			incomplete := false
			for _, child := range []string{paths.Baseline, paths.Repository, paths.Checkout} {
				if _, err := os.Lstat(child); errors.Is(err, os.ErrNotExist) {
					incomplete = true
					break
				} else if err != nil {
					s.markBlocked(record.Scope, record, fmt.Errorf("workspace startup child identity failed: %w", err))
					incomplete = true
					break
				}
			}
			if incomplete {
				continue
			}
		}
		used, err := DiskUsage(ctx, paths.Root, s.limits)
		if err == nil {
			var manifest, baseline Manifest
			manifest, err = BuildManifest(ctx, paths.Checkout, s.limits)
			if err == nil {
				baseline, err = BuildManifest(ctx, paths.Baseline, s.limits)
			}
			if err == nil {
				updated := record
				updated.UsedBytes = used
				updated.Snapshot.WorkspaceDigest = manifest.Digest
				updated.Snapshot.ChangedFiles = changedManifestEntries(baseline.Entries, manifest.Entries)
				if updated.UsedBytes != record.UsedBytes || updated.Snapshot.WorkspaceDigest != record.Snapshot.WorkspaceDigest || updated.Snapshot.ChangedFiles != record.Snapshot.ChangedFiles {
					updated.Snapshot.Cursor++
					updated.Operation.UpdatedAt = time.Now().UTC()
					if err := s.store.Save(ctx, record.Scope, updated, record.Snapshot.Generation); err != nil {
						return err
					}
				}
				continue
			}
		}
		s.markBlocked(record.Scope, record, fmt.Errorf("workspace startup accounting failed: %w", err))
	}
	return nil
}

// recoverInterruptedOperations reconciles journal intents after the previous
// service process has exited. It never starts a model or writer command.
// Unknown roots remain blocked by identity checks instead of being adopted.
func (s *LifecycleService) recoverInterruptedOperations(ctx context.Context, records []Record) error {
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch record.Snapshot.State {
		case StateCreating, StateWriting, StateStopping, StateExporting, StateRemoving:
		case StateBlocked:
			if record.Operation.Process == nil {
				continue
			}
		default:
			continue
		}
		paths, err := s.layout.Paths(record.Snapshot.ID)
		if err != nil {
			return err
		}
		if record.Snapshot.State == StateWriting || record.Snapshot.State == StateStopping || record.Snapshot.State == StateBlocked && record.Operation.Process != nil {
			if record.Operation.Process == nil {
				if err := s.persistInterruptedWriter(ctx, record, "writer process identity is missing; workspace retained"); err != nil {
					return err
				}
				continue
			}
			if err := proc.StopTrackedProcess(*record.Operation.Process, 5*time.Second); err != nil {
				if persistErr := s.persistInterruptedWriter(ctx, record, "writer process identity could not be safely stopped: "+err.Error()); persistErr != nil {
					return persistErr
				}
				continue
			}
			if err := s.settleRecoveredWriter(ctx, record, paths); err != nil {
				if persistErr := s.persistInterruptedWriter(ctx, record, "writer stopped but workspace settlement failed: "+err.Error()); persistErr != nil {
					return persistErr
				}
			}
			continue
		}
		_, statErr := os.Lstat(paths.Root)
		if record.Snapshot.State == StateCreating && record.RootIdentity == (RootIdentity{}) && statErr == nil {
			// Allocate creates the root before its device/inode is durably saved.
			// If the service died in that narrow window, keep the path unknown:
			// do not adopt or delete it, but persist a queryable fail-closed fact.
			interrupted := record
			interrupted.Snapshot.State = StateInterrupted
			interrupted.Snapshot.Error = unknownCreatingRootIdentityReason
			interrupted.Snapshot.Cursor++
			interrupted.Operation.Phase = "blocked"
			interrupted.Operation.UpdatedAt = time.Now().UTC()
			if err := s.store.Save(ctx, record.Scope, interrupted, record.Snapshot.Generation); err != nil {
				return err
			}
			continue
		}
		if record.Snapshot.State == StateRemoving && errors.Is(statErr, os.ErrNotExist) {
			// Resolve the session binding before finalizing the removal. If the
			// binding cannot be read or removed, leave the journal in removing so
			// a later startup can retry this identity-checked cleanup.
			boundID, err := s.readBinding(record.Scope)
			if err != nil {
				return err
			}
			if boundID == record.Snapshot.ID {
				if err := s.writeBinding(record.Scope, ""); err != nil {
					return err
				}
			}
			removed := record
			removed.Snapshot.State = StateRemoved
			removed.Snapshot.WriterRunID = ""
			removed.Snapshot.Cursor++
			removed.Operation.Phase = "complete"
			removed.Operation.UpdatedAt = time.Now().UTC()
			if err := s.store.Save(ctx, record.Scope, removed, record.Snapshot.Generation); err != nil {
				return err
			}
			continue
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		interrupted := record
		interrupted.Snapshot.State = StateInterrupted
		interrupted.Snapshot.Error = "service restarted during " + record.Operation.Kind + "; resource retained for explicit recovery"
		interrupted.Snapshot.Cursor++
		interrupted.Operation.Phase = "blocked"
		interrupted.Operation.UpdatedAt = time.Now().UTC()
		if err := s.store.Save(ctx, record.Scope, interrupted, record.Snapshot.Generation); err != nil {
			return err
		}
	}
	return nil
}

func (s *LifecycleService) persistInterruptedWriter(ctx context.Context, record Record, reason string) error {
	updated := record
	updated.Snapshot.State = StateInterrupted
	updated.Snapshot.Error = boundedError(errors.New(reason))
	updated.Snapshot.Cursor++
	updated.Operation.Phase = "blocked"
	updated.Operation.UpdatedAt = time.Now().UTC()
	return s.store.Save(ctx, record.Scope, updated, record.Snapshot.Generation)
}

func (s *LifecycleService) settleRecoveredWriter(ctx context.Context, record Record, paths Paths) error {
	used, err := DiskUsage(ctx, paths.Root, s.limits)
	if err != nil {
		return err
	}
	manifest, err := BuildManifest(ctx, paths.Checkout, s.limits)
	if err != nil {
		return err
	}
	baseline, err := BuildManifest(ctx, paths.Baseline, s.limits)
	if err != nil {
		return err
	}
	updated := record
	updated.Snapshot.State = StateKept
	updated.Snapshot.WriterRunID = ""
	updated.Snapshot.WorkspaceDigest = manifest.Digest
	updated.Snapshot.ChangedFiles = changedManifestEntries(baseline.Entries, manifest.Entries)
	updated.Snapshot.Cursor++
	updated.Snapshot.Error = "writer process stopped during service recovery; workspace retained"
	updated.UsedBytes = used
	updated.Operation.Phase = "complete"
	updated.Operation.Process = nil
	updated.Operation.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(ctx, record.Scope, updated, record.Snapshot.Generation); err != nil {
		return err
	}
	return nil
}

func (s *LifecycleService) Create(ctx context.Context, scope Scope, label string) (Snapshot, error) {
	return s.create(ctx, scope, label, "")
}

// CreateWithID creates a workspace with a caller-supplied service-generated
// identity. It is used by durable lifecycle intents so replay can safely
// finish the same create instead of allocating a duplicate workspace.
func (s *LifecycleService) CreateWithID(ctx context.Context, scope Scope, label, id string) (Snapshot, error) {
	if !ValidID(id) {
		return Snapshot{}, ErrOwnership
	}
	return s.create(ctx, scope, label, id)
}

func (s *LifecycleService) create(ctx context.Context, scope Scope, label, requestedID string) (Snapshot, error) {
	if err := s.validateProjectAuthority(scope); err != nil {
		return Snapshot{}, err
	}
	if err := ValidateLabel(label); err != nil {
		return Snapshot{}, err
	}
	if requestedID != "" {
		if existing, loadErr := s.store.Load(ctx, scope, requestedID); loadErr == nil {
			if existing.Scope.SameOwner(scope) && existing.Snapshot.Label == label && existing.Snapshot.State == StateReady {
				return existing.Snapshot, nil
			}
			return Snapshot{}, ErrUnavailable
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Snapshot{}, ErrClosed
	}
	if s.unknownRootIdentity {
		s.mu.Unlock()
		return Snapshot{}, ErrQuota
	}
	id := requestedID
	if id == "" {
		var err error
		id, err = NewID()
		if err != nil {
			s.mu.Unlock()
			return Snapshot{}, err
		}
	}
	reservation, err := s.budget.ReserveCreate(id, 0)
	s.mu.Unlock()
	if err != nil {
		if requestedID != "" {
			if existing, loadErr := s.store.Load(ctx, scope, id); loadErr == nil {
				if existing.Scope.SameOwner(scope) && existing.Snapshot.Label == label && existing.Snapshot.State == StateReady {
					return existing.Snapshot, nil
				}
			}
		}
		return Snapshot{}, err
	}
	// Persist ownership before allocating any resource path. If queue admission
	// or materialization fails, the journal remains available for recovery.
	operationID, err := NewID()
	if err != nil {
		reservation.Release()
		return Snapshot{}, err
	}
	record, err := s.store.Create(ctx, scope, id, label, operationID)
	if err != nil {
		reservation.Release()
		return Snapshot{}, err
	}
	handle, err := s.materializer.SubmitPersisted(ctx, nil, func(jobCtx context.Context) error {
		_, err := s.git.Materialize(jobCtx, scope, id)
		return err
	})
	if err != nil {
		_ = reservation.Commit(0)
		s.failCreate(scope, record, err)
		return Snapshot{}, err
	}
	select {
	case err := <-handle.Results():
		if err != nil {
			_ = reservation.Commit(0)
			s.failCreate(scope, record, err)
			return Snapshot{}, err
		}
	case <-ctx.Done():
		handle.Cancel()
		// Await the actual materializer callback exit before settling ownership.
		<-handle.Results()
		_ = reservation.Commit(0)
		s.failCreate(scope, record, ctx.Err())
		return Snapshot{}, ctx.Err()
	}
	if err := reservation.Commit(0); err != nil {
		s.failCreate(scope, record, err)
		return Snapshot{}, err
	}
	gitState, err := s.git.Validate(ctx, scope, id)
	if err != nil {
		s.failCreate(scope, record, err)
		return Snapshot{}, err
	}
	paths, err := s.layout.Paths(id)
	if err != nil {
		s.failCreate(scope, record, err)
		return Snapshot{}, err
	}
	used, err := DiskUsage(ctx, paths.Root, s.limits)
	if err != nil {
		s.failCreate(scope, record, err)
		return Snapshot{}, err
	}
	updated := record
	updated.Snapshot.State = StateReady
	updated.Snapshot.BaselineDigest = gitState.BaselineDigest
	updated.Snapshot.WorkspaceDigest = gitState.BaselineDigest
	updated.Snapshot.Cursor++
	updated.UsedBytes = used
	updated.Operation.Phase = "complete"
	updated.Operation.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(ctx, scope, updated, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	if err := s.refreshBudget(ctx); err != nil {
		// Retain the durable resource but block use when accounting cannot prove
		// that project and per-workspace limits hold.
		s.markBlocked(scope, updated, err)
		return Snapshot{}, err
	}
	return updated.Snapshot, nil
}

func (s *LifecycleService) failCreate(scope Scope, record Record, cause error) {
	updated := record
	updated.Snapshot.State = StateBlocked
	updated.Snapshot.Error = boundedError(cause)
	updated.Snapshot.Cursor++
	updated.Operation.Phase = "blocked"
	updated.Operation.UpdatedAt = time.Now().UTC()
	_ = s.store.Save(context.Background(), scope, updated, record.Snapshot.Generation)
}

func (s *LifecycleService) Get(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	return record.Snapshot, nil
}

func (s *LifecycleService) List(ctx context.Context, scope Scope, cursor uint64, limit int) ([]Snapshot, error) {
	if err := scope.Validate(); err != nil || scope.ProjectID != s.layout.ProjectID() {
		return nil, ErrOwnership
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	records, err := s.store.Records(ctx)
	if err != nil {
		return nil, err
	}
	visible := make([]Snapshot, 0, min(limit, len(records)))
	for _, record := range records {
		if !record.Scope.SameOwner(scope) {
			continue
		}
		visible = append(visible, record.Snapshot)
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
	start := len(visible)
	if cursor < uint64(len(visible)) {
		start = int(cursor)
	}
	end := min(start+limit, len(visible))
	return visible[start:end], nil
}

func (s *LifecycleService) Enter(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	unlock, err := s.lockBindingOperation(ctx, scope)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	if err := s.guardSwitch(ctx, scope); err != nil {
		return Snapshot{}, err
	}
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if record.Snapshot.State != StateReady && record.Snapshot.State != StateKept && record.Snapshot.State != StateExported {
		return Snapshot{}, ErrOwnership
	}
	boundID, err := s.readBinding(scope)
	if err != nil {
		return Snapshot{}, err
	}
	if boundID != "" && boundID != id {
		return Snapshot{}, ErrOwnership
	}
	if _, err := s.git.Validate(ctx, scope, id); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	boundID, err = s.readBinding(scope)
	if err != nil {
		return Snapshot{}, err
	}
	if boundID != "" && boundID != id {
		return Snapshot{}, ErrOwnership
	}
	if err := s.writeBinding(scope, id); err != nil {
		return Snapshot{}, err
	}
	return record.Snapshot, nil
}

func (s *LifecycleService) Exit(ctx context.Context, scope Scope) (Snapshot, error) {
	unlock, err := s.lockBindingOperation(ctx, scope)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	id, err := s.readBinding(scope)
	if err != nil {
		return Snapshot{}, err
	}
	if id == "" {
		return Snapshot{}, nil
	}
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	_, writing := s.writers[id]
	s.mu.Unlock()
	if writing || record.Snapshot.State == StateWriting || record.Snapshot.State == StateStopping || record.Snapshot.WriterRunID != "" {
		if _, err := s.StopWriter(ctx, scope, id); err != nil {
			return Snapshot{}, err
		}
	}
	if err := s.guardSwitch(ctx, scope); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if current.Snapshot.State == StateWriting || current.Snapshot.State == StateStopping || current.Snapshot.WriterRunID != "" {
		return Snapshot{}, ErrUnavailable
	}
	currentID, err := s.readBinding(scope)
	if err != nil {
		return Snapshot{}, err
	}
	if currentID != id {
		return Snapshot{}, ErrOwnership
	}
	if err := s.writeBinding(scope, ""); err != nil {
		return Snapshot{}, err
	}
	return s.Get(ctx, scope, id)
}

func (s *LifecycleService) Keep(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	unlock, err := s.lockBindingOperation(ctx, scope)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	s.mu.Lock()
	_, writing := s.writers[id]
	s.mu.Unlock()
	if writing {
		if _, err := s.StopWriter(ctx, scope, id); err != nil {
			return Snapshot{}, err
		}
	}
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if record.Snapshot.State == StateKept {
		return record.Snapshot, nil
	}
	if record.Snapshot.State != StateReady && record.Snapshot.State != StateExported {
		return Snapshot{}, ErrOwnership
	}
	return s.transition(ctx, scope, record, StateKept)
}

func (s *LifecycleService) AcquireWriter(ctx context.Context, scope Scope, id, runID string) (WriterLease, error) {
	return s.acquireWriter(ctx, scope, id, runID, false)
}

// AcquireLeadWriter is restricted to a service-authenticated ordinary session
// or goal lead run whose exact workspace binding is held under binding admission.
// The lead authority and writer run ID are intentionally identical.
func (s *LifecycleService) AcquireLeadWriter(ctx context.Context, scope Scope, id, runID string) (WriterLease, error) {
	if (scope.Work.Kind != agent.WorkSession && scope.Work.Kind != agent.WorkGoal) || scope.OriginRunID != runID || scope.OriginTaskID != "" {
		return WriterLease{}, ErrOwnership
	}
	return s.acquireWriter(ctx, scope, id, runID, true)
}

func (s *LifecycleService) acquireWriter(ctx context.Context, scope Scope, id, runID string, allowSameRun bool) (WriterLease, error) {
	if err := scope.ValidateAuthority(); err != nil {
		return WriterLease{}, err
	}
	if scope.Authority.Mode == permission.ModePlan || scope.Authority.RunID == runID && !allowSameRun {
		return WriterLease{}, ErrOwnership
	}
	if !ValidID(runID) {
		return WriterLease{}, ErrOwnership
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return WriterLease{}, err
	}
	if s.closed || s.writers[id].RunID != "" {
		return WriterLease{}, ErrOwnership
	}
	if record.Snapshot.State != StateReady && record.Snapshot.State != StateKept && record.Snapshot.State != StateExported {
		return WriterLease{}, ErrOwnership
	}
	paths, err := s.layout.Paths(id)
	if err != nil {
		return WriterLease{}, err
	}
	if scope.ProjectID != s.layout.ProjectID() || filepath.Clean(scope.Authority.FormalRoot) != filepath.Clean(s.layout.FormalRoot()) || filepath.Clean(scope.Authority.AllowedRoot) != filepath.Clean(s.layout.FormalRoot()) {
		return WriterLease{}, ErrOwnership
	}
	if _, err := s.git.Validate(ctx, scope, id); err != nil {
		return WriterLease{}, err
	}
	updated := record
	updated.Snapshot.State = StateWriting
	updated.Snapshot.WriterRunID = runID
	updated.Snapshot.Generation++
	updated.Snapshot.Cursor++
	updated.Resolution, updated.Discard = nil, nil
	updated.Snapshot.PreviewID, updated.Snapshot.ResolutionID, updated.Snapshot.DiscardID, updated.Snapshot.DiscardDigest = "", "", "", ""
	updated.Snapshot.Conflicts, updated.Snapshot.DiscardPaths = nil, nil
	updated.Snapshot.ConflictCount, updated.Snapshot.ResolvedCount = 0, 0
	updated.Snapshot.ConflictNext = ""
	updated.Snapshot.CandidateID = ""
	updated.Operation = Operation{ID: mustID(), Kind: "writer", Phase: "complete", Generation: updated.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, scope, updated, record.Snapshot.Generation); err != nil {
		return WriterLease{}, err
	}
	writerAuthority := scope.Authority
	writerAuthority.RunID = runID
	writerAuthority.AllowedRoot = paths.Baseline
	writerAuthority.CandidateRoot = paths.Checkout
	writerAuthority.PlanFilePath = ""
	writerAuthority.Network = nil
	writerScope := scope
	writerScope.Authority = writerAuthority
	lease := WriterLease{WorkspaceID: id, RunID: runID, Generation: updated.Snapshot.Generation, Authority: writerAuthority, Paths: paths, Scope: writerScope}
	s.writers[id] = lease
	return lease, nil
}

// ReleaseCompletedWriter settles an inline child writer after its runner has
// returned and all sandboxed file-tool processes have exited. It does not
// cancel or stop a live run; callers must invoke it only after terminal.
func (s *LifecycleService) ReleaseCompletedWriter(ctx context.Context, lease WriterLease) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.writers[lease.WorkspaceID]
	if !ok {
		record, err := s.store.Load(ctx, lease.Scope, lease.WorkspaceID)
		if err == nil && record.Snapshot.Generation == lease.Generation && record.Snapshot.WriterRunID == "" && (record.Snapshot.State == StateKept || record.Snapshot.State == StateExported) {
			return record.Snapshot, nil
		}
	}
	if !ok || current.RunID != lease.RunID || current.Generation != lease.Generation || !current.Scope.SameOwner(lease.Scope) {
		return Snapshot{}, ErrOwnership
	}
	record, err := s.store.Load(ctx, lease.Scope, lease.WorkspaceID)
	if err != nil {
		return Snapshot{}, err
	}
	if record.Snapshot.State == StateStopping && record.Snapshot.WriterRunID == lease.RunID && record.Snapshot.Generation == lease.Generation {
		// StopWriter owns settlement after its stopper has confirmed exit.
		return record.Snapshot, nil
	}
	if record.Snapshot.State != StateWriting && record.Snapshot.State != StateBlocked || record.Snapshot.WriterRunID != lease.RunID || record.Snapshot.Generation != lease.Generation {
		return Snapshot{}, ErrOwnership
	}
	if record.Operation.Process != nil {
		blocked := record
		blocked.Snapshot.State = StateBlocked
		blocked.Snapshot.Error = "workspace sandbox process group exit was not confirmed; writer identity retained"
		blocked.Snapshot.Cursor++
		blocked.Operation.Phase = "blocked"
		blocked.Operation.UpdatedAt = time.Now().UTC()
		if err := s.store.Save(ctx, lease.Scope, blocked, record.Snapshot.Generation); err != nil {
			return Snapshot{}, err
		}
		return Snapshot{}, ErrUnavailable
	}
	paths, err := s.layout.Paths(lease.WorkspaceID)
	if err != nil {
		return Snapshot{}, err
	}
	used, err := DiskUsage(ctx, paths.Root, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	manifest, err := BuildManifest(ctx, paths.Checkout, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	baseline, err := BuildManifest(ctx, paths.Baseline, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	updated := record
	updated.Snapshot.State = StateKept
	updated.Snapshot.WriterRunID = ""
	updated.Snapshot.WorkspaceDigest = manifest.Digest
	updated.Snapshot.ChangedFiles = changedManifestEntries(baseline.Entries, manifest.Entries)
	updated.Snapshot.Cursor++
	updated.UsedBytes = used
	updated.Operation.Phase = "complete"
	updated.Operation.Process = nil
	updated.Operation.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(ctx, lease.Scope, updated, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	delete(s.writers, lease.WorkspaceID)
	if err := s.refreshBudget(ctx); err != nil {
		s.markBlocked(lease.Scope, updated, err)
		return Snapshot{}, err
	}
	return updated.Snapshot, nil
}

type writerWriteReservation struct {
	service *LifecycleService
	lease   WriterLease
	before  int64
	reserve *Reservation
	settled bool
}

func (s *LifecycleService) ReserveWriterWrite(ctx context.Context, lease WriterLease, growth int64) (WriteReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.writers[lease.WorkspaceID]
	if s.closed || !ok || current.RunID != lease.RunID || current.Generation != lease.Generation || current.Paths != lease.Paths || current.Authority.RunID != lease.Authority.RunID || !current.Scope.SameOwner(lease.Scope) {
		return nil, ErrOwnership
	}
	record, err := s.store.Load(ctx, lease.Scope, lease.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if record.Snapshot.State != StateWriting || record.Snapshot.WriterRunID != lease.RunID || record.Snapshot.Generation != lease.Generation {
		return nil, ErrOwnership
	}
	if err := s.git.validateFormalRoot(ctx, lease.Scope, lease.WorkspaceID); err != nil {
		return nil, err
	}
	reservation, err := s.budget.ReserveWrite(lease.WorkspaceID, growth)
	if err != nil {
		return nil, err
	}
	return &writerWriteReservation{service: s, lease: lease, reserve: reservation}, nil
}

// RegisterWriterProcess makes the process identity durable before command
// execution proceeds. A generation may have only one active command sandbox.
func (s *LifecycleService) RegisterWriterProcess(ctx context.Context, lease WriterLease, process proc.TrackedProcess) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.writers[lease.WorkspaceID]
	if s.closed || !ok || current.RunID != lease.RunID || current.Generation != lease.Generation || !current.Scope.SameOwner(lease.Scope) {
		return ErrOwnership
	}
	record, err := s.store.Load(ctx, lease.Scope, lease.WorkspaceID)
	if err != nil {
		return err
	}
	if record.Snapshot.State != StateWriting || record.Snapshot.WriterRunID != lease.RunID || record.Snapshot.Generation != lease.Generation || record.Operation.Process != nil {
		return ErrOwnership
	}
	if err := s.git.validateFormalRoot(ctx, lease.Scope, lease.WorkspaceID); err != nil {
		return err
	}
	if process.PID <= 0 || process.ProcessGroup != process.PID || process.StartTimeTicks == 0 || len(process.Token) != 64 || process.WorkspaceID != lease.WorkspaceID || process.RunID != lease.RunID || process.Generation != lease.Generation {
		return ErrOwnership
	}
	updated := record
	processCopy := process
	updated.Operation.Process = &processCopy
	updated.Operation.UpdatedAt = time.Now().UTC()
	return s.store.Save(ctx, lease.Scope, updated, record.Snapshot.Generation)
}

// ClearWriterProcess is called only after the sandbox manager has waited for
// the command process group to exit. Exact identity matching fences stale
// callbacks from a later command or workspace generation.
func (s *LifecycleService) ClearWriterProcess(ctx context.Context, lease WriterLease, process proc.TrackedProcess) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.writers[lease.WorkspaceID]
	if !ok || current.RunID != lease.RunID || current.Generation != lease.Generation || !current.Scope.SameOwner(lease.Scope) {
		return ErrOwnership
	}
	record, err := s.store.Load(ctx, lease.Scope, lease.WorkspaceID)
	if err != nil {
		return err
	}
	if record.Snapshot.WriterRunID != lease.RunID || record.Snapshot.Generation != lease.Generation || record.Operation.Process == nil || *record.Operation.Process != process {
		return ErrOwnership
	}
	updated := record
	updated.Operation.Process = nil
	updated.Operation.UpdatedAt = time.Now().UTC()
	return s.store.Save(ctx, lease.Scope, updated, record.Snapshot.Generation)
}

func (r *writerWriteReservation) Release() {
	if r == nil || r.settled {
		return
	}
	r.settled = true
	r.reserve.Release()
}

func (r *writerWriteReservation) Commit(ctx context.Context) error {
	if r == nil || r.service == nil || r.settled {
		return ErrOwnership
	}
	s := r.service
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.writers[r.lease.WorkspaceID]
	if !ok || current.RunID != r.lease.RunID || current.Generation != r.lease.Generation {
		r.Release()
		return ErrOwnership
	}
	paths, err := s.layout.Paths(r.lease.WorkspaceID)
	if err != nil {
		r.Release()
		return err
	}
	used, err := DiskUsage(ctx, paths.Root, s.limits)
	if err != nil {
		record, loadErr := s.store.Load(context.Background(), r.lease.Scope, r.lease.WorkspaceID)
		if loadErr == nil {
			s.markBlocked(r.lease.Scope, record, err)
		}
		r.Release()
		return err
	}
	manifest, err := BuildManifest(ctx, paths.Checkout, s.limits)
	if err != nil {
		record, loadErr := s.store.Load(context.Background(), r.lease.Scope, r.lease.WorkspaceID)
		if loadErr == nil {
			s.markBlocked(r.lease.Scope, record, err)
		}
		r.Release()
		return err
	}
	baseline, err := BuildManifest(ctx, paths.Baseline, s.limits)
	if err != nil {
		r.Release()
		return err
	}
	record, err := s.store.Load(ctx, r.lease.Scope, r.lease.WorkspaceID)
	if err != nil {
		r.Release()
		return err
	}
	if record.Snapshot.State != StateWriting && record.Snapshot.State != StateStopping || record.Snapshot.WriterRunID != r.lease.RunID || record.Snapshot.Generation != r.lease.Generation {
		r.Release()
		return ErrOwnership
	}
	updated := record
	updated.UsedBytes = used
	updated.Snapshot.WorkspaceDigest = manifest.Digest
	updated.Snapshot.ChangedFiles = changedManifestEntries(baseline.Entries, manifest.Entries)
	updated.Snapshot.Cursor++
	updated.Operation.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(ctx, r.lease.Scope, updated, record.Snapshot.Generation); err != nil {
		r.Release()
		return err
	}
	if err := r.reserve.Commit(used - record.UsedBytes); err != nil {
		r.settled = true
		s.markBlocked(r.lease.Scope, updated, err)
		return err
	}
	r.settled = true
	return nil
}

func changedManifestEntries(baseline, current []ManifestEntry) int {
	before := make(map[string]ManifestEntry, len(baseline))
	after := make(map[string]ManifestEntry, len(current))
	for _, entry := range baseline {
		before[entry.Path] = entry
	}
	for _, entry := range current {
		after[entry.Path] = entry
	}
	changed := 0
	for path, entry := range before {
		if got, ok := after[path]; !ok || got != entry {
			changed++
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			changed++
		}
	}
	return changed
}

// validateProjectAuthority binds worktree creation and writes to the exact
// formal project root represented by this manager. A narrower AllowedRoot
// would still snapshot sibling files into the checkout and widen child access.
func (s *LifecycleService) validateProjectAuthority(scope Scope) error {
	if err := scope.ValidateAuthority(); err != nil {
		return err
	}
	if scope.ProjectID != s.layout.ProjectID() || filepath.Clean(scope.Authority.FormalRoot) != filepath.Clean(s.layout.FormalRoot()) || filepath.Clean(scope.Authority.AllowedRoot) != filepath.Clean(s.layout.FormalRoot()) {
		return ErrOwnership
	}
	return nil
}

func (s *LifecycleService) StopWriter(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	if err := scope.Validate(); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.writers[id]
	if !ok {
		record, err := s.store.Load(ctx, scope, id)
		if err != nil {
			return Snapshot{}, err
		}
		if record.Snapshot.State == StateWriting || record.Snapshot.State == StateStopping || record.Snapshot.WriterRunID != "" {
			return Snapshot{}, ErrUnavailable
		}
		return record.Snapshot, nil
	}
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if record.Snapshot.State != StateWriting && record.Snapshot.State != StateBlocked || record.Snapshot.Generation != lease.Generation || record.Snapshot.WriterRunID != lease.RunID {
		return Snapshot{}, ErrOwnership
	}
	if s.deps.Stopper == nil {
		return Snapshot{}, ErrUnavailable
	}
	stopping := record
	stopping.Snapshot.State = StateStopping
	stopping.Snapshot.Cursor++
	process := stopping.Operation.Process
	stopping.Operation = Operation{ID: mustID(), Kind: "stop", Phase: "intent", Generation: stopping.Snapshot.Generation, UpdatedAt: time.Now().UTC(), Process: process}
	if err := s.store.Save(ctx, scope, stopping, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	// Tool settlement and runner completion require this same lock. Persist
	// stopping before dropping it, so no new tool reservation can begin.
	s.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	stopErr := s.deps.Stopper.StopWorkspaceWriter(stopCtx, lease)
	cancel()
	s.mu.Lock()
	latest, loadErr := s.store.Load(context.Background(), scope, id)
	if loadErr != nil {
		return Snapshot{}, loadErr
	}
	if latest.Snapshot.Generation != lease.Generation || latest.Snapshot.WriterRunID != lease.RunID {
		return Snapshot{}, ErrOwnership
	}
	stopping = latest
	if stopErr != nil {
		blocked := stopping
		blocked.Snapshot.State = StateBlocked
		blocked.Snapshot.Error = boundedError(stopErr)
		blocked.Operation.Phase = "blocked"
		blocked.Operation.UpdatedAt = time.Now().UTC()
		_ = s.store.Save(context.Background(), scope, blocked, stopping.Snapshot.Generation)
		return Snapshot{}, stopErr
	}
	stopping.Snapshot.State = StateKept
	stopping.Snapshot.WriterRunID = ""
	stopping.Snapshot.Cursor++
	stopping.Operation.Phase = "complete"
	stopping.Operation.Process = nil
	stopping.Operation.UpdatedAt = time.Now().UTC()
	paths, err := s.layout.Paths(id)
	if err != nil {
		return Snapshot{}, err
	}
	manifest, err := BuildManifest(ctx, paths.Checkout, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	stopping.Snapshot.WorkspaceDigest = manifest.Digest
	baseline, err := BuildManifest(ctx, paths.Baseline, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	stopping.Snapshot.ChangedFiles = changedManifestEntries(baseline.Entries, manifest.Entries)
	used, err := DiskUsage(ctx, paths.Root, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	stopping.UsedBytes = used
	if err := s.store.Save(ctx, scope, stopping, stopping.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	delete(s.writers, id)
	if err := s.refreshBudget(ctx); err != nil {
		s.markBlocked(scope, stopping, err)
		return Snapshot{}, err
	}
	return stopping.Snapshot, nil
}

func (s *LifecycleService) Export(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	if s.deps.Exporter == nil {
		return Snapshot{}, ErrUnavailable
	}
	if _, err := s.StopWriterIfActive(ctx, scope, id); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if record.Snapshot.State != StateReady && record.Snapshot.State != StateKept && record.Snapshot.State != StateExported {
		return Snapshot{}, ErrOwnership
	}
	gitState, err := s.git.Validate(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	record.FormalRootIdentity = gitState.FormalRootIdentity
	if record.Snapshot.State == StateExported && record.Snapshot.CandidateID != "" {
		paths, err := s.layout.Paths(id)
		if err != nil {
			return Snapshot{}, err
		}
		preview, err := BuildMergePreview(ctx, paths, s.limits)
		if err != nil {
			return Snapshot{}, err
		}
		if preview.BaselineDigest != record.Snapshot.BaselineDigest || preview.FormalDigest != record.Snapshot.FormalDigest || preview.WorkspaceDigest != record.Snapshot.WorkspaceDigest {
			return Snapshot{}, ErrSourceChanged
		}
		return record.Snapshot, nil
	}
	paths, err := s.layout.Paths(id)
	if err != nil {
		return Snapshot{}, err
	}
	exporting := record
	exporting.Snapshot.State = StateExporting
	exporting.Snapshot.Cursor++
	exporting.Operation = Operation{ID: mustID(), Kind: "export", Phase: "intent", Generation: exporting.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, scope, exporting, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	exported, err := s.deps.Exporter.ExportWorkspace(ctx, scope, exporting, paths)
	if err != nil {
		kept := exporting
		kept.Snapshot.State = StateKept
		kept.Snapshot.Error = boundedError(err)
		kept.Snapshot.Cursor++
		kept.Operation.Phase = "blocked"
		kept.Operation.UpdatedAt = time.Now().UTC()
		_ = s.store.Save(context.Background(), scope, kept, exporting.Snapshot.Generation)
		return Snapshot{}, err
	}
	if err := ValidateRootIdentity(paths.FormalRoot, record.FormalRootIdentity); err != nil {
		return Snapshot{}, err
	}
	if exported.ID != id || exported.CandidateID == "" || !ValidID(exported.CandidateID) {
		return Snapshot{}, ErrOwnership
	}
	exporting.Snapshot.CandidateID = exported.CandidateID
	exporting.Snapshot.BaselineDigest = exported.BaselineDigest
	exporting.Snapshot.FormalDigest = exported.FormalDigest
	exporting.Snapshot.WorkspaceDigest = exported.WorkspaceDigest
	exporting.Snapshot.ChangedFiles = exported.ChangedFiles
	exporting.Snapshot.Error = ""
	exporting.Snapshot.State = StateExported
	exporting.Snapshot.SessionID = scope.SessionID
	exporting.Snapshot.Generation = record.Snapshot.Generation
	exporting.Snapshot.Cursor = record.Snapshot.Cursor + 2
	exporting.Snapshot.Label = record.Snapshot.Label
	exporting.Operation.Phase = "complete"
	exporting.Operation.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(ctx, scope, exporting, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	return exporting.Snapshot, nil
}

// Preview stops any active writer before inspecting the frozen checkout. The
// returned snapshot contains only bounded paths and digests, never file data.
func (s *LifecycleService) Preview(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	previewer, ok := s.deps.Exporter.(WorkspacePreviewer)
	if !ok {
		return Snapshot{}, ErrUnavailable
	}
	if _, err := s.StopWriterIfActive(ctx, scope, id); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if record.Snapshot.State != StateReady && record.Snapshot.State != StateKept {
		return Snapshot{}, ErrOwnership
	}
	gitState, err := s.git.Validate(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	record.FormalRootIdentity = gitState.FormalRootIdentity
	paths, err := s.layout.Paths(id)
	if err != nil {
		return Snapshot{}, err
	}
	preview, err := previewer.PreviewWorkspace(ctx, scope, record, paths)
	if err != nil {
		return Snapshot{}, err
	}
	if err := ValidateRootIdentity(paths.FormalRoot, record.FormalRootIdentity); err != nil {
		return Snapshot{}, err
	}
	if preview.ID != id || preview.CandidateID != "" || preview.ConflictCount < len(preview.Conflicts) || preview.ConflictCount < 0 || len(preview.Conflicts) > 100 || !validDigest(preview.BaselineDigest) || !validDigest(preview.FormalDigest) || !validDigest(preview.WorkspaceDigest) {
		return Snapshot{}, ErrOwnership
	}
	preview.Label = record.Snapshot.Label
	preview.SessionID = scope.SessionID
	preview.State = record.Snapshot.State
	preview.Generation = record.Snapshot.Generation
	preview.Cursor = record.Snapshot.Cursor + 1
	preview.ChangedFiles = record.Snapshot.ChangedFiles
	if preview.ConflictCount > len(preview.Conflicts) && len(preview.Conflicts) > 0 {
		preview.ConflictNext = preview.Conflicts[len(preview.Conflicts)-1]
	}
	updated := record
	preview.PreviewID = mustID()
	updated.Resolution, updated.Discard = nil, nil
	if record.Snapshot.PreviewID != "" && record.Snapshot.BaselineDigest == preview.BaselineDigest && record.Snapshot.FormalDigest == preview.FormalDigest && record.Snapshot.WorkspaceDigest == preview.WorkspaceDigest {
		preview.PreviewID = record.Snapshot.PreviewID
		preview.ResolutionID, preview.ResolvedCount = record.Snapshot.ResolutionID, record.Snapshot.ResolvedCount
		updated.Resolution = record.Resolution
	}
	updated.Snapshot = preview
	updated.Operation = Operation{ID: mustID(), Kind: "preview", Phase: "complete", Generation: updated.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, scope, updated, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	return updated.Snapshot, nil
}

// RebindAcceptedFormalRoot replays one receipt-backed candidate acceptance
// transition across existing workspaces. Exact old-identity matching makes
// this operation idempotent and prevents it from adopting an unrelated root.
func (s *LifecycleService) RebindAcceptedFormalRoot(ctx context.Context, expected, target RootIdentity) error {
	if expected.Inode == 0 || target.Inode == 0 || expected == target {
		return ErrOwnership
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ValidateRootIdentity(s.layout.FormalRoot(), target); err != nil {
		return err
	}
	records, err := s.store.Records(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if record.Snapshot.State == StateRemoved || record.Snapshot.State == StateCreating || record.RootIdentity.Inode == 0 {
			continue
		}
		if _, err := s.git.ValidateForDiscard(ctx, record.Scope, record.Snapshot.ID); err != nil {
			return err
		}
		if err := s.git.rebindFormalRootIdentity(record.Scope, record.Snapshot.ID, expected, target); err != nil {
			return err
		}
	}
	return ValidateRootIdentity(s.layout.FormalRoot(), target)
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func (s *LifecycleService) StopWriterIfActive(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	s.mu.Lock()
	_, active := s.writers[id]
	s.mu.Unlock()
	if active {
		return s.StopWriter(ctx, scope, id)
	}
	return s.Get(ctx, scope, id)
}

func (s *LifecycleService) RemoveClean(ctx context.Context, scope Scope, id string) (Snapshot, error) {
	unlock, err := s.lockBindingOperation(ctx, scope)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.removableRecord(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	_, active := s.writers[id]
	if active || record.Snapshot.State == StateWriting || record.Snapshot.State == StateStopping {
		return Snapshot{}, ErrOwnership
	}
	if record.Snapshot.State != StateReady && record.Snapshot.State != StateKept {
		return Snapshot{}, ErrOwnership
	}
	boundID, err := s.readBinding(scope)
	if err != nil {
		return Snapshot{}, err
	}
	if boundID == id {
		return Snapshot{}, ErrOwnership
	}
	state, err := s.git.ValidateForDiscard(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	paths, err := s.layout.Paths(id)
	if err != nil {
		return Snapshot{}, err
	}
	manifest, err := BuildManifest(ctx, paths.Checkout, s.limits)
	if err != nil {
		return Snapshot{}, err
	}
	if manifest.Digest != state.BaselineDigest {
		return Snapshot{}, ErrOwnership
	}
	return s.removeRecordLocked(ctx, scope, record)
}

func (s *LifecycleService) removeRecordLocked(ctx context.Context, scope Scope, record Record) (Snapshot, error) {
	id := record.Snapshot.ID
	removing := record
	removing.Snapshot.State = StateRemoving
	removing.Snapshot.Cursor++
	removing.Operation = Operation{ID: mustID(), Kind: "remove", Phase: "intent", Generation: removing.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, scope, removing, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	if err := s.layout.projectIdentity.Revalidate(); err != nil {
		return Snapshot{}, err
	}
	project, projectInfo, err := openVerifiedRoot(s.layout.projectRoot())
	if err != nil {
		return Snapshot{}, err
	}
	currentProject, err := os.Lstat(s.layout.projectRoot())
	if err != nil || !os.SameFile(projectInfo, currentProject) {
		project.Close()
		return Snapshot{}, ErrOwnership
	}
	if err := project.RemoveAll(id); err != nil {
		project.Close()
		return Snapshot{}, err
	}
	if err := s.layout.projectIdentity.Revalidate(); err != nil {
		project.Close()
		return Snapshot{}, err
	}
	currentProject, err = os.Lstat(s.layout.projectRoot())
	if err != nil || !os.SameFile(projectInfo, currentProject) {
		project.Close()
		return Snapshot{}, ErrOwnership
	}
	if err := project.Close(); err != nil {
		return Snapshot{}, err
	}
	removing.Snapshot.State = StateRemoved
	removing.Snapshot.Cursor++
	removing.Operation.Phase = "complete"
	removing.Operation.UpdatedAt = time.Now().UTC()
	if err := s.store.Save(ctx, scope, removing, removing.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	_ = s.budget.Remove(id)
	return removing.Snapshot, nil
}

func (s *LifecycleService) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	materializerErr := s.materializer.Close(ctx)
	bindingErr := s.bindings.Close()
	storeErr := s.store.Close()
	layoutErr := s.layout.Close()
	return errors.Join(materializerErr, bindingErr, storeErr, layoutErr)
}

func (s *LifecycleService) guardSwitch(ctx context.Context, scope Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if s.deps.IdleGuard == nil {
		return ErrUnavailable
	}
	return s.deps.IdleGuard.CanSwitchWorkspace(ctx, scope)
}

func (s *LifecycleService) lockBindingOperation(ctx context.Context, scope Scope) (func(), error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if s.deps.IdleGuard == nil {
		return nil, ErrUnavailable
	}
	if admission, ok := s.deps.IdleGuard.(BindingAdmission); ok {
		return admission.LockWorkspaceBinding(ctx, scope)
	}
	return nil, ErrUnavailable
}

func (s *LifecycleService) transition(ctx context.Context, scope Scope, record Record, to State) (Snapshot, error) {
	updated := record
	updated.Snapshot.State = to
	updated.Snapshot.Cursor++
	updated.Operation = Operation{ID: mustID(), Kind: string(to), Phase: "complete", Generation: updated.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, scope, updated, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	return updated.Snapshot, nil
}

func (s *LifecycleService) writeBinding(scope Scope, workspaceID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := s.validateBindingsRoot(); err != nil {
		return err
	}
	name := scope.SessionID + ".json"
	if workspaceID == "" {
		if err := s.bindings.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncRoot(s.bindings)
	}
	binding := bindingRecord{Version: 1, Scope: scope, WorkspaceID: workspaceID}
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	temp := "." + scope.SessionID + "-" + mustID()
	file, err := s.bindings.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		_ = s.bindings.Remove(temp)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = s.bindings.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = s.bindings.Remove(temp)
		return err
	}
	if err := s.bindings.Rename(temp, name); err != nil {
		_ = s.bindings.Remove(temp)
		return err
	}
	return syncRoot(s.bindings)
}

func (s *LifecycleService) readBinding(scope Scope) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if err := s.validateBindingsRoot(); err != nil {
		return "", err
	}
	name := scope.SessionID + ".json"
	info, err := s.bindings.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Size() > 16<<10 {
		return "", ErrQuota
	}
	if err := validatePrivateFile(info, filepath.Join(s.layout.projectRoot(), ".bindings", name)); err != nil {
		return "", err
	}
	file, err := openRegular(s.bindings, name, info)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var binding bindingRecord
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return "", ErrOwnership
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return "", ErrOwnership
	}
	if binding.Version != 1 || !binding.Scope.SameOwner(scope) || !ValidID(binding.WorkspaceID) {
		return "", ErrOwnership
	}
	return binding.WorkspaceID, nil
}

func (s *LifecycleService) validateBindingsRoot() error {
	path := filepath.Join(s.layout.projectRoot(), ".bindings")
	if s.bindingInfo == nil {
		return ErrOwnership
	}
	return revalidateRoot(path, s.bindingInfo)
}

type bindingRecord struct {
	Version     int    `json:"version"`
	Scope       Scope  `json:"scope"`
	WorkspaceID string `json:"workspace_id"`
}

func (s *LifecycleService) refreshBudget(ctx context.Context) error {
	records, err := s.store.Records(ctx)
	if err != nil {
		return err
	}
	return s.budget.Restore(records)
}

func (s *LifecycleService) markBlocked(scope Scope, record Record, cause error) {
	updated := record
	updated.Snapshot.State = StateBlocked
	updated.Snapshot.Error = boundedError(cause)
	updated.Snapshot.Cursor++
	updated.Operation.Phase = "blocked"
	updated.Operation.UpdatedAt = time.Now().UTC()
	_ = s.store.Save(context.Background(), scope, updated, record.Snapshot.Generation)
}

func mustID() string {
	id, err := NewID()
	if err != nil {
		// Randomness failure must make subsequent journal writes fail closed.
		return "randomness-unavailable"
	}
	return id
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToValidUTF8(err.Error(), "�")
	if len(message) > 1024 {
		message = message[:1024]
		for !utf8.ValidString(message) && len(message) > 0 {
			message = message[:len(message)-1]
		}
	}
	return message
}

func syncRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Binding returns the server-persisted workspace ID for internal request
// scope construction. It does not expose a physical path or change cwd.
func (s *LifecycleService) Binding(scope Scope) (string, error) { return s.readBinding(scope) }
