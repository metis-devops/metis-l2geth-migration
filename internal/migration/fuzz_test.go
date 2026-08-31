package migration

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func FuzzSemanticRecords(f *testing.F) {
	account, err := bundle.EncodeAccount(types.NewEmptyStateAccount())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(bundle.RecordAccount, account)
	f.Add(bundle.RecordStorage, []byte{1})
	f.Add(bundle.RecordCode, []byte{0x60})
	f.Fuzz(func(t *testing.T, typ byte, payload []byte) {
		if len(payload) > 1024 {
			return
		}
		consumer := newRecordConsumer(types.EmptyRootHash, nil, nil)
		record := bundle.Record{
			Type: typ, AccountHash: common.HexToHash("0x01"), SubHash: common.HexToHash("0x02"), Payload: payload,
		}
		if err := consumer.consume(record); err == nil {
			_, _ = consumer.finish()
		}
	})
}
