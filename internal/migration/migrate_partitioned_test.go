package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestNormalizeMigrateWorkers(t *testing.T) {
	for _, test := range []struct {
		configured int
		want       int
	}{
		{configured: -1, want: 2},
		{configured: 0, want: 2},
		{configured: 1, want: 2},
		{configured: 2, want: 2},
		{configured: 4, want: 4},
		{configured: 16, want: 16},
	} {
		if got := normalizeMigrateWorkers(test.configured); got != test.want {
			t.Fatalf("normalize workers %d = %d, want %d", test.configured, got, test.want)
		}
	}
	if err := validateMigrateOptions(MigrateOptions{
		SourceChaindata: "source", Output: "output", Scheme: rawdb.HashScheme, Workers: 17,
	}); err == nil || err.Error() != "workers must not exceed 16" {
		t.Fatalf("workers above maximum returned %v", err)
	}
}

func TestMigratePartitionRangesCoverHashSpace(t *testing.T) {
	var previousEnd []byte
	for partition := range migrateTriePartitions {
		start, end := migratePartitionRange(byte(partition))
		if partition == 0 {
			if start != nil {
				t.Fatalf("first partition starts at %x", start)
			}
		} else if !bytes.Equal(start, previousEnd) {
			t.Fatalf("partition %x starts at %x after previous end %x", partition, start, previousEnd)
		}
		if partition == migrateTriePartitions-1 {
			if end != nil {
				t.Fatalf("last partition ends at %x", end)
			}
		} else {
			if len(end) != common.HashLength || end[0] != byte(partition+1)<<4 {
				t.Fatalf("partition %x has invalid end %x", partition, end)
			}
			for _, value := range end[1:] {
				if value != 0 {
					t.Fatalf("partition %x end is not a clean boundary: %x", partition, end)
				}
			}
		}
		previousEnd = end
	}
}

func TestPartitionIteratorsIncludeExactRangeBoundaries(t *testing.T) {
	source := rawdb.NewDatabase(memorydb.New())
	writer := &capturingKeyValueWriter{target: source}
	stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		rawdb.WriteTrieNode(writer, common.Hash{}, path, hash, blob, rawdb.HashScheme)
	})
	keys := make([]common.Hash, migrateTriePartitions)
	for partition := range migrateTriePartitions {
		keys[partition][0] = byte(partition) << 4
		if err := stack.Update(keys[partition][:], bytes.Repeat([]byte{byte(partition + 1)}, 80)); err != nil {
			t.Fatal(err)
		}
	}
	root := stack.Hash()
	if err := writer.Err(); err != nil {
		t.Fatal(err)
	}
	trieDB := triedb.NewDatabase(source, triedb.HashDefaults)
	defer func() {
		if err := trieDB.Close(); err != nil {
			t.Errorf("close range trie database: %v", err)
		}
	}()
	migrator := &partitionedStateMigrator{trieDB: trieDB}
	for partition := range migrateTriePartitions {
		it, err := migrator.openPartitionIterator(trie.StateTrieID(root), byte(partition))
		if err != nil {
			t.Fatal(err)
		}
		if !it.Next() {
			t.Fatalf("partition %x omitted its exact lower-bound key: %v", partition, it.Err)
		}
		if got := common.BytesToHash(it.Key); got != keys[partition] {
			t.Fatalf("partition %x returned %s, want %s", partition, got, keys[partition])
		}
		if it.Next() {
			t.Fatalf("partition %x returned extra key %x", partition, it.Key)
		}
		if it.Err != nil {
			t.Fatalf("partition %x iterator failed: %v", partition, it.Err)
		}
	}
}

func TestPartitionedTrieAssemblyMatchesSerialNodes(t *testing.T) {
	tests := []struct {
		name string
		keys []common.Hash
	}{
		{name: "empty"},
		{name: "single-leaf", keys: []common.Hash{partitionTestHash(0x30, 0x00)}},
		{name: "single-partition-branch", keys: []common.Hash{
			partitionTestHash(0x30, 0x00), partitionTestHash(0x37, 0x00), partitionTestHash(0x3a, 0x00),
		}},
		{name: "single-partition-extension", keys: []common.Hash{
			partitionTestHash(0x31, 0x10), partitionTestHash(0x31, 0x15), partitionTestHash(0x31, 0x1a),
		}},
		{name: "multiple-partitions", keys: []common.Hash{
			partitionTestHash(0x00, 0x01), partitionTestHash(0x30, 0x01), partitionTestHash(0x70, 0x01), partitionTestHash(0xf0, 0x01),
		}},
	}
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		for _, test := range tests {
			t.Run(scheme+"/"+test.name, func(t *testing.T) {
				sort.Slice(test.keys, func(i, j int) bool { return bytes.Compare(test.keys[i][:], test.keys[j][:]) < 0 })
				values := make([][]byte, len(test.keys))
				for index := range values {
					values[index] = bytes.Repeat([]byte{byte(index + 1)}, 80)
				}
				referenceDB := rawdb.NewDatabase(memorydb.New())
				expectedRoot := buildSerialTestTrie(t, referenceDB, scheme, test.keys, values)
				partitionedDB := rawdb.NewDatabase(memorydb.New())
				partitions := buildPartialTestTries(t, partitionedDB, scheme, test.keys, values)
				writer := newDirectStateWriter(partitionedDB, scheme)
				gotRoot, err := assembleMigratedTrie(writer, common.Hash{}, expectedRoot, partitions)
				if err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				if gotRoot != expectedRoot {
					t.Fatalf("assembled root %s, want %s", gotRoot, expectedRoot)
				}
				assertMemoryDatabaseEqual(t, partitionedDB, referenceDB)
			})
		}
	}
}

func TestStorageProbeThresholdAndDiscard(t *testing.T) {
	t.Run("slots=0", func(t *testing.T) {
		target := rawdb.NewDatabase(memorydb.New())
		migrator := &partitionedStateMigrator{
			target: target, scheme: rawdb.PathScheme, limiter: newMigrateWorkLimiter(2),
		}
		lease := newMigrateWorkLease(migrator.limiter)
		if err := lease.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		counts, err := migrator.migrateStorage(context.Background(), common.HexToHash("0xa1"), types.EmptyRootHash, lease)
		lease.release()
		if err != nil {
			t.Fatal(err)
		}
		if counts != (bundle.Counts{}) || len(collectMemoryEntries(t, target)) != 0 {
			t.Fatalf("empty storage produced counts or target entries: %+v", counts)
		}
	})
	for _, slots := range []int{1, migrateStoragePartitionThreshold, migrateStoragePartitionThreshold + 1} {
		t.Run(fmt.Sprintf("slots=%d", slots), func(t *testing.T) {
			source, root, accountHash := buildStorageProbeSource(t, slots)
			trieDB := triedb.NewDatabase(source, triedb.HashDefaults)
			defer func() {
				if err := trieDB.Close(); err != nil {
					t.Errorf("close probe trie database: %v", err)
				}
			}()
			target := rawdb.NewDatabase(memorydb.New())
			progress := new(progressCounts)
			migrator := &partitionedStateMigrator{
				ctx: context.Background(), source: source, trieDB: trieDB, target: target,
				scheme: rawdb.PathScheme, root: root, limiter: newMigrateWorkLimiter(2),
				codeHashes: newConcurrentHashSet(), progress: progress,
			}
			partition, counts, err := migrator.probeStorage(context.Background(), accountHash, root)
			if err != nil {
				t.Fatal(err)
			}
			wantPartition := slots > migrateStoragePartitionThreshold
			if partition != wantPartition {
				t.Fatalf("partition=%t, want %t", partition, wantPartition)
			}
			entries := collectMemoryEntries(t, target)
			if wantPartition {
				if counts != (bundle.Counts{}) || progress.snapshot() != (bundle.Counts{}) {
					t.Fatalf("discarded probe leaked counts: result=%+v progress=%+v", counts, progress.snapshot())
				}
				if len(entries) != 0 {
					t.Fatalf("discarded probe wrote %d target entries", len(entries))
				}
				return
			}
			if counts.StorageSlots != uint64(slots) || counts.Records != uint64(slots) {
				t.Fatalf("unexpected probe counts: %+v", counts)
			}
			if progress.snapshot() != counts {
				t.Fatalf("probe progress %+v does not match counts %+v", progress.snapshot(), counts)
			}
			if len(entries) == 0 {
				t.Fatal("accepted probe did not write target state")
			}
		})
	}
}

func TestPartitionedLargeStorageMatchesSerialNodes(t *testing.T) {
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		for _, workers := range []int{2, 4, 8, 16} {
			t.Run(fmt.Sprintf("%s/workers=%d", scheme, workers), func(t *testing.T) {
				source, root, accountHash := buildStorageProbeSource(t, migrateStoragePartitionThreshold+1)
				trieDB := triedb.NewDatabase(source, triedb.HashDefaults)
				defer func() {
					if err := trieDB.Close(); err != nil {
						t.Errorf("close partitioned storage trie database: %v", err)
					}
				}()
				target := rawdb.NewDatabase(memorydb.New())
				progress := new(progressCounts)
				migrator := &partitionedStateMigrator{
					ctx: context.Background(), source: source, trieDB: trieDB, target: target,
					scheme: scheme, root: root, limiter: newMigrateWorkLimiter(workers),
					codeHashes: newConcurrentHashSet(), progress: progress,
				}
				lease := newMigrateWorkLease(migrator.limiter)
				if err := lease.acquire(context.Background()); err != nil {
					t.Fatal(err)
				}
				counts, err := migrator.migrateStorage(context.Background(), accountHash, root, lease)
				lease.release()
				if err != nil {
					t.Fatal(err)
				}
				if counts.StorageSlots != migrateStoragePartitionThreshold+1 || counts.Records != migrateStoragePartitionThreshold+1 {
					t.Fatalf("unexpected partitioned storage counts: %+v", counts)
				}
				if progress.snapshot() != counts {
					t.Fatalf("partitioned storage progress %+v does not match counts %+v", progress.snapshot(), counts)
				}
				reference := buildSerialStorageTarget(t, source, root, accountHash, scheme)
				assertMemoryDatabaseEqual(t, target, reference)
			})
		}
	}
}

func TestDirectMigrateLargeStorageEndToEndBothSchemes(t *testing.T) {
	chaindata := filepath.Join(t.TempDir(), "chaindata")
	root, counts := buildTraversalBenchmarkState(t, chaindata, traversalBenchmarkWorkload{
		accounts: 1, storageEvery: 1, slotsPerAccount: migrateStoragePartitionThreshold + 1,
		codeSize: traversalBenchmarkCodeSize,
	})
	writeTraversalBenchmarkHead(t, chaindata, root)
	before := directoryContentDigest(t, chaindata)
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			artifact := filepath.Join(t.TempDir(), "artifact")
			migrated, err := Migrate(context.Background(), MigrateOptions{
				SourceChaindata: chaindata, Output: artifact, Scheme: scheme,
				CacheMB: 16, Handles: 16, Workers: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			if migrated.Report.RecomputedRoot != root || migrated.Report.Counts != counts {
				t.Fatalf("unexpected large-storage migration report: %+v", migrated.Report)
			}
			verified, err := VerifyDirect(context.Background(), DirectVerifyOptions{
				SourceChaindata: chaindata, Artifact: artifact, CacheMB: 16, Handles: 16,
			})
			if err != nil {
				t.Fatal(err)
			}
			if verified.RecomputedRoot != root || verified.Counts != counts {
				t.Fatalf("unexpected large-storage verification report: %+v", verified)
			}
		})
	}
	if after := directoryContentDigest(t, chaindata); after != before {
		t.Fatalf("large-storage source changed: before %s after %s", before, after)
	}
}

func TestDirectMigrateActiveCancellationCleansOutput(t *testing.T) {
	chaindata := filepath.Join(t.TempDir(), "chaindata")
	root, _ := buildTraversalBenchmarkState(t, chaindata, traversalBenchmarkWorkload{
		accounts: 1, storageEvery: 1, slotsPerAccount: 64 * migrateStoragePartitionThreshold,
		codeSize: traversalBenchmarkCodeSize,
	})
	writeTraversalBenchmarkHead(t, chaindata, root)
	before := directoryContentDigest(t, chaindata)
	parent := t.TempDir()
	output := filepath.Join(parent, "artifact")
	progress := newWaitBuffer()
	logger := log.NewLogger(log.LogfmtHandlerWithLevel(progress, log.LevelInfo))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Migrate(ctx, MigrateOptions{
			SourceChaindata: chaindata, Output: output, Scheme: rawdb.HashScheme,
			CacheMB: 16, Handles: 16, Workers: 2,
			Progress: ProgressOptions{Logger: logger, Interval: time.Millisecond},
		})
		done <- err
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for !hasPositiveMigrateStorageProgress(progress.String()) {
		select {
		case err := <-done:
			t.Fatalf("migration completed before active storage cancellation: %v\n%s", err, progress.String())
		case <-progress.notify:
		case <-timer.C:
			cancel()
			err := <-done
			t.Fatalf("timed out waiting for active storage workers: %v\n%s", err, progress.String())
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active migration cancellation returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active migration cancellation did not join all workers")
	}
	assertPathAbsent(t, output)
	partials, err := filepath.Glob(filepath.Join(parent, ".artifact.partial-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial outputs survived active cancellation: %v", partials)
	}
	if after := directoryContentDigest(t, chaindata); after != before {
		t.Fatalf("active cancellation changed source: before %s after %s", before, after)
	}
}

func TestMigrateGlobalWorkerLimitSupportsNestedPartitions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	limiter := newMigrateWorkLimiter(2)
	var active, peak atomic.Int64
	enter := func() {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				return
			}
		}
	}
	leave := func() { active.Add(-1) }
	err := runMigratePartitionTasks(ctx, func(taskCtx context.Context, _ int) error {
		outer := newMigrateWorkLease(limiter)
		if err := outer.acquire(taskCtx); err != nil {
			return err
		}
		enter()
		leave()
		outer.release()
		if err := runMigratePartitionTasks(taskCtx, func(innerCtx context.Context, _ int) error {
			inner := newMigrateWorkLease(limiter)
			if err := inner.acquire(innerCtx); err != nil {
				return err
			}
			defer inner.release()
			enter()
			defer leave()
			time.Sleep(time.Millisecond)
			return nil
		}); err != nil {
			return err
		}
		if err := outer.acquire(taskCtx); err != nil {
			return err
		}
		enter()
		leave()
		outer.release()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 {
		t.Fatalf("%d worker slots remain active", active.Load())
	}
	if peak.Load() > 2 {
		t.Fatalf("worker peak %d exceeded limit 2", peak.Load())
	}
}

func TestPartitionedMigrateReadsSharedCodeOnce(t *testing.T) {
	code := bytes.Repeat([]byte{0x60, 0x00, 0x55}, 1024)
	codeHash := crypto.Keccak256Hash(code)
	baseSource := rawdb.NewDatabase(memorydb.New())
	if err := baseSource.Put(codeHash[:], code); err != nil {
		t.Fatal(err)
	}
	source := &countingCodeDatabase{Database: baseSource, codeHash: codeHash}
	target := rawdb.NewDatabase(memorydb.New())
	progress := new(progressCounts)
	migrator := &partitionedStateMigrator{
		ctx: context.Background(), source: source, target: target,
		codeHashes: newConcurrentHashSet(), progress: progress,
	}
	var counts [migrateTriePartitions]bundle.Counts
	if err := runMigratePartitionTasks(context.Background(), func(ctx context.Context, index int) error {
		writer := newDirectStateWriter(target, rawdb.HashScheme)
		defer writer.Abort()
		accountHash := partitionTestHash(byte(index)<<4, byte(index))
		if err := migrator.migrateCode(ctx, writer, accountHash, codeHash, &counts[index]); err != nil {
			return err
		}
		return writer.CloseContext(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	var total bundle.Counts
	for _, count := range counts {
		addBundleCounts(&total, count)
	}
	if total.CodeReferences != migrateTriePartitions || total.CodeRecords != 1 || total.Records != 1 || total.PayloadBytes != uint64(len(code)) {
		t.Fatalf("unexpected shared-code counts: %+v", total)
	}
	if progress.snapshot() != total {
		t.Fatalf("shared-code progress %+v does not match counts %+v", progress.snapshot(), total)
	}
	if source.reads.Load() != 1 {
		t.Fatalf("shared code was read %d times, want 1", source.reads.Load())
	}
	if stored := rawdb.ReadCode(target, codeHash); !bytes.Equal(stored, code) {
		t.Fatalf("stored shared code changed: have %x want %x", stored, code)
	}
}

func partitionTestHash(first, second byte) common.Hash {
	var hash common.Hash
	hash[0] = first
	hash[1] = second
	return hash
}

func hasPositiveMigrateStorageProgress(output string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.Contains(line, "phase=migrate_state") || !strings.Contains(line, "status=progress") {
			continue
		}
		for field := range strings.FieldsSeq(line) {
			const prefix = "storage_slots="
			if !strings.HasPrefix(field, prefix) {
				continue
			}
			value, err := strconv.ParseUint(strings.TrimPrefix(field, prefix), 10, 64)
			if err == nil && value > 0 {
				return true
			}
			break
		}
	}
	return false
}

func buildSerialTestTrie(t *testing.T, db ethdb.Database, scheme string, keys []common.Hash, values [][]byte) common.Hash {
	t.Helper()
	writer := newDirectStateWriter(db, scheme)
	defer writer.Abort()
	var nodeErr error
	stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		if nodeErr == nil {
			nodeErr = writer.TrieNode(common.Hash{}, path, hash, blob)
		}
	})
	for index := range keys {
		if err := stack.Update(keys[index][:], values[index]); err != nil {
			t.Fatal(err)
		}
		if nodeErr != nil {
			t.Fatal(nodeErr)
		}
	}
	root := stack.Hash()
	if nodeErr != nil {
		t.Fatal(nodeErr)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func buildPartialTestTries(
	t *testing.T,
	db ethdb.Database,
	scheme string,
	keys []common.Hash,
	values [][]byte,
) [migrateTriePartitions]migratePartitionResult {
	t.Helper()
	var results [migrateTriePartitions]migratePartitionResult
	for partition := range migrateTriePartitions {
		writer := newDirectStateWriter(db, scheme)
		var nodeErr error
		partial := trie.NewPartialStackTrie(byte(partition), func(path []byte, hash common.Hash, blob []byte) {
			if nodeErr != nil {
				return
			}
			if len(path) == 1 {
				results[partition].rootBlob = bytes.Clone(blob)
			}
			nodeErr = writer.TrieNode(common.Hash{}, path, hash, blob)
		})
		for index, key := range keys {
			if key[0]>>4 != byte(partition) {
				continue
			}
			if err := partial.Update(key[:], values[index]); err != nil {
				t.Fatal(err)
			}
			results[partition].populated = true
		}
		results[partition].root = partial.Hash()
		if nodeErr != nil {
			t.Fatal(nodeErr)
		}
		if err := validateMigratePartitionResult(byte(partition), results[partition]); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return results
}

func buildStorageProbeSource(t *testing.T, slots int) (ethdb.Database, common.Hash, common.Hash) {
	t.Helper()
	source := rawdb.NewDatabase(memorydb.New())
	accountHash := common.HexToHash("0xa1")
	writer := &capturingKeyValueWriter{target: source}
	stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		rawdb.WriteTrieNode(writer, accountHash, path, hash, blob, rawdb.HashScheme)
	})
	for index := range slots {
		var slotHash common.Hash
		binary.BigEndian.PutUint64(slotHash[common.HashLength-8:], uint64(index+1))
		value, err := rlp.EncodeToBytes([]byte{byte(index%255 + 1)})
		if err != nil {
			t.Fatal(err)
		}
		if err := stack.Update(slotHash[:], value); err != nil {
			t.Fatal(err)
		}
	}
	root := stack.Hash()
	if err := writer.Err(); err != nil {
		t.Fatal(err)
	}
	return source, root, accountHash
}

func buildSerialStorageTarget(
	t *testing.T,
	source ethdb.Database,
	root, accountHash common.Hash,
	scheme string,
) ethdb.Database {
	t.Helper()
	trieDB := triedb.NewDatabase(source, triedb.HashDefaults)
	defer func() {
		if err := trieDB.Close(); err != nil {
			t.Errorf("close serial storage trie database: %v", err)
		}
	}()
	storageTrie, err := trie.New(trie.StorageTrieID(root, accountHash, root), trieDB)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := storageTrie.NodeIterator(nil)
	if err != nil {
		t.Fatal(err)
	}
	storage := trie.NewIterator(nodes)
	target := rawdb.NewDatabase(memorydb.New())
	writer := newDirectStateWriter(target, scheme)
	defer writer.Abort()
	var (
		nodeErr error
		counts  bundle.Counts
	)
	stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		if nodeErr == nil {
			nodeErr = writer.TrieNode(accountHash, path, hash, blob)
		}
	})
	for storage.Next() {
		if err := processMigratedStorageSlot(writer, stack, accountHash, storage.Key, storage.Value, &nodeErr, &counts); err != nil {
			t.Fatal(err)
		}
	}
	if storage.Err != nil {
		t.Fatal(storage.Err)
	}
	if got := stack.Hash(); got != root {
		t.Fatalf("serial storage root %s, want %s", got, root)
	}
	if nodeErr != nil {
		t.Fatal(nodeErr)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertMemoryDatabaseEqual(t *testing.T, left, right ethdb.Database) {
	t.Helper()
	leftEntries := collectMemoryEntries(t, left)
	rightEntries := collectMemoryEntries(t, right)
	if len(leftEntries) != len(rightEntries) {
		t.Fatalf("entry count differs: left=%d right=%d", len(leftEntries), len(rightEntries))
	}
	for index := range leftEntries {
		if !bytes.Equal(leftEntries[index].key, rightEntries[index].key) ||
			!bytes.Equal(leftEntries[index].value, rightEntries[index].value) {
			t.Fatalf("entry %d differs: left=%x=%x right=%x=%x", index,
				leftEntries[index].key, leftEntries[index].value,
				rightEntries[index].key, rightEntries[index].value)
		}
	}
}

func collectMemoryEntries(t *testing.T, db ethdb.Database) []logicalEntry {
	t.Helper()
	it := db.NewIterator(nil, nil)
	defer it.Release()
	var entries []logicalEntry
	for it.Next() {
		entries = append(entries, logicalEntry{key: bytes.Clone(it.Key()), value: bytes.Clone(it.Value())})
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	return entries
}

type countingCodeDatabase struct {
	ethdb.Database
	codeHash common.Hash
	reads    atomic.Int64
}

func (db *countingCodeDatabase) Get(key []byte) ([]byte, error) {
	if bytes.Equal(key, db.codeHash[:]) {
		db.reads.Add(1)
	}
	return db.Database.Get(key)
}
