package bundle

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// EncodeAccount encodes an account using geth's canonical slim-account
// representation. Empty storage roots and empty code hashes are omitted.
func EncodeAccount(account *types.StateAccount) ([]byte, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if account.Balance == nil {
		return nil, errors.New("account balance is nil")
	}
	if len(account.CodeHash) != common.HashLength {
		return nil, fmt.Errorf("account code hash has length %d", len(account.CodeHash))
	}
	slim := types.SlimAccount{
		Nonce:   account.Nonce,
		Balance: account.Balance,
	}
	if account.Root != types.EmptyRootHash {
		slim.Root = account.Root[:]
	}
	if !bytes.Equal(account.CodeHash, types.EmptyCodeHash[:]) {
		slim.CodeHash = account.CodeHash
	}
	data, err := rlp.EncodeToBytes(&slim)
	if err != nil {
		return nil, fmt.Errorf("encode slim account RLP: %w", err)
	}
	return data, nil
}

// DecodeAccount strictly decodes a canonical slim-account payload and returns
// both the expanded account and its full consensus RLP representation.
func DecodeAccount(data []byte) (*types.StateAccount, []byte, error) {
	var slim types.SlimAccount
	// DecodeBytes rejects non-canonical sizes and integers as well as trailing
	// values. Field checks below enforce the additional v3 slim contract.
	if err := rlp.DecodeBytes(data, &slim); err != nil {
		return nil, nil, fmt.Errorf("decode slim account RLP: %w", err)
	}
	if slim.Balance == nil {
		return nil, nil, errors.New("account balance is nil")
	}
	if len(slim.Root) != 0 && len(slim.Root) != common.HashLength {
		return nil, nil, fmt.Errorf("account storage root has length %d", len(slim.Root))
	}
	if len(slim.CodeHash) != 0 && len(slim.CodeHash) != common.HashLength {
		return nil, nil, fmt.Errorf("account code hash has length %d", len(slim.CodeHash))
	}
	if bytes.Equal(slim.Root, types.EmptyRootHash[:]) {
		return nil, nil, errors.New("account explicitly encodes the empty storage root")
	}
	if bytes.Equal(slim.CodeHash, types.EmptyCodeHash[:]) {
		return nil, nil, errors.New("account explicitly encodes the empty code hash")
	}

	account := &types.StateAccount{
		Nonce:    slim.Nonce,
		Balance:  slim.Balance,
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash.Bytes(),
	}
	if len(slim.Root) != 0 {
		account.Root = common.BytesToHash(slim.Root)
	}
	if len(slim.CodeHash) != 0 {
		account.CodeHash = common.CopyBytes(slim.CodeHash)
	}
	fullRLP, err := rlp.EncodeToBytes(account)
	if err != nil {
		return nil, nil, fmt.Errorf("encode full account RLP: %w", err)
	}
	return account, fullRLP, nil
}
