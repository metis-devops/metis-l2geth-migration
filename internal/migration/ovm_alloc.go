package migration

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

func applyOVMGenesisAlloc(ctx context.Context, db ethdb.Database, root common.Hash, index *ovmIndex) (newRoot common.Hash, retErr error) {
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	defer func() { retErr = errors.Join(retErr, tdb.Close()) }()
	t := ovmTransformer{ctx: ctx, db: db, trieDB: tdb, root: root, index: index}
	accounts := index.db.NewIterator([]byte{ovmAllocAccountPrefix}, nil)
	defer accounts.Release()
	writer := newDirectStateWriter(db, "hash")
	defer writer.Abort()
	for accounts.Next() {
		if err := ctx.Err(); err != nil {
			return newRoot, err
		}
		if len(accounts.Key()) != 33 {
			return newRoot, errors.New("invalid GenesisAlloc account index key")
		}
		owner := common.BytesToHash(accounts.Key()[1:])
		var patch ovmAllocAccount
		if err := rlp.DecodeBytes(accounts.Value(), &patch); err != nil {
			return newRoot, fmt.Errorf("decode GenesisAlloc account patch: %w", err)
		}
		if err := t.applyAllocAccount(owner, &patch, writer); err != nil {
			return newRoot, fmt.Errorf("apply GenesisAlloc account %s: %w", owner, err)
		}
	}
	if err := accounts.Error(); err != nil {
		return newRoot, err
	}
	if err := writer.CloseContext(ctx); err != nil {
		return newRoot, err
	}
	if err := index.flush(); err != nil {
		return newRoot, err
	}
	it, err := ovmIterator(tdb, trie.StateTrieID(root))
	if err != nil {
		return newRoot, err
	}
	return t.rebuildPatchedTrie(common.Hash{}, it, ovmAllocPatchedPrefix)
}

func (t *ovmTransformer) applyAllocAccount(owner common.Hash, patch *ovmAllocAccount, writer *directStateWriter) error {
	if patch.Fields == 0 {
		return nil
	}
	account, err := readOVMAccount(t.trieDB, t.root, owner)
	if err != nil {
		return err
	}
	if patch.Fields&ovmAllocBalanceField != 0 {
		account.Balance.SetBytes(patch.Balance)
	}
	if patch.Fields&ovmAllocNonceField != 0 {
		account.Nonce = patch.Nonce
	}
	if patch.Fields&ovmAllocCodeField != 0 {
		hash := crypto.Keccak256Hash(patch.Code)
		account.CodeHash = hash.Bytes()
		if len(patch.Code) != 0 {
			if err := writer.Code(owner, hash, patch.Code); err != nil {
				return err
			}
		}
	}
	if patch.Fields&ovmAllocStorageField != 0 {
		it, err := ovmIterator(t.trieDB, trie.StorageTrieID(t.root, owner, account.Root))
		if err != nil {
			return err
		}
		prefix := prefixedKey([]byte{ovmAllocStoragePrefix}, owner[:])
		account.Root, err = t.rebuildPatchedTriePrefix(owner, it, prefix)
		if err != nil {
			return err
		}
	}
	blob, err := rlp.EncodeToBytes(account)
	if err != nil {
		return err
	}
	return t.index.put(ovmAllocPatchedPrefix, owner[:], blob)
}

func (w *ovmWork) applyGenesisAlloc(root common.Hash) (newRoot common.Hash, evidence *OVMGenesisAllocEvidence, retErr error) {
	if w.opts.OVM.GenesisAlloc == "" {
		return root, nil, nil
	}
	phase := w.reporter.StartPhase("apply_genesis_alloc", nil)
	defer func() { phase.Finish(retErr) }()
	converted, err := runOVMPartitioned(w.ctx, w.base, nil, root, "hash", w.opts.Workers, w.limiter, targetConfig{}.readCode, false)
	if err != nil {
		return newRoot, nil, fmt.Errorf("validate converted state before GenesisAlloc: %w", err)
	}
	newRoot, err = applyOVMGenesisAlloc(w.ctx, w.base, root, w.index)
	if err != nil {
		return newRoot, nil, err
	}
	return newRoot, &OVMGenesisAllocEvidence{FileSHA256: w.inputs.allocDigest, Converted: OVMStateEvidence(converted)}, nil
}
