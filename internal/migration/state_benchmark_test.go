package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
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
	accounts        int
	storageEvery    int
	slotsPerAccount int
	codeSize        int
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
		CodeIndex: codeHashIndexOptions{Parent: b.TempDir(), CacheMB: 16, Handles: 16},
		ReadCode: func(db ethdb.KeyValueReader, hash common.Hash) []byte {
			code, _ := db.Get(hash[:])
			return code
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

func buildTraversalBenchmarkState(b *testing.B, chaindata string, workload traversalBenchmarkWorkload) (common.Hash, bundle.Counts) {
	b.Helper()
	kv, err := gethleveldb.New(chaindata, 128, 128, "traverse-benchmark-fixture", false)
	if err != nil {
		b.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	diskClosed := false
	defer func() {
		if !diskClosed {
			if err := disk.Close(); err != nil {
				b.Errorf("close benchmark fixture database: %v", err)
			}
		}
	}()
	sharedCode := make([]byte, workload.codeSize)
	for i := range sharedCode {
		sharedCode[i] = byte(i*31 + 7)
	}
	codeHash := crypto.Keccak256Hash(sharedCode)
	if err := disk.Put(codeHash[:], sharedCode); err != nil {
		b.Fatal(err)
	}
	counts := bundle.Counts{
		CodeRecords:  1,
		Records:      1,
		PayloadBytes: uint64(len(sharedCode)),
	}
	built := make([]traversalBenchmarkAccount, 0, workload.accounts)
	for i := 1; i <= workload.accounts; i++ {
		var seed [8]byte
		binary.BigEndian.PutUint64(seed[:], uint64(i))
		accountHash := crypto.Keccak256Hash([]byte("account"), seed[:])
		storageRoot := types.EmptyRootHash
		if i%workload.storageEvery == 0 {
			type benchmarkSlot struct {
				hash  common.Hash
				value []byte
			}
			slots := make([]benchmarkSlot, 0, workload.slotsPerAccount)
			for j := range workload.slotsPerAccount {
				var slotSeed [16]byte
				binary.BigEndian.PutUint64(slotSeed[:8], uint64(i))
				binary.BigEndian.PutUint64(slotSeed[8:], uint64(j+1))
				slotHash := crypto.Keccak256Hash([]byte("slot"), slotSeed[:])
				valueHash := crypto.Keccak256Hash([]byte("value"), slotSeed[:])
				valueRLP, err := rlp.EncodeToBytes(common.TrimLeftZeroes(valueHash[:]))
				if err != nil {
					b.Fatal(err)
				}
				slots = append(slots, benchmarkSlot{hash: slotHash, value: valueRLP})
			}
			sort.Slice(slots, func(i, j int) bool {
				return bytes.Compare(slots[i].hash[:], slots[j].hash[:]) < 0
			})
			stack := trie.NewStackTrie(nil)
			for _, slot := range slots {
				if err := stack.Update(slot.hash[:], slot.value); err != nil {
					b.Fatal(err)
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
			b.Fatal(err)
		}
		if err := accountStack.Update(entry.hash[:], encoded); err != nil {
			b.Fatal(err)
		}
		counts.Accounts++
		counts.Records++
		counts.PayloadBytes += uint64(len(encoded))
	}
	root := accountStack.Hash()
	stats, err := triedb.GenerateTrie(disk, rawdb.HashScheme, root, nil)
	if err != nil {
		b.Fatal(err)
	}
	if stats.Updated != 0 || stats.Deleted != 0 {
		b.Fatalf("benchmark fixture state reconciled unexpectedly: %+v", stats)
	}
	if err := removeFlatState(context.Background(), disk); err != nil {
		b.Fatal(err)
	}
	if err := disk.SyncKeyValue(); err != nil {
		b.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		b.Fatal(err)
	}
	diskClosed = true
	return root, counts
}
