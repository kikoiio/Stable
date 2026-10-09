package workspace

import (
	"errors"
	"fmt"
	"testing"
)

func TestBudgetDefaultWorkspaceCountLimitAtBoundary(t *testing.T) {
	limit := DefaultLimits().MaxWorkspaces
	if limit != 16 {
		t.Fatalf("production default MaxWorkspaces=%d, want 16", limit)
	}

	records := make([]Record, 0, limit+1)
	for i := 0; i < limit; i++ {
		records = append(records, Record{Snapshot: Snapshot{ID: fmt.Sprintf("workspace-%02d", i), State: StateKept}})
	}
	// Removed workspaces no longer consume a live-workspace slot after restore.
	records = append(records, Record{Snapshot: Snapshot{ID: "workspace-removed", State: StateRemoved}})

	budget := NewBudget(Limits{})
	if err := budget.Restore(records); err != nil {
		t.Fatalf("restore exactly the default number of live workspaces: %v", err)
	}
	if usage := budget.Usage(); usage.Workspaces != limit {
		t.Fatalf("restored live workspace count=%d, want %d", usage.Workspaces, limit)
	}
	if _, err := budget.ReserveCreate("workspace-over", 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("creation beyond the default live-workspace limit did not return ErrQuota: %v", err)
	}

	if err := budget.Remove("workspace-00"); err != nil {
		t.Fatalf("remove one owned workspace: %v", err)
	}
	reservation, err := budget.ReserveCreate("workspace-over", 0)
	if err != nil {
		t.Fatalf("creation after a live workspace was removed: %v", err)
	}
	if err := reservation.Commit(0); err != nil {
		t.Fatalf("commit replacement workspace reservation: %v", err)
	}
	if usage := budget.Usage(); usage.Workspaces != limit {
		t.Fatalf("workspace count after remove and replacement=%d, want %d", usage.Workspaces, limit)
	}
}
