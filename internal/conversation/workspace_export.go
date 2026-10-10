package conversation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"stable/internal/candidate"
	"stable/internal/platform/secfile"
	"stable/internal/store"
	"stable/internal/workspace"
)

// workspaceCandidateExporter turns an uncontested B/F/W merge into an
// ordinary frozen candidate. Candidate review and acceptance remain on the
// existing M03 path and are never performed by the workspace service.
type workspaceCandidateExporter struct {
	service *Service

	// afterCandidateEntry is an optional observation point for crash-boundary
	// tests. Production exporters leave it nil; callbacks must not alter files.
	afterCandidateEntry func(string)
	// syncCandidateDirectories is a per-exporter fault-injection seam for the
	// final candidate directory durability boundary. Production exporters leave
	// it nil; the callback must not alter files.
	syncCandidateDirectories func(string) error
	// afterExistingCandidateLookup is an optional test seam for mutations that
	// race an idempotent export lookup. Production exporters leave it nil.
	afterExistingCandidateLookup func(string)
}

func (e workspaceCandidateExporter) PreviewWorkspace(ctx context.Context, scope workspace.Scope, record workspace.Record, paths workspace.Paths) (workspace.Snapshot, error) {
	if e.service == nil || !record.Scope.SameOwner(scope) || scope.Validate() != nil || paths.FormalRoot == "" {
		return workspace.Snapshot{}, workspace.ErrOwnership
	}
	unlockFormalRoot := candidate.LockProjectTransaction(paths.FormalRoot)
	defer unlockFormalRoot()
	if err := workspace.ValidateRootIdentity(paths.FormalRoot, record.FormalRootIdentity); err != nil {
		return workspace.Snapshot{}, err
	}
	baseline, err := workspace.BuildManifest(ctx, paths.Baseline, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	formal, err := workspace.BuildManifest(ctx, paths.FormalRoot, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	working, err := workspace.BuildManifest(ctx, paths.Checkout, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	preview, err := workspace.ThreeWayPreview(baseline, formal, working, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if err := workspace.ValidateRootIdentity(paths.FormalRoot, record.FormalRootIdentity); err != nil {
		return workspace.Snapshot{}, err
	}
	pathsOnly := make([]string, 0, min(100, len(preview.Conflicts)))
	for _, conflict := range preview.Conflicts {
		if len(pathsOnly) == cap(pathsOnly) {
			break
		}
		pathsOnly = append(pathsOnly, conflict.Path)
	}
	return workspace.Snapshot{
		ID: record.Snapshot.ID, BaselineDigest: preview.BaselineDigest, FormalDigest: preview.FormalDigest,
		WorkspaceDigest: preview.WorkspaceDigest, ConflictCount: len(preview.Conflicts), Conflicts: pathsOnly,
	}, nil
}

func (e workspaceCandidateExporter) ExportWorkspace(ctx context.Context, scope workspace.Scope, record workspace.Record, paths workspace.Paths) (workspace.Snapshot, error) {
	if e.service == nil || e.service.deps.Store == nil || !record.Scope.SameOwner(scope) || scope.Validate() != nil {
		return workspace.Snapshot{}, workspace.ErrOwnership
	}
	formalRoot := paths.FormalRoot
	if formalRoot == "" || !filepath.IsAbs(formalRoot) {
		return workspace.Snapshot{}, workspace.ErrOwnership
	}
	// Serialize the complete formal-root observation and candidate creation
	// window with acceptance. This guard is outermost for formal-root work;
	// do not acquire workspace lifecycle locks while holding it.
	unlockFormalRoot := candidate.LockProjectTransaction(formalRoot)
	defer unlockFormalRoot()
	if err := workspace.ValidateRootIdentity(formalRoot, record.FormalRootIdentity); err != nil {
		return workspace.Snapshot{}, err
	}
	baseline, err := workspace.BuildManifest(ctx, paths.Baseline, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	formal, err := workspace.BuildManifest(ctx, formalRoot, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	working, err := workspace.BuildManifest(ctx, paths.Checkout, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	preview, err := workspace.ThreeWayPreview(baseline, formal, working, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if len(preview.Conflicts) != 0 {
		if !record.Resolution.Matches(record, preview) {
			return workspace.Snapshot{}, fmt.Errorf("workspace export has %d unresolved path conflicts; a current user resolution is required", len(preview.Conflicts))
		}
		preview.Manifest, err = workspace.ResolveThreeWay(preview, record.Resolution.Choices, workspace.DefaultLimits())
		if err != nil {
			return workspace.Snapshot{}, err
		}
	}
	// Bind candidate identity to all three inputs. A retry after a crash finds
	// the same candidate; a changed formal tree creates a distinct candidate.
	identity := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", record.Snapshot.ID, record.Snapshot.Generation, preview.BaselineDigest, preview.FormalDigest, preview.WorkspaceDigest, preview.Manifest.Digest)
	idDigest := sha256.Sum256([]byte(identity))
	candidateID := "worktree-" + hex.EncodeToString(idDigest[:16])
	actionID := "workspace-export-" + hex.EncodeToString(idDigest[16:24])
	goalID := scope.Work.GoalID
	if goalID == "" {
		goalID = "session-" + scope.SessionID
	}
	candidateParent := filepath.Join(filepath.Dir(formalRoot), ".stable-candidates")
	if existing, getErr := e.service.deps.Store.GetCandidate(ctx, candidateID); getErr == nil {
		if e.afterExistingCandidateLookup != nil {
			e.afterExistingCandidateLookup(candidateID)
		}
		// The project root and its inputs can change independently of this
		// process-local lock while the candidate lookup is in flight. Do not
		// return an idempotent result unless it still refers to the captured
		// formal root and B/F/W snapshot.
		if err := workspace.ValidateRootIdentity(formalRoot, record.FormalRootIdentity); err != nil {
			return workspace.Snapshot{}, err
		}
		formalAfter, err := workspace.BuildManifest(ctx, formalRoot, workspace.DefaultLimits())
		if err != nil {
			return workspace.Snapshot{}, err
		}
		workingAfter, err := workspace.BuildManifest(ctx, paths.Checkout, workspace.DefaultLimits())
		if err != nil {
			return workspace.Snapshot{}, err
		}
		baselineAfter, err := workspace.BuildManifest(ctx, paths.Baseline, workspace.DefaultLimits())
		if err != nil {
			return workspace.Snapshot{}, err
		}
		if formalAfter.Digest != formal.Digest || workingAfter.Digest != working.Digest || baselineAfter.Digest != baseline.Digest {
			return workspace.Snapshot{}, workspace.ErrSourceChanged
		}
		if existing.GoalID != goalID || existing.ActionID != actionID || existing.Candidate.FormalRoot != formalRoot || filepath.Clean(existing.Candidate.CandidateRoot) != filepath.Join(candidateParent, candidateID) || existing.Candidate.ManifestPolicy != candidate.ManifestPolicyProject || existing.Candidate.BaselineDigest != formal.Digest {
			return workspace.Snapshot{}, workspace.ErrOwnership
		}
		_, digest, digestErr := candidate.BuildManifestForPolicy(existing.Candidate.CandidateRoot, candidate.ManifestPolicyProject)
		if digestErr != nil || digest != existing.Candidate.CandidateDigest {
			return workspace.Snapshot{}, workspace.ErrSourceChanged
		}
		return workspace.Snapshot{ID: record.Snapshot.ID, CandidateID: candidateID, State: workspace.StateExported, BaselineDigest: baseline.Digest, FormalDigest: formal.Digest, WorkspaceDigest: working.Digest, ChangedFiles: record.Snapshot.ChangedFiles}, nil
	} else if !errors.Is(getErr, sql.ErrNoRows) {
		return workspace.Snapshot{}, getErr
	}
	if err := secfile.MkdirAllPrivate(candidateParent, 0700); err != nil {
		return workspace.Snapshot{}, err
	}
	created, err := candidate.CreateCandidateForPolicy(candidateID, formalRoot, candidateParent, candidate.ManifestPolicyProject)
	if err != nil {
		return workspace.Snapshot{}, err
	}
	createdIdentity, identityErr := candidate.CaptureRootIdentity(created.CandidateRoot)
	if identityErr != nil {
		// The path exists, but without an identity captured by the operation we
		// cannot prove that it is safe to remove. Leave it for reconciliation.
		return workspace.Snapshot{}, identityErr
	}
	cleanupPartial := func(cause error) error {
		// SaveCandidate may have committed despite returning an error. Preserve
		// the directory whenever a record exists or the store cannot prove that
		// no record exists; otherwise a ready candidate could lose its root.
		_, lookupErr := e.service.deps.Store.GetCandidate(context.Background(), candidateID)
		if lookupErr == nil || !errors.Is(lookupErr, sql.ErrNoRows) {
			return cause
		}
		if cleanupErr := removeOwnedPartialCandidate(created.CandidateRoot, createdIdentity); cleanupErr != nil {
			return errors.Join(cause, cleanupErr)
		}
		return cause
	}
	if err := installMergedManifestObserved(ctx, created.CandidateRoot, formalRoot, paths.Checkout, formal, working, preview.Manifest, e.afterCandidateEntry); err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	syncCandidateDirectories := e.syncCandidateDirectories
	if syncCandidateDirectories == nil {
		syncCandidateDirectories = candidate.SyncDirectoryTree
	}
	if err := syncCandidateDirectories(created.CandidateRoot); err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	if err := workspace.ValidateRootIdentity(formalRoot, record.FormalRootIdentity); err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	formalAfter, err := workspace.BuildManifest(ctx, formalRoot, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	workingAfter, err := workspace.BuildManifest(ctx, paths.Checkout, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	baselineAfter, err := workspace.BuildManifest(ctx, paths.Baseline, workspace.DefaultLimits())
	if err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	if formalAfter.Digest != preview.FormalDigest || workingAfter.Digest != preview.WorkspaceDigest || baselineAfter.Digest != preview.BaselineDigest {
		return workspace.Snapshot{}, cleanupPartial(workspace.ErrSourceChanged)
	}
	entries, candidateDigest, err := candidate.BuildManifestForPolicy(created.CandidateRoot, candidate.ManifestPolicyProject)
	if err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	if candidateDigest != preview.Manifest.Digest || len(entries) != len(preview.Manifest.Entries) {
		return workspace.Snapshot{}, cleanupPartial(workspace.ErrSourceChanged)
	}
	if err := workspace.ValidateRootIdentity(formalRoot, record.FormalRootIdentity); err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	frozen, err := candidate.FreezeCandidate(created, nil, ctx)
	if err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	if frozen.CandidateDigest != candidateDigest {
		return workspace.Snapshot{}, cleanupPartial(workspace.ErrSourceChanged)
	}
	if err := workspace.ValidateRootIdentity(formalRoot, record.FormalRootIdentity); err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	// Persist the existing candidate lifecycle's reviewable state after the
	// freezer has sealed and verified the merged manifest.
	frozen.Status = "ready"
	frozen.ManifestPolicy = candidate.ManifestPolicyProject
	if err := e.service.deps.Store.SaveCandidate(ctx, store.CandidateRecord{
		Candidate: frozen, ActionID: actionID, GoalID: goalID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return workspace.Snapshot{}, cleanupPartial(err)
	}
	return workspace.Snapshot{ID: record.Snapshot.ID, CandidateID: candidateID, State: workspace.StateExported, BaselineDigest: baseline.Digest, FormalDigest: formal.Digest, WorkspaceDigest: working.Digest, ChangedFiles: record.Snapshot.ChangedFiles}, nil
}

// removeOwnedPartialCandidate only removes a candidate root when its current
// filesystem identity still matches the identity captured immediately after
// this export created it. Unknown or replaced paths are retained.
func removeOwnedPartialCandidate(path, expectedIdentity string) error {
	if expectedIdentity == "" {
		return workspace.ErrOwnership
	}
	actualIdentity, err := candidate.CaptureRootIdentity(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || actualIdentity != expectedIdentity {
		return workspace.ErrOwnership
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return candidate.SyncDirectory(filepath.Dir(path))
}

func installMergedManifest(ctx context.Context, target, formalRoot, workspaceRoot string, formal, working, merged workspace.Manifest) error {
	return installMergedManifestObserved(ctx, target, formalRoot, workspaceRoot, formal, working, merged, nil)
}

func installMergedManifestObserved(ctx context.Context, target, formalRoot, workspaceRoot string, formal, working, merged workspace.Manifest, afterEntry func(string)) error {
	if _, err := workspace.ManifestDigest(merged.Entries); err != nil {
		return err
	}
	formalEntries := make(map[string]workspace.ManifestEntry, len(formal.Entries))
	workingEntries := make(map[string]workspace.ManifestEntry, len(working.Entries))
	for _, entry := range formal.Entries {
		formalEntries[entry.Path] = entry
	}
	for _, entry := range working.Entries {
		workingEntries[entry.Path] = entry
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	rootIdentity, err := root.Stat(".")
	if err != nil {
		return err
	}
	pathIdentity, err := os.Lstat(target)
	if err != nil || !pathIdentity.IsDir() || pathIdentity.Mode()&os.ModeSymlink != 0 || !os.SameFile(rootIdentity, pathIdentity) {
		return workspace.ErrOwnership
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	names, err := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, name := range names {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	formalSecure, err := secfile.OpenRoot(formalRoot)
	if err != nil {
		return err
	}
	workingSecure, err := secfile.OpenRoot(workspaceRoot)
	if err != nil {
		return err
	}
	defer formalSecure.Close()
	defer workingSecure.Close()
	for _, entry := range merged.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := formalSecure
		if candidateEntryMatches(formalEntries[entry.Path], entry) {
			// Keep the current formal bytes when the merge selected that exact
			// version; otherwise the selected version must exist in the checkout.
		} else if candidateEntryMatches(workingEntries[entry.Path], entry) {
			source = workingSecure
		} else {
			return workspace.ErrSourceChanged
		}
		input, err := source.Open(entry.Path)
		if err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(entry.Path)), 0700); err != nil {
			input.Close()
			return err
		}
		output, err := root.OpenFile(filepath.FromSlash(entry.Path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(entry.Mode))
		if err != nil {
			input.Close()
			return err
		}
		h := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, h), io.LimitReader(input, entry.Size+1))
		if copyErr == nil && (written != entry.Size || hex.EncodeToString(h.Sum(nil)) != entry.Digest) {
			copyErr = workspace.ErrSourceChanged
		}
		if copyErr == nil {
			copyErr = output.Sync()
		}
		if closeErr := output.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if closeErr := input.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return copyErr
		}
		if err := secfile.ChmodRoot(root, filepath.FromSlash(entry.Path), os.FileMode(entry.Mode)); err != nil {
			return err
		}
		modeFile, err := root.OpenFile(filepath.FromSlash(entry.Path), os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		syncErr := modeFile.Sync()
		closeErr := modeFile.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		if afterEntry != nil {
			afterEntry(entry.Path)
		}
	}
	currentIdentity, err := os.Lstat(target)
	if err != nil || !os.SameFile(rootIdentity, currentIdentity) {
		return workspace.ErrOwnership
	}
	return nil
}

func candidateEntryMatches(a, b workspace.ManifestEntry) bool {
	return a.Path == b.Path && a.Mode == b.Mode && a.Size == b.Size && a.Digest == b.Digest
}
