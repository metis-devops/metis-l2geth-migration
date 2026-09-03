package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	gethpebble "github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

const (
	traversalBenchmarkAccounts        = 15_000
	traversalBenchmarkStorageEvery    = 10
	traversalBenchmarkSlotsPerAccount = 8
	traversalBenchmarkCodeSize        = 32 << 10

	storagePipelineBenchmarkAccounts        = 256
	storagePipelineBenchmarkSlotsPerAccount = 128
)

type traversalBenchmarkAccount struct {
	hash    common.Hash
	account types.StateAccount
}

type traversalBenchmarkWorkload struct {
	accounts               int
	storageEvery           int
	slotsPerAccount        int
	codeSize               int
	singleAccountPartition bool
	firstAccountSlots      int
}

func BenchmarkTraverseState(b *testing.B) {
	benchmarkTraverseState(b, traversalBenchmarkWorkload{
		accounts:        traversalBenchmarkAccounts,
		storageEvery:    traversalBenchmarkStorageEvery,
		slotsPerAccount: traversalBenchmarkSlotsPerAccount,
		codeSize:        traversalBenchmarkCodeSize,
	})
}

func BenchmarkTraverseStateStoragePipeline(b *testing.B) {
	benchmarkTraverseState(b, traversalBenchmarkWorkload{
		accounts:        storagePipelineBenchmarkAccounts,
		storageEvery:    1,
		slotsPerAccount: storagePipelineBenchmarkSlotsPerAccount,
		codeSize:        traversalBenchmarkCodeSize,
	})
}

func BenchmarkPortableImport(b *testing.B) {
	chaindata := filepath.Join(b.TempDir(), "chaindata")
	root, _ := buildTraversalBenchmarkState(b, chaindata, traversalBenchmarkWorkload{
		accounts: traversalBenchmarkAccounts, storageEvery: traversalBenchmarkStorageEvery,
		slotsPerAccount: traversalBenchmarkSlotsPerAccount, codeSize: traversalBenchmarkCodeSize,
	})
	writeTraversalBenchmarkHead(b, chaindata, root)
	outputRoot := b.TempDir()
	for _, compression := range []string{bundle.CompressionNone, bundle.CompressionZstd} {
		bundleDir := filepath.Join(outputRoot, "bundle-"+compression)
		exported, err := Export(context.Background(), ExportOptions{
			SourceChaindata: chaindata, Output: bundleDir, Compression: compression, CacheMB: 128, Handles: 128,
		})
		if err != nil {
			b.Fatal(err)
		}
		for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
			b.Run(compression+"/"+scheme+"/direct", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					runRoot, err := os.MkdirTemp(outputRoot, "direct-import-")
					if err != nil {
						b.Fatal(err)
					}
					output := filepath.Join(runRoot, "artifact")
					if _, err := Import(context.Background(), ImportOptions{
						Bundle: bundleDir, Output: output, Scheme: scheme, CacheMB: 128, Handles: 128,
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(compression+"/"+scheme+"/generate-trie-reference", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					runRoot, err := os.MkdirTemp(outputRoot, "generate-trie-reference-")
					if err != nil {
						b.Fatal(err)
					}
					output := filepath.Join(runRoot, "db")
					result := buildGenerateTrieReference(b, bundleDir, output, scheme, exported.Manifest.Source)
					reporter := newProgressReporter("reference-benchmark", ProgressOptions{})
					if _, err := verifyDatabase(context.Background(), output, scheme, exported.Manifest.Source, result.State, 128, 128, reporter, filepath.Dir(output)); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkPartitionedMigrateBuild(b *testing.B) {
	for _, workload := range migrateBenchmarkWorkloads() {
		b.Run(workload.name, func(b *testing.B) {
			chaindata := filepath.Join(b.TempDir(), "chaindata")
			root, counts := buildTraversalBenchmarkState(b, chaindata, workload.workload)
			writeTraversalBenchmarkHead(b, chaindata, root)
			for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
				b.Run(scheme+"/serial-reference", func(b *testing.B) {
					benchmarkMigrateBuild(b, chaindata, scheme, 0, root, counts)
				})
				for _, workers := range []int{2, 4, 8, 16} {
					b.Run(fmt.Sprintf("%s/workers=%d", scheme, workers), func(b *testing.B) {
						benchmarkMigrateBuild(b, chaindata, scheme, workers, root, counts)
					})
				}
			}
		})
	}
}

func BenchmarkPartitionedMigrateEndToEnd(b *testing.B) {
	for _, workload := range migrateBenchmarkWorkloads() {
		b.Run(workload.name, func(b *testing.B) {
			chaindata := filepath.Join(b.TempDir(), "chaindata")
			root, counts := buildTraversalBenchmarkState(b, chaindata, workload.workload)
			writeTraversalBenchmarkHead(b, chaindata, root)
			for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
				for _, workers := range []int{2, 4, 8, 16} {
					b.Run(fmt.Sprintf("%s/workers=%d", scheme, workers), func(b *testing.B) {
						b.ReportAllocs()
						for range b.N {
							output := filepath.Join(b.TempDir(), "artifact")
							result, err := Migrate(context.Background(), MigrateOptions{
								SourceChaindata: chaindata, Output: output, Scheme: scheme,
								CacheMB: 128, Handles: 128, Workers: workers,
							})
							if err != nil {
								b.Fatal(err)
							}
							if result.Report.RecomputedRoot != root || result.Report.Counts != counts {
								b.Fatalf("unexpected migration result: %+v", result.Report)
							}
						}
					})
				}
			}
		})
	}
}

type namedMigrateBenchmarkWorkload struct {
	name     string
	workload traversalBenchmarkWorkload
}

func migrateBenchmarkWorkloads() []namedMigrateBenchmarkWorkload {
	return []namedMigrateBenchmarkWorkload{
		{
			name: "many-small-storage",
			workload: traversalBenchmarkWorkload{
				accounts: 512, storageEvery: 1, slotsPerAccount: 8, codeSize: traversalBenchmarkCodeSize,
			},
		},
		{
			name: "distributed-large-storage",
			workload: traversalBenchmarkWorkload{
				accounts: 8, storageEvery: 1, slotsPerAccount: 8 * migrateStoragePartitionThreshold,
				codeSize: traversalBenchmarkCodeSize,
			},
		},
		{
			name: "single-giant-storage",
			workload: traversalBenchmarkWorkload{
				accounts: 1, storageEvery: 1, slotsPerAccount: 32 * migrateStoragePartitionThreshold,
				codeSize: traversalBenchmarkCodeSize,
			},
		},
		{
			name: "shared-large-code",
			workload: traversalBenchmarkWorkload{
				accounts: 5_000, storageEvery: 5_001, slotsPerAccount: 0, codeSize: traversalBenchmarkCodeSize,
			},
		},
		{
			name: "single-partition-head-heavy",
			workload: traversalBenchmarkWorkload{
				accounts: 64, storageEvery: 1, slotsPerAccount: 16, codeSize: traversalBenchmarkCodeSize,
				singleAccountPartition: true, firstAccountSlots: migrateStoragePartitionThreshold,
			},
		},
	}
}

func benchmarkMigrateBuild(
	b *testing.B,
	chaindata, scheme string,
	workers int,
	wantRoot common.Hash,
	wantCounts bundle.Counts,
) {
	b.Helper()
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		runRoot := b.TempDir()
		dbPath := filepath.Join(runRoot, "db")
		if err := os.Mkdir(dbPath, 0o755); err != nil {
			b.Fatal(err)
		}
		source, err := openLegacySource(chaindata, 128, 128, newProgressReporter("benchmark", ProgressOptions{}))
		if err != nil {
			b.Fatal(err)
		}
		kv, err := gethpebble.New(dbPath, 128, 128, "partitioned-migrate-benchmark", false)
		if err != nil {
			b.Fatal(err)
		}
		target := rawdb.NewDatabase(kv)
		b.StartTimer()
		var result StateResult
		if workers == 0 {
			writer := newDirectStateWriter(target, scheme)
			result, err = source.TraverseWithTrieNodes(context.Background(), writer, writer)
			if err == nil {
				err = writer.Close()
			} else {
				writer.Abort()
			}
		} else {
			var finalWriter *directStateWriter
			result, finalWriter, err = source.migratePartitionedState(context.Background(), target, scheme, workers, nil)
			if err == nil {
				err = finalWriter.Close()
			}
		}
		b.StopTimer()
		if closeErr := source.Close(); err == nil {
			err = closeErr
		}
		if closeErr := target.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			b.Fatal(err)
		}
		if result.Root != wantRoot || result.Counts != wantCounts {
			b.Fatalf("unexpected build result: %+v", result)
		}
	}
}

func writeTraversalBenchmarkHead(t testing.TB, chaindata string, root common.Hash) {
	t.Helper()
	kv, err := gethleveldb.New(chaindata, 128, 128, "portable-import-benchmark-head", false)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	header := &types.Header{
		UncleHash: types.EmptyUncleHash, Root: root,
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
		Difficulty: big.NewInt(1), Number: big.NewInt(1), GasLimit: 30_000_000, Time: 1,
	}
	rawdb.WriteHeader(disk, header)
	rawdb.WriteCanonicalHash(disk, header.Hash(), 1)
	rawdb.WriteHeadBlockHash(disk, header.Hash())
	rawdb.WriteHeadHeaderHash(disk, header.Hash())
	if err := disk.SyncKeyValue(); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
}

func benchmarkTraverseState(b *testing.B, workload traversalBenchmarkWorkload) {
	b.Helper()
	chaindata := filepath.Join(b.TempDir(), "chaindata")
	root, wantCounts := buildTraversalBenchmarkState(b, chaindata, workload)
	kv, err := gethleveldb.New(chaindata, 128, 128, "traverse-benchmark", true)
	if err != nil {
		b.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	b.Cleanup(func() {
		if err := disk.Close(); err != nil {
			b.Errorf("close benchmark database: %v", err)
		}
	})
	trieDB := triedb.NewDatabase(disk, triedb.HashDefaults)
	b.Cleanup(func() {
		if err := trieDB.Close(); err != nil {
			b.Errorf("close benchmark trie database: %v", err)
		}
	})
	opts := stateTraversalOptions{
		ReadCode: func(db ethdb.KeyValueReader, hash common.Hash) ([]byte, error) {
			return db.Get(hash[:])
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, _, err := traverseState(context.Background(), disk, trieDB, root, nil, false, opts)
		if err != nil {
			b.Fatal(err)
		}
		if result.Root != root || result.Counts != wantCounts {
			b.Fatalf("unexpected traversal result: %+v", result)
		}
	}
	b.ReportMetric(float64(wantCounts.Records)*float64(b.N)/b.Elapsed().Seconds(), "records/s")
	b.ReportMetric(float64(wantCounts.StorageSlots)*float64(b.N)/b.Elapsed().Seconds(), "storage-slots/s")
}

func buildTraversalBenchmarkState(t testing.TB, chaindata string, workload traversalBenchmarkWorkload) (common.Hash, bundle.Counts) {
	t.Helper()
	kv, err := gethleveldb.New(chaindata, 128, 128, "traverse-benchmark-fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	diskClosed := false
	defer func() {
		if !diskClosed {
			if err := disk.Close(); err != nil {
				t.Errorf("close benchmark fixture database: %v", err)
			}
		}
	}()
	sharedCode := make([]byte, workload.codeSize)
	for i := range sharedCode {
		sharedCode[i] = byte(i*31 + 7)
	}
	codeHash := crypto.Keccak256Hash(sharedCode)
	if err := disk.Put(codeHash[:], sharedCode); err != nil {
		t.Fatal(err)
	}
	var counts bundle.Counts
	if workload.accounts >= 2 {
		counts = bundle.Counts{
			CodeRecords: 1, Records: 1, PayloadBytes: uint64(len(sharedCode)),
		}
	}
	built := make([]traversalBenchmarkAccount, 0, workload.accounts)
	for i := 1; i <= workload.accounts; i++ {
		var seed [8]byte
		binary.BigEndian.PutUint64(seed[:], uint64(i))
		accountHash := benchmarkAccountHash(workload, seed)
		storageRoot := types.EmptyRootHash
		if i%workload.storageEvery == 0 {
			type benchmarkSlot struct {
				hash  common.Hash
				value []byte
			}
			slotCount := workload.slotsPerAccount
			if i == 1 && workload.firstAccountSlots > 0 {
				slotCount = workload.firstAccountSlots
			}
			slots := make([]benchmarkSlot, 0, slotCount)
			for j := range slotCount {
				var slotSeed [16]byte
				binary.BigEndian.PutUint64(slotSeed[:8], uint64(i))
				binary.BigEndian.PutUint64(slotSeed[8:], uint64(j+1))
				slotHash := crypto.Keccak256Hash([]byte("slot"), slotSeed[:])
				valueHash := crypto.Keccak256Hash([]byte("value"), slotSeed[:])
				valueRLP, err := rlp.EncodeToBytes(common.TrimLeftZeroes(valueHash[:]))
				if err != nil {
					t.Fatal(err)
				}
				slots = append(slots, benchmarkSlot{hash: slotHash, value: valueRLP})
			}
			sort.Slice(slots, func(i, j int) bool {
				return bytes.Compare(slots[i].hash[:], slots[j].hash[:]) < 0
			})
			stack := trie.NewStackTrie(nil)
			for _, slot := range slots {
				if err := stack.Update(slot.hash[:], slot.value); err != nil {
					t.Fatal(err)
				}
				rawdb.WriteStorageSnapshot(disk, accountHash, slot.hash, slot.value)
				counts.StorageSlots++
				counts.Records++
				counts.PayloadBytes += uint64(len(slot.value))
			}
			storageRoot = stack.Hash()
		}
		accountCodeHash := types.EmptyCodeHash
		if i%2 == 0 {
			accountCodeHash = codeHash
			counts.CodeReferences++
		}
		account := types.StateAccount{
			Nonce:    uint64(i),
			Balance:  uint256.NewInt(uint64(i)),
			Root:     storageRoot,
			CodeHash: accountCodeHash.Bytes(),
		}
		rawdb.WriteAccountSnapshot(disk, accountHash, types.SlimAccountRLP(account))
		built = append(built, traversalBenchmarkAccount{hash: accountHash, account: account})
	}
	sort.Slice(built, func(i, j int) bool {
		return bytes.Compare(built[i].hash[:], built[j].hash[:]) < 0
	})
	accountStack := trie.NewStackTrie(nil)
	for _, entry := range built {
		encoded, err := rlp.EncodeToBytes(&entry.account)
		if err != nil {
			t.Fatal(err)
		}
		if err := accountStack.Update(entry.hash[:], encoded); err != nil {
			t.Fatal(err)
		}
		counts.Accounts++
		counts.Records++
		counts.PayloadBytes += uint64(len(encoded))
	}
	root := accountStack.Hash()
	stats, err := triedb.GenerateTrie(disk, rawdb.HashScheme, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Updated != 0 || stats.Deleted != 0 {
		t.Fatalf("benchmark fixture state reconciled unexpectedly: %+v", stats)
	}
	if err := removeFlatStateForTest(context.Background(), disk); err != nil {
		t.Fatal(err)
	}
	if err := disk.SyncKeyValue(); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	diskClosed = true
	return root, counts
}

func benchmarkAccountHash(workload traversalBenchmarkWorkload, seed [8]byte) common.Hash {
	if !workload.singleAccountPartition {
		return crypto.Keccak256Hash([]byte("account"), seed[:])
	}
	var hash common.Hash
	hash[0] = 0x80
	copy(hash[common.HashLength-len(seed):], seed[:])
	return hash
}
