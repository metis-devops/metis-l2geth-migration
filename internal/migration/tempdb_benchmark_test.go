package migration

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
)

func ovmBenchmarkSettings(b *testing.B) (int, TempDBMode) {
	b.Helper()
	count := 10000
	if value := os.Getenv("L2STATE_BENCH_HOLDERS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1000 {
			b.Fatalf("invalid holders: %s", value)
		}
		count = n
	}
	mode := TempDBMode(os.Getenv("L2STATE_BENCH_TEMP_DB"))
	if err := mode.validate(); err != nil {
		b.Fatal(err)
	}
	return count, mode.normalized()
}

type temporaryBenchmarkSampler struct {
	mu          sync.Mutex
	filesystems []vfs.FS
}

// Sampling excludes fixture setup. Files can disappear during compaction or
// cleanup; only not-found is ignored. File lengths are not allocator capacities.
func sampleTemporaryBenchmark(b *testing.B, parent context.Context, root string) (context.Context, func()) {
	b.Helper()
	sampler := new(temporaryBenchmarkSampler)
	ctx := context.WithValue(parent, temporaryStorageObserverKey{}, func(fs vfs.FS) {
		sampler.mu.Lock()
		sampler.filesystems = append(sampler.filesystems, fs)
		sampler.mu.Unlock()
	})
	done := make(chan struct{})
	joined := make(chan struct{})
	var peakHeap, peakFiles, peakDisk, peakTemp uint64
	var sampleErr error
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	sample := func() {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		peakHeap = max(peakHeap, stats.HeapAlloc)
		var files uint64
		sampler.mu.Lock()
		for _, fs := range sampler.filesystems {
			size, err := memoryFileBytes(fs, "/")
			files += size
			sampleErr = errors.Join(sampleErr, err)
		}
		sampler.mu.Unlock()
		peakFiles = max(peakFiles, files)
		var disk, temp uint64
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			disk += uint64(info.Size())
			if strings.Contains(path, ".ovm-work") || strings.Contains(path, trieNodeIndexTempPrefix) {
				temp += uint64(info.Size())
			}
			return nil
		})
		sampleErr = errors.Join(sampleErr, err)
		peakDisk = max(peakDisk, disk)
		peakTemp = max(peakTemp, temp)
	}
	sample()
	go func() {
		defer close(joined)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				sample()
				return
			case <-ticker.C:
				sample()
			}
		}
	}()
	return ctx, func() {
		close(done)
		<-joined
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(peakHeap), "sampled-heap-peak-B")
		b.ReportMetric(float64(peakFiles), "sampled-memory-files-peak-B")
		b.ReportMetric(float64(peakDisk), "sampled-disk-peak-B")
		b.ReportMetric(float64(peakTemp), "sampled-temp-disk-peak-B")
		b.ReportMetric(float64(after.NumGC-before.NumGC), "GC-cycles")
		if sampleErr != nil {
			b.Fatal(sampleErr)
		}
	}
}

func memoryFileBytes(fs vfs.FS, path string) (uint64, error) {
	names, err := fs.List(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, name := range names {
		next := fs.PathJoin(path, name)
		info, err := fs.Stat(next)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if info.IsDir() {
			size, err := memoryFileBytes(fs, next)
			if err != nil {
				return 0, err
			}
			total += size
		} else {
			total += uint64(info.Size())
		}
	}
	return total, nil
}

// Component comparisons deliberately do not claim full memorydb migration or
// independent reopen support. All backends execute the same deterministic trace.
func BenchmarkTemporaryDB(b *testing.B) {
	for _, backend := range []string{"disk", "memory", "geth"} {
		for _, count := range []int{10000, 100000, 1000000} {
			for _, operation := range []string{"batch", "dedup", "get", "scan", "prefix"} {
				b.Run(fmt.Sprintf("backend=%s/records=%d/op=%s", backend, count, operation), func(b *testing.B) { benchmarkTemporaryDB(b, backend, count, operation) })
			}
		}
	}
}

func benchmarkTemporaryDB(b *testing.B, backend string, count int, operation string) {
	b.StopTimer()
	root := b.TempDir()
	var db ethdb.KeyValueStore
	var memFS vfs.FS
	if backend == "geth" {
		db = memorydb.New()
	} else {
		fs := vfs.Default
		if backend == "memory" {
			fs = vfs.NewMem()
			memFS = fs
		}
		var err error
		db, err = openMemoryPebble(fs, filepath.Join(root, ".ovm-work", "db"), 32, 32, false)
		if err != nil {
			b.Fatal(err)
		}
	}
	defer func() {
		if err := db.Close(); err != nil {
			b.Fatal(err)
		}
	}()
	// Fixed high-entropy payloads avoid artificially rewarding compression of
	// mostly-zero synthetic keys. Their retained memory is equal across backends.
	keys := make([][32]byte, count)
	values := make([][32]byte, count)
	for n := range count {
		var seed [8]byte
		binary.BigEndian.PutUint64(seed[:], uint64(n))
		keys[n] = sha256.Sum256(seed[:])
		binary.BigEndian.PutUint32(keys[n][:4], uint32(n%1000))
		values[n] = sha256.Sum256(keys[n][:])
	}
	key := func(n int) []byte { return keys[n][:] }

	write := func() {
		batch := db.NewBatch()
		defer batch.Close()
		for n := range count {
			k := key(n)
			value := values[n][:]
			if operation == "dedup" {
				value = nil
			}
			if err := batch.Put(k, value); err != nil {
				b.Fatal(err)
			}
			if batch.ValueSize() >= ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					b.Fatal(err)
				}
				batch.Reset()
			}
		}
		if err := batch.Write(); err != nil {
			b.Fatal(err)
		}
	}
	write()
	if err := db.SyncKeyValue(); err != nil {
		b.Fatal(err)
	}
	ctx, stop := sampleTemporaryBenchmark(b, b.Context(), root)
	if memFS != nil {
		observeTemporaryStorage(ctx, &temporaryStorage{fs: memFS})
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for range b.N {
		switch operation {
		case "batch", "dedup":
			write()
		case "get":
			for n := range count {
				if _, err := db.Get(key((n * 7919) % count)); err != nil {
					b.Fatal(err)
				}
			}
		case "scan":
			if n := benchmarkCountIterator(b, db, nil); n != count {
				b.Fatalf("scan count %d", n)
			}
		case "prefix":
			total := 0
			for n := range 1000 {
				total += benchmarkCountIterator(b, db, key(n)[:4])
			}
			if total != count {
				b.Fatalf("prefix count %d", total)
			}
		}
	}
	b.StopTimer()
	stop()
	b.ReportMetric(float64(count), "records/op")
}

func benchmarkCountIterator(b *testing.B, db ethdb.KeyValueStore, prefix []byte) int {
	b.Helper()
	it := db.NewIterator(prefix, nil)
	defer it.Release()
	count := 0
	for it.Next() {
		count++
	}
	if err := it.Error(); err != nil {
		b.Fatal(err)
	}
	return count
}
