package conversation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"stable/internal/candidate"
	"stable/internal/platform/secfile"
	"stable/internal/sessionlog"
	"stable/internal/store"
)

// boundProjectRoot resolves the project root this service is bound to.
// Snapshot and question ops are session-scoped and do not carry a root: the
// server-bound root is the only trusted one.
func (s *Service) boundProjectRoot() (string, error) {
	if s.deps.ProjectRoot == "" {
		return "", errors.New("service is not bound to a project root")
	}
	return sessionRoot(s.deps.ProjectRoot)
}

// snapshotRefsFor returns the snapshot events one session recorded for one
// candidate, oldest first. Snapshots recorded by other sessions or for other
// candidates are invisible here, which is what ownership means for listing
// and rewinding.
func snapshotRefsFor(events []sessionlog.Event, candidateID string) []sessionlog.SnapshotRef {
	refs := []sessionlog.SnapshotRef{}
	for _, e := range events {
		if e.Type != sessionlog.EventSnapshot {
			continue
		}
		var ref sessionlog.SnapshotRef
		if decodeSessionData(e.Data, &ref) != nil {
			continue
		}
		if ref.CandidateID == candidateID {
			refs = append(refs, ref)
		}
	}
	return refs
}

func (s *Service) listSnapshots(c ClientMsg) ([]sessionlog.SnapshotRef, error) {
	root, err := s.boundProjectRoot()
	if err != nil {
		return nil, err
	}
	replay, err := sessionlog.Replay(root, c.SessionID)
	if err != nil {
		return nil, err
	}
	return snapshotRefsFor(replay.Events, c.CandidateID), nil
}

// rewindSnapshot rolls one ready, unaccepted candidate back to a snapshot
// the same session recorded. Every refusal is explicit: wrong ownership,
// wrong lifecycle, an active run, an unfinished rewind or acceptance, or a
// digest that moved since the client confirmed it. The formal project tree
// is never a target.
func (s *Service) rewindSnapshot(ctx context.Context, c ClientMsg) (sessionlog.RewindRecord, error) {
	if s.deps.Snapshots == nil {
		return sessionlog.RewindRecord{}, errors.New("candidate snapshots are unavailable")
	}
	if s.deps.Store == nil {
		return sessionlog.RewindRecord{}, errors.New("state store unavailable")
	}
	root, err := s.boundProjectRoot()
	if err != nil {
		return sessionlog.RewindRecord{}, err
	}
	replay, err := sessionlog.Replay(root, c.SessionID)
	if err != nil {
		return sessionlog.RewindRecord{}, err
	}
	owned := false
	for _, ref := range snapshotRefsFor(replay.Events, c.CandidateID) {
		if ref.SnapshotID == c.SnapshotID {
			owned = true
			break
		}
	}
	if !owned {
		return sessionlog.RewindRecord{}, fmt.Errorf("snapshot %s is not owned by this session and candidate", c.SnapshotID)
	}
	record, err := s.deps.Store.GetCandidate(ctx, c.CandidateID)
	if err != nil {
		return sessionlog.RewindRecord{}, fmt.Errorf("candidate unavailable: %w", err)
	}
	cand := record.Candidate
	if filepath.Clean(cand.FormalRoot) != filepath.Clean(root) {
		return sessionlog.RewindRecord{}, errors.New("candidate belongs to a different project")
	}
	if cand.Status != "ready" {
		return sessionlog.RewindRecord{}, fmt.Errorf("candidate is %q; only a ready, unaccepted candidate can be rewound", cand.Status)
	}
	s.mu.Lock()
	for _, sessionID := range s.activeRuns {
		if sessionID == c.SessionID {
			s.mu.Unlock()
			return sessionlog.RewindRecord{}, errors.New("a run is active in this session; let it finish or cancel it before rewinding")
		}
	}
	s.mu.Unlock()
	if _, ok, err := s.deps.Store.UnfinishedRewindFor(ctx, c.CandidateID); err != nil {
		return sessionlog.RewindRecord{}, err
	} else if ok {
		return sessionlog.RewindRecord{}, errors.New("an earlier rewind is unfinished; restart the runtime to reconcile it")
	}
	pending, err := s.deps.Store.PendingAcceptances(ctx)
	if err != nil {
		return sessionlog.RewindRecord{}, err
	}
	for _, p := range pending {
		if p.Decision.CandidateID == c.CandidateID {
			return sessionlog.RewindRecord{}, errors.New("an acceptance is unfinished for this candidate; restart the runtime to reconcile it")
		}
	}
	if err = candidate.GuardRewindTarget(cand.FormalRoot, cand.CandidateRoot); err != nil {
		return sessionlog.RewindRecord{}, err
	}
	snap, err := s.deps.Snapshots.ValidateRestore(c.CandidateID, c.SnapshotID)
	if err != nil {
		return sessionlog.RewindRecord{}, fmt.Errorf("snapshot cannot be restored: %w", err)
	}
	if snap.SessionID != c.SessionID {
		return sessionlog.RewindRecord{}, errors.New("snapshot belongs to a different session")
	}
	_, digest, err := candidate.BuildManifest(cand.CandidateRoot)
	if err != nil {
		return sessionlog.RewindRecord{}, err
	}
	if digest != c.CandidateDigest {
		return sessionlog.RewindRecord{}, errors.New("candidate changed since it was reviewed; reload the review and confirm again")
	}
	if cand.CandidateDigest != "" && digest != cand.CandidateDigest {
		return sessionlog.RewindRecord{}, errors.New("candidate content no longer matches its frozen digest")
	}

	// The conversation service is the only session log writer: the pending
	// record lands before any filesystem change, the outcome after it.
	s.eventMu.Lock()
	_, err = sessionlog.Append(root, c.SessionID, sessionlog.EventRewind, sessionlog.RewindRecord{
		SnapshotID: c.SnapshotID, CandidateID: c.CandidateID, Status: sessionlog.RewindPending, CreatedAt: time.Now().UTC(),
	})
	s.eventMu.Unlock()
	if err != nil {
		return sessionlog.RewindRecord{}, fmt.Errorf("record rewind intent: %w", err)
	}
	fail := func(cause error) (sessionlog.RewindRecord, error) {
		failed := sessionlog.RewindRecord{SnapshotID: c.SnapshotID, CandidateID: c.CandidateID, Status: sessionlog.RewindFailed, Error: cause.Error(), CreatedAt: time.Now().UTC()}
		s.eventMu.Lock()
		_, _ = sessionlog.Append(root, c.SessionID, sessionlog.EventRewind, failed)
		s.eventMu.Unlock()
		return failed, cause
	}

	staging, err := candidate.StageRewind(s.deps.Snapshots, snap, cand.CandidateRoot)
	if err != nil {
		return fail(err)
	}
	id, err := sessionlog.NewID()
	if err != nil {
		_ = os.RemoveAll(staging)
		return fail(err)
	}
	journal := store.RewindJournal{ID: id, CandidateID: c.CandidateID, SnapshotID: c.SnapshotID, ExpectedDigest: digest, TargetDigest: snap.Digest, StagingDir: staging, TransactionMode: secfile.TransactionMode(), RollbackPath: staging + ".rollback"}
	if err = s.deps.Store.BeginRewind(ctx, journal); err != nil {
		_ = os.RemoveAll(staging)
		return fail(err)
	}
	// A crash from here on leaves the journal for startup reconciliation;
	// the synchronous path reports the same transitions as they happen.
	tx := candidate.DirectoryTransaction{ID: journal.ID, Kind: candidate.TransactionRewind, CurrentRoot: cand.CandidateRoot, IncomingRoot: staging, RollbackRoot: journal.RollbackPath, ExpectedDigest: journal.ExpectedDigest, TargetDigest: journal.TargetDigest, Mode: journal.TransactionMode}
	if err = candidate.NewTransactionCoordinator().Apply(ctx, tx, rewindJournalAdapter{store: s.deps.Store}); err != nil {
		return fail(fmt.Errorf("exchange candidate with staged snapshot: %w", err))
	}
	if _, after, err := candidate.BuildManifest(cand.CandidateRoot); err != nil {
		return fail(err)
	} else if after != snap.Digest {
		return fail(errors.New("rewound candidate does not match the snapshot digest"))
	}
	if err = s.deps.Store.FinalizeRewind(ctx, journal); err != nil {
		return fail(err)
	}
	_ = os.RemoveAll(staging) // staging now holds the previous candidate content
	// Reviews were taken against the pre-rewind digest: they no longer
	// describe the candidate and must not gate a later accept.
	if _, err = s.deps.Store.DB().ExecContext(ctx, "DELETE FROM candidate_reviews WHERE candidate_id=?", c.CandidateID); err != nil {
		return fail(fmt.Errorf("invalidate stale reviews: %w", err))
	}
	done := sessionlog.RewindRecord{SnapshotID: c.SnapshotID, CandidateID: c.CandidateID, Status: sessionlog.RewindCompleted, CreatedAt: time.Now().UTC()}
	s.eventMu.Lock()
	_, err = sessionlog.Append(root, c.SessionID, sessionlog.EventRewind, done)
	s.eventMu.Unlock()
	if err != nil {
		return done, fmt.Errorf("rewind finished but the receipt could not be recorded: %w", err)
	}
	return done, nil
}

type rewindJournalAdapter struct{ store *store.Store }

func (j rewindJournalAdapter) Advance(ctx context.Context, id string, from, to candidate.TransactionPhase, reason string) error {
	return j.store.SetRewindPhase(ctx, id, string(from), string(to), reason)
}

func (s *Service) searchSessions(c ClientMsg) (sessionlog.SearchResult, error) {
	root, err := s.requestProjectRoot(c)
	if err != nil {
		return sessionlog.SearchResult{}, err
	}
	return sessionlog.Search(root, c.Text, c.Limit)
}
