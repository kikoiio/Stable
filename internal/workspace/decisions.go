package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// These records are produced only by authenticated user operations. They are
// not part of the model-facing Service interface and contain no file data.
type UserResolution struct {
	ID              string            `json:"id"`
	UserID          string            `json:"user_id"`
	SessionID       string            `json:"session_id"`
	WorkspaceID     string            `json:"workspace_id"`
	PreviewID       string            `json:"preview_id"`
	Generation      uint64            `json:"generation"`
	BaselineDigest  string            `json:"baseline_digest"`
	FormalDigest    string            `json:"formal_digest"`
	WorkspaceDigest string            `json:"workspace_digest"`
	Choices         map[string]string `json:"choices"`
}

type UserDiscard struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	Generation  uint64 `json:"generation"`
	Digest      string `json:"digest"`
}

// BuildMergePreview reads the three complete project manifests. No decision
// can be validated against only the first page of displayed conflict paths.
func BuildMergePreview(ctx context.Context, paths Paths, limits Limits) (MergePreview, error) {
	base, err := BuildManifest(ctx, paths.Baseline, limits)
	if err != nil {
		return MergePreview{}, err
	}
	formal, err := BuildManifest(ctx, paths.FormalRoot, limits)
	if err != nil {
		return MergePreview{}, err
	}
	working, err := BuildManifest(ctx, paths.Checkout, limits)
	if err != nil {
		return MergePreview{}, err
	}
	return ThreeWayPreview(base, formal, working, limits)
}

func (d *UserResolution) Matches(record Record, preview MergePreview) bool {
	return d != nil && d.UserID != "" && d.ID == record.Snapshot.ResolutionID && d.SessionID == record.Scope.SessionID && d.WorkspaceID == record.Snapshot.ID && d.PreviewID == record.Snapshot.PreviewID && d.Generation == record.Snapshot.Generation && d.BaselineDigest == preview.BaselineDigest && d.FormalDigest == preview.FormalDigest && d.WorkspaceDigest == preview.WorkspaceDigest
}

// ResolveUser accepts at most one bounded page per action, accumulating choices
// for the same immutable preview. Export still requires the exact complete set.
func (s *LifecycleService) ResolveUser(ctx context.Context, scope Scope, id, userID, previewID string, generation uint64, choices map[string]string) (Snapshot, error) {
	if userID == "" || len(userID) > 128 || !ValidID(previewID) || len(choices) == 0 || len(choices) > 100 {
		return Snapshot{}, ErrOwnership
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if s.closed || record.Snapshot.WriterRunID != "" || record.Snapshot.Generation != generation || record.Snapshot.PreviewID != previewID || (record.Snapshot.State != StateReady && record.Snapshot.State != StateKept) {
		return Snapshot{}, ErrOwnership
	}
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
	conflicts := make(map[string]bool, len(preview.Conflicts))
	for _, conflict := range preview.Conflicts {
		conflicts[conflict.Path] = true
	}
	decision := UserResolution{ID: mustID(), UserID: userID, SessionID: scope.SessionID, WorkspaceID: id, PreviewID: previewID, Generation: generation, BaselineDigest: preview.BaselineDigest, FormalDigest: preview.FormalDigest, WorkspaceDigest: preview.WorkspaceDigest, Choices: map[string]string{}}
	if record.Resolution.Matches(record, preview) {
		if record.Resolution.UserID != userID {
			return Snapshot{}, ErrOwnership
		}
		for path, choice := range record.Resolution.Choices {
			decision.Choices[path] = choice
		}
	}
	for path, choice := range choices {
		if !conflicts[path] || (choice != UseWorkspace && choice != UseFormal) {
			return Snapshot{}, errors.New("resolution must select a current conflict path and either use_workspace or use_formal")
		}
		decision.Choices[path] = choice
	}
	updated := record
	updated.Resolution = &decision
	updated.Snapshot.ResolutionID = decision.ID
	updated.Snapshot.ResolvedCount = len(decision.Choices)
	updated.Snapshot.Cursor++
	updated.Operation = Operation{ID: mustID(), Kind: "resolve", Phase: "complete", Generation: generation, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, scope, updated, generation); err != nil {
		return Snapshot{}, err
	}
	return updated.Snapshot, nil
}

// ConflictPage returns a bounded continuation of an already generated preview.
func (s *LifecycleService) ConflictPage(ctx context.Context, scope Scope, id, after string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	if record.Snapshot.PreviewID == "" || record.Snapshot.WriterRunID != "" {
		return Snapshot{}, ErrOwnership
	}
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
	snapshot := record.Snapshot
	snapshot.Conflicts = nil
	snapshot.ConflictNext = ""
	for _, conflict := range preview.Conflicts {
		if conflict.Path <= after {
			continue
		}
		if len(snapshot.Conflicts) == 100 {
			snapshot.ConflictNext = snapshot.Conflicts[len(snapshot.Conflicts)-1]
			break
		}
		snapshot.Conflicts = append(snapshot.Conflicts, conflict.Path)
	}
	return snapshot, nil
}

func (s *LifecycleService) PreviewDiscardUser(ctx context.Context, scope Scope, id, userID string) (Snapshot, error) {
	if userID == "" || len(userID) > 128 {
		return Snapshot{}, ErrOwnership
	}
	if _, err := s.StopWriterIfActive(ctx, scope, id); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.removableRecord(ctx, scope, id)
	if err != nil {
		return Snapshot{}, err
	}
	digest, changed, paths, err := s.discardFacts(ctx, scope, record)
	if err != nil {
		return Snapshot{}, err
	}
	decision := UserDiscard{ID: mustID(), UserID: userID, SessionID: scope.SessionID, WorkspaceID: id, Generation: record.Snapshot.Generation, Digest: digest}
	record.Discard = &decision
	record.Snapshot.DiscardID, record.Snapshot.DiscardDigest = decision.ID, digest
	record.Snapshot.DiscardPaths = paths
	record.Snapshot.ChangedFiles = changed
	record.Snapshot.Cursor++
	record.Operation = Operation{ID: mustID(), Kind: "discard_preview", Phase: "complete", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err := s.store.Save(ctx, scope, record, record.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}
	return record.Snapshot, nil
}

func (s *LifecycleService) RemoveDiscardUser(ctx context.Context, scope Scope, id, userID, decisionID, digest string, generation uint64) (Snapshot, error) {
	return s.removeDiscardUserWithHooks(ctx, scope, id, userID, decisionID, digest, generation, removeHooks{})
}

func (s *LifecycleService) removeDiscardUserWithHooks(ctx context.Context, scope Scope, id, userID, decisionID, digest string, generation uint64, hooks removeHooks) (Snapshot, error) {
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
	d := record.Discard
	if d == nil || userID == "" || d.UserID != userID || d.ID != decisionID || d.SessionID != scope.SessionID || d.WorkspaceID != id || d.Generation != generation || record.Snapshot.Generation != generation || d.Digest != digest {
		return Snapshot{}, ErrOwnership
	}
	current, _, _, err := s.discardFacts(ctx, scope, record)
	if err != nil {
		return Snapshot{}, err
	}
	if current != digest {
		return Snapshot{}, ErrSourceChanged
	}
	hooks.validateBeforeRename = func(validateCtx context.Context) error {
		current, _, _, err := s.discardFacts(validateCtx, scope, record)
		if err != nil {
			return err
		}
		if current != digest {
			return ErrSourceChanged
		}
		return nil
	}
	return s.removeRecordLockedWithHooks(ctx, scope, record, hooks)
}

func (s *LifecycleService) removableRecord(ctx context.Context, scope Scope, id string) (Record, error) {
	record, err := s.store.Load(ctx, scope, id)
	if err != nil {
		return Record{}, err
	}
	if s.closed || s.writers[id].RunID != "" || record.Snapshot.WriterRunID != "" || (record.Snapshot.State != StateReady && record.Snapshot.State != StateKept && record.Snapshot.State != StateExported) {
		return Record{}, ErrOwnership
	}
	bound, err := s.readBinding(scope)
	if err != nil {
		return Record{}, err
	}
	if bound == id {
		return Record{}, errors.New("exit the workspace before deleting it")
	}
	return record, nil
}

func (s *LifecycleService) discardFacts(ctx context.Context, scope Scope, record Record) (string, int, []string, error) {
	state, err := s.git.ValidateForDiscard(ctx, scope, record.Snapshot.ID)
	if err != nil {
		return "", 0, nil, err
	}
	paths, err := s.layout.Paths(record.Snapshot.ID)
	if err != nil {
		return "", 0, nil, err
	}
	base, err := BuildManifest(ctx, paths.Baseline, s.limits)
	if err != nil {
		return "", 0, nil, err
	}
	working, err := BuildManifest(ctx, paths.Checkout, s.limits)
	if err != nil {
		return "", 0, nil, err
	}
	changed := changedManifestEntries(base.Entries, working.Entries)
	before := map[string]ManifestEntry{}
	after := map[string]ManifestEntry{}
	for _, e := range base.Entries {
		before[e.Path] = e
	}
	for _, e := range working.Entries {
		after[e.Path] = e
	}
	changedPaths := make([]string, 0, changed)
	for path, entry := range before {
		if current, ok := after[path]; !ok || current != entry {
			changedPaths = append(changedPaths, path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			changedPaths = append(changedPaths, path)
		}
	}
	sort.Strings(changedPaths)
	if len(changedPaths) > 100 {
		changedPaths = changedPaths[:100]
	}
	encoded := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s", record.Snapshot.ID, record.Snapshot.Generation, base.Digest, working.Digest, state.BaselineCommit)
	digest := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(digest[:]), changed, changedPaths, nil
}

func validateUserDecisions(record Record) error {
	for _, id := range []string{record.Snapshot.PreviewID, record.Snapshot.ResolutionID, record.Snapshot.DiscardID} {
		if id != "" && !ValidID(id) {
			return ErrOwnership
		}
	}
	if record.Snapshot.ResolvedCount < 0 || record.Snapshot.ResolvedCount > record.Snapshot.ConflictCount || len(record.Snapshot.DiscardPaths) > 100 {
		return ErrOwnership
	}
	for _, path := range record.Snapshot.DiscardPaths {
		if clean, err := CleanRelative(path); err != nil || clean != path || ProtectedRoot(path) {
			return ErrUnsafePath
		}
	}
	if d := record.Resolution; d != nil {
		if d.UserID == "" || len(d.UserID) > 128 || !ValidID(d.ID) || !ValidID(d.PreviewID) || d.ID != record.Snapshot.ResolutionID || d.SessionID != record.Scope.SessionID || d.WorkspaceID != record.Snapshot.ID || d.Generation != record.Snapshot.Generation || len(d.Choices) > DefaultLimits().MaxFiles {
			return ErrOwnership
		}
		for _, digest := range []string{d.BaselineDigest, d.FormalDigest, d.WorkspaceDigest} {
			if !validDigest(digest) {
				return ErrOwnership
			}
		}
		for path, choice := range d.Choices {
			if clean, err := CleanRelative(path); err != nil || clean != path || ProtectedRoot(path) || (choice != UseFormal && choice != UseWorkspace) {
				return ErrUnsafePath
			}
		}
		encoded, err := json.Marshal(d)
		if err != nil || len(encoded) > 1<<20 {
			return ErrQuota
		}
	}
	if d := record.Discard; d != nil {
		if d.UserID == "" || len(d.UserID) > 128 || !ValidID(d.ID) || d.ID != record.Snapshot.DiscardID || d.SessionID != record.Scope.SessionID || d.WorkspaceID != record.Snapshot.ID || d.Generation != record.Snapshot.Generation || !validDigest(d.Digest) || d.Digest != record.Snapshot.DiscardDigest {
			return ErrOwnership
		}
	}
	return nil
}
