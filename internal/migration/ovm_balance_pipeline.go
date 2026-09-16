package migration

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

type ovmBalanceJob struct {
	address          common.Address
	slot, hash       common.Hash
	value            uint256.Int
	account          *types.StateAccount
	contract, retain bool
}

// Only fixed-size addresses, hashes, uint256s and canonical account RLP enter
// this queue. At most 2*workers records are retained, independently of trie size.
type ovmBalanceBatch struct {
	ctx         context.Context
	cancel      context.CancelFunc
	transformer *ovmTransformer
	jobs        sync.WaitGroup
	queue       []*ovmBalanceJob
	failure     migrateRunFailure
	readers     chan *ovmAccountReader
}

func (b *ovmBalanceBatch) submit(address common.Address, slot common.Hash, value *uint256.Int) error {
	if err := b.ctx.Err(); err != nil {
		return errors.Join(err, b.failure.load())
	}
	if len(b.queue) == 2*normalizeMigrateWorkers(b.transformer.workers) {
		if err := b.drain(); err != nil {
			return err
		}
	}
	job := &ovmBalanceJob{address: address, slot: slot, hash: crypto.Keccak256Hash(address[:]), value: *value}
	b.queue = append(b.queue, job)
	b.jobs.Go(func() {
		if err := b.inspect(job); err != nil {
			b.failure.record(err)
			b.cancel()
		}
	})
	return nil
}

func (b *ovmBalanceBatch) inspect(job *ovmBalanceJob) error {
	lease := newMigrateWorkLease(b.transformer.limiter)
	if err := lease.acquire(b.ctx); err != nil {
		return err
	}
	defer lease.release()
	if job.address == ovmETHAddress || job.value.IsZero() {
		return nil
	}
	reader := b.borrowReader()
	account, err := reader.read(job.hash)
	b.returnReader(reader)
	if err != nil {
		return err
	}
	if !account.Balance.IsZero() {
		return errors.New("OVM conversion encountered nonzero source native balance")
	}
	job.account = account
	job.contract = !bytes.Equal(account.CodeHash, types.EmptyCodeHash[:])
	if !job.contract {
		return nil // EOAs always convert, regardless of Transfer-from history.
	}
	_, job.retain, err = b.transformer.index.get('f', job.address[:])
	return err
}

// Readers are borrowed only while holding a worker lease, so active plus idle
// readers cannot exceed the normalized worker count. The bounded channel also
// permits direct serial inspection without a pool in focused tests.
func (b *ovmBalanceBatch) borrowReader() *ovmAccountReader {
	select {
	case r := <-b.readers:
		return r
	default:
		return &ovmAccountReader{db: b.transformer.trieDB, root: b.transformer.root}
	}
}

func (b *ovmBalanceBatch) returnReader(r *ovmAccountReader) {
	select {
	case b.readers <- r:
	default:
	}
}

func (b *ovmBalanceBatch) drain() error {
	b.jobs.Wait()
	if err := b.failure.load(); err != nil {
		return err
	}
	for _, job := range b.queue {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		if err := b.transformer.applyBalance(job); err != nil {
			return err
		}
	}
	clear(b.queue)
	b.queue = b.queue[:0]
	return nil
}
