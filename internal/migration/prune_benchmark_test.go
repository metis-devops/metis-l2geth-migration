package migration

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

type pruneBenchmarkOps struct {
	run         func(context.Context, PruneOptions) (PruneResult, error)
	collect     func(context.Context, ethdb.Database, string, bundle.Head, common.Hash, []byte) (StateResult, error)
	verify      func(context.Context, ethdb.Database, bundle.Head, StateResult) error
	scan        func(context.Context, ethdb.Database, ethdb.Database) (pruneInventory, error)
	remove      func(context.Context, *pruneDatabase, ethdb.Database, pruneHooks, *bool) (PruneKVCount, error)
	openKeep    func(string, bool) (ethdb.Database, error)
	sourceCache int
}

func serialPruneBenchmarkOps() pruneBenchmarkOps {
	return pruneBenchmarkOps{
		run: serialPrune, collect: serialBuildPruneKeep, verify: serialVerifyPruneState, scan: serialScanPruneDB,
		remove: serialDeletePruneDifference, openKeep: serialOpenPruneKeep, sourceCache: 112,
	}
}

func BenchmarkPruneSerial(b *testing.B) { benchmarkPruneSuite(b, serialPruneBenchmarkOps()) }

func optimizedPruneBenchmarkOps(workers int) pruneBenchmarkOps {
	e := newPruneExecution(PruneOptions{Workers: workers, CacheMB: 128, Handles: 128})
	return pruneBenchmarkOps{
		run: func(ctx context.Context, o PruneOptions) (PruneResult, error) {
			o.Workers = workers
			return Prune(ctx, o)
		},
		collect: func(ctx context.Context, db ethdb.Database, path string, head bundle.Head, genesis common.Hash, node []byte) (StateResult, error) {
			return buildPruneKeep(ctx, db, path, head, genesis, node, e)
		},
		verify: func(ctx context.Context, db ethdb.Database, head bundle.Head, state StateResult) error {
			return verifyPruneState(ctx, db, head, state, e)
		},
		scan: func(ctx context.Context, db, keep ethdb.Database) (pruneInventory, error) {
			return scanPruneDB(ctx, db, keep, e)
		},
		remove: func(ctx context.Context, db *pruneDatabase, keep ethdb.Database, hooks pruneHooks, started *bool) (PruneKVCount, error) {
			return deletePruneDifference(ctx, db, keep, hooks, started, e)
		},
		openKeep:    func(path string, readonly bool) (ethdb.Database, error) { return openPruneKeep(path, readonly, e) },
		sourceCache: e.sourceCache,
	}
}

func BenchmarkPruneOptimized(b *testing.B) {
	for _, workers := range []int{2, 4, 8, 16} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) { benchmarkPruneSuite(b, optimizedPruneBenchmarkOps(workers)) })
	}
}

func benchmarkPruneSuite(b *testing.B, ops pruneBenchmarkOps) {
	for _, w := range []struct {
		name                              string
		accounts, every, slots, code, old int
	}{
		{"light", 12000, 12001, 0, 32, 0},
		{"dense-storage", 256, 1, 64, 32, 0},
		{"storage-1024", 8, 1, 1024, 32, 0},
		{"storage-1025", 8, 1, 1025, 32, 0},
		{"giant-storage", 1, 1, 32768, 32, 0},
		{"shared-code", 12000, 12001, 0, 32768, 0},
		{"history-90pct", 1000, 1001, 0, 32, 16000},
		{"history-99pct", 1000, 1001, 0, 32, 160000},
	} {
		b.Run(w.name, func(b *testing.B) {
			source := filepath.Join(b.TempDir(), "source")
			root, _ := buildTraversalBenchmarkState(b, source, traversalBenchmarkWorkload{accounts: w.accounts, storageEvery: w.every, slotsPerAccount: w.slots, codeSize: w.code})
			writeTraversalBenchmarkHead(b, source, root)
			kv, err := gethleveldb.New(source, 16, 16, "", false)
			benchPruneCheck(b, err)
			db := rawdb.NewDatabase(kv)
			genesis := &types.Header{Root: types.EmptyRootHash, Number: big.NewInt(0), Difficulty: big.NewInt(1)}
			rawdb.WriteHeader(db, genesis)
			rawdb.WriteCanonicalHash(db, genesis.Hash(), 0)
			batch := db.NewBatch()
			for i := range w.old {
				value := fmt.Appendf(nil, "obsolete-state-%08d", i)
				benchPruneCheck(b, batch.Put(crypto.Keccak256(value), value))
				if batch.ValueSize() > 1<<20 {
					benchPruneCheck(b, batch.Write())
					batch.Reset()
				}
			}
			benchPruneCheck(b, batch.Write())
			batch.Close()
			benchPruneCheck(b, db.Close())
			for _, phase := range []string{"collect", "verify", "scan", "delete", "end-to-end"} {
				b.Run(phase, func(b *testing.B) { benchmarkPrunePhase(b, source, phase, ops) })
			}
		})
	}
}

func benchmarkPrunePhase(b *testing.B, original, phase string, ops pruneBenchmarkOps) {
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	var peakHeap, tempBytes uint64
	var ioReads, ioWrites int64
	for range b.N {
		parent, err := filepath.EvalSymlinks(b.TempDir())
		benchPruneCheck(b, err)
		source := original
		if phase == "delete" || phase == "end-to-end" {
			source = filepath.Join(parent, "source")
			copyPruneBenchmarkDB(b, original, source)
		}
		temp := filepath.Join(parent, "temp")
		benchPruneCheck(b, os.Mkdir(temp, 0755))
		opts := PruneOptions{Chaindata: source, TempDir: temp, CacheMB: 128, Handles: 128}
		if phase == "end-to-end" {
			stop := samplePruneBenchmarkHeap()
			reads, writes := pruneBenchmarkIO()
			b.StartTimer()
			_, err := ops.run(context.Background(), opts)
			b.StopTimer()
			r, w := pruneBenchmarkIO()
			ioReads += r - reads
			ioWrites += w - writes
			peakHeap = max(peakHeap, stop())
			benchPruneCheck(b, err)
			continue
		}
		db, err := openPruneDatabase(source, ops.sourceCache, ops.sourceCache, phase != "delete")
		benchPruneCheck(b, err)
		head, _, err := readLegacyHead(db.view())
		benchPruneCheck(b, err)
		var state StateResult
		var keep ethdb.Database
		if phase != "collect" {
			state, err = serialBuildPruneKeep(context.Background(), db.view(), temp, head, types.EmptyRootHash, nil)
			benchPruneCheck(b, err)
			keep, err = ops.openKeep(temp, true)
			benchPruneCheck(b, err)
		}
		if phase == "delete" {
			benchPruneCheck(b, db.reopen(false))
		}
		stop := samplePruneBenchmarkHeap()
		reads, writes := pruneBenchmarkIO()
		b.StartTimer()
		switch phase {
		case "collect":
			_, err = ops.collect(context.Background(), db.view(), temp, head, types.EmptyRootHash, nil)
		case "verify":
			err = ops.verify(context.Background(), keep, head, state)
		case "scan":
			_, err = ops.scan(context.Background(), db.view(), keep)
		case "delete":
			started := false
			_, err = ops.remove(context.Background(), db, keep, defaultPruneHooks(), &started)
		}
		b.StopTimer()
		r, w := pruneBenchmarkIO()
		ioReads += r - reads
		ioWrites += w - writes
		peakHeap = max(peakHeap, stop())
		benchPruneCheck(b, err)
		tempBytes = max(tempBytes, pruneBenchmarkDisk(b, temp))
		if keep != nil {
			benchPruneCheck(b, keep.Close())
		}
		benchPruneCheck(b, db.close())
	}
	b.ReportMetric(float64(peakHeap), "peak-heap-B")
	if phase != "end-to-end" {
		b.ReportMetric(float64(tempBytes), "temp-disk-B")
	}
	if ioReads >= 0 && ioWrites >= 0 {
		b.ReportMetric(float64(ioReads)/float64(b.N), "fs-read-blocks/op")
		b.ReportMetric(float64(ioWrites)/float64(b.N), "fs-write-blocks/op")
	}
}

func samplePruneBenchmarkHeap() func() uint64 {
	runtime.GC() // Exclude garbage from fixture preparation and previous phases.
	var peak atomic.Uint64
	done := make(chan struct{})
	joined := make(chan struct{})
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		for old := peak.Load(); m.HeapInuse > old; old = peak.Load() {
			if peak.CompareAndSwap(old, m.HeapInuse) {
				break
			}
		}
	}
	sample()
	go func() {
		defer close(joined)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-done:
				return
			}
		}
	}()
	return func() uint64 { close(done); <-joined; sample(); return peak.Load() }
}

func copyPruneBenchmarkDB(b *testing.B, source, target string) {
	b.Helper()
	benchPruneCheck(b, os.Mkdir(target, 0755))
	entries, err := os.ReadDir(source)
	benchPruneCheck(b, err)
	for _, e := range entries {
		in, err := os.Open(filepath.Join(source, e.Name()))
		benchPruneCheck(b, err)
		out, err := os.Create(filepath.Join(target, e.Name()))
		benchPruneCheck(b, err)
		_, err = io.Copy(out, in)
		benchPruneCheck(b, err)
		benchPruneCheck(b, out.Close())
		benchPruneCheck(b, in.Close())
	}
}

func pruneBenchmarkDisk(b *testing.B, path string) uint64 {
	b.Helper()
	var total uint64
	benchPruneCheck(b, filepath.WalkDir(path, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			info, err := e.Info()
			if err != nil {
				return err
			}
			total += uint64(info.Size())
		}
		return nil
	}))
	return total
}

func benchPruneCheck(b *testing.B, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
}
