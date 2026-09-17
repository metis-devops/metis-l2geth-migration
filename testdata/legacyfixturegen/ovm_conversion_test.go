package main

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/MetisProtocol/mvm/l2geth/common"
	"github.com/MetisProtocol/mvm/l2geth/common/hexutil"
	"github.com/MetisProtocol/mvm/l2geth/core/rawdb"
	"github.com/MetisProtocol/mvm/l2geth/core/state"
	"github.com/MetisProtocol/mvm/l2geth/core/types"
	"github.com/MetisProtocol/mvm/l2geth/rollup/rcfg"
)

// The independent pinned legacy module validates the new logical source fixture.
// It never regenerates either this corpus or the original production canary.
func TestOVMConversionSourceWithLegacyAPIs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "migration", "testdata", "ovm-conversion", "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]string
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	for k, v := range entries {
		key, err := hexutil.Decode(k)
		if err != nil {
			t.Fatal(err)
		}
		value, err := hexutil.Decode(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	hash := rawdb.ReadHeadBlockHash(db)
	head := rawdb.ReadHeader(db, hash, 1)
	if head == nil {
		t.Fatal("legacy reader cannot read head")
	}
	receipts := rawdb.ReadRawReceipts(db, hash, 1)
	if len(receipts) != 1 || len(receipts[0].Logs) != 3 || types.DeriveSha(receipts) != head.ReceiptHash {
		t.Fatal("legacy receipt root/logs mismatch")
	}
	previous := rcfg.UsingOVM
	rcfg.UsingOVM = true
	defer func() { rcfg.UsingOVM = previous }()
	s, err := state.New(head.Root, state.NewDatabase(db))
	if err != nil {
		t.Fatal(err)
	}
	for n, amount := range []int64{11, 22, 33, 44, 55, 66} {
		address := common.BigToAddress(big.NewInt(int64(1001 + n)))
		if s.GetBalance(address).Cmp(big.NewInt(amount)) != 0 {
			t.Fatalf("legacy balance %s: %s", address, s.GetBalance(address))
		}
	}
	ovm := common.HexToAddress("0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000")
	if s.GetState(ovm, common.HexToHash("0x02")).Big().Cmp(big.NewInt(308)) != 0 || s.GetBalance(ovm).Cmp(big.NewInt(77)) != 0 {
		t.Fatal("legacy supply/self balance mismatch")
	}
}
