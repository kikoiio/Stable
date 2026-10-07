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
type workspaceCandidateExporter struct{ service *Service }

func (e workspaceCandidateExporter) PreviewWorkspace(ctx context.Context, scope workspace.Scope, record workspace.Record, paths workspace.Paths) (workspace.Snapshot, error) {
	if e.service == nil || !record.Scope.SameOwner(scope) || scope.Validate() != nil || paths.FormalRoot == "" {
		return workspace.Snapshot{}, workspace.ErrOwnership
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
	if err := installMergedManifest(ctx, created.CandidateRoot, formalRoot, paths.Checkout, formal, working, preview.Manifest); err != nil {
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
	if formalAfter.Digest != preview.FormalDigest || workingAfter.Digest != preview.WorkspaceDigest || baselineAfter.Digest != preview.BaselineDigest {
		return workspace.Snapshot{}, workspace.ErrSourceChanged
	}
	entries, candidateDigest, err := candidate.BuildManifestForPolicy(created.CandidateRoot, candidate.ManifestPolicyProject)
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if candidateDigest != preview.Manifest.Digest || len(entries) != len(preview.Manifest.Entries) {
		return workspace.Snapshot{}, workspace.ErrSourceChanged
	}
	created.CandidateDigest = candidateDigest
	created.Status = "ready"
	created.ManifestPolicy = candidate.ManifestPolicyProject
	if err := e.service.deps.Store.SaveCandidate(ctx, store.CandidateRecord{
		Candidate: created, ActionID: actionID, GoalID: goalID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return workspace.Snapshot{}, err
	}
	return workspace.Snapshot{ID: record.Snapshot.ID, CandidateID: candidateID, State: workspace.StateExported, BaselineDigest: baseline.Digest, FormalDigest: formal.Digest, WorkspaceDigest: working.Digest, ChangedFiles: record.Snapshot.ChangedFiles}, nil
}

func installMergedManifest(ctx context.Context, target, formalRoot, workspaceRoot string, formal, working, merged workspace.Manifest) error {
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
