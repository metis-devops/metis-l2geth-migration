package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// StateVisitor receives canonical state entries in trie iteration order.
type StateVisitor interface {
	Account(hash common.Hash, account *types.StateAccount, fullRLP []byte) error
	Storage(accountHash, slotHash common.Hash, valueRLP []byte) error
	Code(accountHash, codeHash common.Hash, code []byte) error
}

// StateResult contains a rebuilt state root and its entry counts.
type StateResult struct {
	Root   common.Hash
	Counts bundle.Counts
}

type stateInventory struct {
	TrieNodes   uint64
	CodeEntries uint64

	scheme     string
	hashNodes  map[common.Hash]struct{}
	codeHashes map[common.Hash]struct{}
}

// TraverseState validates and visits all state reachable from root.
func TraverseState(ctx context.Context, disk ethdb.Database, trieDB *triedb.Database, root common.Hash, visitor StateVisitor) (StateResult, error) {
	result, _, err := traverseState(ctx, disk, trieDB, root, visitor, false)
	return result, err
}

func traverseState(ctx context.Context, disk ethdb.Database, trieDB *triedb.Database, root common.Hash, visitor StateVisitor, collectInventory bool) (StateResult, stateInventory, error) {
	inventory := stateInventory{scheme: trieDB.Scheme()}
	if collectInventory {
		inventory.codeHashes = make(map[common.Hash]struct{})
		if inventory.scheme == rawdb.HashScheme {
			inventory.hashNodes = make(map[common.Hash]struct{})
		}
	}
	accountTrie, err := trie.NewStateTrie(trie.StateTrieID(root), trieDB)
	if err != nil {
		return StateResult{}, stateInventory{}, fmt.Errorf("open account trie %s: %w", root, err)
	}
	nodeIt, err := accountTrie.NodeIterator(nil)
	if err != nil {
		return StateResult{}, stateInventory{}, fmt.Errorf("open account iterator: %w", err)
	}
	if collectInventory {
		nodeIt = &inventoryNodeIterator{NodeIterator: nodeIt, inventory: &inventory}
	}
	accounts := trie.NewIterator(nodeIt)
	accountStack := trie.NewStackTrie(nil)
	var (
		counts    bundle.Counts
		seenCodes = make(map[common.Hash]struct{})
	)
	for accounts.Next() {
		if err := ctx.Err(); err != nil {
			return StateResult{}, stateInventory{}, err
		}
		if len(accounts.Key) != common.HashLength {
			return StateResult{}, stateInventory{}, fmt.Errorf("account trie key has length %d", len(accounts.Key))
		}
		accountHash := common.BytesToHash(accounts.Key)
		account, canonicalRLP, err := decodeFullAccount(accounts.Value)
		if err != nil {
			return StateResult{}, stateInventory{}, fmt.Errorf("account %s: %w", accountHash, err)
		}
		if !bytes.Equal(canonicalRLP, accounts.Value) {
			return StateResult{}, stateInventory{}, fmt.Errorf("account %s uses a non-canonical v1.17.5 account encoding", accountHash)
		}
		if visitor != nil {
			if err := visitor.Account(accountHash, account, canonicalRLP); err != nil {
				return StateResult{}, stateInventory{}, err
			}
		}
		counts.Accounts++
		counts.Records++
		counts.PayloadBytes += uint64(len(canonicalRLP))

		storageStack := trie.NewStackTrie(nil)
		if account.Root != types.EmptyRootHash {
			storageTrie, err := trie.NewStateTrie(trie.StorageTrieID(root, accountHash, account.Root), trieDB)
			if err != nil {
				return StateResult{}, stateInventory{}, fmt.Errorf("open storage trie for account %s at %s: %w", accountHash, account.Root, err)
			}
			storageNodeIt, err := storageTrie.NodeIterator(nil)
			if err != nil {
				return StateResult{}, stateInventory{}, fmt.Errorf("open storage iterator for account %s: %w", accountHash, err)
			}
			if collectInventory {
				storageNodeIt = &inventoryNodeIterator{NodeIterator: storageNodeIt, inventory: &inventory}
			}
			storage := trie.NewIterator(storageNodeIt)
			for storage.Next() {
				if err := ctx.Err(); err != nil {
					return StateResult{}, stateInventory{}, err
				}
				if len(storage.Key) != common.HashLength {
					return StateResult{}, stateInventory{}, fmt.Errorf("account %s storage key has length %d", accountHash, len(storage.Key))
				}
				if err := validateStorageRLP(storage.Value); err != nil {
					return StateResult{}, stateInventory{}, fmt.Errorf("account %s slot %x: %w", accountHash, storage.Key, err)
				}
				if err := storageStack.Update(storage.Key, storage.Value); err != nil {
					return StateResult{}, stateInventory{}, fmt.Errorf("rebuild storage trie for account %s: %w", accountHash, err)
				}
				slotHash := common.BytesToHash(storage.Key)
				if visitor != nil {
					if err := visitor.Storage(accountHash, slotHash, storage.Value); err != nil {
						return StateResult{}, stateInventory{}, err
					}
				}
				counts.StorageSlots++
				counts.Records++
				counts.PayloadBytes += uint64(len(storage.Value))
			}
			if storage.Err != nil {
				return StateResult{}, stateInventory{}, fmt.Errorf("iterate storage trie for account %s: %w", accountHash, storage.Err)
			}
		}
		computedStorageRoot := storageStack.Hash()
		if computedStorageRoot != account.Root {
			return StateResult{}, stateInventory{}, fmt.Errorf("account %s storage root mismatch: computed %s account %s", accountHash, computedStorageRoot, account.Root)
		}
		codeHash := common.BytesToHash(account.CodeHash)
		if codeHash != types.EmptyCodeHash {
			code := rawdb.ReadCode(disk, codeHash)
			if len(code) == 0 {
				return StateResult{}, stateInventory{}, fmt.Errorf("account %s code %s is missing", accountHash, codeHash)
			}
			if computed := crypto.Keccak256Hash(code); computed != codeHash {
				return StateResult{}, stateInventory{}, fmt.Errorf("account %s code hash mismatch: computed %s account %s", accountHash, computed, codeHash)
			}
			if visitor != nil {
				if err := visitor.Code(accountHash, codeHash, code); err != nil {
					return StateResult{}, stateInventory{}, err
				}
			}
			counts.CodeReferences++
			_, seen := seenCodes[codeHash]
			if !seen {
				counts.Records++
				counts.PayloadBytes += uint64(len(code))
				counts.CodeRecords++
			}
			seenCodes[codeHash] = struct{}{}
			if collectInventory {
				inventory.codeHashes[codeHash] = struct{}{}
			}
		}
		if err := accountStack.Update(accounts.Key, canonicalRLP); err != nil {
			return StateResult{}, stateInventory{}, fmt.Errorf("rebuild account trie: %w", err)
		}
	}
	if accounts.Err != nil {
		return StateResult{}, stateInventory{}, fmt.Errorf("iterate account trie: %w", accounts.Err)
	}
	computedRoot := accountStack.Hash()
	if computedRoot != root {
		return StateResult{}, stateInventory{}, fmt.Errorf("state root mismatch: computed %s header %s", computedRoot, root)
	}
	if collectInventory {
		inventory.CodeEntries = uint64(len(inventory.codeHashes))
		if inventory.scheme == rawdb.HashScheme {
			inventory.TrieNodes = uint64(len(inventory.hashNodes))
		}
		inventory.hashNodes = nil
		inventory.codeHashes = nil
	}
	return StateResult{Root: computedRoot, Counts: counts}, inventory, nil
}

type inventoryNodeIterator struct {
	trie.NodeIterator
	inventory *stateInventory
}

func (it *inventoryNodeIterator) Next(descend bool) bool {
	if !it.NodeIterator.Next(descend) {
		return false
	}
	if len(it.NodeBlob()) == 0 {
		return true
	}
	if it.inventory.scheme == rawdb.HashScheme {
		it.inventory.hashNodes[it.Hash()] = struct{}{}
	} else {
		it.inventory.TrieNodes++
	}
	return true
}

func decodeFullAccount(data []byte) (*types.StateAccount, []byte, error) {
	var account types.StateAccount
	if err := rlp.DecodeBytes(data, &account); err != nil {
		return nil, nil, fmt.Errorf("decode account RLP: %w", err)
	}
	if account.Balance == nil {
		return nil, nil, errors.New("account balance is nil")
	}
	if len(account.CodeHash) != common.HashLength {
		return nil, nil, fmt.Errorf("account code hash has length %d", len(account.CodeHash))
	}
	encoded, err := rlp.EncodeToBytes(&account)
	if err != nil {
		return nil, nil, fmt.Errorf("encode canonical account RLP: %w", err)
	}
	return &account, encoded, nil
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
