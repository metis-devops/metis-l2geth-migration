package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

const pruneUnclassified = -1

type pruneScanRecord struct {
	key, value []byte
	role       int
	err        error
	done       chan struct{}
}

// pruneMergeCursor never point-looks-up keys in the keep database. A missing
// keep key is an error rather than silently advancing past protected state.
type pruneMergeCursor struct {
	keep  ethdb.Iterator
	valid bool
}

func (m *pruneMergeCursor) advance() error {
	m.valid = m.keep.Next()
	if !m.valid {
		return m.keep.Error()
	}
	return nil
}
func (m *pruneMergeCursor) role(key, value []byte) (int, error) {
	if m.valid {
		order := bytes.Compare(key, m.keep.Key())
		if order > 0 {
			return 0, fmt.Errorf("prune keep key %x is missing from source", m.keep.Key())
		}
		if order == 0 {
			if err := comparePruneValue(key, m.keep.Value(), value); err != nil {
				return 0, err
			}
			return pruneRetainState, m.advance()
		}
	}
	if len(key) == common.HashLength {
		return pruneUnclassified, nil
	}
	return pruneProtect, nil
}

type pruneScanPipeline struct {
	ctx                   context.Context
	slots                 []pruneScanRecord
	first, pending, bytes int
	limit                 int
	jobs                  chan *pruneScanRecord
	workers               int
	tasks                 sync.WaitGroup
	consume               func(*pruneScanRecord) error
	classify              func(context.Context, *pruneScanRecord) error
	cancel                context.CancelFunc
	failure               migrateRunFailure
	direct                pruneScanRecord
}

func newPruneScanPipeline(ctx context.Context, e *pruneExecution, consume func(*pruneScanRecord) error) *pruneScanPipeline {
	workCtx, cancel := context.WithCancel(ctx)
	// Reserve one execution slot for the ordered digest/write coordinator.
	p := &pruneScanPipeline{ctx: workCtx, cancel: cancel, slots: make([]pruneScanRecord, 2*e.workers), limit: e.scanBytes, workers: max(1, e.workers-1), consume: consume}
	p.classify = e.classifyRecord
	if p.classify == nil {
		p.classify = classifyPruneRecord
	}
	for i := range p.slots {
		p.slots[i].done = make(chan struct{}, 1)
	}
	return p
}

func (p *pruneScanPipeline) startWorkers() {
	if p.jobs != nil {
		return
	}
	p.jobs = make(chan *pruneScanRecord, p.workers)
	for range p.workers {
		p.tasks.Go(func() {
			for r := range p.jobs {
				r.err = p.classify(p.ctx, r)
				p.fail(r.err)
				r.done <- struct{}{}
			}
		})
	}
}

func (p *pruneScanPipeline) close() {
	p.cancel()
	if p.jobs != nil {
		close(p.jobs)
		p.tasks.Wait()
	}
}

func (p *pruneScanPipeline) fail(err error) {
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && p.failure.record(err) {
		p.cancel()
	}
}

func (p *pruneScanPipeline) drainOne() error {
	r := &p.slots[p.first]
	<-r.done
	err := r.err
	if err == nil {
		err = p.ctx.Err()
	}
	if err == nil {
		err = p.consume(r)
	}
	p.fail(err)
	p.bytes -= len(r.key) + len(r.value)
	r.key = nil
	r.value = nil
	r.err = nil
	p.first = (p.first + 1) % len(p.slots)
	p.pending--
	return err
}

func (p *pruneScanPipeline) drain() error {
	for p.pending > 0 {
		if err := p.drainOne(); err != nil {
			return err
		}
	}
	return p.ctx.Err()
}

func (p *pruneScanPipeline) push(key, value []byte, role int) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	size := len(key) + len(value)
	// Small hashes cost less than dispatch/copying. Empty-pipeline small records
	// and single oversize records use the iterator-owned bytes synchronously.
	parallel := role == pruneUnclassified && len(value) >= pruneParallelHashBytes
	if size > p.limit || (!parallel && p.pending == 0) {
		if err := p.drain(); err != nil {
			return err
		}
		return p.consumeDirect(key, value, role)
	}
	for p.pending == len(p.slots) || p.bytes+size > p.limit {
		if err := p.drainOne(); err != nil {
			return err
		}
	}
	r := &p.slots[(p.first+p.pending)%len(p.slots)]
	buffer := make([]byte, size)
	copy(buffer, key)
	copy(buffer[len(key):], value)
	r.key = buffer[:len(key)]
	r.value = buffer[len(key):]
	r.role = role
	p.pending++
	p.bytes += size
	p.startWorkers()
	p.jobs <- r
	return nil
}

// The direct record is never handed to a worker. Reusing it avoids an escaping
// allocation per small source record; callbacks may only borrow its slices.
func (p *pruneScanPipeline) consumeDirect(key, value []byte, role int) error {
	p.direct.key = key
	p.direct.value = value
	p.direct.role = role
	defer func() { p.direct.key = nil; p.direct.value = nil }()
	if err := p.classify(p.ctx, &p.direct); err != nil {
		return err
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return p.consume(&p.direct)
}

func classifyPruneRecord(ctx context.Context, r *pruneScanRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.role == pruneUnclassified {
		if crypto.Keccak256Hash(r.value) == common.BytesToHash(r.key) {
			r.role = pruneDelete
		} else {
			r.role = pruneUnknown
		}
	}
	if r.role != pruneDelete && r.role != pruneRetainState {
		return rejectPruneForeignKey(r.key, r.value)
	}
	return nil
}

func walkPruneDifference(ctx context.Context, db, keep ethdb.Database, e *pruneExecution, consume func(*pruneScanRecord) error) (retErr error) {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	source := db.NewIterator(nil, nil)
	defer source.Release()
	cursor := pruneMergeCursor{keep: keep.NewIterator(nil, nil)}
	defer cursor.keep.Release()
	if err := cursor.advance(); err != nil {
		return fmt.Errorf("open prune keep cursor: %w", err)
	}
	pipeline := newPruneScanPipeline(workCtx, e, func(r *pruneScanRecord) error {
		if err := workCtx.Err(); err != nil {
			return err
		}
		if err := consume(r); err != nil {
			return err
		}
		if e.scan != nil {
			e.scan.keys.Add(1)
			e.scan.bytes.Add(uint64(len(r.key) + len(r.value)))
		}
		return nil
	})
	defer func() {
		cancel()
		pipeline.close()
		if failure := pipeline.failure.load(); failure != nil {
			retErr = failure
		}
	}()
	for source.Next() {
		role, err := cursor.role(source.Key(), source.Value())
		if err != nil {
			return err
		}
		if err := pipeline.push(source.Key(), source.Value(), role); err != nil {
			return err
		}
	}
	if err := source.Error(); err != nil {
		return fmt.Errorf("scan prune source: %w", err)
	}
	if cursor.valid {
		return fmt.Errorf("prune keep key %x is missing from source", cursor.keep.Key())
	}
	return pipeline.drain()
}
