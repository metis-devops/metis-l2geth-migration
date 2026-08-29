package migration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

const defaultProgressInterval = 30 * time.Second

// ProgressOptions configures optional human-readable migration progress logs.
// A nil Logger disables all progress reporting.
type ProgressOptions struct {
	Logger   log.Logger
	Interval time.Duration
}

type progressSnapshot func(elapsed time.Duration, completed bool) []any

type progressReporter struct {
	logger    log.Logger
	interval  time.Duration
	startedAt time.Time
}

func newProgressReporter(operation string, opts ProgressOptions, attrs ...any) *progressReporter {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultProgressInterval
	}
	reporter := &progressReporter{interval: interval, startedAt: time.Now()}
	if opts.Logger == nil {
		return reporter
	}
	reporter.logger = opts.Logger.New("operation", operation)
	reporter.logger.Info("Migration operation started", append([]any{"status", "started"}, attrs...)...)
	return reporter
}

func (r *progressReporter) Info(msg string, attrs ...any) {
	if r == nil || r.logger == nil {
		return
	}
	r.logger.Info(msg, attrs...)
}

func (r *progressReporter) Enabled() bool {
	return r != nil && r.logger != nil
}

func (r *progressReporter) StartPhase(name string, snapshot progressSnapshot, attrs ...any) *phaseProgress {
	phase := &phaseProgress{snapshot: snapshot, startedAt: time.Now()}
	if r == nil || r.logger == nil {
		return phase
	}
	phase.logger = r.logger.New(append([]any{"phase", name}, attrs...)...)
	phase.done = make(chan struct{})
	phase.logger.Info("Migration phase started", "status", "started")
	phase.wg.Go(func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-phase.done:
				return
			case <-ticker.C:
				elapsed := time.Since(phase.startedAt)
				ctx := []any{"status", "progress", "elapsed", common.PrettyDuration(elapsed)}
				if phase.snapshot != nil {
					ctx = append(ctx, phase.snapshot(elapsed, false)...)
				}
				phase.logger.Info("Migration progress", ctx...)
			}
		}
	})
	return phase
}

func (r *progressReporter) Finish(err error, attrs ...any) {
	if r == nil || r.logger == nil {
		return
	}
	ctx := []any{"status", progressStatus(err), "elapsed", common.PrettyDuration(time.Since(r.startedAt))}
	ctx = append(ctx, attrs...)
	switch {
	case err == nil:
		r.logger.Info("Migration operation completed", ctx...)
	case isCancellation(err):
		r.logger.Warn("Migration operation canceled", append(ctx, "err", err)...)
	default:
		r.logger.Error("Migration operation failed", append(ctx, "err", err)...)
	}
}

type phaseProgress struct {
	logger    log.Logger
	snapshot  progressSnapshot
	startedAt time.Time
	done      chan struct{}
	wg        sync.WaitGroup
	once      sync.Once
}

func (p *phaseProgress) Finish(err error, attrs ...any) {
	if p == nil || p.logger == nil {
		return
	}
	p.once.Do(func() {
		close(p.done)
		p.wg.Wait()
		elapsed := time.Since(p.startedAt)
		ctx := []any{"status", progressStatus(err), "elapsed", common.PrettyDuration(elapsed)}
		if p.snapshot != nil {
			ctx = append(ctx, p.snapshot(elapsed, err == nil)...)
		}
		ctx = append(ctx, attrs...)
		switch {
		case err == nil:
			p.logger.Info("Migration phase completed", ctx...)
		case isCancellation(err):
			p.logger.Warn("Migration phase canceled", append(ctx, "err", err)...)
		default:
			p.logger.Error("Migration phase failed", append(ctx, "err", err)...)
		}
	})
}

func progressStatus(err error) string {
	if err == nil {
		return "completed"
	}
	if isCancellation(err) {
		return "canceled"
	}
	return "failed"
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

type progressCounts struct {
	accounts       atomic.Uint64
	storageSlots   atomic.Uint64
	codeReferences atomic.Uint64
	records        atomic.Uint64
	payloadBytes   atomic.Uint64
}

func (c *progressCounts) snapshot() bundle.Counts {
	if c == nil {
		return bundle.Counts{}
	}
	return bundle.Counts{
		Accounts:       c.accounts.Load(),
		StorageSlots:   c.storageSlots.Load(),
		CodeReferences: c.codeReferences.Load(),
		Records:        c.records.Load(),
		PayloadBytes:   c.payloadBytes.Load(),
	}
}

type countingStateVisitor struct {
	next   StateVisitor
	counts *progressCounts
}

func newCountingStateVisitor(next StateVisitor, counts *progressCounts) StateVisitor {
	return &countingStateVisitor{next: next, counts: counts}
}

func (v *countingStateVisitor) Account(hash common.Hash, account *types.StateAccount, fullRLP []byte) error {
	if v.next != nil {
		if err := v.next.Account(hash, account, fullRLP); err != nil {
			return err
		}
	}
	v.counts.accounts.Add(1)
	v.counts.records.Add(1)
	v.counts.payloadBytes.Add(uint64(len(fullRLP)))
	return nil
}

func (v *countingStateVisitor) Storage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	if v.next != nil {
		if err := v.next.Storage(accountHash, slotHash, valueRLP); err != nil {
			return err
		}
	}
	v.counts.storageSlots.Add(1)
	v.counts.records.Add(1)
	v.counts.payloadBytes.Add(uint64(len(valueRLP)))
	return nil
}

func (v *countingStateVisitor) Code(accountHash, codeHash common.Hash, code []byte) error {
	if v.next != nil {
		if err := v.next.Code(accountHash, codeHash, code); err != nil {
			return err
		}
	}
	v.counts.codeReferences.Add(1)
	v.counts.records.Add(1)
	v.counts.payloadBytes.Add(uint64(len(code)))
	return nil
}

func countProgressSnapshot(counts *progressCounts, total *bundle.Counts) progressSnapshot {
	return func(elapsed time.Duration, completed bool) []any {
		return countProgressAttrs(counts.snapshot(), total, elapsed, completed)
	}
}

func countProgressAttrs(current bundle.Counts, total *bundle.Counts, elapsed time.Duration, completed bool) []any {
	attrs := []any{
		"accounts", current.Accounts,
		"storage_slots", current.StorageSlots,
		"code_references", current.CodeReferences,
		"records", current.Records,
		"payload_bytes", current.PayloadBytes,
	}
	if elapsed > 0 {
		attrs = append(attrs, "records_per_second", uint64(float64(current.Records)/elapsed.Seconds()))
	}
	if total == nil {
		return attrs
	}
	percent := 0.0
	if total.Records == 0 {
		if completed {
			percent = 100
		}
	} else {
		percent = 100 * float64(current.Records) / float64(total.Records)
	}
	return append(attrs,
		"total_records", total.Records,
		"progress", fmt.Sprintf("%.1f%%", percent),
	)
}

func totalCountAttrs(total bundle.Counts) []any {
	return []any{
		"total_accounts", total.Accounts,
		"total_storage_slots", total.StorageSlots,
		"total_code_references", total.CodeReferences,
		"total_records", total.Records,
		"total_payload_bytes", total.PayloadBytes,
	}
}

func counterProgressSnapshot(counter *atomic.Uint64, name, rateName string) progressSnapshot {
	return func(elapsed time.Duration, _ bool) []any {
		value := counter.Load()
		attrs := []any{name, value}
		if elapsed > 0 {
			attrs = append(attrs, rateName, uint64(float64(value)/elapsed.Seconds()))
		}
		return attrs
	}
}

func percentageProgressSnapshot(percent *atomic.Uint64, estimated bool) progressSnapshot {
	return func(_ time.Duration, completed bool) []any {
		value := percent.Load()
		if completed {
			value = 100
		}
		return []any{"progress", fmt.Sprintf("%d%%", value), "estimated", estimated}
	}
}
