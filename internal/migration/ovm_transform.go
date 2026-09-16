package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// OVMBalanceEvidence records nonzero holders and the conserved token amounts.
type OVMBalanceEvidence struct {
	OriginalSupply     *uint256.Int `json:"original_supply"`
	MigratedNative     *uint256.Int `json:"migrated_native"`
	RemainingSupply    *uint256.Int `json:"remaining_supply"`
	EOAs               uint64       `json:"eoa_holders"`
	ConvertedContracts uint64       `json:"converted_contract_holders"`
	RetainedContracts  uint64       `json:"retained_contract_holders"`
	SelfBalance        *uint256.Int `json:"self_balance"`
}

type ovmTransformer struct {
	ctx      context.Context
	db       ethdb.Database
	trieDB   *triedb.Database
	root     common.Hash
	index    *ovmIndex
	inputs   ovmInputs
	account  *types.StateAccount
	evidence OVMBalanceEvidence
	total    uint256.Int
	workers  int
	limiter  *migrateWorkLimiter
}

func readOVMAccount(db *triedb.Database, root, hash common.Hash) (*types.StateAccount, error) {
	t, err := trie.New(trie.StateTrieID(root), db)
	if err != nil {
		return nil, err
	}
	blob, err := t.Get(hash[:])
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return &types.StateAccount{Balance: new(uint256.Int), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()}, nil
	}
	return decodeFullAccount(hash, blob)
}

func ovmIterator(db *triedb.Database, id *trie.ID) (*trie.Iterator, error) {
	t, err := trie.New(id, db)
	if err != nil {
		return nil, err
	}
	nodes, err := t.NodeIterator(nil)
	if err != nil {
		return nil, err
	}
	return trie.NewIterator(nodes), nil
}

func decodeOVMValue(blob []byte) (*uint256.Int, error) {
	if len(blob) == 0 {
		return new(uint256.Int), nil
	}
	if err := validateStorageRLP(blob); err != nil {
		return nil, err
	}
	var value []byte
	if err := rlp.DecodeBytes(blob, &value); err != nil {
		return nil, err
	}
	return new(uint256.Int).SetBytes(value), nil
}

func (t *ovmTransformer) storageValue(slot common.Hash) (*uint256.Int, error) {
	owner := crypto.Keccak256Hash(ovmETHAddress[:])
	storage, err := trie.New(trie.StorageTrieID(t.root, owner, t.account.Root), t.trieDB)
	if err != nil {
		return nil, err
	}
	hash := ovmStorageHash(slot)
	blob, err := storage.Get(hash[:])
	if err != nil {
		return nil, err
	}
	return decodeOVMValue(blob)
}

func transformOVMState(ctx context.Context, db ethdb.Database, root common.Hash, index *ovmIndex, inputs ovmInputs, workers int, limiter *migrateWorkLimiter) (newRoot common.Hash, evidence OVMBalanceEvidence, retErr error) {
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	defer func() { retErr = errors.Join(retErr, tdb.Close()) }()
	t := ovmTransformer{ctx: ctx, db: db, trieDB: tdb, root: root, index: index, inputs: inputs, workers: workers, limiter: limiter}
	owner := crypto.Keccak256Hash(ovmETHAddress[:])
	account, err := readOVMAccount(tdb, root, owner)
	if err != nil {
		return newRoot, evidence, err
	}
	if common.BytesToHash(account.CodeHash) == types.EmptyCodeHash {
		return newRoot, evidence, errors.New("OVM_ETH contract is missing or has no code")
	}
	t.account = account
	supply, err := t.storageValue(common.HexToHash("0x02"))
	if err != nil {
		return newRoot, evidence, err
	}
	t.evidence = OVMBalanceEvidence{OriginalSupply: supply, MigratedNative: new(uint256.Int), RemainingSupply: new(uint256.Int), SelfBalance: new(uint256.Int)}
	if err := t.recognizeMetadata(); err != nil {
		return newRoot, evidence, err
	}
	if err := t.classifyStorage(); err != nil {
		return newRoot, evidence, err
	}
	if !t.total.Eq(supply) {
		return newRoot, evidence, fmt.Errorf("OVM totalSupply mismatch: balances sum %s, totalSupply %s", &t.total, supply)
	}
	if _, underflow := new(uint256.Int).SubOverflow(supply, t.evidence.MigratedNative); underflow {
		return newRoot, evidence, errors.New("OVM supply subtraction underflow")
	}
	if !new(uint256.Int).Sub(supply, t.evidence.MigratedNative).Eq(t.evidence.RemainingSupply) {
		return newRoot, evidence, errors.New("OVM balance conservation mismatch")
	}
	if err := t.index.flush(); err != nil {
		return newRoot, evidence, err
	}
	storageRoot, err := t.rebuildStorage(owner)
	if err != nil {
		return newRoot, evidence, err
	}
	t.account.Root = storageRoot
	t.account.Balance.Set(t.evidence.RemainingSupply)
	codeHash := crypto.Keccak256Hash(inputs.code)
	t.account.CodeHash = codeHash.Bytes()
	writer := newDirectStateWriter(db, "hash")
	defer writer.Abort()
	if err := writer.Code(owner, codeHash, inputs.code); err != nil {
		return newRoot, evidence, err
	}
	if err := writer.CloseContext(ctx); err != nil {
		return newRoot, evidence, err
	}
	if err := t.patchAccount(owner, t.account); err != nil {
		return newRoot, evidence, err
	}
	if err := index.flush(); err != nil {
		return newRoot, evidence, err
	}
	newRoot, err = t.rebuildAccounts()
	return newRoot, t.evidence, err
}

func (t *ovmTransformer) recognizeMetadata() error {
	for n := uint64(0); n <= 6; n++ {
		slot := common.BigToHash(new(big.Int).SetUint64(n))
		hash := ovmStorageHash(slot)
		if n < 2 {
			// Mapping base slots cannot contain ordinary storage values.
			v, err := t.storageValue(slot)
			if err != nil {
				return err
			}
			if !v.IsZero() {
				return errors.New("nonzero OVM mapping base slot")
			}
			continue
		}
		if err := t.index.put('k', hash[:], []byte{byte(n)}); err != nil {
			return err
		}
		if n == 3 || n == 4 {
			if err := t.recognizeString(slot); err != nil {
				return err
			}
		}
	}
	return t.index.flush()
}

func (t *ovmTransformer) recognizeString(slot common.Hash) error {
	v, err := t.storageValue(slot)
	if err != nil {
		return err
	}
	word := v.Bytes32()
	if word[31]&1 == 0 {
		length := int(word[31] / 2)
		if length > 31 {
			return errors.New("invalid short OVM string length")
		}
		for _, b := range word[length:31] {
			if b != 0 {
				return errors.New("noncanonical short OVM string padding")
			}
		}
		return nil
	}
	length := new(uint256.Int).Rsh(v, 1)
	if !length.IsUint64() || length.Uint64() < 32 {
		return errors.New("invalid long OVM string length")
	}
	chunks := (length.Uint64()-1)/32 + 1
	base := new(uint256.Int).SetBytes(crypto.Keccak256(slot[:]))
	for n := range chunks {
		if err := t.ctx.Err(); err != nil {
			return err
		}
		key := new(uint256.Int).Add(base, new(uint256.Int).SetUint64(n)).Bytes32()
		hash := crypto.Keccak256Hash(key[:])
		if err := t.index.put('k', hash[:], []byte{slot[31]}); err != nil {
			return err
		}
	}
	return nil
}

func (t *ovmTransformer) classifyStorage() (retErr error) {
	ctx, cancel := context.WithCancel(t.ctx)
	batch := ovmBalanceBatch{ctx: ctx, transformer: t, cancel: cancel}
	defer func() { cancel(); batch.jobs.Wait(); retErr = errors.Join(retErr, batch.failure.load()) }()
	owner := crypto.Keccak256Hash(ovmETHAddress[:])
	it, err := ovmIterator(t.trieDB, trie.StorageTrieID(t.root, owner, t.account.Root))
	if err != nil {
		return err
	}
	lease := newMigrateWorkLease(t.limiter)
	defer lease.release()
	for {
		if err := lease.acquire(ctx); err != nil {
			return err
		}
		if !it.Next() {
			lease.release()
			break
		}
		value, err := decodeOVMValue(it.Value)
		if err != nil {
			return err
		}
		if len(it.Key) != 32 {
			return errors.New("invalid OVM storage key length")
		}
		if err := t.classifySlot(it.Key, value, &batch); err != nil {
			return fmt.Errorf("OVM storage %x: %w", it.Key, err)
		}
		lease.release()
	}
	if it.Err != nil {
		return it.Err
	}
	return batch.drain()
}

func (t *ovmTransformer) classifySlot(key []byte, value *uint256.Int, batch *ovmBalanceBatch) error {
	var found int
	var address []byte
	for _, prefix := range []byte{'b', 'a', 'k'} {
		v, ok, err := t.index.get(prefix, key)
		if err != nil {
			return err
		}
		if ok {
			found++
			if prefix == 'b' {
				address = v
			}
		}
	}
	if found != 1 {
		return errors.New("storage ownership is unknown or ambiguous; supplement --ovm-state-witness")
	}
	if address == nil {
		return nil
	}
	if len(address) != 20 {
		return errors.New("invalid indexed balance address")
	}
	return batch.submit(common.Address(address), common.Hash(key), value)
}

func (t *ovmTransformer) applyBalance(b *ovmBalanceJob) error {
	value := &b.value
	if _, overflow := t.total.AddOverflow(&t.total, value); overflow {
		return errors.New("OVM balances sum overflows uint256")
	}
	if value.IsZero() {
		return nil
	}
	if b.address == ovmETHAddress {
		t.evidence.SelfBalance.Set(value)
		t.evidence.RemainingSupply.Add(t.evidence.RemainingSupply, value)
		return nil
	}
	if b.contract && b.retain {
		t.evidence.RetainedContracts++
		t.evidence.RemainingSupply.Add(t.evidence.RemainingSupply, value)
		return nil
	}
	if b.contract {
		t.evidence.ConvertedContracts++
	} else {
		t.evidence.EOAs++
	}
	t.evidence.MigratedNative.Add(t.evidence.MigratedNative, value)
	b.account.Balance.Set(value)
	if err := t.patchAccount(b.hash, b.account); err != nil {
		return err
	}
	return t.index.put('s', b.slot[:], nil)
}

func (t *ovmTransformer) patchAccount(hash common.Hash, account *types.StateAccount) error {
	blob, err := rlp.EncodeToBytes(account)
	if err != nil {
		return err
	}
	return t.index.put('p', hash[:], blob)
}

func (t *ovmTransformer) rebuildStorage(owner common.Hash) (common.Hash, error) {
	supplyHash := ovmStorageHash(common.HexToHash("0x02"))
	var supply []byte
	var err error
	if !t.evidence.RemainingSupply.IsZero() {
		supply, err = rlp.EncodeToBytes(t.evidence.RemainingSupply.Bytes())
		if err != nil {
			return common.Hash{}, err
		}
	}
	if err := t.index.put('s', supplyHash[:], supply); err != nil {
		return common.Hash{}, err
	}
	if err := t.index.flush(); err != nil {
		return common.Hash{}, err
	}
	it, err := ovmIterator(t.trieDB, trie.StorageTrieID(t.root, owner, t.account.Root))
	if err != nil {
		return common.Hash{}, err
	}
	return t.rebuildPatchedTrie(owner, it, 's')
}

func (t *ovmTransformer) rebuildAccounts() (common.Hash, error) {
	it, err := ovmIterator(t.trieDB, trie.StateTrieID(t.root))
	if err != nil {
		return common.Hash{}, err
	}
	return t.rebuildPatchedTrie(common.Hash{}, it, 'p')
}

func (t *ovmTransformer) rebuildPatchedTrie(owner common.Hash, original *trie.Iterator, prefix byte) (common.Hash, error) {
	return t.rebuildPatchedTriePrefix(owner, original, []byte{prefix})
}

func (t *ovmTransformer) rebuildPatchedTriePrefix(owner common.Hash, original *trie.Iterator, prefix []byte) (common.Hash, error) {
	patches := t.index.db.NewIterator(prefix, nil)
	defer patches.Release()
	writer := newDirectStateWriter(t.db, "hash")
	defer writer.Abort()
	var writeErr error
	stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		if writeErr == nil {
			writeErr = writer.TrieNode(owner, path, hash, blob)
		}
	})
	a, b := original.Next(), patches.Next()
	for a || b {
		if err := t.ctx.Err(); err != nil {
			return common.Hash{}, err
		}
		var key, value []byte
		usePatch := b && (!a || bytes.Compare(patches.Key()[len(prefix):], original.Key) <= 0)
		if usePatch {
			key, value = patches.Key()[len(prefix):], patches.Value()
		} else {
			key, value = original.Key, original.Value
		}
		if len(value) != 0 {
			if err := stack.Update(key, value); err != nil {
				return common.Hash{}, err
			}
			if writeErr != nil {
				return common.Hash{}, writeErr
			}
		}
		if usePatch {
			if a && bytes.Equal(key, original.Key) {
				a = original.Next()
			}
			b = patches.Next()
		} else {
			a = original.Next()
		}
	}
	if err := errors.Join(original.Err, patches.Error()); err != nil {
		return common.Hash{}, err
	}
	root := stack.Hash()
	if writeErr != nil {
		return common.Hash{}, writeErr
	}
	if err := writer.CloseContext(t.ctx); err != nil {
		return common.Hash{}, err
	}
	return root, nil
}
