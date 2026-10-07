package candidate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type transactionJournalRecorder struct {
	phases []string
}

func (r *transactionJournalRecorder) Advance(_ context.Context, id string, from, to TransactionPhase, _ string) error {
	r.phases = append(r.phases, id+":"+string(from)+"->"+string(to))
	return nil
}

func transactionRoot(t *testing.T, name, content string) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	_, digest, err := BuildManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, digest
}

func TestTransactionCoordinatorAtomicApply(t *testing.T) {
	oldRoot, oldDigest := transactionRoot(t, "old", "old")
	newRoot, newDigest := transactionRoot(t, "new", "new")
	recorder := &transactionJournalRecorder{}
	tx := DirectoryTransaction{ID: "tx-atomic", Kind: TransactionAcceptance, CurrentRoot: oldRoot, IncomingRoot: newRoot, ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: "atomic-exchange"}
	if err := NewTransactionCoordinator().Apply(context.Background(), tx, recorder); err != nil {
		t.Fatal(err)
	}
	if _, digest, err := BuildManifest(oldRoot); err != nil || digest != newDigest {
		t.Fatalf("installed root digest = %s, err=%v", digest, err)
	}
	if _, digest, err := BuildManifest(newRoot); err != nil || digest != oldDigest {
		t.Fatalf("spent root digest = %s, err=%v", digest, err)
	}
	if len(recorder.phases) != 1 || recorder.phases[0] != "tx-atomic:prepared->swapped" {
		t.Fatalf("phase history = %v", recorder.phases)
	}
}

func TestTransactionCoordinatorRejectsStaleRoot(t *testing.T) {
	oldRoot, oldDigest := transactionRoot(t, "old", "old")
	newRoot, newDigest := transactionRoot(t, "new", "new")
	if err := os.WriteFile(filepath.Join(oldRoot, "file.txt"), []byte("raced"), 0600); err != nil {
		t.Fatal(err)
	}
	recorder := &transactionJournalRecorder{}
	tx := DirectoryTransaction{ID: "tx-stale", Kind: TransactionAcceptance, CurrentRoot: oldRoot, IncomingRoot: newRoot, ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: "atomic-exchange"}
	if err := NewTransactionCoordinator().Apply(context.Background(), tx, recorder); err == nil {
		t.Fatal("stale current root accepted")
	}
	if _, digest, err := BuildManifest(oldRoot); err != nil || digest == newDigest {
		t.Fatalf("stale failure changed current root: %s, %v", digest, err)
	}
}

func TestTransactionCoordinatorJournaledRecovery(t *testing.T) {
	oldRoot, oldDigest := transactionRoot(t, "old", "old")
	newRoot, newDigest := transactionRoot(t, "new", "new")
	rollback := filepath.Join(filepath.Dir(oldRoot), "rollback")
	recorder := &transactionJournalRecorder{}
	tx := DirectoryTransaction{ID: "tx-journal", Kind: TransactionAcceptance, CurrentRoot: oldRoot, IncomingRoot: newRoot, RollbackRoot: rollback, ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: "journaled-move"}
	if err := NewTransactionCoordinator().Apply(context.Background(), tx, recorder); err != nil {
		t.Fatal(err)
	}
	if _, digest, err := BuildManifest(oldRoot); err != nil || digest != newDigest {
		t.Fatalf("journaled installed digest = %s, err=%v", digest, err)
	}
	if _, digest, err := BuildManifest(newRoot); err != nil || digest != oldDigest {
		t.Fatalf("journaled spent digest = %s, err=%v", digest, err)
	}
	if len(recorder.phases) != 3 {
		t.Fatalf("journaled phase history = %v", recorder.phases)
	}

	oldAgain, oldAgainDigest := transactionRoot(t, "old-again", "old")
	newAgain, newAgainDigest := transactionRoot(t, "new-again", "new")
	rollbackAgain := filepath.Join(filepath.Dir(oldAgain), "rollback-again")
	if err := os.Rename(oldAgain, rollbackAgain); err != nil {
		t.Fatal(err)
	}
	recoveryRecorder := &transactionJournalRecorder{}
	recoveryTx := DirectoryTransaction{ID: "tx-recover", Kind: TransactionAcceptance, CurrentRoot: oldAgain, IncomingRoot: newAgain, RollbackRoot: rollbackAgain, ExpectedDigest: oldAgainDigest, TargetDigest: newAgainDigest, Mode: "journaled-move"}
	if err := NewTransactionCoordinator().Recover(context.Background(), recoveryTx, PhaseOldSaved, recoveryRecorder); err != nil {
		t.Fatal(err)
	}
	if _, digest, err := BuildManifest(oldAgain); err != nil || digest != newAgainDigest {
		t.Fatalf("recovered installed digest = %s, err=%v", digest, err)
	}
	if _, digest, err := BuildManifest(newAgain); err != nil || digest != oldAgainDigest {
		t.Fatalf("recovered spent digest = %s, err=%v", digest, err)
	}
	if len(recoveryRecorder.phases) != 2 {
		t.Fatalf("recovery phase history = %v", recoveryRecorder.phases)
	}
	if err := NewTransactionCoordinator().Apply(context.Background(), recoveryTx, recoveryRecorder); err == nil {
		t.Fatal("reused transaction unexpectedly applied")
	}
}

func TestTransactionRecoveryRefusesUnrecognizedRollback(t *testing.T) {
	current, oldDigest := transactionRoot(t, "formal", "old")
	incoming, newDigest := transactionRoot(t, "incoming", "new")
	rollback := filepath.Join(filepath.Dir(current), "rollback")
	if err := os.Rename(current, rollback); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, "file.txt"), []byte("external change"), 0600); err != nil {
		t.Fatal(err)
	}
	tx := DirectoryTransaction{ID: "tx-unknown", Kind: TransactionAcceptance, CurrentRoot: current, IncomingRoot: incoming, RollbackRoot: rollback, ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: "journaled-move"}
	if err := NewTransactionCoordinator().Recover(context.Background(), tx, PhaseOldSaved, &transactionJournalRecorder{}); err == nil {
		t.Fatal("unrecognized rollback was installed")
	}
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		t.Fatalf("missing formal root was replaced: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rollback, "file.txt"))
	if err != nil || string(data) != "external change" {
		t.Fatalf("unknown rollback changed: %q %v", data, err)
	}
}

func TestLegacyGitTransactionRequiresNewExport(t *testing.T) {
	current, oldDigest := transactionRoot(t, "formal", "old")
	incoming, newDigest := transactionRoot(t, "incoming", "new")
	if err := os.WriteFile(filepath.Join(current, ".git"), []byte("gitdir: /external\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tx := DirectoryTransaction{ID: "legacy-git", Kind: TransactionAcceptance, CurrentRoot: current, IncomingRoot: incoming, ExpectedDigest: oldDigest, TargetDigest: newDigest, Mode: "atomic-exchange"}
	if err := NewTransactionCoordinator().Apply(context.Background(), tx, &transactionJournalRecorder{}); err == nil {
		t.Fatal("legacy Git acceptance was permitted")
	}
	if err := NewTransactionCoordinator().Recover(context.Background(), tx, PhasePrepared, &transactionJournalRecorder{}); err == nil {
		t.Fatal("legacy Git recovery was permitted")
	}
}
