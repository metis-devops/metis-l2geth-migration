package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// Extend the disposable canonical fixture. Storage changes and corresponding
// synthetic receipts model accounting, not historical EVM execution evidence.
func advanceAuditFixture(t testing.TB, f *ovmFixture, change func([]fixtureAccount), logs ...*types.Log) {
	t.Helper()
	if change != nil {
		change(f.accounts)
	}
	editOVMSource(t, *f, func(db ethdb.Database) {
		root := writeOVMFixtureState(t, db, f.accounts)
		h := &types.Header{ParentHash: f.head.Hash(), UncleHash: types.EmptyUncleHash, Root: root, Number: new(big.Int).Add(f.head.Number, big.NewInt(1)), Difficulty: big.NewInt(1), GasLimit: f.head.GasLimit, Time: f.head.Time + 1, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash}
		receipt := []byte{0xc0}
		if len(logs) > 0 {
			r := &types.Receipt{Status: 1, CumulativeGasUsed: 21000, Logs: logs}
			r.Bloom = types.CreateBloom(r)
			h.ReceiptHash = types.DeriveSha(types.Receipts{r}, trie.NewStackTrie(nil))
			h.Bloom = r.Bloom
			h.GasUsed = 21000
			tx := types.NewTx(&types.LegacyTx{GasPrice: big.NewInt(1), Gas: 21000, To: &ovmETHAddress, Value: new(big.Int)})
			h.TxHash = types.DeriveSha(types.Transactions{tx}, trie.NewStackTrie(nil))
			receipt = encodeOVMTestReceipts(t, logs)
		}
		rawdb.WriteHeader(db, h)
		rawdb.WriteCanonicalHash(db, h.Hash(), h.Number.Uint64())
		rawdb.WriteHeadBlockHash(db, h.Hash())
		rawdb.WriteHeadHeaderHash(db, h.Hash())
		putOVMTest(t, db, ovmReceiptKey(h), receipt)
		f.head = h
		f.root = root
		f.receipts = receipt
	})
}

func evolveAuditFixture(t testing.TB, f *ovmFixture) {
	t.Helper()
	// A new positive holder, a fully emptied holder, then a full burn. B/S and
	// the count all differ from H, while B-S stays unchanged.
	advanceAuditFixture(t, f, func(a []fixtureAccount) {
		a[0].storage[ovmBalanceSlot(f.holders[6])] = common.HexToHash("0x28")
		a[0].storage[common.HexToHash("0x02")] = common.BigToHash(new(big.Int).Add(a[0].storage[common.HexToHash("0x02")].Big(), big.NewInt(40)))
	}, ovmTestEvent(ovmTransferTopic, common.Address{}, f.holders[6], 40))
	advanceAuditFixture(t, f, func(a []fixtureAccount) {
		a[0].storage[ovmBalanceSlot(f.holders[5])] = common.Hash{}
		a[0].storage[ovmBalanceSlot(f.holders[6])] = common.HexToHash("0x6a")
	}, ovmTestEvent(ovmTransferTopic, f.holders[5], f.holders[6], 66))
	advanceAuditFixture(t, f, func(a []fixtureAccount) {
		a[0].storage[ovmBalanceSlot(f.holders[4])] = common.Hash{}
		a[0].storage[common.HexToHash("0x02")] = common.BigToHash(new(big.Int).Sub(a[0].storage[common.HexToHash("0x02")].Big(), big.NewInt(55)))
	}, ovmTestEvent(ovmTransferTopic, f.holders[4], common.Address{}, 55))
}

func TestOVMAuditHistoricalTargets(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, engine := range []string{DBEnginePebble, DBEngineLevelDB} {
			for _, scheme := range []string{"hash", "path"} {
				for _, overlay := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/overlay=%t", mode, engine, scheme, overlay), func(t *testing.T) {
						f := auditFixture(t, 300)
						a := fixtureAudit(t, f)
						input := writeAudit(t, a)
						evolveAuditFixture(t, &f)
						if scheme == "path" {
							writeOVMFixtureAncients(t, f)
							editOVMSource(t, f, func(db ethdb.Database) {
								rawdb.DeleteCanonicalHash(db, 0)
								rawdb.DeleteHeader(db, a.Identity.Genesis, 0)
								rawdb.DeleteCanonicalHash(db, a.Identity.To)
								rawdb.DeleteHeader(db, a.Identity.ToHash, a.Identity.To)
							})
						}
						// The source need not retain the audited historical state trie.
						editOVMSource(t, f, func(db ethdb.Database) {
							if err := db.Delete(a.Target.Root[:]); err != nil {
								t.Fatal(err)
							}
						})
						before := directoryContentDigest(t, f.source)
						opts := f.options(t, engine, scheme, 4)
						opts.TempDB = mode
						opts.OVM.AuditSummary = input
						var apply func(*state.StateDB)
						if overlay {
							opts.OVM.ERC20RetainList = writeRetainList(t, f.holders[2].Hex())
							opts.OVM.GenesisAlloc = writeAllocFile(t, fmt.Sprintf(`{%q:{"balance":"0x7b"}}`, f.holders[5].Hex()))
							apply = func(s *state.StateDB) {
								s.SetBalance(f.holders[2], new(uint256.Int), tracing.BalanceChangeUnspecified)
								s.SetState(ovmETHAddress, ovmBalanceSlot(f.holders[2]), common.HexToHash("0x21"))
								s.SetState(ovmETHAddress, common.HexToHash("0x02"), common.HexToHash("0x84"))
								s.SetBalance(ovmETHAddress, uint256.NewInt(132), tracing.BalanceChangeUnspecified)
								s.SetBalance(f.holders[5], uint256.NewInt(123), tracing.BalanceChangeUnspecified)
							}
						}
						result, err := Migrate(t.Context(), opts)
						if err != nil {
							t.Fatal(err)
						}
						r := requireOVMReport(t, result)
						wantHead := bundle.Head{BlockNumber: a.Identity.To, BlockHash: a.Identity.ToHash, StateRoot: a.Target.Root}
						if r.AuditSummary == nil || r.AuditSummary.AuditHead == nil || *r.AuditSummary.AuditHead != wantHead || r.AuditSummary.BalanceSum.Uint64() != 593 || r.Balances.OriginalSupply.Uint64() != 293 || r.AuditSummary.SupplyDifference != "300" || r.Balances.MigratedNative.Cmp(r.Balances.OriginalSupply) <= 0 {
							t.Fatalf("wrong current accounting: %+v", r)
						}
						var expected []logicalEntry
						root := ovmReferenceWithAllocDB(t, f, apply, func(db ethdb.Database, root common.Hash) {
							expected = allocReferenceInventory(t, db, root, scheme, r.Checkpoint)
						})
						if r.Target.Root != root || !reflect.DeepEqual(readLogicalDatabase(t, filepath.Join(opts.Output, "chaindata"), "historical"), expected) {
							t.Fatal("independent reference state/inventory mismatch")
						}
						if r.History.LastBlock != 4 || r.History.Blocks != 5 {
							t.Fatal("history stopped at H")
						}
						if _, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDir: t.TempDir(), TempDB: oppositeTempMode(mode), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 3, OVM: opts.OVM}); err != nil {
							t.Fatal(err)
						}
						if before != directoryContentDigest(t, f.source) {
							t.Fatal("source changed")
						}
					})
				}
			}
		}
	}
}

func TestOVMAuditHistoricalSameRoot(t *testing.T) {
	f := auditFixture(t, 300)
	a := fixtureAudit(t, f)
	path := writeAudit(t, a)
	for range 2 {
		advanceAuditFixture(t, &f, nil)
		if f.root != a.Target.Root {
			t.Fatal("fixture changed root")
		}
		opts := f.options(t, DBEnginePebble, "hash", 2)
		opts.OVM.AuditSummary = path
		result, err := Migrate(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		r := requireOVMReport(t, result)
		if r.AuditSummary.AuditHead == nil || r.AuditSummary.AuditHead.BlockNumber != 1 {
			t.Fatal("equal root confused with equal height")
		}
	}
}

func TestOVMAuditHistoricalRejectsMismatch(t *testing.T) {
	for _, kind := range []string{"future", "genesis", "anchor-hash", "anchor-root", "code", "plus-one", "minus-one", "missing-history", "conflicting-ancient"} {
		t.Run(kind, func(t *testing.T) {
			f := auditFixture(t, 300)
			a := fixtureAudit(t, f)
			evolveAuditFixture(t, &f)
			switch kind {
			case "future":
				a.Identity.To = 5
				a.VerifiedBlocks = 5
			case "genesis":
				a.Identity.Genesis[0] ^= 1
			case "anchor-hash":
				a.Identity.ToHash[0] ^= 1
			case "anchor-root":
				a.Target.Root[0] ^= 1
			case "code":
				advanceAuditFixture(t, &f, func(accounts []fixtureAccount) { accounts[0].code = []byte{0x00} })
			case "plus-one", "minus-one":
				advanceAuditFixture(t, &f, func(accounts []fixtureAccount) {
					v := accounts[0].storage[ovmBalanceSlot(f.holders[0])].Big()
					if kind == "plus-one" {
						v.Add(v, big.NewInt(1))
					} else {
						v.Sub(v, big.NewInt(1))
					}
					accounts[0].storage[ovmBalanceSlot(f.holders[0])] = common.BigToHash(v)
				})
			case "missing-history":
				editOVMSource(t, f, func(db ethdb.Database) { rawdb.DeleteHeader(db, a.Identity.ToHash, a.Identity.To) })
			case "conflicting-ancient":
				writeOVMFixtureAncients(t, f)
				editOVMSource(t, f, func(db ethdb.Database) { rawdb.WriteCanonicalHash(db, common.Hash{1}, a.Identity.To) })
			}
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.OVM.AuditSummary = writeAudit(t, a)
			if _, err := Migrate(t.Context(), opts); err == nil {
				t.Fatal("invalid historical audit/source accepted")
			}
			assertPathAbsent(t, opts.Output)
		})
	}
}

func TestOVMAuditHistoricalReportTampering(t *testing.T) {
	f := auditFixture(t, 300)
	a := fixtureAudit(t, f)
	path := writeAudit(t, a)
	evolveAuditFixture(t, &f)
	opts := f.options(t, DBEnginePebble, "hash", 2)
	opts.OVM.AuditSummary = path
	result, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(requireOVMReport(t, result))
	if err != nil {
		t.Fatal(err)
	}
	verify := OVMVerifyOptions{TempDir: t.TempDir(), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}
	for _, kind := range []string{"missing", "null", "field-missing", "field-null", "zero-height", "same-height", "future-height", "older-height", "hash", "root", "unknown", "duplicate", "missing-summary", "changed-summary"} {
		t.Run(kind, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(original, &m); err != nil {
				t.Fatal(err)
			}
			v := verify
			e := m["audit_summary"].(map[string]any)
			h := e["audit_head"].(map[string]any)
			switch kind {
			case "missing":
				delete(e, "audit_head")
			case "null":
				e["audit_head"] = nil
			case "field-missing":
				delete(h, "block_number")
			case "field-null":
				h["block_hash"] = nil
			case "zero-height":
				h["block_number"] = 0
			case "same-height":
				h["block_number"] = f.head.Number.Uint64()
			case "future-height":
				h["block_number"] = f.head.Number.Uint64() + 1
			case "older-height":
				h["block_number"] = 2
			case "hash":
				h["block_hash"] = (common.Hash{1}).Hex()
			case "root":
				h["state_root"] = (common.Hash{1}).Hex()
			case "unknown":
				h["unchecked"] = true
			case "missing-summary":
				v.OVM.AuditSummary = ""
			case "changed-summary":
				v.OVM.AuditSummary = writeAudit(t, a)
				data, err := os.ReadFile(v.OVM.AuditSummary)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(v.OVM.AuditSummary, append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "duplicate" {
				data = bytes.Replace(data, []byte(`"block_number":1`), []byte(`"block_number":1,"block_number":1`), 1)
			}
			if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyOVM(t.Context(), v); err == nil {
				t.Fatal("tampered audit_head accepted")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyOVM(t.Context(), verify); err != nil {
		t.Fatal(err)
	}
	originalInput, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	late := retentionLateWriter{database: filepath.Join(opts.Output, "chaindata"), mutate: func() {
		if err := os.WriteFile(path, append(bytes.Clone(originalInput), '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}}
	verify.Progress.Logger = log.NewLogger(log.NewTerminalHandler(&late, false))
	if _, err := VerifyOVM(t.Context(), verify); err == nil || !strings.Contains(err.Error(), "audit summary changed") {
		t.Fatalf("late historical input mutation accepted: %v", err)
	}
	entries, err := os.ReadDir(verify.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("historical verification scratch leaked: %v %v", entries, err)
	}
	if err := os.WriteFile(path, originalInput, 0600); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"to":1`) {
		t.Fatal("historical summary was rewritten")
	}
}

func TestOVMAuditHistoricalZero(t *testing.T) {
	f := auditFixture(t, 0)
	old := writeAudit(t, fixtureAudit(t, f))
	evolveAuditFixture(t, &f)
	current := writeAudit(t, fixtureAudit(t, f))
	var root common.Hash
	for _, path := range []string{"", current, old} {
		opts := f.options(t, DBEnginePebble, "hash", 2)
		opts.OVM.AuditSummary = path
		result, err := Migrate(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		r := requireOVMReport(t, result)
		if root == (common.Hash{}) {
			root = r.Target.Root
		} else if root != r.Target.Root {
			t.Fatal("audit mode changed target state")
		}
		if path == "" {
			if r.AuditSummary != nil {
				t.Fatal("extra evidence")
			}
		} else if r.AuditSummary == nil || r.AuditSummary.BalanceSum.Uint64() != 293 || ((r.AuditSummary.AuditHead != nil) != (path == old)) {
			t.Fatal("wrong mode or current balance sum")
		}
	}
}
