package migration

import (
	"context"
	"sync"
)

type migratePipelineFailure struct {
	mu      sync.Mutex
	err     error
	cancel  context.CancelFunc
	onFirst func(error)
}

func (f *migratePipelineFailure) record(err error) {
	if err == nil {
		return
	}
	f.mu.Lock()
	first := f.err == nil
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
	if first {
		f.cancel()
		if f.onFirst != nil {
			f.onFirst(err)
		}
	}
}

type migrateRunFailure struct {
	mu  sync.Mutex
	err error
}

func (f *migrateRunFailure) record(err error) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false
	}
	f.err = err
	return true
}

func (f *migrateRunFailure) load() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (m *partitionedStateMigrator) recordRunFailure(err error) {
	if m.runFailure.record(err) {
		if m.cancelRun != nil {
			m.cancelRun()
		}
	}
}

func (f *migratePipelineFailure) load() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

type migrateAccountWindow struct {
	slots chan struct{}
}

func newMigrateAccountWindow(workers int) *migrateAccountWindow {
	return &migrateAccountWindow{slots: make(chan struct{}, 2*normalizeMigrateWorkers(workers))}
}

func (w *migrateAccountWindow) acquire(ctx context.Context) error {
	select {
	case w.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *migrateAccountWindow) tryAcquire() bool {
	select {
	case w.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (w *migrateAccountWindow) pending() bool {
	return len(w.slots) != 0
}

func (w *migrateAccountWindow) release() {
	<-w.slots
}

func (w *migrateAccountWindow) capacity() int {
	return cap(w.slots)
}

type migrateWorkLimiter struct {
	tokens chan struct{}
}

func newMigrateWorkLimiter(workers int) *migrateWorkLimiter {
	return &migrateWorkLimiter{tokens: make(chan struct{}, normalizeMigrateWorkers(workers))}
}

func (l *migrateWorkLimiter) acquire(ctx context.Context) error {
	select {
	case l.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *migrateWorkLimiter) release() {
	<-l.tokens
}

type migrateWorkLease struct {
	limiter *migrateWorkLimiter
	held    bool
}

func newMigrateWorkLease(limiter *migrateWorkLimiter) *migrateWorkLease {
	return &migrateWorkLease{limiter: limiter}
}

func (l *migrateWorkLease) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.held {
		return nil
	}
	if err := l.limiter.acquire(ctx); err != nil {
		return err
	}
	l.held = true
	return nil
}

func (l *migrateWorkLease) release() {
	if l == nil || !l.held {
		return
	}
	l.held = false
	l.limiter.release()
}

func runMigratePartitionTasks(ctx context.Context, task func(context.Context, int) error) error {
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for index := range migrateTriePartitions {
		wg.Go(func() {
			if err := task(taskCtx, index); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
