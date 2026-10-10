package workspace

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func awaitMaterialization(t *testing.T, h *MaterializationHandle) error {
	t.Helper()
	select {
	case err, ok := <-h.Results():
		if !ok {
			t.Fatal("result closed without a terminal outcome")
		}
		return err
	case <-time.After(time.Second):
		t.Fatal("materialization did not settle")
		return nil
	}
}

func closeMaterializer(t *testing.T, m *Materializer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Error(err)
	}
}

func TestMaterializerSingleWorkerAndEightPendingSlots(t *testing.T) {
	m := NewMaterializer(Limits{})
	t.Cleanup(func() { closeMaterializer(t, m) })
	started := make(chan struct{})
	first, err := m.Submit(context.Background(), func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first job did not start")
	}
	var invoked atomic.Int32
	var queued []*MaterializationHandle
	for range 8 {
		h, err := m.Submit(context.Background(), func(context.Context) error {
			invoked.Add(1)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, h)
	}
	if _, err := m.SubmitPersisted(context.Background(), func() error {
		t.Error("queue-full request persisted")
		return nil
	}, func(context.Context) error { invoked.Add(1); return nil }); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("ninth pending job accepted: %v", err)
	}
	if invoked.Load() != 0 {
		t.Fatal("a second materializer worker ran concurrently")
	}
	closeMaterializer(t, m)
	if err := awaitMaterialization(t, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("running job was not canceled: %v", err)
	}
	for _, h := range queued {
		if err := awaitMaterialization(t, h); !errors.Is(err, context.Canceled) {
			t.Fatalf("queued job was not canceled: %v", err)
		}
	}
	if invoked.Load() != 0 {
		t.Fatal("queued job ran after shutdown")
	}
}

func TestMaterializerPersistenceFailureAndCancel(t *testing.T) {
	m := NewMaterializer(Limits{QueueCapacity: 1})
	t.Cleanup(func() { closeMaterializer(t, m) })
	failure := errors.New("journal unavailable")
	var invoked atomic.Int32
	if _, err := m.SubmitPersisted(context.Background(), func() error { return failure }, func(context.Context) error {
		invoked.Add(1)
		return nil
	}); !errors.Is(err, failure) {
		t.Fatalf("persistence failure lost: %v", err)
	}
	started := make(chan struct{})
	h, err := m.Submit(context.Background(), func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("failed persistence leaked a slot: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job did not start")
	}
	queued, err := m.Submit(context.Background(), func(context.Context) error { invoked.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	queued.Cancel()
	h.Cancel()
	if err := awaitMaterialization(t, h); !errors.Is(err, context.Canceled) {
		t.Fatalf("running cancel lost: %v", err)
	}
	if err := awaitMaterialization(t, queued); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancel lost: %v", err)
	}
	if invoked.Load() != 0 {
		t.Fatal("rejected/canceled job invoked")
	}
}

func TestMaterializerCloseWaitsForActualCallbackExit(t *testing.T) {
	m := NewMaterializer(Limits{})
	release := make(chan struct{})
	started := make(chan struct{})
	h, err := m.Submit(context.Background(), func(context.Context) error {
		close(started)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(release); closeMaterializer(t, m) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close falsely reported callback exit: %v", err)
	}
	select {
	case <-h.Results():
		t.Fatal("result settled before callback exit")
	default:
	}
	if _, err := m.Submit(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("submit after Close accepted: %v", err)
	}
}

func TestMaterializerDeadline(t *testing.T) {
	m := NewMaterializer(Limits{MaxDuration: 10 * time.Millisecond})
	t.Cleanup(func() { closeMaterializer(t, m) })
	h, err := m.Submit(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := awaitMaterialization(t, h); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("materialization deadline absent: %v", err)
	}
}

func TestMaterializerProductionDeadlineAndParentDeadline(t *testing.T) {
	if got := DefaultLimits().MaxDuration; got != 3*time.Minute {
		t.Fatalf("production create/materialize deadline = %s, want 3m", got)
	}
	m := NewMaterializer(Limits{})
	t.Cleanup(func() { closeMaterializer(t, m) })
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := make(chan time.Time, 1)
	h, err := m.Submit(parent, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			return errors.New("materializer context has no deadline")
		}
		started <- deadline
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case deadline := <-started:
		if remaining := time.Until(deadline); remaining > 20*time.Millisecond {
			t.Fatalf("materializer widened parent deadline: %s remain", remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("materializer did not start")
	}
	if err := awaitMaterialization(t, h); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("parent deadline was not enforced: %v", err)
	}
}
