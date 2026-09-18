package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

// The first preimage read and canonical read rendezvous while holding leases.
// A serial evidence lane therefore cannot pass, including with only two workers.
type evidenceBarrierDB struct {
	ethdb.Database
	ctx               context.Context
	preimage, history chan struct{}
	historyOnce       sync.Once
	released          atomic.Bool
	failure           string
	injected          error
}

func (d *evidenceBarrierDB) Get(key []byte) ([]byte, error) {
	if len(key) == 10 && key[0] == 'h' && key[9] == 'n' {
		d.historyOnce.Do(func() { close(d.history) })
		select {
		case <-d.preimage:
		case <-d.ctx.Done():
			return nil, d.ctx.Err()
		}
		if d.failure == "history" {
			return nil, d.injected
		}
	}
	return d.Database.Get(key)
}

func (d *evidenceBarrierDB) NewIterator(prefix, start []byte) ethdb.Iterator {
	it := d.Database.NewIterator(prefix, start)
	if bytes.Equal(prefix, []byte("secure-key-")) {
		return &evidenceBarrierIterator{Iterator: it, db: d}
	}
	return it
}

type evidenceBarrierIterator struct {
	ethdb.Iterator
	db   *evidenceBarrierDB
	once sync.Once
}

func (it *evidenceBarrierIterator) Next() bool {
	it.once.Do(func() {
		close(it.db.preimage)
		select {
		case <-it.db.history:
		case <-it.db.ctx.Done():
		}
	})
	if it.db.failure == "preimage" || it.db.ctx.Err() != nil {
		return false
	}
	return it.Iterator.Next()
}

func (it *evidenceBarrierIterator) Error() error {
	if it.db.failure == "preimage" {
		return it.db.injected
	}
	return errors.Join(it.Iterator.Error(), it.db.ctx.Err())
}

func (it *evidenceBarrierIterator) Release() {
	it.Iterator.Release()
	it.db.released.Store(true)
}

func TestOVMEvidenceParallel(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, workers := range []int{2, 16} {
			t.Run(fmt.Sprintf("%s/workers=%d", mode, workers), func(t *testing.T) {
				f := newOVMFixtureSized(t, 6000, nil) // Cross the evidence batch flush boundary.
				editOVMSource(t, f, func(db ethdb.Database) {
					for _, address := range f.holders {
						putOVMTest(t, db, append([]byte("secure-key-"), crypto.Keccak256(address[:])...), address[:])
					}
				})
				w := prepareEvidenceWork(t, f, mode, workers)
				reference := testRetentionIndex(t, mode)
				if err := reference.address(ovmETHAddress); err != nil {
					t.Fatal(err)
				}
				if err := collectOVMPreimages(t.Context(), w.source.db, reference, w.limiter); err != nil {
					t.Fatal(err)
				}
				history, err := scanOVMHistory(t.Context(), w.source, w.opts, reference, w.limiter, w.reporter)
				if err != nil {
					t.Fatal(err)
				}
				tracked := installEvidenceBarrier(w, "")
				if err := w.migrateOriginalAndHistory(); err != nil {
					t.Fatal(err)
				}
				if !tracked.released.Load() || len(w.limiter.tokens) != 0 {
					t.Fatal("preimage iterator or worker lease leaked")
				}
				if history != w.history || !reflect.DeepEqual(collectDatabaseEntries(t, reference.db), collectDatabaseEntries(t, w.indexDB)) {
					t.Fatal("parallel evidence differs from serial collection")
				}
				if w.original.Root != f.root {
					t.Fatal("original state changed")
				}
			})
		}
	}
}

func TestOVMEvidenceFailureJoins(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, failure := range []string{"preimage", "history", "preimage-flush"} {
			t.Run(string(mode)+"/"+failure, func(t *testing.T) {
				w := prepareEvidenceWork(t, newOVMFixture(t, nil), mode, 2)
				base := w.base
				tracked := installEvidenceBarrier(w, failure)
				if failure == "preimage-flush" {
					w.indexDB = &evidenceFlushDB{Database: w.indexDB, err: tracked.injected}
				}
				err := w.migrateOriginalAndHistory()
				if !errors.Is(err, tracked.injected) {
					t.Fatalf("lost evidence error: %v", err)
				}
				if !tracked.released.Load() || len(w.limiter.tokens) != 0 {
					t.Fatal("returned before workers/iterator joined")
				}
				if w.base != base {
					t.Fatal("reopened original state after evidence failure")
				}
			})
		}
	}
}

func prepareEvidenceWork(t *testing.T, f ovmFixture, mode TempDBMode, workers int) *ovmWork {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	opts := f.options(t, DBEnginePebble, "hash", workers)
	opts.TempDB, opts.OVM.StateWitness = mode, ""
	w := &ovmWork{ctx: ctx, opts: opts, path: t.TempDir(), storage: newTemporaryStorage(mode), reporter: newProgressReporter("test", ProgressOptions{}), limiter: newMigrateWorkLimiter(workers)}
	t.Cleanup(func() {
		if err := errors.Join(w.close(), w.storage.remove(w.path)); err != nil {
			t.Error(err)
		}
	})
	if err := w.prepare(); err != nil {
		t.Fatal(err)
	}
	return w
}

func installEvidenceBarrier(w *ovmWork, failure string) *evidenceBarrierDB {
	d := &evidenceBarrierDB{Database: w.source.db, ctx: w.ctx, preimage: make(chan struct{}), history: make(chan struct{}), failure: failure, injected: errors.New("injected evidence failure")}
	w.source.db = d
	return d
}

// Only the new preimage batch sees this failure; history retains its own batch.
type evidenceFlushDB struct {
	ethdb.Database
	err error
}

func (d *evidenceFlushDB) NewBatch() ethdb.Batch {
	return &evidenceFlushBatch{Batch: d.Database.NewBatch(), err: d.err}
}

type evidenceFlushBatch struct {
	ethdb.Batch
	err error
}

func (b *evidenceFlushBatch) Write() error { return b.err }
