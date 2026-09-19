package migration

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func (m *partitionedStateMigrator) migrateStorage(
	ctx context.Context,
	accountHash, expectedRoot common.Hash,
	lease *migrateWorkLease,
) (bundle.Counts, error) {
	if expectedRoot == types.EmptyRootHash {
		return bundle.Counts{}, nil
	}
	partition, counts, err := m.probeStorage(ctx, accountHash, expectedRoot)
	if err != nil {
		return bundle.Counts{}, err
	}
	if !partition {
		return counts, nil
	}
	lease.release()
	return m.migratePartitionedStorage(ctx, accountHash, expectedRoot, lease)
}

func (m *partitionedStateMigrator) probeStorage(
	ctx context.Context,
	accountHash, expectedRoot common.Hash,
) (partition bool, counts bundle.Counts, retErr error) {
	if err := ctx.Err(); err != nil {
		return false, bundle.Counts{}, err
	}
	storage, err := m.openIterator(trie.StorageTrieID(m.root, accountHash, expectedRoot))
	if err != nil {
		return false, bundle.Counts{}, fmt.Errorf("open storage iterator for account %s: %w", accountHash, err)
	}
	writer := m.newOutput(true)
	defer writer.Abort()
	var nodeErr error
	storageStack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		if nodeErr == nil {
			nodeErr = writer.TrieNode(accountHash, path, hash, blob)
		}
	})
	for range migrateStoragePartitionThreshold {
		if err := ctx.Err(); err != nil {
			return false, bundle.Counts{}, err
		}
		if !storage.Next() {
			return m.finishProbedStorage(ctx, writer, storageStack, storage.Err, &nodeErr, accountHash, expectedRoot, counts)
		}
		if err := processMigratedStorageSlot(writer, storageStack, accountHash, storage.Key, storage.Value, &nodeErr, &counts); err != nil {
			return false, bundle.Counts{}, err
		}
	}
	if !storage.Next() {
		return m.finishProbedStorage(ctx, writer, storageStack, storage.Err, &nodeErr, accountHash, expectedRoot, counts)
	}
	if err := ctx.Err(); err != nil {
		return false, bundle.Counts{}, err
	}
	return true, bundle.Counts{}, nil
}

func (m *partitionedStateMigrator) finishProbedStorage(
	ctx context.Context,
	writer partitionStateOutput,
	stack *trie.StackTrie,
	iteratorErr error,
	nodeErr *error,
	accountHash, expectedRoot common.Hash,
	counts bundle.Counts,
) (bool, bundle.Counts, error) {
	if iteratorErr != nil {
		return false, bundle.Counts{}, fmt.Errorf("iterate storage trie for account %s: %w", accountHash, iteratorErr)
	}
	computedRoot := stack.Hash()
	if *nodeErr != nil {
		return false, bundle.Counts{}, fmt.Errorf("write final storage trie nodes for account %s: %w", accountHash, *nodeErr)
	}
	if computedRoot != expectedRoot {
		return false, bundle.Counts{}, fmt.Errorf("account %s storage root mismatch: computed %s account %s", accountHash, computedRoot, expectedRoot)
	}
	if err := writer.CloseContext(ctx); err != nil {
		return false, bundle.Counts{}, fmt.Errorf("flush account %s storage trie: %w", accountHash, err)
	}
	m.addProgress(counts)
	return false, counts, nil
}

func (m *partitionedStateMigrator) migratePartitionedStorage(
	ctx context.Context,
	accountHash, expectedRoot common.Hash,
	accountLease *migrateWorkLease,
) (bundle.Counts, error) {
	var partitions [migrateTriePartitions]migratePartitionResult
	err := runMigratePartitionTasks(ctx, func(taskCtx context.Context, index int) error {
		lease := newMigrateWorkLease(m.limiter)
		if err := lease.acquire(taskCtx); err != nil {
			return err
		}
		defer lease.release()
		result, err := m.migrateStoragePartition(taskCtx, accountHash, expectedRoot, byte(index))
		if err != nil {
			return fmt.Errorf("migrate account %s storage partition %x: %w", accountHash, index, err)
		}
		partitions[index] = result
		return nil
	})
	if err != nil {
		return bundle.Counts{}, err
	}
	if err := accountLease.acquire(ctx); err != nil {
		return bundle.Counts{}, err
	}
	var counts bundle.Counts
	for _, partition := range partitions {
		addBundleCounts(&counts, partition.counts)
	}
	writer := m.newOutput(false)
	defer writer.Abort()
	if _, err := assembleMigratedTrie(writer, accountHash, expectedRoot, partitions); err != nil {
		return bundle.Counts{}, fmt.Errorf("assemble account %s storage trie: %w", accountHash, err)
	}
	if err := writer.CloseContext(ctx); err != nil {
		return bundle.Counts{}, fmt.Errorf("flush account %s storage root: %w", accountHash, err)
	}
	return counts, nil
}

func (m *partitionedStateMigrator) migrateStoragePartition(
	ctx context.Context,
	accountHash, storageRoot common.Hash,
	partition byte,
) (migratePartitionResult, error) {
	if err := ctx.Err(); err != nil {
		return migratePartitionResult{}, err
	}
	storage, err := m.openPartitionIterator(trie.StorageTrieID(m.root, accountHash, storageRoot), partition)
	if err != nil {
		return migratePartitionResult{}, fmt.Errorf("open iterator: %w", err)
	}
	writer := m.newOutput(false)
	defer writer.Abort()
	var (
		result  migratePartitionResult
		nodeErr error
	)
	storageStack := trie.NewPartialStackTrie(partition, func(path []byte, hash common.Hash, blob []byte) {
		if nodeErr != nil {
			return
		}
		if len(path) == 1 {
			result.rootBlob = bytes.Clone(blob)
		}
		nodeErr = writer.TrieNode(accountHash, path, hash, blob)
	})
	for storage.Next() {
		if err := ctx.Err(); err != nil {
			return migratePartitionResult{}, err
		}
		if err := processMigratedStorageSlot(writer, storageStack, accountHash, storage.Key, storage.Value, &nodeErr, &result.counts); err != nil {
			return migratePartitionResult{}, err
		}
		m.addProgress(bundle.Counts{StorageSlots: 1, Records: 1, PayloadBytes: uint64(len(storage.Value))})
		result.populated = true
	}
	if storage.Err != nil {
		return migratePartitionResult{}, fmt.Errorf("iterate storage partition %x: %w", partition, storage.Err)
	}
	result.root = storageStack.Hash()
	if nodeErr != nil {
		return migratePartitionResult{}, fmt.Errorf("write final storage partition %x trie nodes: %w", partition, nodeErr)
	}
	if err := validateMigratePartitionResult(partition, result); err != nil {
		return migratePartitionResult{}, err
	}
	if err := writer.CloseContext(ctx); err != nil {
		return migratePartitionResult{}, fmt.Errorf("flush storage partition %x: %w", partition, err)
	}
	return result, nil
}

type migrateTrieUpdater interface {
	Update(key, value []byte) error
}

func processMigratedStorageSlot(
	writer partitionStateOutput,
	stack migrateTrieUpdater,
	accountHash common.Hash,
	key, value []byte,
	nodeErr *error,
	counts *bundle.Counts,
) error {
	if len(key) != common.HashLength {
		return fmt.Errorf("account %s storage key has length %d", accountHash, len(key))
	}
	if err := validateStorageRLP(value); err != nil {
		return fmt.Errorf("account %s slot %x: %w", accountHash, key, err)
	}
	if err := stack.Update(key, value); err != nil {
		return fmt.Errorf("rebuild storage trie for account %s: %w", accountHash, err)
	}
	if *nodeErr != nil {
		return fmt.Errorf("write storage trie nodes for account %s: %w", accountHash, *nodeErr)
	}
	if err := writer.Storage(accountHash, common.Hash(key), value); err != nil {
		return err
	}
	addBundleCounts(counts, bundle.Counts{StorageSlots: 1, Records: 1, PayloadBytes: uint64(len(value))})
	return nil
}
