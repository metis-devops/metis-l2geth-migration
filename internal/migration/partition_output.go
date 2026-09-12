package migration

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// partitionStateOutput separates state validation from target layout generation.
// A validation output never receives a writable database or creates batches.
type partitionStateOutput interface {
	StateVisitor
	trieNodeSink
	DeleteTrieNode(common.Hash, []byte, common.Hash) error
	CloseContext(context.Context) error
	Abort()
}

func (m *partitionedStateMigrator) newOutput(deferred bool) partitionStateOutput {
	if m.outputFactory != nil {
		return m.outputFactory(deferred)
	}
	if deferred {
		return newDeferredDirectStateWriter(m.target, m.scheme)
	}
	return newDirectStateWriter(m.target, m.scheme)
}

type validationStateOutput struct{}

func (validationStateOutput) Account(common.Hash, *types.StateAccount, []byte) error  { return nil }
func (validationStateOutput) Storage(common.Hash, common.Hash, []byte) error          { return nil }
func (validationStateOutput) Code(common.Hash, common.Hash, []byte) error             { return nil }
func (validationStateOutput) TrieNode(common.Hash, []byte, common.Hash, []byte) error { return nil }
func (validationStateOutput) DeleteTrieNode(common.Hash, []byte, common.Hash) error   { return nil }
func (validationStateOutput) CloseContext(ctx context.Context) error                  { return ctx.Err() }
func (validationStateOutput) Abort()                                                  {}

func validatePartitionedState(ctx context.Context, db ethdb.Database, head bundle.Head, workers int, progress *progressCounts) (result StateResult, retErr error) {
	nodes := triedb.NewDatabase(db, triedb.HashDefaults)
	defer func() { retErr = errors.Join(retErr, nodes.Close()) }()
	walker := &partitionedStateMigrator{
		ctx: ctx, source: db, trieDB: nodes, root: head.StateRoot,
		limiter: newMigrateWorkLimiter(workers), accounts: newMigrateAccountWindow(workers),
		codeHashes: newConcurrentHashSet(), progress: progress,
		outputFactory: func(bool) partitionStateOutput { return validationStateOutput{} },
	}
	result, output, err := walker.run()
	if err != nil {
		return result, err
	}
	return result, output.CloseContext(ctx)
}
