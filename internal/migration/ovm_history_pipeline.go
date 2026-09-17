package migration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type ovmHistoryBlock struct {
	number            uint64
	hash              common.Hash
	header            types.Header
	headerRLP, raw    []byte
	alternateReceipts []byte
	receipts          types.Receipts
	err               error
}

// The queue accounts for raw input bytes; decoded receipts are released when
// the ordered batch is consumed. One current block, including both receipt
// copies when they differ, may exceed the budget: drain the queue and decode
// that block synchronously.
type ovmHistoryBatch struct {
	ctx     context.Context
	limiter *migrateWorkLimiter
	blocks  []*ovmHistoryBlock
	bytes   int
	jobs    sync.WaitGroup
	decode  func(*ovmHistoryBlock) error
	cancel  context.CancelFunc
	failure migrateRunFailure
}

func (s *ovmHistoryScanner) scanParallel(ctx context.Context, opts MigrateOptions, limiter *migrateWorkLimiter, completed *atomic.Uint64) (retErr error) {
	ctx, cancel := context.WithCancel(ctx)
	batch := ovmHistoryBatch{ctx: ctx, limiter: limiter, cancel: cancel}
	defer func() { cancel(); batch.jobs.Wait(); retErr = errors.Join(retErr, batch.failure.load()) }()
	budget := max(1<<20, min(16<<20, opts.CacheMB*1024*1024/8))
	capacity := 2 * normalizeMigrateWorkers(opts.Workers)
	for number := uint64(0); ; number++ {
		lease := newMigrateWorkLease(limiter)
		if err := lease.acquire(ctx); err != nil {
			return err
		}
		block, err := s.readBlock(ctx, number)
		lease.release()
		if err != nil {
			return fmt.Errorf("OVM history block %d: %w", number, err)
		}
		size := len(block.raw) + len(block.alternateReceipts) + len(block.headerRLP)
		if len(batch.blocks) == capacity || size > budget-batch.bytes {
			if err := s.drainHistory(&batch, completed); err != nil {
				return err
			}
		}
		if size > budget {
			if err := batch.process(block); err != nil {
				return err
			}
			if err := s.acceptHistory(ctx, limiter, block); err != nil {
				return err
			}
			completed.Add(1)
		} else {
			batch.submit(block, size)
		}
		if number == s.source.head.BlockNumber {
			break
		}
	}
	return s.drainHistory(&batch, completed)
}

func (b *ovmHistoryBatch) process(block *ovmHistoryBlock) error {
	lease := newMigrateWorkLease(b.limiter)
	if err := lease.acquire(b.ctx); err != nil {
		return err
	}
	defer lease.release()
	decode := b.decode
	if decode == nil {
		decode = func(block *ovmHistoryBlock) error { return block.decode(b.ctx) }
	}
	if err := decode(block); err != nil {
		return fmt.Errorf("decode OVM block %d: %w", block.number, err)
	}
	return b.ctx.Err()
}

func (b *ovmHistoryBatch) submit(block *ovmHistoryBlock, size int) {
	b.blocks = append(b.blocks, block)
	b.bytes += size
	b.jobs.Go(func() {
		block.err = b.process(block)
		if block.err != nil {
			b.failure.record(block.err)
			b.cancel()
		}
	})
}

func (s *ovmHistoryScanner) drainHistory(b *ovmHistoryBatch, completed *atomic.Uint64) error {
	b.jobs.Wait()
	var failure error
	for _, block := range b.blocks {
		failure = errors.Join(failure, block.err)
	}
	if failure != nil {
		return failure
	}
	for _, block := range b.blocks {
		if err := s.acceptHistory(b.ctx, b.limiter, block); err != nil {
			return err
		}
		completed.Add(1)
	}
	clear(b.blocks)
	b.blocks = b.blocks[:0]
	b.bytes = 0
	return nil
}

func (s *ovmHistoryScanner) acceptHistory(ctx context.Context, limiter *migrateWorkLimiter, block *ovmHistoryBlock) error {
	lease := newMigrateWorkLease(limiter)
	if err := lease.acquire(ctx); err != nil {
		return err
	}
	defer lease.release()
	return s.acceptBlock(ctx, block)
}
