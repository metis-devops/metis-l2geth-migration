package migration

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// StateVisitor receives canonical state entries in trie iteration order. Code
// is called once for each unique code hash, at its first account reference.
type StateVisitor interface {
	Account(hash common.Hash, account *types.StateAccount, fullRLP []byte) error
	Storage(accountHash, slotHash common.Hash, valueRLP []byte) error
	Code(accountHash, codeHash common.Hash, code []byte) error
}

type codeReader func(ethdb.KeyValueReader, common.Hash) ([]byte, error)

type stateTraversalOptions struct {
	NodeIndex trieNodeIndexOptions
	ReadCode  codeReader
	TrieNodes trieNodeSink
}

// StateResult contains a rebuilt state root and its entry counts.
type StateResult struct {
	Root   common.Hash
	Counts bundle.Counts
}

type stateTraverser struct {
	ctx              context.Context
	disk             ethdb.Database
	trieDB           *triedb.Database
	root             common.Hash
	visitor          StateVisitor
	codeHashes       hashSet
	readCode         codeReader
	trieNodes        trieNodeSink
	inventory        *stateInventory
	inventoryTracker *stateInventoryTracker
	counts           bundle.Counts
	accountStack     *trie.StackTrie
	nodeWriteErr     error
}

// TraverseState validates and visits all state reachable from root.
func TraverseState(ctx context.Context, disk ethdb.Database, trieDB *triedb.Database, root common.Hash, visitor StateVisitor) (StateResult, error) {
	result, _, err := traverseState(ctx, disk, trieDB, root, visitor, false, stateTraversalOptions{})
	return result, err
}

func traverseState(
	ctx context.Context,
	disk ethdb.Database,
	trieDB *triedb.Database,
	root common.Hash,
	visitor StateVisitor,
	collectInventory bool,
	opts stateTraversalOptions,
) (result StateResult, inventory stateInventory, retErr error) {
	var nodeIndex *temporaryTrieNodeIndex
	if collectInventory && trieDB.Scheme() == rawdb.HashScheme {
		var err error
		nodeIndex, err = newTemporaryTrieNodeIndex(opts.NodeIndex)
		if err != nil {
			return StateResult{}, stateInventory{}, err
		}
		defer func() {
			if err := nodeIndex.Close(); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}()
	}
	traverser := newStateTraverser(ctx, disk, trieDB, root, visitor, collectInventory, opts, nodeIndex)
	return traverser.run()
}

func newStateTraverser(
	ctx context.Context,
	disk ethdb.Database,
	trieDB *triedb.Database,
	root common.Hash,
	visitor StateVisitor,
	collectInventory bool,
	opts stateTraversalOptions,
	nodeIndex *temporaryTrieNodeIndex,
) *stateTraverser {
	readCode := opts.ReadCode
	if readCode == nil {
		readCode = readCodeWithFallback
	}
	inventory := &stateInventory{scheme: trieDB.Scheme()}
	traverser := &stateTraverser{
		ctx:        ctx,
		disk:       disk,
		trieDB:     trieDB,
		root:       root,
		visitor:    visitor,
		codeHashes: newHashSet(),
		readCode:   readCode,
		trieNodes:  opts.TrieNodes,
		inventory:  inventory,
	}
	if collectInventory {
		if inventory.scheme == rawdb.HashScheme {
			inventory.nodeIndex = nodeIndex
		}
		traverser.inventoryTracker = &stateInventoryTracker{inventory: inventory}
	}
	traverser.accountStack = newTraversalStackTrie(common.Hash{}, opts.TrieNodes, &traverser.nodeWriteErr)
	return traverser
}

func (t *stateTraverser) run() (StateResult, stateInventory, error) {
	accounts, err := t.openAccountIterator()
	if err != nil {
		return StateResult{}, stateInventory{}, err
	}
	stream := newAccountTrieStream(t.ctx, accounts)
	defer stream.close()
	for leaf := range stream.leaves {
		if err := t.processAccount(leaf); err != nil {
			return StateResult{}, stateInventory{}, err
		}
	}
	if err := stream.wait(); err != nil {
		return StateResult{}, stateInventory{}, err
	}
	return t.finalize()
}

func (t *stateTraverser) openAccountIterator() (*trie.Iterator, error) {
	accountTrie, err := trie.NewStateTrie(trie.StateTrieID(t.root), t.trieDB)
	if err != nil {
		return nil, fmt.Errorf("open account trie %s: %w", t.root, err)
	}
	nodeIt, err := accountTrie.NodeIterator(nil)
	if err != nil {
		return nil, fmt.Errorf("open account iterator: %w", err)
	}
	return trie.NewIterator(t.trackInventory(nodeIt)), nil
}

func (t *stateTraverser) trackInventory(nodeIt trie.NodeIterator) trie.NodeIterator {
	if t.inventoryTracker == nil {
		return nodeIt
	}
	return &inventoryNodeIterator{NodeIterator: nodeIt, tracker: t.inventoryTracker}
}

func (t *stateTraverser) processAccount(leaf accountTrieLeaf) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	accountHash := leaf.accountHash
	account, err := decodeFullAccount(accountHash, leaf.value)
	if err != nil {
		return err
	}
	if t.visitor != nil {
		if err := t.visitor.Account(accountHash, account, leaf.value); err != nil {
			return err
		}
	}
	t.counts.Accounts++
	t.counts.Records++
	t.counts.PayloadBytes += uint64(len(leaf.value))

	computedStorageRoot, err := t.traverseStorage(accountHash, account.Root)
	if err != nil {
		return err
	}
	if computedStorageRoot != account.Root {
		return fmt.Errorf("account %s storage root mismatch: computed %s account %s", accountHash, computedStorageRoot, account.Root)
	}
	if err := t.processCode(accountHash, common.BytesToHash(account.CodeHash)); err != nil {
		return err
	}
	if err := t.accountStack.Update(accountHash[:], leaf.value); err != nil {
		return fmt.Errorf("rebuild account trie: %w", err)
	}
	if t.nodeWriteErr != nil {
		return fmt.Errorf("write account trie nodes: %w", t.nodeWriteErr)
	}
	return nil
}

func (t *stateTraverser) processCode(accountHash, codeHash common.Hash) error {
	if codeHash == types.EmptyCodeHash {
		return nil
	}
	t.counts.CodeReferences++
	if !t.codeHashes.Add(codeHash) {
		return nil
	}
	code, err := t.readCode(t.disk, codeHash)
	if err != nil {
		return fmt.Errorf("read account %s code %s: %w", accountHash, codeHash, err)
	}
	if len(code) == 0 {
		return fmt.Errorf("account %s code %s is missing", accountHash, codeHash)
	}
	if computed := crypto.Keccak256Hash(code); computed != codeHash {
		return fmt.Errorf("account %s code hash mismatch: computed %s account %s", accountHash, computed, codeHash)
	}
	if t.visitor != nil {
		if err := t.visitor.Code(accountHash, codeHash, code); err != nil {
			return err
		}
	}
	t.counts.Records++
	t.counts.PayloadBytes += uint64(len(code))
	t.counts.CodeRecords++
	return nil
}

func readCodeWithFallback(db ethdb.KeyValueReader, hash common.Hash) ([]byte, error) {
	for _, key := range [][]byte{prefixedKey(rawdb.CodePrefix, hash[:]), hash[:]} {
		has, err := db.Has(key)
		if err != nil {
			return nil, err
		}
		if has {
			return db.Get(key)
		}
	}
	return nil, nil
}

func (t *stateTraverser) finalize() (StateResult, stateInventory, error) {
	computedRoot := t.accountStack.Hash()
	if t.nodeWriteErr != nil {
		return StateResult{}, stateInventory{}, fmt.Errorf("write final account trie nodes: %w", t.nodeWriteErr)
	}
	if computedRoot != t.root {
		return StateResult{}, stateInventory{}, fmt.Errorf("state root mismatch: computed %s header %s", computedRoot, t.root)
	}
	if t.inventoryTracker != nil {
		t.inventory.CodeEntries = t.counts.CodeRecords
		if t.inventory.scheme == rawdb.HashScheme {
			count, err := t.inventory.nodeIndex.Count(t.ctx)
			if err != nil {
				return StateResult{}, stateInventory{}, fmt.Errorf("count reachable hash trie nodes: %w", err)
			}
			t.inventory.TrieNodes = count
		}
		t.inventory.nodeIndex = nil
	}
	return StateResult{Root: computedRoot, Counts: t.counts}, *t.inventory, nil
}
