package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

func TestPruneParallelMatchesSerialReference(t *testing.T) {
	for _, workload := range []struct {
		name            string
		accounts, slots int
		single          bool
	}{
		{"light", 40, 0, false}, {"small-storage", 32, 8, false}, {"threshold", 4, 1024, false},
		{"partitioned-storage", 4, 1025, false}, {"giant", 1, 8192, false}, {"single-partition", 16, 8, true},
	} {
		t.Run(workload.name, func(t *testing.T) {
			parent, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(parent, "source")
			root, _ := buildTraversalBenchmarkState(t, source, traversalBenchmarkWorkload{accounts: workload.accounts, storageEvery: 1, slotsPerAccount: workload.slots, codeSize: 32, singleAccountPartition: workload.single})
			writeTraversalBenchmarkHead(t, source, root)
			mutatePruneFixture(t, source, func(db ethdb.Database) {
				h := &types.Header{Root: types.EmptyRootHash, Number: big.NewInt(0)}
				rawdb.WriteHeader(db, h)
				rawdb.WriteCanonicalHash(db, h.Hash(), 0)
			})
			db, err := openPruneDatabase(source, 32, 32, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.close(); err != nil {
					t.Error(err)
				}
			}()
			head, _, err := readLegacyHead(db.view())
			if err != nil {
				t.Fatal(err)
			}
			serialDir := filepath.Join(parent, "serial")
			want, err := serialBuildPruneKeep(context.Background(), db.view(), serialDir, head, types.EmptyRootHash, nil)
			if err != nil {
				t.Fatal(err)
			}
			serialKeep, err := serialOpenPruneKeep(serialDir, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := serialKeep.Close(); err != nil {
					t.Error(err)
				}
			}()
			expectedKV := pruneTestKV(t, serialKeep)
			expectedInventory, err := serialScanPruneDB(context.Background(), db.view(), serialKeep)
			if err != nil {
				t.Fatal(err)
			}
			for _, workers := range []int{2, 4, 8, 16} {
				t.Run(fmt.Sprint(workers), func(t *testing.T) {
					e := newPruneExecution(PruneOptions{Workers: workers, CacheMB: 64, Handles: 64})
					dir := filepath.Join(parent, fmt.Sprint(workers))
					got, err := buildPruneKeep(context.Background(), db.view(), dir, head, types.EmptyRootHash, nil, e)
					if err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Fatalf("root/counts differ: %+v %+v", got, want)
					}
					keep, err := openPruneKeep(dir, true, e)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := keep.Close(); err != nil {
							t.Error(err)
						}
					}()
					actualKV := pruneTestKV(t, keep)
					if len(actualKV) != len(expectedKV) {
						t.Fatalf("raw inventory size differs %d %d", len(actualKV), len(expectedKV))
					}
					for k, v := range expectedKV {
						if !bytes.Equal(actualKV[k], v) {
							t.Fatalf("raw KV differs %x", k)
						}
					}
					inventory, err := scanPruneDB(context.Background(), db.view(), keep, e)
					if err != nil {
						t.Fatal(err)
					}
					if inventory != expectedInventory {
						t.Fatalf("inventory/digest differ %+v %+v", inventory, expectedInventory)
					}
				})
			}
		})
	}
}

func pruneTestKV(t *testing.T, db ethdb.Database) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		out[string(it.Key())] = bytes.Clone(it.Value())
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	return out
}

type pruneNoPointReads struct{ ethdb.Database }

func (pruneNoPointReads) Get([]byte) ([]byte, error) { return nil, errors.New("unexpected point read") }
func (pruneNoPointReads) Has([]byte) (bool, error) {
	return false, errors.New("unexpected point lookup")
}

func TestPruneMergeJoinAndMissingKeys(t *testing.T) {
	db, keep := rawdb.NewMemoryDatabase(), rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if err := keep.Close(); err != nil {
			t.Error(err)
		}
	}()
	for i := range 100 {
		value := bytes.Repeat([]byte{byte(i)}, 4096+i)
		key := crypto.Keccak256(value)
		if err := db.Put(key, value); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if err := keep.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, key := range [][]byte{{}, []byte("LastBlock"), bytes.Repeat([]byte{0xff}, 33)} {
		if err := db.Put(key, []byte("metadata")); err != nil {
			t.Fatal(err)
		}
	}
	want, err := serialScanPruneDB(context.Background(), db, keep)
	if err != nil {
		t.Fatal(err)
	}
	e := newPruneExecution(PruneOptions{Workers: 4, CacheMB: 32, Handles: 32})
	e.scanBytes = 8192
	got, err := scanPruneDB(context.Background(), pruneNoPointReads{db}, pruneNoPointReads{keep}, e)
	if err != nil || got != want {
		t.Fatalf("merge differs: %+v %+v %v", got, want, err)
	}
	missing := bytes.Repeat([]byte{0xff}, 32)
	if err := keep.Put(missing, []byte("missing")); err != nil {
		t.Fatal(err)
	}
	if _, err := scanPruneDB(context.Background(), db, keep, e); err == nil {
		t.Fatal("missing keep key accepted")
	}
}

func TestPruneHashPipelineOrderingAndBounds(t *testing.T) {
	for _, budget := range []int{8192, 1 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			e := newPruneExecution(PruneOptions{Workers: 4, CacheMB: 32, Handles: 32})
			e.scanBytes = budget
			var active, maxActive atomic.Int64
			later := make(chan struct{})
			e.classifyRecord = func(ctx context.Context, r *pruneScanRecord) error {
				now := active.Add(1)
				defer active.Add(-1)
				for old := maxActive.Load(); now > old; old = maxActive.Load() {
					if maxActive.CompareAndSwap(old, now) {
						break
					}
				}
				if budget > 16384 && r.value[0] == 0 {
					select {
					case <-later:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				err := classifyPruneRecord(ctx, r)
				if budget > 16384 && r.value[0] == 1 {
					close(later)
				}
				return err
			}
			consumed := 0
			pipeline := newPruneScanPipeline(context.Background(), e, func(r *pruneScanRecord) error {
				if r.value[0] != byte(consumed) {
					t.Errorf("completion order differs: %d %d", r.value[0], consumed)
				}
				if r.role != pruneDelete {
					t.Errorf("copied record corrupted, role=%d", r.role)
				}
				consumed++
				return nil
			})
			defer pipeline.close()
			for i := range 25 {
				size := 4096 + i
				if i == 12 {
					size = budget + 1
				}
				value := bytes.Repeat([]byte{byte(i)}, size)
				key := crypto.Keccak256(value)
				if err := pipeline.push(key, value, pruneUnclassified); err != nil {
					t.Fatal(err)
				}
				clear(value)
				clear(key)
				if pipeline.pending > 2*e.workers || pipeline.bytes > budget {
					t.Fatal("pipeline exceeded count/byte budget")
				}
			}
			if err := pipeline.drain(); err != nil {
				t.Fatal(err)
			}
			if consumed != 25 || active.Load() != 0 || maxActive.Load() > int64(e.workers) {
				t.Fatalf("unfinished or unbounded work: consumed=%d active=%d maximum=%d", consumed, active.Load(), maxActive.Load())
			}
		})
	}
}

type pruneFailingBatchDB struct {
	ethdb.Database
	failure error
	failPut bool
}

func (d pruneFailingBatchDB) NewBatchWithSize(int) ethdb.Batch {
	return pruneFailingBatch{Batch: d.NewBatch(), failure: d.failure, failPut: d.failPut}
}

type pruneFailingBatch struct {
	ethdb.Batch
	failure error
	failPut bool
}

func (b pruneFailingBatch) Write() error { return b.failure }
func (b pruneFailingBatch) Put(key, value []byte) error {
	if b.failPut {
		return b.failure
	}
	return b.Batch.Put(key, value)
}

func TestPruneCollectionPreservesWriterFailure(t *testing.T) {
	opts, fixture := pruneTestOptions(t)
	source, err := openPruneDatabase(opts.Chaindata, 16, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.close(); err != nil {
			t.Error(err)
		}
	}()
	target := rawdb.NewMemoryDatabase()
	defer func() {
		if err := target.Close(); err != nil {
			t.Error(err)
		}
	}()
	injected := errors.New("injected keep write failure")
	e := newPruneExecution(PruneOptions{Workers: 4, CacheMB: 32, Handles: 32})
	for _, failPut := range []bool{false, true} {
		_, err = collectPruneState(context.Background(), source.view(), pruneFailingBatchDB{target, injected, failPut}, fixture.head, types.EmptyRootHash, nil, e)
		if !errors.Is(err, injected) {
			t.Fatalf("lost original collection/flush error (put=%t): %v", failPut, err)
		}
	}
}

func TestPruneParallelHashErrorJoinsWorkers(t *testing.T) {
	db, keep := rawdb.NewMemoryDatabase(), rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if err := keep.Close(); err != nil {
			t.Error(err)
		}
	}()
	for i := range 50 {
		value := bytes.Repeat([]byte{byte(i)}, 8192)
		if err := db.Put(crypto.Keccak256(value), value); err != nil {
			t.Fatal(err)
		}
	}
	injected := errors.New("hash worker failed")
	var active atomic.Int64
	e := newPruneExecution(PruneOptions{Workers: 4, CacheMB: 32, Handles: 32})
	e.classifyRecord = func(ctx context.Context, r *pruneScanRecord) error {
		active.Add(1)
		defer active.Add(-1)
		if r.value[0] == 7 {
			return injected
		}
		return classifyPruneRecord(ctx, r)
	}
	if _, err := scanPruneDB(context.Background(), db, keep, e); !errors.Is(err, injected) {
		t.Fatalf("lost worker error: %v", err)
	}
	if active.Load() != 0 {
		t.Fatal("returned before hash workers joined")
	}
}

func TestPruneWorkerFailureStopsOrderedConsumer(t *testing.T) {
	db, keep := rawdb.NewMemoryDatabase(), rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
		if err := keep.Close(); err != nil {
			t.Error(err)
		}
	}()
	var keys [][]byte
	for i := range 2 {
		value := bytes.Repeat([]byte{byte(i)}, 8192)
		key := crypto.Keccak256(value)
		keys = append(keys, key)
		if err := db.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	injected := errors.New("later worker failed")
	e := newPruneExecution(PruneOptions{Workers: 4, CacheMB: 32, Handles: 32})
	e.classifyRecord = func(ctx context.Context, r *pruneScanRecord) error {
		if bytes.Equal(r.key, keys[0]) {
			<-ctx.Done()
			r.role = pruneDelete
			return nil
		}
		return injected
	}
	consumed := 0
	err := walkPruneDifference(context.Background(), db, keep, e, func(*pruneScanRecord) error { consumed++; return nil })
	if !errors.Is(err, injected) || consumed != 0 {
		t.Fatalf("consumer ran after failure: consumed=%d err=%v", consumed, err)
	}
}

func TestPruneDeletionMatchesSerialReport(t *testing.T) {
	for _, workers := range []int{2, 16} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			opts, _ := pruneTestOptions(t)
			before := pruneLogicalKV(t, opts.Chaindata)
			other := filepath.Join(filepath.Dir(opts.Chaindata), "optimized")
			mutatePruneFixture(t, other, func(db ethdb.Database) {
				for key, value := range before {
					if err := db.Put([]byte(key), value); err != nil {
						t.Fatal(err)
					}
				}
			})
			want, err := serialPrune(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			original := opts.Chaindata
			opts.Chaindata = other
			opts.Workers = workers
			got, err := Prune(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("report changed: %+v %+v", got, want)
			}
			a, b := pruneLogicalKV(t, original), pruneLogicalKV(t, other)
			if len(a) != len(b) {
				t.Fatal("deletion inventory differs")
			}
			for key, value := range a {
				if !bytes.Equal(b[key], value) {
					t.Fatalf("deletion differs at %x", key)
				}
			}
		})
	}
}

func TestPruneExecutionBudgets(t *testing.T) {
	for _, total := range []int{32, 64, 128, 512} {
		e := newPruneExecution(PruneOptions{Workers: -1, CacheMB: total, Handles: total})
		if e.workers != 2 || e.keepCache < 16 || e.sourceCache < 16 || e.keepCache+e.sourceCache != total || e.keepHandles+e.sourceHandles != total {
			t.Fatalf("bad resource budget %+v", e)
		}
	}
	opts, _ := pruneTestOptions(t)
	opts.Workers = 17
	if _, err := Prune(context.Background(), opts); err == nil {
		t.Fatal("workers above 16 accepted")
	}
}
