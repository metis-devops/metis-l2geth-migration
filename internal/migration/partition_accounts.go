package migration

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"sync"
)

type migrateAccountJob struct {
	sequence  uint64
	hash      common.Hash
	rlp       []byte
	account   *types.StateAccount
	codeHash  common.Hash
	writeCode bool
	counts    bundle.Counts
}

type migrateAccountResult struct {
	job     migrateAccountJob
	account *types.StateAccount
	code    []byte
	counts  bundle.Counts
	err     error
}

type migratePreparedAccount struct {
	hash          common.Hash
	account       *types.StateAccount
	codeHash      common.Hash
	writeCode     bool
	codeReference bool
}

func (p migratePreparedAccount) result(sequence uint64, accountRLP []byte) migrateAccountResult {
	var counts bundle.Counts
	if p.codeReference {
		counts.CodeReferences = 1
	}
	job := migrateAccountJob{
		sequence:  sequence,
		hash:      p.hash,
		rlp:       accountRLP,
		account:   p.account,
		codeHash:  p.codeHash,
		writeCode: p.writeCode,
		counts:    counts,
	}
	return migrateAccountResult{job: job, account: p.account, counts: counts}
}

type migrateAccountProcessor func(context.Context, migrateAccountJob) migrateAccountResult

func (m *partitionedStateMigrator) migrateAccountPartition(
	ctx context.Context,
	partition byte,
) (migratePartitionResult, error) {
	if err := ctx.Err(); err != nil {
		return migratePartitionResult{}, err
	}
	iteratorLease := newMigrateWorkLease(m.limiter)
	if err := iteratorLease.acquire(ctx); err != nil {
		return migratePartitionResult{}, err
	}
	accounts, err := m.openPartitionIterator(trie.StateTrieID(m.root), partition)
	iteratorLease.release()
	if err != nil {
		return migratePartitionResult{}, fmt.Errorf("open account iterator: %w", err)
	}
	writer := m.newOutput(false)
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
	accountLease := newMigrateWorkLease(m.limiter)
	defer accountLease.release()
	var sequence uint64
	for {
		if err := accountLease.acquire(ctx); err != nil {
			return migratePartitionResult{}, err
		}
		next := accounts.Next()
		if !next {
			iteratorErr := accounts.Err
			accountLease.release()
			if iteratorErr != nil {
				return migratePartitionResult{}, fmt.Errorf("iterate account partition %x: %w", partition, iteratorErr)
			}
			break
		}
		prepared, async, err := m.prepareMigrateAccount(partition, sequence, accounts.Key, accounts.Value)
		if err != nil {
			return migratePartitionResult{}, err
		}
		if !async {
			var counts bundle.Counts
			if prepared.codeReference {
				counts.CodeReferences = 1
			}
			err := m.mergeMigrateAccountDataHeld(
				partition, writer, accountStack, &nodeErr,
				prepared.hash, prepared.account, accounts.Value, prepared.codeHash, nil, counts, &result,
			)
			if err != nil {
				return migratePartitionResult{}, err
			}
			sequence++
			m.yieldAccountLeaseForPendingBurst(accountLease)
			continue
		}
		first := prepared.result(sequence, bytes.Clone(accounts.Value))
		accountLease.release()
		var eof bool
		sequence, eof, err = m.runMigrateAccountBurst(
			ctx, partition, accounts, first, writer, accountStack, &nodeErr, &result,
		)
		if err != nil {
			return migratePartitionResult{}, err
		}
		if eof {
			break
		}
	}

	return m.finishMigrateAccountPartition(ctx, partition, writer, accountStack, &nodeErr, &result)
}

func (m *partitionedStateMigrator) yieldAccountLeaseForPendingBurst(lease *migrateWorkLease) {
	if m.accounts.pending() {
		lease.release()
	}
}

func (m *partitionedStateMigrator) finishMigrateAccountPartition(
	ctx context.Context,
	partition byte,
	writer partitionStateOutput,
	accountStack *trie.PartialStackTrie,
	nodeErr *error,
	result *migratePartitionResult,
) (migratePartitionResult, error) {
	lease := newMigrateWorkLease(m.limiter)
	if err := lease.acquire(ctx); err != nil {
		return migratePartitionResult{}, err
	}
	defer lease.release()
	result.root = accountStack.Hash()
	if *nodeErr != nil {
		return migratePartitionResult{}, fmt.Errorf("write final account partition %x trie nodes: %w", partition, *nodeErr)
	}
	if err := validateMigratePartitionResult(partition, *result); err != nil {
		return migratePartitionResult{}, err
	}
	if err := writer.CloseContext(ctx); err != nil {
		return migratePartitionResult{}, fmt.Errorf("flush account partition %x: %w", partition, err)
	}
	return *result, nil
}

func (m *partitionedStateMigrator) prepareMigrateAccount(
	partition byte,
	sequence uint64,
	key, accountRLP []byte,
) (migratePreparedAccount, bool, error) {
	if len(key) != common.HashLength {
		return migratePreparedAccount{}, false, fmt.Errorf(
			"account partition %x sequence %d: account trie key has length %d", partition, sequence, len(key),
		)
	}
	accountHash := common.Hash(key)
	account, err := decodeFullAccount(accountHash, accountRLP)
	if err != nil {
		return migratePreparedAccount{}, false, err
	}
	if m.validateAccount != nil {
		if err := m.validateAccount(accountHash, account); err != nil {
			return migratePreparedAccount{}, false, err
		}
	}
	codeHash := common.BytesToHash(account.CodeHash)
	prepared := migratePreparedAccount{
		hash: accountHash, account: account, codeHash: codeHash,
	}
	if codeHash != types.EmptyCodeHash {
		prepared.codeReference = true
		m.addProgress(bundle.Counts{CodeReferences: 1})
		prepared.writeCode = m.codeHashes.Add(codeHash)
	}
	async := account.Root != types.EmptyRootHash || prepared.writeCode
	return prepared, async, nil
}

func (m *partitionedStateMigrator) runMigrateAccountBurst(
	ctx context.Context,
	partition byte,
	accounts *trie.Iterator,
	first migrateAccountResult,
	writer partitionStateOutput,
	accountStack migrateTrieUpdater,
	nodeErr *error,
	partitionResult *migratePartitionResult,
) (nextSequence uint64, eof bool, retErr error) {
	burstCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	failure := &migratePipelineFailure{cancel: cancel, onFirst: m.recordRunFailure}
	results := make(chan migrateAccountResult, m.accounts.capacity())
	var jobs sync.WaitGroup
	if err := m.accounts.acquire(burstCtx); err != nil {
		return first.job.sequence, false, err
	}
	m.submitMigrateAccountTask(burstCtx, partition, first, results, failure, &jobs)
	nextSequence = first.job.sequence + 1
	for m.accounts.tryAcquire() {
		lease := newMigrateWorkLease(m.limiter)
		if err := lease.acquire(burstCtx); err != nil {
			m.accounts.release()
			failure.record(err)
			break
		}
		next := accounts.Next()
		if !next {
			iteratorErr := accounts.Err
			lease.release()
			m.accounts.release()
			if iteratorErr != nil {
				failure.record(fmt.Errorf("iterate account partition %x: %w", partition, iteratorErr))
			}
			eof = true
			break
		}
		prepared, async, err := m.prepareMigrateAccount(partition, nextSequence, accounts.Key, accounts.Value)
		if err != nil {
			lease.release()
			m.accounts.release()
			failure.record(err)
			break
		}
		ready := prepared.result(nextSequence, bytes.Clone(accounts.Value))
		lease.release()
		if async {
			m.submitMigrateAccountTask(burstCtx, partition, ready, results, failure, &jobs)
		} else {
			results <- ready
		}
		nextSequence++
	}
	go func() {
		jobs.Wait()
		close(results)
	}()
	if err := m.mergeMigrateAccounts(
		burstCtx, partition, first.job.sequence, writer, accountStack, nodeErr, results, partitionResult, failure,
	); err != nil {
		return nextSequence, eof, err
	}
	return nextSequence, eof, nil
}

func (m *partitionedStateMigrator) submitMigrateAccountTask(
	ctx context.Context,
	partition byte,
	prepared migrateAccountResult,
	results chan<- migrateAccountResult,
	failure *migratePipelineFailure,
	jobs *sync.WaitGroup,
) {
	process := m.accountProcessor
	if process == nil {
		process = m.processMigrateAccount
	}
	jobs.Go(func() {
		result := process(ctx, prepared.job)
		if result.err != nil {
			failure.record(fmt.Errorf(
				"account partition %x sequence %d account %s: %w",
				partition, prepared.job.sequence, prepared.job.hash, result.err,
			))
		}
		results <- result
	})
}

func (m *partitionedStateMigrator) processMigrateAccount(ctx context.Context, job migrateAccountJob) migrateAccountResult {
	result := migrateAccountResult{job: job, counts: job.counts}
	lease := newMigrateWorkLease(m.limiter)
	if err := lease.acquire(ctx); err != nil {
		result.err = err
		return result
	}
	defer lease.release()
	result.account = job.account
	storageCounts, err := m.migrateStorage(ctx, job.hash, job.account.Root, lease)
	if err != nil {
		result.err = err
		return result
	}
	addBundleCounts(&result.counts, storageCounts)
	if job.writeCode {
		code, err := m.readMigrateCode(ctx, job.hash, job.codeHash)
		if err != nil {
			result.err = err
			return result
		}
		result.code = code
	}
	return result
}

func (m *partitionedStateMigrator) mergeMigrateAccounts(
	ctx context.Context,
	partition byte,
	next uint64,
	writer partitionStateOutput,
	accountStack migrateTrieUpdater,
	nodeErr *error,
	results <-chan migrateAccountResult,
	partitionResult *migratePartitionResult,
	failure *migratePipelineFailure,
) error {
	pending := make(map[uint64]migrateAccountResult, m.accounts.capacity())
	for result := range results {
		if failure.load() != nil {
			m.accounts.release()
			for range pending {
				m.accounts.release()
			}
			clear(pending)
			continue
		}
		pending[result.job.sequence] = result
		for {
			ordered, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			err := m.mergeMigrateAccount(ctx, partition, writer, accountStack, nodeErr, ordered, partitionResult)
			m.accounts.release()
			if err != nil {
				failure.record(fmt.Errorf(
					"merge account partition %x sequence %d account %s: %w",
					partition, ordered.job.sequence, ordered.job.hash, err,
				))
				for range pending {
					m.accounts.release()
				}
				clear(pending)
				break
			}
			next++
		}
	}
	if err := failure.load(); err != nil {
		return err
	}
	if len(pending) != 0 {
		return fmt.Errorf("account partition %x pipeline ended with %d unmerged results", partition, len(pending))
	}
	return ctx.Err()
}

func (m *partitionedStateMigrator) mergeMigrateAccount(
	ctx context.Context,
	partition byte,
	writer partitionStateOutput,
	accountStack migrateTrieUpdater,
	nodeErr *error,
	result migrateAccountResult,
	partitionResult *migratePartitionResult,
) error {
	if result.err != nil {
		return result.err
	}
	lease := newMigrateWorkLease(m.limiter)
	if err := lease.acquire(ctx); err != nil {
		return err
	}
	defer lease.release()
	return m.mergeMigrateAccountDataHeld(
		partition, writer, accountStack, nodeErr,
		result.job.hash, result.account, result.job.rlp, result.job.codeHash, result.code, result.counts, partitionResult,
	)
}

func (m *partitionedStateMigrator) mergeMigrateAccountDataHeld(
	partition byte,
	writer partitionStateOutput,
	accountStack migrateTrieUpdater,
	nodeErr *error,
	accountHash common.Hash,
	account *types.StateAccount,
	accountRLP []byte,
	codeHash common.Hash,
	code []byte,
	counts bundle.Counts,
	partitionResult *migratePartitionResult,
) error {
	if err := writer.Account(accountHash, account, accountRLP); err != nil {
		return err
	}
	if len(code) != 0 {
		if err := writer.Code(accountHash, codeHash, code); err != nil {
			return err
		}
		record := bundle.Counts{CodeRecords: 1, Records: 1, PayloadBytes: uint64(len(code))}
		addBundleCounts(&counts, record)
		m.addProgress(record)
	}
	if err := accountStack.Update(accountHash[:], accountRLP); err != nil {
		return fmt.Errorf("rebuild account partition %x: %w", partition, err)
	}
	if *nodeErr != nil {
		return fmt.Errorf("write account partition %x trie nodes: %w", partition, *nodeErr)
	}
	accountCounts := bundle.Counts{Accounts: 1, Records: 1, PayloadBytes: uint64(len(accountRLP))}
	addBundleCounts(&partitionResult.counts, accountCounts)
	addBundleCounts(&partitionResult.counts, counts)
	m.addProgress(accountCounts)
	partitionResult.populated = true
	return nil
}
