package workspace

import (
	"errors"
	"fmt"
	"testing"
)

func TestBudgetProductionDefaultStorageLimitsAtBoundary(t *testing.T) {
	limits := DefaultLimits()
	const workspaceBytes int64 = 512 << 20
	const projectBytes int64 = 2 << 30
	if limits.MaxWorkspaceBytes != workspaceBytes || limits.MaxProjectBytes != projectBytes {
		t.Fatalf("production storage limits workspace=%d project=%d, want workspace=%d project=%d",
			limits.MaxWorkspaceBytes, limits.MaxProjectBytes, workspaceBytes, projectBytes)
	}

	t.Run("workspace", func(t *testing.T) {
		budget := NewBudget(Limits{})
		reservation, err := budget.ReserveCreate("workspace-exact", workspaceBytes)
		if err != nil {
			t.Fatalf("reserve exact production workspace limit: %v", err)
		}
		if err := reservation.Commit(workspaceBytes); err != nil {
			t.Fatalf("commit exact production workspace limit: %v", err)
		}
		if usage := budget.Usage(); usage.UsedBytes != workspaceBytes || usage.ReservedBytes != 0 {
			t.Fatalf("usage at workspace boundary=%+v, want %d used and no reservation", usage, workspaceBytes)
		}
		if _, err := budget.ReserveWrite("workspace-exact", 1); !errors.Is(err, ErrQuota) {
			t.Fatalf("one byte beyond production workspace limit returned %v, want ErrQuota", err)
		}
	})

	t.Run("project aggregate", func(t *testing.T) {
		budget := NewBudget(Limits{})
		const workspaceCount = 4 // 4 * 512 MiB exactly equals the default 2 GiB project cap.
		for i := 0; i < workspaceCount; i++ {
			id := fmt.Sprintf("workspace-%d", i)
			reservation, err := budget.ReserveCreate(id, workspaceBytes)
			if err != nil {
				t.Fatalf("reserve workspace %d at production limits: %v", i, err)
			}
			if err := reservation.Commit(workspaceBytes); err != nil {
				t.Fatalf("commit workspace %d at production limits: %v", i, err)
			}
		}
		if usage := budget.Usage(); usage.UsedBytes != projectBytes || usage.Workspaces != workspaceCount {
			t.Fatalf("usage at project boundary=%+v, want %d bytes across %d workspaces", usage, projectBytes, workspaceCount)
		}
		if _, err := budget.ReserveCreate("workspace-over", 1); !errors.Is(err, ErrQuota) {
			t.Fatalf("one byte beyond production project limit returned %v, want ErrQuota", err)
		}
	})
}
