package workspace

import (
	"errors"
	"sync"
	"testing"
)

func TestBudgetConcurrentReservationsAndRollback(t *testing.T) {
	b := NewBudget(Limits{MaxWorkspaceBytes: 100, MaxProjectBytes: 100, MaxWorkspaces: 2})
	creation, err := b.ReserveCreate("work", 20)
	if err != nil {
		t.Fatal(err)
	}
	if err := creation.Commit(20); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan *Reservation, 32)
	errorsCh := make(chan error, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			r, err := b.ReserveWrite("work", 10)
			if err == nil {
				accepted <- r
			} else {
				errorsCh <- err
			}
		})
	}
	wg.Wait()
	close(accepted)
	close(errorsCh)
	if got := b.Usage(); got.UsedBytes != 20 || got.ReservedBytes != 80 || len(accepted) != 8 {
		t.Fatalf("concurrent quota over/under-reserved: %+v, accepted %d", got, len(accepted))
	}
	for err := range errorsCh {
		if !errors.Is(err, ErrQuota) {
			t.Fatal(err)
		}
	}
	for r := range accepted {
		r.Release()
		r.Release()
	}
	r, err := b.ReserveWrite("work", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(11); !errors.Is(err, ErrQuota) {
		t.Fatalf("unreserved growth committed: %v", err)
	}
	if err := r.Commit(7); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(7); err != nil {
		t.Fatal(err)
	}
	r.Release()
	if got := b.Usage(); got.UsedBytes != 27 || got.ReservedBytes != 0 {
		t.Fatalf("settlement not idempotent: %+v", got)
	}
}

func TestBudgetCreateSlotsProjectLimitAndRecovery(t *testing.T) {
	b := NewBudget(Limits{MaxWorkspaceBytes: 100, MaxProjectBytes: 120, MaxWorkspaces: 2})
	a, err := b.ReserveCreate("a", 80)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReserveCreate("b", 50); !errors.Is(err, ErrQuota) {
		t.Fatalf("project reservation ignored: %v", err)
	}
	second, err := b.ReserveCreate("b", 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReserveCreate("c", 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("pending item slot not charged: %v", err)
	}
	a.Release()
	second.Release()
	if got := b.Usage(); got != (Usage{}) {
		t.Fatalf("creation rollback leaked budget: %+v", got)
	}
	records := []Record{{Snapshot: Snapshot{ID: "kept", State: StateKept}, UsedBytes: 70}, {Snapshot: Snapshot{ID: "interrupted", State: StateInterrupted}, UsedBytes: 20}, {Snapshot: Snapshot{ID: "removed", State: StateRemoved}, UsedBytes: 999}}
	if err := b.Restore(records); err != nil {
		t.Fatal(err)
	}
	if got := b.Usage(); got.Workspaces != 2 || got.UsedBytes != 90 {
		t.Fatalf("recovery lost live usage: %+v", got)
	}
	if err := b.Remove("kept"); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove("kept"); err != nil {
		t.Fatal(err)
	}
	if err := b.Restore([]Record{{Snapshot: Snapshot{ID: "over", State: StateKept}, UsedBytes: 130}}); !errors.Is(err, ErrQuota) {
		t.Fatalf("over-limit recovery silently accepted: %v", err)
	}
	if _, err := b.ReserveCreate("new", 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("over-limit usage discarded: %v", err)
	}
	shrink, err := b.ReserveWrite("over", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := shrink.Commit(-50); err != nil {
		t.Fatal(err)
	}
	if got := b.Usage(); got.UsedBytes != 80 {
		t.Fatalf("could not reduce retained usage: %+v", got)
	}
}
