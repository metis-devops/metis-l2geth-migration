package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

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

type partitionedStateMigrator struct {
	ctx        context.Context
	source     ethdb.Database
	trieDB     *triedb.Database
	target     ethdb.Database
	scheme     string
	root       common.Hash
	limiter    *migrateWorkLimiter
	codeHashes *concurrentHashSet
	progress   *progressCounts
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
) (result StateResult, finalWriter *directStateWriter, retErr error) {
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
		ctx:        ctx,
		source:     s.db,
		trieDB:     trieDB,
		target:     target,
		scheme:     scheme,
		root:       s.head.StateRoot,
		limiter:    newMigrateWorkLimiter(workers),
		codeHashes: newConcurrentHashSet(),
		progress:   progress,
	}
	return migrator.run()
}

func (m *partitionedStateMigrator) run() (StateResult, *directStateWriter, error) {
	var partitions [migrateTriePartitions]migratePartitionResult
	err := runMigratePartitionTasks(m.ctx, func(ctx context.Context, index int) error {
		lease := newMigrateWorkLease(m.limiter)
		if err := lease.acquire(ctx); err != nil {
			return err
		}
		defer lease.release()
		result, err := m.migrateAccountPartition(ctx, byte(index), lease)
		if err != nil {
			return fmt.Errorf("migrate account partition %x: %w", index, err)
		}
		partitions[index] = result
		return nil
	})
	if err != nil {
		return StateResult{}, nil, err
	}
	var counts bundle.Counts
	for _, partition := range partitions {
		addBundleCounts(&counts, partition.counts)
	}
	finalWriter := newDirectStateWriter(m.target, m.scheme)
	root, err := assembleMigratedTrie(finalWriter, common.Hash{}, m.root, partitions)
	if err != nil {
		finalWriter.Abort()
		return StateResult{}, nil, fmt.Errorf("assemble account trie: %w", err)
	}
	return StateResult{Root: root, Counts: counts}, finalWriter, nil
}

func (m *partitionedStateMigrator) migrateAccountPartition(
	ctx context.Context,
	partition byte,
	lease *migrateWorkLease,
) (migratePartitionResult, error) {
	if err := ctx.Err(); err != nil {
		return migratePartitionResult{}, err
	}
	accounts, err := m.openPartitionIterator(trie.StateTrieID(m.root), partition)
	if err != nil {
		return migratePartitionResult{}, fmt.Errorf("open account iterator: %w", err)
	}
	writer := newDirectStateWriter(m.target, m.scheme)
	defer writer.Abort()
	var (
		result  migratePartitionResult
		nodeErr error
	)
	accountStack := trie.NewPartialStackTrie(partition, func(path []byte, hash common.Hash, blob []byte) {
		if nodeErr != nil {
			return
		}
		if len(path) == 1 {
			result.rootBlob = bytes.Clone(blob)
		}
		nodeErr = writer.TrieNode(common.Hash{}, path, hash, blob)
	})
	for accounts.Next() {
		if err := ctx.Err(); err != nil {
			return migratePartitionResult{}, err
		}
		if len(accounts.Key) != common.HashLength {
			return migratePartitionResult{}, fmt.Errorf("account trie key has length %d", len(accounts.Key))
		}
		accountHash := common.Hash(accounts.Key)
		account, err := decodeFullAccount(accountHash, accounts.Value)
		if err != nil {
			return migratePartitionResult{}, err
		}
		if err := writer.Account(accountHash, account, accounts.Value); err != nil {
			return migratePartitionResult{}, err
		}
		accountCounts := bundle.Counts{Accounts: 1, Records: 1, PayloadBytes: uint64(len(accounts.Value))}
		addBundleCounts(&result.counts, accountCounts)
		m.addProgress(accountCounts)

		storageCounts, err := m.migrateStorage(ctx, accountHash, account.Root, lease)
		if err != nil {
			return migratePartitionResult{}, err
		}
		addBundleCounts(&result.counts, storageCounts)
		if err := m.migrateCode(ctx, writer, accountHash, common.BytesToHash(account.CodeHash), &result.counts); err != nil {
			return migratePartitionResult{}, err
		}
		if err := accountStack.Update(accountHash[:], accounts.Value); err != nil {
			return migratePartitionResult{}, fmt.Errorf("rebuild account partition %x: %w", partition, err)
		}
		if nodeErr != nil {
			return migratePartitionResult{}, fmt.Errorf("write account partition %x trie nodes: %w", partition, nodeErr)
		}
		result.populated = true
	}
	if accounts.Err != nil {
		return migratePartitionResult{}, fmt.Errorf("iterate account partition %x: %w", partition, accounts.Err)
	}
	result.root = accountStack.Hash()
	if nodeErr != nil {
		return migratePartitionResult{}, fmt.Errorf("write final account partition %x trie nodes: %w", partition, nodeErr)
	}
	if err := validateMigratePartitionResult(partition, result); err != nil {
		return migratePartitionResult{}, err
	}
	if err := writer.CloseContext(ctx); err != nil {
		return migratePartitionResult{}, fmt.Errorf("flush account partition %x: %w", partition, err)
	}
	return result, nil
}

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
	writer := newDeferredDirectStateWriter(m.target, m.scheme)
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
			return m.finishProbedStorage(ctx, writer, storageStack, storage.Err, nodeErr, accountHash, expectedRoot, counts)
		}
		if err := processMigratedStorageSlot(writer, storageStack, accountHash, storage.Key, storage.Value, &nodeErr, &counts); err != nil {
			return false, bundle.Counts{}, err
		}
	}
	if !storage.Next() {
		return m.finishProbedStorage(ctx, writer, storageStack, storage.Err, nodeErr, accountHash, expectedRoot, counts)
	}
	if err := ctx.Err(); err != nil {
		return false, bundle.Counts{}, err
	}
	return true, bundle.Counts{}, nil
}

func (m *partitionedStateMigrator) finishProbedStorage(
	ctx context.Context,
	writer *directStateWriter,
	stack *trie.StackTrie,
	iteratorErr, nodeErr error,
	accountHash, expectedRoot common.Hash,
	counts bundle.Counts,
) (bool, bundle.Counts, error) {
	if iteratorErr != nil {
		return false, bundle.Counts{}, fmt.Errorf("iterate storage trie for account %s: %w", accountHash, iteratorErr)
	}
	computedRoot := stack.Hash()
	if nodeErr != nil {
		return false, bundle.Counts{}, fmt.Errorf("write final storage trie nodes for account %s: %w", accountHash, nodeErr)
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
	writer := newDirectStateWriter(m.target, m.scheme)
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
	writer := newDirectStateWriter(m.target, m.scheme)
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

func (m *partitionedStateMigrator) migrateCode(
	ctx context.Context,
	writer *directStateWriter,
	accountHash, codeHash common.Hash,
	counts *bundle.Counts,
) error {
	if codeHash == types.EmptyCodeHash {
		return nil
	}
	reference := bundle.Counts{CodeReferences: 1}
	addBundleCounts(counts, reference)
	m.addProgress(reference)
	if !m.codeHashes.Add(codeHash) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	code, err := m.source.Get(codeHash[:])
	if errors.Is(err, leveldb.ErrNotFound) {
		code = nil
		err = nil
	}
	if err != nil {
		return fmt.Errorf("read account %s code %s: %w", accountHash, codeHash, err)
	}
	if len(code) == 0 {
		return fmt.Errorf("account %s code %s is missing", accountHash, codeHash)
	}
	if computed := crypto.Keccak256Hash(code); computed != codeHash {
		return fmt.Errorf("account %s code hash mismatch: computed %s account %s", accountHash, computed, codeHash)
	}
	if err := writer.Code(accountHash, codeHash, code); err != nil {
		return err
	}
	record := bundle.Counts{CodeRecords: 1, Records: 1, PayloadBytes: uint64(len(code))}
	addBundleCounts(counts, record)
	m.addProgress(record)
	return nil
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

type migrateTrieUpdater interface {
	Update(key, value []byte) error
}

func processMigratedStorageSlot(
	writer *directStateWriter,
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
	writer *directStateWriter,
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

type migrateWorkLimiter struct {
	tokens chan struct{}
}

func newMigrateWorkLimiter(workers int) *migrateWorkLimiter {
	return &migrateWorkLimiter{tokens: make(chan struct{}, normalizeMigrateWorkers(workers))}
}

func (l *migrateWorkLimiter) acquire(ctx context.Context) error {
	select {
	case l.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *migrateWorkLimiter) release() {
	<-l.tokens
}

type migrateWorkLease struct {
	limiter *migrateWorkLimiter
	held    bool
}

func newMigrateWorkLease(limiter *migrateWorkLimiter) *migrateWorkLease {
	return &migrateWorkLease{limiter: limiter}
}

func (l *migrateWorkLease) acquire(ctx context.Context) error {
	if l.held {
		return nil
	}
	if err := l.limiter.acquire(ctx); err != nil {
		return err
	}
	l.held = true
	return nil
}

func (l *migrateWorkLease) release() {
	if l == nil || !l.held {
		return
	}
	l.held = false
	l.limiter.release()
}

func runMigratePartitionTasks(ctx context.Context, task func(context.Context, int) error) error {
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for index := range migrateTriePartitions {
		wg.Go(func() {
			if err := task(taskCtx, index); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
