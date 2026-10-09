//go:build linux || darwin

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type countingWorkspaceExporter struct{ calls int }

func (e *countingWorkspaceExporter) ExportWorkspace(context.Context, Scope, Record, Paths) (Snapshot, error) {
	e.calls++
	return Snapshot{}, errors.New("unexpected export replay")
}

func TestInterruptedExportRestartNeverReplaysExporterImplicitly(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()
	formal := filepath.Join(parent, "formal")
	if err := os.Mkdir(formal, 0700); err != nil {
		t.Fatal(err)
	}
	scope := testScope()
	workspaceID := "1123456789abcdef0123456789abcdef"
	candidateID := "2123456789abcdef0123456789abcdef"
	operationID := "3123456789abcdef0123456789abcdef"
	exportOperationID := "4123456789abcdef0123456789abcdef"

	layout, err := NewLayout(filepath.Join(parent, "state"), formal, "project")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewOwnershipStore(layout)
	if err != nil {
		_ = layout.Close()
		t.Fatal(err)
	}
	record, err := store.Create(ctx, scope, workspaceID, "interrupted export", operationID)
	if err != nil {
		_ = store.Close()
		_ = layout.Close()
		t.Fatal(err)
	}
	record.Snapshot.State = StateReady
	record.Snapshot.Cursor++
	record.Operation = Operation{ID: operationID, Kind: "create", Phase: "complete", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err = store.Save(ctx, scope, record, record.Snapshot.Generation); err != nil {
		_ = store.Close()
		_ = layout.Close()
		t.Fatal(err)
	}
	record.Snapshot.State = StateExporting
	record.Snapshot.CandidateID = candidateID
	record.Snapshot.Cursor++
	record.Operation = Operation{ID: exportOperationID, Kind: "export", Phase: "intent", Generation: record.Snapshot.Generation, UpdatedAt: time.Now().UTC()}
	if err = store.Save(ctx, scope, record, record.Snapshot.Generation); err != nil {
		_ = store.Close()
		_ = layout.Close()
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = layout.Close(); err != nil {
		t.Fatal(err)
	}

	exporter := &countingWorkspaceExporter{}
	openRecovered := func() *LifecycleService {
		t.Helper()
		reopened, openErr := NewLayout(filepath.Join(parent, "state"), formal, "project")
		if openErr != nil {
			t.Fatal(openErr)
		}
		service, openErr := NewService(reopened, Limits{}, ServiceDependencies{Exporter: exporter})
		if openErr != nil {
			_ = reopened.Close()
			t.Fatal(openErr)
		}
		return service
	}

	service := openRecovered()
	got, err := service.Get(ctx, scope, workspaceID)
	if err != nil || got.State != StateInterrupted || got.CandidateID != candidateID || got.Error != "service restarted during export; resource retained for explicit recovery" {
		t.Fatalf("first recovered export snapshot=%+v err=%v", got, err)
	}
	if _, err = service.Export(ctx, scope, workspaceID); !errors.Is(err, ErrOwnership) {
		t.Fatalf("explicit export request replayed interrupted operation: %v", err)
	}
	if exporter.calls != 0 {
		t.Fatalf("exporter calls after interrupted operation: %d", exporter.calls)
	}
	if err = service.Close(ctx); err != nil {
		t.Fatal(err)
	}

	service = openRecovered()
	defer service.Close(ctx)
	got, err = service.Get(ctx, scope, workspaceID)
	if err != nil || got.State != StateInterrupted || got.CandidateID != candidateID {
		t.Fatalf("second recovered export snapshot=%+v err=%v", got, err)
	}
	if exporter.calls != 0 {
		t.Fatalf("recovery implicitly replayed exporter %d times", exporter.calls)
	}
	stored, err := service.store.Load(ctx, scope, workspaceID)
	if err != nil || stored.Operation.ID != exportOperationID || stored.Operation.Phase != "blocked" {
		t.Fatalf("recovered export journal=%+v err=%v", stored.Operation, err)
	}
}
