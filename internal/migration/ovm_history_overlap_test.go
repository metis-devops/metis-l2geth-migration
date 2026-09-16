package migration

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/golang/snappy"
)

// Retain hot copies to model the interval between freezing and hot deletion.
func writeOVMFixtureAncients(t *testing.T, f ovmFixture) {
	t.Helper()
	editOVMSource(t, f, func(db ethdb.Database) {
		path := filepath.Join(f.source, "ancient")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		for table, name := range []string{"hashes", "headers", "receipts"} {
			var data []byte
			index := make([]byte, 18)
			for number := range uint64(2) {
				hash := rawdb.ReadCanonicalHash(db, number)
				var blob []byte
				switch table {
				case 0:
					blob = hash[:]
				case 1:
					blob = rawdb.ReadHeaderRLP(db, hash, number)
				case 2:
					header := rawdb.ReadHeader(db, hash, number)
					if header == nil {
						t.Fatal("fixture header missing")
					}
					var err error
					blob, err = db.Get(ovmReceiptKey(header))
					if err != nil {
						t.Fatal(err)
					}
				}
				if table != 0 {
					blob = snappy.Encode(nil, blob)
				}
				data = append(data, blob...)
				binary.BigEndian.PutUint32(index[(number+1)*6+2:], uint32(len(data)))
			}
			indexExt, dataExt := ".ridx", ".rdat"
			if table != 0 {
				indexExt, dataExt = ".cidx", ".cdat"
			}
			if err := os.WriteFile(filepath.Join(path, name+indexExt), index, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, name+".0000"+dataExt), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestOVMAncientsPastExecutedHead(t *testing.T) {
	for _, target := range targetTestCases() {
		t.Run(target.name(), func(t *testing.T) {
			f := newOVMFixture(t, nil)
			// Genesis has no Approval event, so its nonzero allowance needs a witness.
			witness, err := os.ReadFile(f.witness)
			if err != nil {
				t.Fatal(err)
			}
			witness = append(witness, fmt.Appendf(nil, "{\"type\":\"allowance\",\"owner\":%q,\"spender\":%q}\n", f.holders[1].Hex(), f.holders[3].Hex())...)
			if err := os.WriteFile(f.witness, witness, 0600); err != nil {
				t.Fatal(err)
			}
			editOVMSource(t, f, func(db ethdb.Database) {
				rawdb.WriteHeadBlockHash(db, rawdb.ReadCanonicalHash(db, 0))
				rawdb.WriteHeadFastBlockHash(db, f.head.Hash())
			})
			baselineOptions := f.options(t, target.engine, target.scheme, 2)
			baseline, err := Migrate(t.Context(), baselineOptions)
			if err != nil {
				t.Fatal(err)
			}
			writeOVMFixtureAncients(t, f)
			before := directoryContentDigest(t, f.source)
			converted, err := Migrate(t.Context(), f.options(t, target.engine, target.scheme, 4))
			if err != nil {
				t.Fatal(err)
			}
			got, want := converted.OVMReport, baseline.OVMReport
			if got.Target != want.Target || got.History != want.History {
				t.Fatal("freezing later blocks changed selected state or history")
			}
			if got.Source.HeadBefore.BlockNumber != 0 || got.History.Blocks != 1 || got.History.Transfers != 0 || got.Balances.RemainingSupply.Uint64() != 77 {
				t.Fatal("history past LastBlock influenced retention eligibility")
			}
			if _, err := VerifyOVM(t.Context(), OVMVerifyOptions{SourceChaindata: f.source, Artifact: baselineOptions.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: baselineOptions.OVM}); err != nil {
				t.Fatal(err)
			}
			if after := directoryContentDigest(t, f.source); after != before {
				t.Fatal("history scan modified source or ancient files")
			}
		})
	}
}

func ovmOverlapLogs(f ovmFixture) []*types.Log {
	return []*types.Log{
		ovmTestEvent(ovmTransferTopic, f.holders[1], f.holders[3], 1),
		ovmTestEvent(ovmTransferTopic, f.holders[0], f.holders[3], 1),
		ovmTestEvent(ovmApprovalTopic, f.holders[1], f.holders[3], 0x1234),
	}
}

func encodeOVMOverlapReceipts(t *testing.T, f ovmFixture, format string) []byte {
	t.Helper()
	logs := ovmOverlapLogs(f)
	var fields []any
	switch format {
	case "v3":
		bloom := types.CreateBloom(&types.Receipt{Logs: logs})
		fields = []any{[]byte{1}, uint64(21000), bloom, common.Hash{}, common.Address{}, logs, uint64(21000)}
	case "v4":
		fields = []any{[]byte{1}, uint64(21000), common.Hash{}, common.Address{}, logs, uint64(21000)}
	case "fee-metadata":
		fields = []any{[]byte{1}, uint64(21000), logs, big.NewInt(1), big.NewInt(2), big.NewInt(3), "1.25"}
	default:
		t.Fatalf("unknown test receipt format %s", format)
	}
	blob, err := rlp.EncodeToBytes([]any{fields})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func TestOVMEquivalentHotColdReceipts(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMEquivalentHotColdReceipts(t, mode) })
	}
}

func testOVMEquivalentHotColdReceipts(t *testing.T, tempMode TempDBMode) {
	for _, target := range targetTestCases() {
		for _, format := range []string{"v3", "v4", "fee-metadata"} {
			t.Run(target.name()+"/"+format, func(t *testing.T) {
				f := newOVMFixture(t, nil)
				writeOVMFixtureAncients(t, f)
				opts := f.options(t, target.engine, target.scheme, 2)
				opts.TempDB = tempMode
				baseline, err := Migrate(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				alternate := encodeOVMOverlapReceipts(t, f, format)
				editOVMSource(t, f, func(db ethdb.Database) { putOVMTest(t, db, ovmReceiptKey(f.head), alternate) })
				before := directoryContentDigest(t, f.source)
				converted, err := Migrate(t.Context(), f.options(t, target.engine, target.scheme, 4))
				if err != nil {
					t.Fatal(err)
				}
				if converted.OVMReport.History != baseline.OVMReport.History || converted.OVMReport.Target != baseline.OVMReport.Target {
					t.Fatal("equivalent hot receipt encoding changed selected cold evidence or target")
				}
				if _, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDB: tempMode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}); err != nil {
					t.Fatal(err)
				}
				if after := directoryContentDigest(t, f.source); after != before {
					t.Fatal("equivalent receipts were rewritten in source")
				}
			})
		}
	}
}

func TestOVMReceiptOverlapRejectsCorruption(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMReceiptOverlapRejectsCorruption(t, mode) })
	}
}

func testOVMReceiptOverlapRejectsCorruption(t *testing.T, tempMode TempDBMode) {
	f := newOVMFixture(t, nil)
	logs := ovmOverlapLogs(f)
	logs[0].Topics[1] = common.BytesToHash(f.holders[2][:])
	changed := encodeOVMTestReceipts(t, logs)
	for _, tc := range []struct {
		name      string
		cold, hot []byte
	}{
		{"hot-root", f.receipts, changed},
		{"cold-root", changed, f.receipts},
		{"hot-trailing", f.receipts, append(append([]byte(nil), f.receipts...), 0)},
		{"cold-trailing", append(append([]byte(nil), f.receipts...), 0), f.receipts},
		{"hot-malformed", f.receipts, []byte{0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := ovmHistoryBlock{header: *f.head, raw: tc.cold, alternateReceipts: tc.hot}
			if err := block.decode(t.Context()); err == nil {
				t.Fatal("corrupt or conflicting receipt copy accepted")
			}
		})
	}
	for _, format := range []string{"v3", "v4"} {
		block := ovmHistoryBlock{header: *f.head, raw: encodeOVMOverlapReceipts(t, f, format), alternateReceipts: f.receipts}
		if err := block.decode(t.Context()); err != nil {
			t.Fatalf("old cold/current hot: %v", err)
		}
	}
	writeOVMFixtureAncients(t, f)
	editOVMSource(t, f, func(db ethdb.Database) { putOVMTest(t, db, ovmReceiptKey(f.head), nil) })
	opts := f.options(t, "pebble", "hash", 2)
	opts.TempDB = tempMode
	if _, err := Migrate(t.Context(), opts); err == nil {
		t.Fatal("empty hot receipt was mistaken for a missing copy")
	}
	if _, err := mergeLegacyHistoryValue([]byte{1}, []byte{2}); err == nil {
		t.Fatal("hash/header byte conflicts were relaxed")
	}
}
