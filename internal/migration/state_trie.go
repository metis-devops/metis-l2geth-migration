package migration

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

type trieNodeSink interface {
	TrieNode(owner common.Hash, path []byte, hash common.Hash, blob []byte) error
}

type stateInventory struct {
	TrieNodes   uint64
	CodeEntries uint64

	scheme    string
	nodeIndex *temporaryCodeHashIndex
}

func (t *stateTraverser) traverseStorage(accountHash, expectedRoot common.Hash) (common.Hash, error) {
	if expectedRoot == types.EmptyRootHash {
		return types.EmptyRootHash, nil
	}
	storageStack := newTraversalStackTrie(accountHash, t.trieNodes, &t.nodeWriteErr)
	storageTrie, err := trie.NewStateTrie(trie.StorageTrieID(t.root, accountHash, expectedRoot), t.trieDB)
	if err != nil {
		return common.Hash{}, fmt.Errorf("open storage trie for account %s at %s: %w", accountHash, expectedRoot, err)
	}
	storageNodeIt, err := storageTrie.NodeIterator(nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("open storage iterator for account %s: %w", accountHash, err)
	}
	storage := trie.NewIterator(t.trackInventory(storageNodeIt))
	if err := t.consumeStorage(accountHash, storageStack, storage); err != nil {
		return common.Hash{}, err
	}
	computedRoot := storageStack.Hash()
	if t.nodeWriteErr != nil {
		return common.Hash{}, fmt.Errorf("write final storage trie nodes for account %s: %w", accountHash, t.nodeWriteErr)
	}
	return computedRoot, nil
}

func (t *stateTraverser) consumeStorage(accountHash common.Hash, storageStack *trie.StackTrie, storage *trie.Iterator) error {
	for range storageTriePipelineThreshold {
		if err := t.ctx.Err(); err != nil {
			return err
		}
		if !storage.Next() {
			if storage.Err != nil {
				return fmt.Errorf("iterate storage trie for account %s: %w", accountHash, storage.Err)
			}
			return nil
		}
		if err := t.processStorageSlot(accountHash, storageStack, storage.Key, storage.Value); err != nil {
			return err
		}
	}
	stream := newStorageTrieStream(t.ctx, accountHash, storage)
	defer stream.close()
	for leaf := range stream.leaves {
		if err := t.processStorageSlot(accountHash, storageStack, leaf.slotHash[:], leaf.value); err != nil {
			return err
		}
	}
	if err := stream.wait(); err != nil {
		return err
	}
	return nil
}

func (t *stateTraverser) processStorageSlot(accountHash common.Hash, stack *trie.StackTrie, key, value []byte) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if len(key) != common.HashLength {
		return fmt.Errorf("account %s storage key has length %d", accountHash, len(key))
	}
	if err := validateStorageRLP(value); err != nil {
		return fmt.Errorf("account %s slot %x: %w", accountHash, key, err)
	}
	if err := stack.Update(key, value); err != nil {
		return fmt.Errorf("rebuild storage trie for account %s: %w", accountHash, err)
	}
	if t.nodeWriteErr != nil {
		return fmt.Errorf("write storage trie nodes for account %s: %w", accountHash, t.nodeWriteErr)
	}
	if t.visitor != nil {
		if err := t.visitor.Storage(accountHash, common.BytesToHash(key), value); err != nil {
			return err
		}
	}
	t.counts.StorageSlots++
	t.counts.Records++
	t.counts.PayloadBytes += uint64(len(value))
	return nil
}

func newTraversalStackTrie(owner common.Hash, sink trieNodeSink, sinkErr *error) *trie.StackTrie {
	if sink == nil {
		return trie.NewStackTrie(nil)
	}
	return trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		if *sinkErr != nil {
			return
		}
		*sinkErr = sink.TrieNode(owner, path, hash, blob)
	})
}

type inventoryNodeIterator struct {
	trie.NodeIterator
	tracker *stateInventoryTracker
	err     error
}

type stateInventoryTracker struct {
	mu        sync.Mutex
	inventory *stateInventory
}

// recordTrieNode serializes inventory updates from the account and storage
// producers, which may iterate concurrently.
func (t *stateInventoryTracker) recordTrieNode(hash common.Hash) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inventory.scheme == rawdb.HashScheme {
		return t.inventory.nodeIndex.MarkTrieNode(hash)
	}
	t.inventory.TrieNodes++
	return nil
}

func (it *inventoryNodeIterator) Next(descend bool) bool {
	if !it.NodeIterator.Next(descend) {
		return false
	}
	hash := it.Hash()
	if hash == (common.Hash{}) {
		return true
	}
	if err := it.tracker.recordTrieNode(hash); err != nil {
		it.err = err
		return false
	}
	return true
}

func (it *inventoryNodeIterator) Error() error {
	if it.err != nil {
		return it.err
	}
	return it.NodeIterator.Error()
}

func decodeFullAccount(accountHash common.Hash, data []byte) (*types.StateAccount, error) {
	var account types.StateAccount
	if err := rlp.DecodeBytes(data, &account); err != nil {
		return nil, fmt.Errorf("account %s: decode account RLP: %w", accountHash, err)
	}
	if account.Balance == nil {
		return nil, fmt.Errorf("account %s: account balance is nil", accountHash)
	}
	if len(account.CodeHash) != common.HashLength {
		return nil, fmt.Errorf("account %s: account code hash has length %d", accountHash, len(account.CodeHash))
	}
	encoded, err := rlp.EncodeToBytes(&account)
	if err != nil {
		return nil, fmt.Errorf("account %s: encode canonical account RLP: %w", accountHash, err)
	}
	if !bytes.Equal(encoded, data) {
		return nil, fmt.Errorf("account %s uses a non-canonical v1.17.5 account encoding", accountHash)
	}
	return &account, nil
}

func validateStorageRLP(data []byte) error {
	kind, content, rest, err := rlp.Split(data)
	if err != nil {
		return fmt.Errorf("decode storage value RLP: %w", err)
	}
	if kind != rlp.Byte && kind != rlp.String {
		return errors.New("storage value is not an RLP byte string")
	}
	if len(rest) != 0 {
		return errors.New("storage value has trailing RLP data")
	}
	if len(content) == 0 {
		return errors.New("zero storage value must not be present in the trie")
	}
	if len(content) > common.HashLength {
		return fmt.Errorf("storage value is %d bytes, maximum is %d", len(content), common.HashLength)
	}
	if content[0] == 0 {
		return errors.New("storage value contains a leading zero")
	}
	return nil
}
