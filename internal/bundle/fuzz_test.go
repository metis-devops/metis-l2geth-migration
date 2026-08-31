package bundle

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

func FuzzDecodeAccount(f *testing.F) {
	for _, account := range []*types.StateAccount{
		types.NewEmptyStateAccount(),
		{Nonce: 7, Balance: uint256.NewInt(11), Root: common.HexToHash("0x01"), CodeHash: common.HexToHash("0x02").Bytes()},
	} {
		payload, err := EncodeAccount(account)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(payload)
	}
	f.Add([]byte{0x80})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxAccountPayload {
			return
		}
		account, _, err := DecodeAccount(data)
		if err != nil {
			return
		}
		canonical, err := EncodeAccount(account)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(canonical, data) {
			t.Fatalf("accepted non-canonical slim account: have %x canonical %x", data, canonical)
		}
	})
}

func FuzzRecordFraming(f *testing.F) {
	f.Add(RecordAccount, uint64(5), make([]byte, common.HashLength))
	f.Add(RecordStorage, uint64(1), make([]byte, 2*common.HashLength))
	f.Add(RecordCode, uint64(1), make([]byte, common.HashLength))
	f.Fuzz(func(t *testing.T, typ byte, length uint64, key []byte) {
		keyLength, err := recordKeyLength(typ)
		if err != nil || validatePayloadLength(typ, length) != nil || len(key) != keyLength {
			return
		}
		header := make([]byte, 1+keyLength+8)
		header[0] = typ
		copy(header[1:], key)
		binary.BigEndian.PutUint64(header[len(header)-8:], length)
		record := recordFromHeader(typ, header, []byte{1})
		if record.Type != typ {
			t.Fatalf("record type changed from %d to %d", typ, record.Type)
		}
	})
}
