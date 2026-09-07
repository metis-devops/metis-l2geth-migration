package migration

import (
	"bytes"
	"math"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

type compatCodec struct {
	Input    hexutil.Bytes `json:"input"`
	Accepted bool          `json:"accepted"`
	Full     hexutil.Bytes `json:"full,omitempty"`
}

func captureCompatCodecs(t *testing.T, c *compatContract) {
	t.Helper()
	c.add(t, "constants", map[string]any{"empty_root": types.EmptyRootHash, "empty_code": types.EmptyCodeHash, "empty_uncles": types.EmptyUncleHash, "empty_transactions": types.EmptyTxsHash, "empty_receipts": types.EmptyReceiptsHash})
	for _, boundary := range []bool{false, true} {
		for _, storage := range []bool{false, true} {
			for _, code := range []bool{false, true} {
				account := types.NewEmptyStateAccount()
				if boundary {
					account.Nonce = math.MaxUint64
					account.Balance = new(uint256.Int).SetAllOne()
				}
				if storage {
					account.Root = common.HexToHash("0x1234")
				}
				if code {
					account.CodeHash = common.HexToHash("0x5678").Bytes()
				}
				slim, err := bundle.EncodeAccount(account)
				if err != nil {
					t.Fatal(err)
				}
				full, err := rlp.EncodeToBytes(account)
				if err != nil {
					t.Fatal(err)
				}
				_, decoded, err := bundle.DecodeAccount(slim)
				if err != nil || !bytes.Equal(decoded, full) {
					t.Fatalf("account round trip: %v", err)
				}
				name := "codec/account/" + boolName(boundary) + "/" + boolName(storage) + "/" + boolName(code)
				c.add(t, name, compatCodec{Input: slim, Accepted: true, Full: full})
			}
		}
	}
	slim := func(root, code []byte) []byte {
		encoded, err := rlp.EncodeToBytes(&types.SlimAccount{Balance: new(uint256.Int), Root: root, CodeHash: code})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	malformed := map[string][]byte{
		"empty": nil, "truncated": {0xc4, 0x80}, "noncanonical-integer": {0xc4, 0, 0x80, 0x80, 0x80},
		"trailing-value": append(slim(nil, nil), 0x80),
		"root-31":        slim(make([]byte, 31), nil), "root-33": slim(make([]byte, 33), nil),
		"code-31": slim(nil, make([]byte, 31)), "code-33": slim(nil, make([]byte, 33)),
		"explicit-empty-root": slim(types.EmptyRootHash.Bytes(), nil), "explicit-empty-code": slim(nil, types.EmptyCodeHash.Bytes()),
	}
	for name, input := range malformed {
		_, _, err := bundle.DecodeAccount(input)
		if err == nil {
			t.Fatalf("invalid account %s accepted; format bumps cannot bless invalid consensus data", name)
		}
		c.add(t, "codec/rejected/"+name, compatCodec{Input: input, Accepted: false})
	}
	// Storage RLP semantics include both single-byte and string encodings.
	for _, input := range [][]byte{{1}, {0x81, 0x80}, {0xa0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32}} {
		var value []byte
		if err := rlp.DecodeBytes(input, &value); err != nil {
			t.Fatal(err)
		}
		c.add(t, "codec/storage/"+hexutil.Encode(input), map[string]any{"input": hexutil.Bytes(input), "value": hexutil.Bytes(value)})
	}
}
func boolName(value bool) string {
	if value {
		return "set"
	}
	return "empty"
}
