package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	leveldb "github.com/syndtr/goleveldb/leveldb"
)

const (
	migrateTriePartitions            = 16
	migrateStoragePartitionThreshold = 1024
	minMigrateWorkers                = 2
	maxMigrateWorkers                = migrateTriePartitions
)

// partitionedStateMigrator is the shared bounded traversal and root-validation
// core. Its output factory selects persisted migration output or validation only.
type partitionedStateMigrator struct {
	outputFactory    partitionOutputFactory
	ctx              context.Context
	source           ethdb.Database
	trieDB           *triedb.Database
	root             common.Hash
	limiter          *migrateWorkLimiter
	accounts         *migrateAccountWindow
	accountProcessor migrateAccountProcessor
	cancelRun        context.CancelFunc
	runFailure       migrateRunFailure
	codeHashes       *concurrentHashSet
	progress         *progressCounts
	readCode         codeReader
	validateAccount  func(common.Hash, *types.StateAccount) error
}

type migratePartitionResult struct {
	populated bool
	root      common.Hash
	rootBlob  []byte
	counts    bundle.Counts
}

func (s *legacySource) migratePartitionedState(
	ctx context.Context,
	target ethdb.Database,
	scheme string,
	workers int,
	progress *progressCounts,
) (result StateResult, finalWriter partitionStateOutput, retErr error) {
	trieDB := triedb.NewDatabase(s.db, triedb.HashDefaults)
	defer func() {
		if err := trieDB.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close source trie database: %w", err))
		}
		if retErr != nil && finalWriter != nil {
			finalWriter.Abort()
			finalWriter = nil
		}
	}()
	migrator := &partitionedStateMigrator{
		ctx:           ctx,
		source:        s.db,
		trieDB:        trieDB,
		outputFactory: persistentPartitionOutput(target, scheme),
		root:          s.head.StateRoot,
		limiter:       newMigrateWorkLimiter(workers),
		accounts:      newMigrateAccountWindow(workers),
		codeHashes:    newConcurrentHashSet(),
		progress:      progress,
	}
	return migrator.run()
}

func (m *partitionedStateMigrator) run() (StateResult, partitionStateOutput, error) {
	if m.outputFactory == nil {
		return StateResult{}, nil, errors.New("partitioned traversal requires an output factory")
	}
	runCtx, cancelRun := context.WithCancel(m.ctx)
	defer cancelRun()
	m.cancelRun = cancelRun
	var partitions [migrateTriePartitions]migratePartitionResult
	err := runMigratePartitionTasks(runCtx, func(ctx context.Context, index int) error {
		result, err := m.migrateAccountPartition(ctx, byte(index))
		if err != nil {
			wrapped := fmt.Errorf("migrate account partition %x: %w", index, err)
			m.recordRunFailure(wrapped)
			return wrapped
		}
		partitions[index] = result
		return nil
	})
	if runErr := m.runFailure.load(); runErr != nil {
		return StateResult{}, nil, runErr
	}
	if err != nil {
		return StateResult{}, nil, err
	}
	var counts bundle.Counts
	for _, partition := range partitions {
		addBundleCounts(&counts, partition.counts)
	}
	finalWriter := m.newOutput(false)
	root, err := assembleMigratedTrie(finalWriter, common.Hash{}, m.root, partitions)
	if err != nil {
		finalWriter.Abort()
		return StateResult{}, nil, fmt.Errorf("assemble account trie: %w", err)
	}
	return StateResult{Root: root, Counts: counts}, finalWriter, nil
}

func (m *partitionedStateMigrator) readMigrateCode(
	ctx context.Context,
	accountHash, codeHash common.Hash,
) ([]byte, error) {
	if codeHash == types.EmptyCodeHash {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var code []byte
	var err error
	if m.readCode != nil {
		code, err = m.readCode(m.source, codeHash)
	} else {
		code, err = m.source.Get(codeHash[:])
	}
	if errors.Is(err, leveldb.ErrNotFound) {
		code = nil
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("read account %s code %s: %w", accountHash, codeHash, err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("account %s code %s is missing", accountHash, codeHash)
	}
	if computed := crypto.Keccak256Hash(code); computed != codeHash {
		return nil, fmt.Errorf("account %s code hash mismatch: computed %s account %s", accountHash, computed, codeHash)
	}
	return code, nil
}

func (m *partitionedStateMigrator) openIterator(id *trie.ID) (*trie.Iterator, error) {
	t, err := trie.New(id, m.trieDB)
	if err != nil {
		return nil, err
	}
	nodes, err := t.NodeIterator(nil)
	if err != nil {
		return nil, err
	}
	return trie.NewIterator(nodes), nil
}

func (m *partitionedStateMigrator) openPartitionIterator(id *trie.ID, partition byte) (*trie.Iterator, error) {
	t, err := trie.New(id, m.trieDB)
	if err != nil {
		return nil, err
	}
	start, end := migratePartitionRange(partition)
	nodes, err := t.NodeIteratorWithRange(start, end)
	if err != nil {
		return nil, err
	}
	return trie.NewIterator(nodes), nil
}

func migratePartitionRange(partition byte) (start, end []byte) {
	if partition > 0 {
		start = make([]byte, common.HashLength)
		start[0] = partition << 4
	}
	if partition < migrateTriePartitions-1 {
		end = make([]byte, common.HashLength)
		end[0] = (partition + 1) << 4
	}
	return start, end
}

func validateMigratePartitionResult(partition byte, result migratePartitionResult) error {
	if !result.populated {
		if result.root != types.EmptyRootHash {
			return fmt.Errorf("empty partition %x has root %s", partition, result.root)
		}
		if len(result.rootBlob) != 0 {
			return fmt.Errorf("empty partition %x emitted a root node", partition)
		}
		return nil
	}
	if len(result.rootBlob) == 0 {
		return fmt.Errorf("partition %x did not emit its root node", partition)
	}
	if computed := crypto.Keccak256Hash(result.rootBlob); computed != result.root {
		return fmt.Errorf("partition %x root blob hash mismatch: computed %s root %s", partition, computed, result.root)
	}
	return nil
}

func assembleMigratedTrie(
	writer partitionStateOutput,
	owner, expectedRoot common.Hash,
	partitions [migrateTriePartitions]migratePartitionResult,
) (common.Hash, error) {
	var (
		populated int
		only      int
		children  [17][]byte
	)
	for index, partition := range partitions {
		if !partition.populated {
			continue
		}
		populated++
		only = index
		children[index] = bytes.Clone(partition.root[:])
	}
	if populated == 0 {
		if expectedRoot != types.EmptyRootHash {
			return common.Hash{}, fmt.Errorf("root mismatch: computed %s expected %s", types.EmptyRootHash, expectedRoot)
		}
		return types.EmptyRootHash, nil
	}
	var (
		rootHash common.Hash
		rootBlob []byte
		orphaned bool
		err      error
	)
	if populated == 1 {
		rootHash, rootBlob, orphaned, err = trie.MountPartitionRoot(partitions[only].rootBlob, byte(only))
		if err != nil {
			return common.Hash{}, fmt.Errorf("mount partition %x root: %w", only, err)
		}
	} else {
		rootBlob, rootHash, err = trie.AssembleBranch(children)
		if err != nil {
			return common.Hash{}, fmt.Errorf("assemble partition branch: %w", err)
		}
	}
	if rootHash != expectedRoot {
		return common.Hash{}, fmt.Errorf("root mismatch: computed %s expected %s", rootHash, expectedRoot)
	}
	if err := writer.TrieNode(owner, nil, rootHash, rootBlob); err != nil {
		return common.Hash{}, err
	}
	if orphaned {
		if err := writer.DeleteTrieNode(owner, []byte{byte(only)}, partitions[only].root); err != nil {
			return common.Hash{}, err
		}
	}
	return rootHash, nil
}

func addBundleCounts(target *bundle.Counts, counts bundle.Counts) {
	target.Accounts += counts.Accounts
	target.StorageSlots += counts.StorageSlots
	target.CodeReferences += counts.CodeReferences
	target.CodeRecords += counts.CodeRecords
	target.Records += counts.Records
	target.PayloadBytes += counts.PayloadBytes
}

func (m *partitionedStateMigrator) addProgress(counts bundle.Counts) {
	if m.progress == nil {
		return
	}
	m.progress.accounts.Add(counts.Accounts)
	m.progress.storageSlots.Add(counts.StorageSlots)
	m.progress.codeReferences.Add(counts.CodeReferences)
	m.progress.codeRecords.Add(counts.CodeRecords)
	m.progress.records.Add(counts.Records)
	m.progress.payloadBytes.Add(counts.PayloadBytes)
}
