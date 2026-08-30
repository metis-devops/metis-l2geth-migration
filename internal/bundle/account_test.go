package bundle

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

func TestAccountCodecRoundTrip(t *testing.T) {
	t.Parallel()

	nonEmptyRoot := common.HexToHash("0x1234")
	nonEmptyCodeHash := common.HexToHash("0x5678")
	tests := []struct {
		name     string
		root     common.Hash
		codeHash common.Hash
	}{
		{name: "empty root and code", root: types.EmptyRootHash, codeHash: types.EmptyCodeHash},
		{name: "non-empty root", root: nonEmptyRoot, codeHash: types.EmptyCodeHash},
		{name: "non-empty code", root: types.EmptyRootHash, codeHash: nonEmptyCodeHash},
		{name: "non-empty root and code", root: nonEmptyRoot, codeHash: nonEmptyCodeHash},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			account := &types.StateAccount{
				Nonce:    7,
				Balance:  uint256.NewInt(11),
				Root:     test.root,
				CodeHash: test.codeHash.Bytes(),
			}
			fullRLP, err := rlp.EncodeToBytes(account)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := EncodeAccount(account)
			if err != nil {
				t.Fatal(err)
			}
			if want := types.SlimAccountRLP(*account); !bytes.Equal(payload, want) {
				t.Fatalf("slim payload %x, want geth encoding %x", payload, want)
			}
			decoded, expandedRLP, err := DecodeAccount(payload)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Nonce != account.Nonce || decoded.Balance.Cmp(account.Balance) != 0 ||
				decoded.Root != account.Root || !bytes.Equal(decoded.CodeHash, account.CodeHash) {
				t.Fatalf("decoded account %+v, want %+v", decoded, account)
			}
			if !bytes.Equal(expandedRLP, fullRLP) {
				t.Fatalf("expanded RLP %x, want %x", expandedRLP, fullRLP)
			}
			if (test.root == types.EmptyRootHash || test.codeHash == types.EmptyCodeHash) && len(payload) >= len(fullRLP) {
				t.Fatalf("slim payload length %d did not reduce full length %d", len(payload), len(fullRLP))
			}
		})
	}
}

func TestEncodeAccountRejectsInvalidAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		account *types.StateAccount
		wantErr string
	}{
		{name: "nil", wantErr: "account is nil"},
		{name: "nil balance", account: &types.StateAccount{CodeHash: types.EmptyCodeHash.Bytes()}, wantErr: "balance is nil"},
		{name: "short code hash", account: &types.StateAccount{Balance: new(uint256.Int), CodeHash: make([]byte, common.HashLength-1)}, wantErr: "length 31"},
		{name: "long code hash", account: &types.StateAccount{Balance: new(uint256.Int), CodeHash: make([]byte, common.HashLength+1)}, wantErr: "length 33"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := EncodeAccount(test.account)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("encode error is %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeAccountRejectsInvalidSlimPayload(t *testing.T) {
	t.Parallel()

	encodeSlim := func(t *testing.T, slim types.SlimAccount) []byte {
		t.Helper()
		data, err := rlp.EncodeToBytes(&slim)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	valid := encodeSlim(t, types.SlimAccount{Balance: new(uint256.Int)})
	nonCanonicalNonce := bytes.Clone(valid)
	nonCanonicalNonce[1] = 0
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{name: "short root", data: encodeSlim(t, types.SlimAccount{Balance: new(uint256.Int), Root: make([]byte, common.HashLength-1)}), wantErr: "storage root has length 31"},
		{name: "long root", data: encodeSlim(t, types.SlimAccount{Balance: new(uint256.Int), Root: make([]byte, common.HashLength+1)}), wantErr: "storage root has length 33"},
		{name: "short code hash", data: encodeSlim(t, types.SlimAccount{Balance: new(uint256.Int), CodeHash: make([]byte, common.HashLength-1)}), wantErr: "code hash has length 31"},
		{name: "long code hash", data: encodeSlim(t, types.SlimAccount{Balance: new(uint256.Int), CodeHash: make([]byte, common.HashLength+1)}), wantErr: "code hash has length 33"},
		{name: "explicit empty root", data: encodeSlim(t, types.SlimAccount{Balance: new(uint256.Int), Root: types.EmptyRootHash.Bytes()}), wantErr: "explicitly encodes the empty storage root"},
		{name: "explicit empty code hash", data: encodeSlim(t, types.SlimAccount{Balance: new(uint256.Int), CodeHash: types.EmptyCodeHash.Bytes()}), wantErr: "explicitly encodes the empty code hash"},
		{name: "non-canonical integer", data: nonCanonicalNonce, wantErr: "non-canonical integer"},
		{name: "trailing value", data: append(bytes.Clone(valid), rlp.EmptyString...), wantErr: "more than one value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := DecodeAccount(test.data)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("decode error is %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}
