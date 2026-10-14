package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
)

func auditFixture(t testing.TB, extra uint64) ovmFixture {
	t.Helper()
	f := newOVMFixture(t, nil)
	if extra == 0 {
		return f
	}
	f.accounts[0].storage[ovmBalanceSlot(f.holders[0])] = common.BigToHash(new(big.Int).SetUint64(11 + extra))
	editOVMSource(t, f, func(db ethdb.Database) {
		f.root = writeOVMFixtureState(t, db, f.accounts)
		f.head.Root = f.root
		rawdb.WriteHeader(db, f.head)
		rawdb.WriteCanonicalHash(db, f.head.Hash(), 1)
		rawdb.WriteHeadBlockHash(db, f.head.Hash())
		rawdb.WriteHeadHeaderHash(db, f.head.Hash())
		putOVMTest(t, db, ovmReceiptKey(f.head), f.receipts)
	})
	return f
}

func fixtureAudit(t testing.TB, f ovmFixture) ovmAuditSummary {
	t.Helper()
	var genesisRoot, genesisHash common.Hash
	withOVMSource(t, f, func(db ethdb.Database) {
		genesisHash = rawdb.ReadCanonicalHash(db, 0)
		h := rawdb.ReadHeader(db, genesisHash, 0)
		if h == nil {
			t.Fatal("missing fixture genesis")
		}
		genesisRoot = h.Root
	})
	sum := new(big.Int)
	var count uint64
	for _, a := range append(append([]common.Address{}, f.holders...), ovmETHAddress) {
		v := f.accounts[0].storage[ovmBalanceSlot(a)].Big()
		if v.Sign() != 0 {
			count++
			sum.Add(sum, v)
		}
	}
	supply := f.accounts[0].storage[common.HexToHash("0x02")].Big()
	diff := new(big.Int).Sub(sum, supply).String()
	genesisSupply := big.NewInt(int64(308 + len(f.holders) - 8))
	// Synthetic gas-shortfall attribution: zero genesis discrepancy plus any
	// later balanced transfers/mints/burns. This is not an EVM execution proof.
	return ovmAuditSummary{Version: 1, Identity: ovmAuditIdentity{Version: 1, Genesis: genesisHash, ConfigHash: crypto.Keccak256Hash([]byte("fixture config")), WitnessHash: crypto.Keccak256Hash(nil), To: f.head.Number.Uint64(), ToHash: f.head.Hash()}, ClientVersion: "mvm-summary-v1-fixture", From: 1, VerifiedBlocks: f.head.Number.Uint64(),
		Genesis: ovmAuditScan{Root: genesisRoot, CodeHash: crypto.Keccak256Hash(f.accounts[0].code), BalanceSlots: uint64(len(f.holders) - 1), Balances: genesisSupply.String(), Supply: genesisSupply.String(), Difference: "0"},
		Target:  ovmAuditScan{Root: f.root, CodeHash: crypto.Keccak256Hash(f.accounts[0].code), BalanceSlots: count, Balances: sum.String(), Supply: supply.String(), Difference: diff},
		Totals:  ovmAuditDelta{Balance: new(big.Int).Sub(sum, genesisSupply).String(), Supply: new(big.Int).Sub(supply, genesisSupply).String(), Difference: diff, Gas: diff, Suicide: "0", Unexplained: "0"}, ExecutionVerified: true, AccountingReconciled: true, IntervalCausesFullyExplained: true, GenesisDifferenceExplained: true}
}

func writeAudit(t testing.TB, a ovmAuditSummary) string {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "summary.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOVMAuditAllTargets(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, engine := range []string{DBEnginePebble, DBEngineLevelDB} {
			for _, scheme := range []string{"hash", "path"} {
				for _, overlay := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/overlay=%t", mode, engine, scheme, overlay), func(t *testing.T) {
						f := auditFixture(t, 300)
						before := directoryContentDigest(t, f.source)
						opts := f.options(t, engine, scheme, 4)
						opts.TempDB = mode
						opts.OVM.AuditSummary = writeAudit(t, fixtureAudit(t, f))
						apply := func(s *state.StateDB) {
							s.SetBalance(f.holders[0], uint256.NewInt(311), tracing.BalanceChangeUnspecified)
							if overlay {
								s.SetBalance(f.holders[2], new(uint256.Int), tracing.BalanceChangeUnspecified)
								s.SetState(ovmETHAddress, ovmBalanceSlot(f.holders[2]), common.HexToHash("0x21"))
								s.SetState(ovmETHAddress, common.HexToHash("0x02"), common.HexToHash("0x84"))
								s.SetBalance(ovmETHAddress, uint256.NewInt(132), tracing.BalanceChangeUnspecified)
								s.SetBalance(f.holders[5], uint256.NewInt(123), tracing.BalanceChangeUnspecified)
							}
						}
						if overlay {
							opts.OVM.ERC20RetainList = writeRetainList(t, f.holders[2].Hex())
							opts.OVM.GenesisAlloc = writeAllocFile(t, fmt.Sprintf(`{%q:{"balance":"0x7b"}}`, f.holders[5].Hex()))
						}
						result, err := Migrate(t.Context(), opts)
						if err != nil {
							t.Fatal(err)
						}
						r := requireOVMReport(t, result)
						if r.AuditSummary == nil || r.AuditSummary.BalanceSum.Uint64() != 608 || r.AuditSummary.SupplyDifference != "300" || r.Balances.OriginalSupply.Uint64() != 308 || r.Balances.MigratedNative.Cmp(r.Balances.OriginalSupply) <= 0 {
							t.Fatalf("wrong evidence %+v", r)
						}
						var expected []logicalEntry
						root := ovmReferenceWithAllocDB(t, f, apply, func(db ethdb.Database, root common.Hash) {
							expected = allocReferenceInventory(t, db, root, scheme, r.Checkpoint)
						})
						if root != r.Target.Root {
							t.Fatal("reference root mismatch")
						}
						actual := readLogicalDatabase(t, filepath.Join(opts.Output, "chaindata"), "audit")
						if !reflect.DeepEqual(actual, expected) {
							t.Fatal("independent inventory mismatch")
						}
						v := OVMVerifyOptions{TempDir: t.TempDir(), TempDB: oppositeTempMode(mode), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 3, OVM: opts.OVM}
						if _, err := VerifyOVM(t.Context(), v); err != nil {
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

func TestOVMAuditDefaultAndZero(t *testing.T) {
	f := auditFixture(t, 300)
	opts := f.options(t, DBEnginePebble, "hash", 2)
	if _, err := Migrate(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "totalSupply mismatch") {
		t.Fatalf("default relaxed: %v", err)
	}
	f = auditFixture(t, 0)
	opts = f.options(t, DBEnginePebble, "hash", 2)
	opts.OVM.AuditSummary = writeAudit(t, fixtureAudit(t, f))
	r, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if requireOVMReport(t, r).Target.Root != ovmReferenceRoot(t, f) || requireOVMReport(t, r).AuditSummary == nil {
		t.Fatal("zero difference changed state or lost evidence")
	}
}

func TestOVMAuditSummaryStrictness(t *testing.T) {
	f := auditFixture(t, 300)
	a := fixtureAudit(t, f)
	good, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"version":          func(m map[string]any) { m["version"] = 2 },
		"identity-version": func(m map[string]any) { m["identity"].(map[string]any)["version"] = 2 },
		"missing":          func(m map[string]any) { delete(m, "unexplainedEvents") },
		"null":             func(m map[string]any) { m["accountingReconciled"] = nil },
		"unknown":          func(m map[string]any) { m["ignoreMismatch"] = true },
		"incomplete":       func(m map[string]any) { m["verifiedBlocks"] = 0 },
		"unexecuted":       func(m map[string]any) { m["executionVerified"] = false },
		"unreconciled":     func(m map[string]any) { m["accountingReconciled"] = false },
		"unexplained":      func(m map[string]any) { m["intervalCausesFullyExplained"] = false },
		"cancelled-net":    func(m map[string]any) { m["unexplainedEvents"] = 2 },
		"genesis":          func(m map[string]any) { m["genesisState"].(map[string]any)["differenceWei"] = "1" },
		"genesis-flag":     func(m map[string]any) { m["genesisDifferenceExplained"] = false },
		"scan":             func(m map[string]any) { m["targetState"].(map[string]any)["balancesWei"] = "609" },
		"numeric":          func(m map[string]any) { m["targetState"].(map[string]any)["balancesWei"] = 608 },
		"leading-zero":     func(m map[string]any) { m["targetState"].(map[string]any)["balancesWei"] = "0608" },
		"overflow": func(m map[string]any) {
			m["targetState"].(map[string]any)["balancesWei"] = new(big.Int).Lsh(big.NewInt(1), 256).String()
		},
		"negative":        func(m map[string]any) { m["targetState"].(map[string]any)["supplyWei"] = "-1" },
		"delta":           func(m map[string]any) { m["totals"].(map[string]any)["balanceDeltaWei"] = "299" },
		"supply-delta":    func(m map[string]any) { m["totals"].(map[string]any)["supplyDeltaWei"] = "1" },
		"cause":           func(m map[string]any) { m["totals"].(map[string]any)["gasShortfallWei"] = "299" },
		"negative-cause":  func(m map[string]any) { m["totals"].(map[string]any)["legacySelfdestructWei"] = "-1" },
		"negative-zero":   func(m map[string]any) { m["totals"].(map[string]any)["supplyDeltaWei"] = "-0" },
		"diagnosis-empty": func(m map[string]any) { m["previousDiagnosisDifferenceWei"] = "" },
		"diagnosis-null":  func(m map[string]any) { m["matchesPreviousDiagnosis"] = nil },
		"short-hash":      func(m map[string]any) { m["identity"].(map[string]any)["toHash"] = "0x01" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(good, &m); err != nil {
				t.Fatal(err)
			}
			change(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "audit")
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readOVMAuditSummary(t.Context(), p); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	for _, b := range [][]byte{append(bytes.Clone(good), []byte(" {}")...), bytes.Replace(good, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(good, []byte(`"root":`), []byte(`"root":"0x01","root":`), 1), []byte(`null`), bytes.Repeat([]byte(" "), (1<<20)+1)} {
		p := filepath.Join(t.TempDir(), "audit")
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readOVMAuditSummary(t.Context(), p); err == nil {
			t.Fatal("malformed input accepted")
		}
	}
	p := writeAudit(t, a)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readOVMAuditSummary(t.Context(), link); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestOVMAuditSourceMismatch(t *testing.T) {
	for _, kind := range []string{"head", "height", "root", "genesis", "genesis-root", "code", "slots", "amount"} {
		t.Run(kind, func(t *testing.T) {
			f := auditFixture(t, 300)
			a := fixtureAudit(t, f)
			switch kind {
			case "head":
				a.Identity.ToHash[0] ^= 1
			case "height":
				a.Identity.To = 2
				a.VerifiedBlocks = 2
			case "root":
				a.Target.Root[0] ^= 1
			case "genesis":
				a.Identity.Genesis[0] ^= 1
			case "genesis-root":
				a.Genesis.Root[0] ^= 1
			case "code":
				a.Target.CodeHash[0] ^= 1
			case "slots":
				a.Target.BalanceSlots++
			case "amount":
				a.Target.Balances = "609"
				a.Target.Difference = "301"
				a.Totals.Balance = "301"
				a.Totals.Difference = "301"
				a.Totals.Gas = "301"
			}
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.OVM.AuditSummary = writeAudit(t, a)
			if _, err := Migrate(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "OVM audit") {
				t.Fatalf("mismatch accepted: %v", err)
			}
			assertPathAbsent(t, opts.Output)
		})
	}
}

func TestOVMAuditCancellationAndMutation(t *testing.T) { testOVMAuditCancellationAndMutation(t, false) }
func TestOVMAuditHistoricalCancellationAndMutation(t *testing.T) {
	testOVMAuditCancellationAndMutation(t, true)
}
func testOVMAuditCancellationAndMutation(t *testing.T, historical bool) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, phase := range []string{"convert_ovm_balances", "publish_artifact"} {
			for _, mutate := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", mode, phase, mutate), func(t *testing.T) {
					f := auditFixture(t, 300)
					opts := f.options(t, DBEnginePebble, "path", 4)
					opts.TempDB = mode
					opts.OVM.AuditSummary = writeAudit(t, fixtureAudit(t, f))
					if historical {
						evolveAuditFixture(t, &f)
					}
					original, err := os.ReadFile(opts.OVM.AuditSummary)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					writer := ovmPhaseWriter{phase: phase, act: func() {
						if mutate {
							if err := os.WriteFile(opts.OVM.AuditSummary, append(bytes.Clone(original), '\n'), 0600); err != nil {
								t.Fatal(err)
							}
						} else {
							cancel()
						}
					}}
					opts.Progress.Logger = log.NewLogger(log.NewTerminalHandler(&writer, false))
					_, err = Migrate(ctx, opts)
					if mutate {
						if err == nil || !strings.Contains(err.Error(), "audit summary changed") {
							t.Fatalf("mutation not detected: %v", err)
						}
					} else if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
					entries, err := os.ReadDir(filepath.Dir(opts.Output))
					if err != nil || len(entries) != 0 {
						t.Fatalf("scratch leak %v %v", entries, err)
					}
				})
			}
		}
	}
}

func withOVMSource(t testing.TB, f ovmFixture, visit func(ethdb.Database)) {
	t.Helper()
	kv, err := readonlydb.Open(f.source, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	visit(db)
}

func TestOVMAuditVerificationTampering(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) {
			f := auditFixture(t, 300)
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.TempDB = mode
			opts.OVM.AuditSummary = writeAudit(t, fixtureAudit(t, f))
			r, err := Migrate(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			original, err := json.Marshal(requireOVMReport(t, r))
			if err != nil {
				t.Fatal(err)
			}
			input, err := os.ReadFile(opts.OVM.AuditSummary)
			if err != nil {
				t.Fatal(err)
			}
			verify := OVMVerifyOptions{TempDir: t.TempDir(), TempDB: mode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}
			for _, kind := range []string{"missing-input", "missing-file", "bytes", "missing-evidence", "missing-both", "null", "missing-digest", "zero-digest", "sum", "difference", "missing-difference", "extra-field", "source-supply", "balanced-report"} {
				t.Run(kind, func(t *testing.T) {
					v := verify
					var m map[string]any
					if err := json.Unmarshal(original, &m); err != nil {
						t.Fatal(err)
					}
					a := m["audit_summary"].(map[string]any)
					switch kind {
					case "missing-input":
						v.OVM.AuditSummary = ""
					case "missing-file":
						v.OVM.AuditSummary = filepath.Join(t.TempDir(), "missing")
					case "bytes":
						v.OVM.AuditSummary = filepath.Join(t.TempDir(), "changed")
						if err := os.WriteFile(v.OVM.AuditSummary, append(bytes.Clone(input), '\n'), 0600); err != nil {
							t.Fatal(err)
						}
					case "missing-evidence":
						delete(m, "audit_summary")
					case "missing-both":
						delete(m, "audit_summary")
						v.OVM.AuditSummary = ""
					case "null":
						m["audit_summary"] = nil
					case "missing-digest":
						delete(a, "file_sha256")
					case "zero-digest":
						a["file_sha256"] = (common.Hash{}).Hex()
					case "sum":
						a["balance_sum"] = "609"
					case "difference":
						a["supply_difference"] = "301"
					case "missing-difference":
						delete(a, "supply_difference")
					case "extra-field":
						a["ignore"] = true
					case "source-supply":
						m["balances"].(map[string]any)["original_supply"] = "307"
						a["supply_difference"] = "301"
					case "balanced-report":
						m["balances"].(map[string]any)["migrated_native"] = "508"
						m["balances"].(map[string]any)["remaining_supply"] = "100"
					}
					b, err := json.Marshal(m)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), b, 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := VerifyOVM(t.Context(), v); err == nil {
						t.Fatal("tampering accepted")
					}
				})
			}
			if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), original, 0600); err != nil {
				t.Fatal(err)
			}
			// Input changes after replay, while the actual artifact is verified.
			late := retentionLateWriter{database: filepath.Join(opts.Output, "chaindata"), mutate: func() {
				if err := os.WriteFile(opts.OVM.AuditSummary, append(bytes.Clone(input), '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}}
			verify.Progress.Logger = log.NewLogger(log.NewTerminalHandler(&late, false))
			if _, err := VerifyOVM(t.Context(), verify); err == nil || !strings.Contains(err.Error(), "audit summary changed") {
				t.Fatalf("late mutation accepted: %v", err)
			}
			if err := os.WriteFile(opts.OVM.AuditSummary, input, 0600); err != nil {
				t.Fatal(err)
			}
			verify.Progress = ProgressOptions{}
			entries, err := os.ReadDir(verify.TempDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("verification scratch leaked")
			}
			// Actual target corruption must still be independently rejected.
			kv, err := openTestTargetKV(filepath.Join(opts.Output, "chaindata"), 16, 16, "tamper", false)
			if err != nil {
				t.Fatal(err)
			}
			rawdb.WriteCode(kv, crypto.Keccak256Hash([]byte{0x33}), []byte{0x33})
			if err := kv.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyOVM(t.Context(), verify); err == nil {
				t.Fatal("orphan code accepted")
			}
			// Even matching zero evidence must not be supplied to a plain artifact.
			plain := auditFixture(t, 0)
			p := plain.options(t, DBEnginePebble, "hash", 2)
			if _, err := Migrate(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			v := OVMVerifyOptions{TempDB: mode, SourceChaindata: plain.source, Artifact: p.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: p.OVM}
			v.OVM.AuditSummary = writeAudit(t, fixtureAudit(t, plain))
			if _, err := VerifyOVM(t.Context(), v); err == nil {
				t.Fatal("extra summary accepted")
			}
		})
	}
}

func TestOVMAuditPreservesSourceGuards(t *testing.T) {
	for _, kind := range []string{"native", "unknown-slot", "history"} {
		t.Run(kind, func(t *testing.T) {
			f := newOVMFixture(t, func(a []fixtureAccount) {
				if kind == "native" {
					a[1].balance.SetUint64(1)
				}
				if kind == "unknown-slot" {
					a[0].storage[common.HexToHash("0xff")] = common.HexToHash("0x01")
				}
			})
			a := fixtureAudit(t, f)
			if kind == "history" {
				editOVMSource(t, f, func(db ethdb.Database) {
					if err := db.Delete(ovmReceiptKey(f.head)); err != nil {
						t.Fatal(err)
					}
				})
			}
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.OVM.AuditSummary = writeAudit(t, a)
			if _, err := Migrate(t.Context(), opts); err == nil {
				t.Fatal("audit bypassed invalid source")
			}
			assertPathAbsent(t, opts.Output)
		})
	}
}

func TestOVMAuditWireFixtures(t *testing.T) {
	for _, name := range []string{"summary.json", "summary-diagnostic.json"} {
		a, _, err := readOVMAuditSummary(t.Context(), filepath.Join("testdata", "ovm-audit", name))
		if err != nil {
			t.Fatal(err)
		}
		if a.Target.Balances != "308" || a.Target.Difference != "0" {
			t.Fatal("wrong fixed fixture")
		}
	}
}

func TestOVMAuditAncientGenesis(t *testing.T) {
	f := auditFixture(t, 300)
	a := fixtureAudit(t, f)
	moveOVMGenesisToAncient(t, f)
	before := directoryContentDigest(t, f.source)
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			audit := a
			if mismatch {
				audit.Genesis.Root[0] ^= 1
			}
			opts := f.options(t, DBEngineLevelDB, "path", 3)
			opts.OVM.AuditSummary = writeAudit(t, audit)
			_, err := Migrate(t.Context(), opts)
			if mismatch {
				if err == nil || !strings.Contains(err.Error(), "audit genesis") {
					t.Fatalf("genesis mismatch: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	if before != directoryContentDigest(t, f.source) {
		t.Fatal("ancient/source changed")
	}
}

func TestOVMAuditRejectsDuplicateReportFields(t *testing.T) {
	f := auditFixture(t, 300)
	opts := f.options(t, DBEnginePebble, "hash", 2)
	opts.OVM.AuditSummary = writeAudit(t, fixtureAudit(t, f))
	result, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	report := requireOVMReport(t, result)
	original, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := json.Marshal(report.AuditSummary)
	if err != nil {
		t.Fatal(err)
	}
	needle := append([]byte(`"audit_summary":`), audit...)
	for _, test := range []struct{ name, replacement string }{
		{"null-first", `"audit_summary":null,` + string(needle)},
		{"null-last", string(needle) + `,"audit_summary":null`},
		{"identical", string(needle) + `,` + string(needle)},
		{"case-alias", string(needle) + `,"AUDIT_SUMMARY":` + string(audit)},
		{"escaped-alias", string(needle) + `,"audit_\u0073ummary":` + string(audit)},
		{"unicode-fold-alias", string(needle) + `,"audit_ſummary":` + string(audit)},
		{"other-evidence", string(needle) + `,"erc20_retention":null,"erc20_retention":{"file_sha256":"` + report.AuditSummary.FileSHA256.Hex() + `"}`},
		{"other-field", string(needle) + `,"verified":true`},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := bytes.Replace(original, needle, []byte(test.replacement), 1)
			if bytes.Equal(data, original) {
				t.Fatal("report was not changed")
			}
			if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := VerifyOVM(t.Context(), OVMVerifyOptions{SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM})
			if err == nil || (!strings.Contains(err.Error(), "duplicate OVM report field") && !strings.Contains(err.Error(), "must not be null")) {
				t.Fatalf("ambiguous report not rejected by decoder: %v", err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyOVM(t.Context(), OVMVerifyOptions{SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}); err != nil {
		t.Fatal(err)
	}
}
