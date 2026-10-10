//go:build linux || darwin

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stable/internal/permission"
)

const exportCASCandidateID = "8123456789abcdef0123456789abcdef"

type journalCASExportEvidence struct {
	WorkspaceID string `json:"workspace_id"`
	CandidateID string `json:"candidate_id"`
	Digest      string `json:"digest"`
}

// casConflictAfterPersistingExporter models a successful, durable candidate
// write followed by a concurrent journal generation advance before Export's
// final StateExported CAS save.
type casConflictAfterPersistingExporter struct {
	service     *LifecycleService
	evidenceDir string
	calls       int
}

func (e *casConflictAfterPersistingExporter) ExportWorkspace(ctx context.Context, scope Scope, record Record, _ Paths) (Snapshot, error) {
	e.calls++
	evidence := journalCASExportEvidence{
		WorkspaceID: record.Snapshot.ID,
		CandidateID: exportCASCandidateID,
		Digest:      "durably stored candidate evidence",
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return Snapshot{}, err
	}
	if err := os.MkdirAll(e.evidenceDir, 0700); err != nil {
		return Snapshot{}, err
	}
	path := filepath.Join(e.evidenceDir, exportCASCandidateID+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return Snapshot{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Snapshot{}, err
	}
	if err := file.Close(); err != nil {
		return Snapshot{}, err
	}

	// Simulate another valid journal writer winning the generation CAS after
	// the candidate store committed but before this export's completion save.
	current, err := e.service.store.Load(ctx, scope, record.Snapshot.ID)
	if err != nil {
		return Snapshot{}, err
	}
	advanced := current
	advanced.Snapshot.Generation++
	advanced.Snapshot.Cursor++
	advanced.Operation.Generation = advanced.Snapshot.Generation
	advanced.Operation.UpdatedAt = time.Now().UTC()
	if err := e.service.store.Save(ctx, scope, advanced, current.Snapshot.Generation); err != nil {
		return Snapshot{}, err
	}

	return Snapshot{
		ID: record.Snapshot.ID, CandidateID: exportCASCandidateID,
		State: StateExported, BaselineDigest: record.Snapshot.BaselineDigest,
		FormalDigest:    record.Snapshot.FormalDigest,
		WorkspaceDigest: record.Snapshot.WorkspaceDigest,
		ChangedFiles:    record.Snapshot.ChangedFiles,
	}, nil
}

func TestExportCandidatePersistsButJournalCASFailureRecoversWithoutReplay(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formal, "base.txt"), []byte("formal baseline"), 0600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(parent, "state")
	evidenceDir := filepath.Join(parent, "candidates")
	scope := testScope()
	scope.Authority = permission.Authority{
		RunID: "export-run", SessionID: scope.SessionID,
		AllowedRoot: formal, FormalRoot: formal,
		CandidateRoot: evidenceDir,
	}

	opener := func(exporter *casConflictAfterPersistingExporter) (*LifecycleService, error) {
		layout, err := NewLayout(stateRoot, formal, scope.ProjectID)
		if err != nil {
			return nil, err
		}
		service, err := NewService(layout, Limits{}, ServiceDependencies{Exporter: exporter})
		if err != nil {
			return nil, err
		}
		exporter.service = service
		return service, nil
	}

	exporter := &casConflictAfterPersistingExporter{evidenceDir: evidenceDir}
	service, err := opener(exporter)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(ctx, scope, "candidate journal race")
	if err != nil {
		_ = service.Close(ctx)
		t.Fatal(err)
	}
	paths, err := service.layout.Paths(created.ID)
	if err != nil {
		_ = service.Close(ctx)
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Checkout, "change.txt"), []byte("candidate change"), 0600); err != nil {
		_ = service.Close(ctx)
		t.Fatal(err)
	}
	if _, err := service.Export(ctx, scope, created.ID); !errors.Is(err, ErrOwnership) {
		_ = service.Close(ctx)
		t.Fatalf("export final journal CAS error=%v, want ownership conflict", err)
	}
	stored, err := service.store.Load(ctx, scope, created.ID)
	if err != nil || stored.Snapshot.State != StateExporting || stored.Snapshot.CandidateID != "" || stored.Operation.Phase != "intent" {
		_ = service.Close(ctx)
		t.Fatalf("journal after completion CAS failure=%+v err=%v", stored, err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Candidate evidence survives independently of the uncommitted journal
	// reference, and a second exporter invocation would be a duplicate write.
	var persisted journalCASExportEvidence
	evidencePath := filepath.Join(evidenceDir, exportCASCandidateID+".json")
	data, err := os.ReadFile(evidencePath)
	if err != nil || json.Unmarshal(data, &persisted) != nil || persisted.WorkspaceID != created.ID || persisted.CandidateID != exportCASCandidateID {
		t.Fatalf("persisted candidate evidence=%+v err=%v data=%q", persisted, err, data)
	}

	service, err = opener(exporter)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	recovered, err := service.Get(ctx, scope, created.ID)
	if err != nil || recovered.State != StateInterrupted || recovered.CandidateID != "" || recovered.Error != "service restarted during export; resource retained for explicit recovery" {
		t.Fatalf("recovered export snapshot=%+v err=%v", recovered, err)
	}
	if _, err := service.Export(ctx, scope, created.ID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("explicit retry replayed interrupted export: %v", err)
	}
	if exporter.calls != 1 {
		t.Fatalf("exporter was called %d times; want exactly one", exporter.calls)
	}
	entries, err := os.ReadDir(evidenceDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != exportCASCandidateID+".json" {
		t.Fatalf("candidate evidence entries=%v err=%v; want one stable candidate", entries, err)
	}
	final, err := service.store.Load(ctx, scope, created.ID)
	if err != nil || final.Snapshot.State != StateInterrupted || final.Snapshot.CandidateID != "" || final.Operation.Phase != "blocked" {
		t.Fatalf("recovered ownership journal=%+v err=%v", final, err)
	}
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	service, err = opener(exporter)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(ctx)
	secondRecovery, err := service.Get(ctx, scope, created.ID)
	if err != nil || secondRecovery.State != StateInterrupted || secondRecovery.CandidateID != "" || secondRecovery.Cursor != recovered.Cursor {
		t.Fatalf("second recovery changed interrupted export: first=%+v second=%+v err=%v", recovered, secondRecovery, err)
	}
	if exporter.calls != 1 {
		t.Fatalf("second recovery replayed exporter; calls=%d", exporter.calls)
	}
}
