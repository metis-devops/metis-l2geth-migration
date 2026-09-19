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

type partitionOutputFactory func(deferred bool) partitionStateOutput

func (m *partitionedStateMigrator) newOutput(deferred bool) partitionStateOutput {
	return m.outputFactory(deferred)
}

func persistentPartitionOutput(db ethdb.Database, scheme string) partitionOutputFactory {
	return func(deferred bool) partitionStateOutput {
		if deferred {
			return newDeferredDirectStateWriter(db, scheme)
		}
		return newDirectStateWriter(db, scheme)
	}
}

func newValidationOutput(bool) partitionStateOutput { return validationStateOutput{} }

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
		outputFactory: newValidationOutput,
	}
	result, output, err := walker.run()
	if err != nil {
		return result, err
	}
	return result, output.CloseContext(ctx)
}
