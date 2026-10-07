package workspace

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Materializer is the sole project-data materialization worker. It has no
// model workers; agent calls continue to use the existing shared model pool.
type Materializer struct {
	mu       sync.Mutex
	closed   bool
	ctx      context.Context
	cancel   context.CancelFunc
	queue    chan *materialization
	slots    chan struct{}
	done     chan struct{}
	duration time.Duration
}

type MaterializationHandle struct {
	result chan error
	cancel context.CancelFunc
}

func (h *MaterializationHandle) Results() <-chan error { return h.result }
func (h *MaterializationHandle) Cancel()               { h.cancel() }

type materialization struct {
	ctx    context.Context
	run    func(context.Context) error
	handle *MaterializationHandle
	stop   func() bool
}

func NewMaterializer(limits Limits) *Materializer {
	limits = limits.Normalized()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Materializer{ctx: ctx, cancel: cancel, queue: make(chan *materialization, limits.QueueCapacity), slots: make(chan struct{}, limits.QueueCapacity), done: make(chan struct{}), duration: limits.MaxDuration}
	go m.worker()
	return m
}

func (m *Materializer) Submit(ctx context.Context, run func(context.Context) error) (*MaterializationHandle, error) {
	return m.SubmitPersisted(ctx, nil, run)
}

// SubmitPersisted reserves a queue position before persisting the accepted
// intent. A persistence failure releases it and never invokes run. queued must
// not re-enter this materializer; it should perform a bounded journal write.
func (m *Materializer) SubmitPersisted(ctx context.Context, queued func() error, run func(context.Context) error) (*MaterializationHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrUnavailable
	}
	select {
	case m.slots <- struct{}{}:
	default:
		return nil, ErrQueueFull
	}
	if queued != nil {
		if err := queued(); err != nil {
			<-m.slots
			return nil, err
		}
	}
	jobCtx, cancel := context.WithTimeout(ctx, m.duration)
	h := &MaterializationHandle{result: make(chan error, 1), cancel: cancel}
	job := &materialization{ctx: jobCtx, run: run, handle: h, stop: context.AfterFunc(m.ctx, cancel)}
	m.queue <- job // capacity was reserved above; accepted intent must settle
	return h, nil
}

func (m *Materializer) worker() {
	defer close(m.done)
	for {
		select {
		case <-m.ctx.Done():
			for {
				select {
				case job := <-m.queue:
					<-m.slots
					job.finish(context.Canceled)
				default:
					return
				}
			}
		case job := <-m.queue:
			<-m.slots
			if err := job.ctx.Err(); err != nil {
				job.finish(err)
				continue
			}
			if err := m.ctx.Err(); err != nil {
				job.finish(err)
				continue
			}
			job.finish(runMaterialization(job))
		}
	}
}

func runMaterialization(job *materialization) (err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("workspace materialization panicked: %v", failure)
		}
	}()
	err = job.run(job.ctx)
	if err == nil {
		err = job.ctx.Err()
	}
	return err
}

func (job *materialization) finish(err error) {
	job.stop()
	job.handle.cancel()
	job.handle.result <- err
	close(job.handle.result)
}

// Close cancels queued/running jobs and waits only as long as ctx permits.
// A deadline error means a callback has not exited; it does not claim that an
// external process or lease has been stopped. Repeated calls can await it.
func (m *Materializer) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
