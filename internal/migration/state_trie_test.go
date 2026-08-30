package migration

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

func TestDecodeFullAccountRejectsNonCanonicalRLP(t *testing.T) {
	t.Parallel()

	accountRLP, err := rlp.EncodeToBytes(types.NewEmptyStateAccount())
	if err != nil {
		t.Fatal(err)
	}
	if len(accountRLP) != 70 || !bytes.Equal(accountRLP[:5], []byte{0xf8, 0x44, 0x80, 0x80, 0xa0}) || accountRLP[37] != 0xa0 {
		t.Fatalf("unexpected empty account encoding: %x", accountRLP)
	}

	nonceLeadingZero := bytes.Clone(accountRLP)
	nonceLeadingZero[2] = 0x00
	balanceLeadingZero := bytes.Clone(accountRLP)
	balanceLeadingZero[3] = 0x00
	nonCanonicalListSize := append([]byte{0xf9, 0x00, accountRLP[1]}, accountRLP[2:]...)
	nonCanonicalCodeSize := make([]byte, 0, len(accountRLP)+1)
	nonCanonicalCodeSize = append(nonCanonicalCodeSize, 0xf8, accountRLP[1]+1)
	nonCanonicalCodeSize = append(nonCanonicalCodeSize, accountRLP[2:37]...)
	nonCanonicalCodeSize = append(nonCanonicalCodeSize, 0xb8, 0x20)
	nonCanonicalCodeSize = append(nonCanonicalCodeSize, accountRLP[38:]...)
	trailingValue := append(bytes.Clone(accountRLP), rlp.EmptyString...)

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{name: "nonce leading zero", data: nonceLeadingZero, wantErr: "non-canonical integer"},
		{name: "balance leading zero", data: balanceLeadingZero, wantErr: "non-canonical integer"},
		{name: "list size with leading zero", data: nonCanonicalListSize, wantErr: "non-canonical size information"},
		{name: "long-form code hash size", data: nonCanonicalCodeSize, wantErr: "non-canonical size information"},
		{name: "trailing value", data: trailingValue, wantErr: "more than one value"},
	}
	accountHash := common.HexToHash("0x1234")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeFullAccount(accountHash, test.data)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("decode error is %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeFullAccountRejectsInvalidCodeHashLength(t *testing.T) {
	t.Parallel()

	accountHash := common.HexToHash("0x1234")
	for _, length := range []int{0, common.HashLength - 1, common.HashLength + 1} {
		t.Run(fmt.Sprintf("length %d", length), func(t *testing.T) {
			t.Parallel()
			account := types.NewEmptyStateAccount()
			account.CodeHash = make([]byte, length)
			accountRLP, err := rlp.EncodeToBytes(account)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeFullAccount(accountHash, accountRLP)
			wantErr := fmt.Sprintf("account code hash has length %d", length)
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("decode error is %v, want it to contain %q", err, wantErr)
			}
		})
	}
}
