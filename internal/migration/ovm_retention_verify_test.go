package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

func TestOVMRetentionVerificationTampering(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) {
			f := newOVMFixture(t, nil)
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.TempDB = mode
			opts.OVM.ERC20RetainList = writeRetainList(t, f.holders[2].Hex())
			r, err := Migrate(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			original, err := json.Marshal(r.OVMReport)
			if err != nil {
				t.Fatal(err)
			}
			verify := OVMVerifyOptions{TempDB: mode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}
			for _, kind := range []string{"missing-input", "different-input", "same-address-new-bytes", "missing-evidence", "missing-both", "null", "upper-null", "unknown", "missing-digest", "short-digest", "zero-digest"} {
				t.Run(kind, func(t *testing.T) {
					v := verify
					var data map[string]any
					if err := json.Unmarshal(original, &data); err != nil {
						t.Fatal(err)
					}
					e := data["erc20_retention"].(map[string]any)
					switch kind {
					case "missing-input":
						v.OVM.ERC20RetainList = ""
					case "different-input":
						v.OVM.ERC20RetainList = writeRetainList(t, f.holders[1].Hex())
					case "same-address-new-bytes":
						v.OVM.ERC20RetainList = writeRetainList(t, f.holders[2].Hex()+"\n")
					case "missing-evidence":
						delete(data, "erc20_retention")
					case "missing-both":
						delete(data, "erc20_retention")
						v.OVM.ERC20RetainList = ""
					case "null":
						data["erc20_retention"] = nil
					case "upper-null":
						delete(data, "erc20_retention")
						data["ERC20_RETENTION"] = nil
					case "unknown":
						e["retain_all"] = true
					case "missing-digest":
						delete(e, "file_sha256")
					case "short-digest":
						e["file_sha256"] = "0x01"
					case "zero-digest":
						e["file_sha256"] = (common.Hash{}).Hex()
					}
					encoded, err := json.Marshal(data)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), encoded, 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := VerifyOVM(t.Context(), v); err == nil {
						t.Fatal("tampered retention input/report accepted")
					}
				})
			}
			if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), original, 0600); err != nil {
				t.Fatal(err)
			}
			plain := f.options(t, DBEnginePebble, "hash", 2)
			plain.TempDB = mode
			without, err := Migrate(t.Context(), plain)
			if err != nil {
				t.Fatal(err)
			}
			if without.OVMReport.ERC20Retention != nil {
				t.Fatal("absent list added evidence")
			}
			verify.Artifact = plain.Output
			if _, err := VerifyOVM(t.Context(), verify); err == nil {
				t.Fatal("unrecorded list accepted")
			}
			empty := f.options(t, DBEnginePebble, "hash", 2)
			empty.TempDB = mode
			empty.OVM.ERC20RetainList = writeRetainList(t, "")
			withEmpty, err := Migrate(t.Context(), empty)
			if err != nil {
				t.Fatal(err)
			}
			if withEmpty.OVMReport.ERC20Retention == nil || withEmpty.OVMReport.Target != without.OVMReport.Target {
				t.Fatal("empty list lost presence or changed state")
			}
			verify.Artifact = empty.Output
			verify.OVM = empty.OVM
			if _, err := VerifyOVM(t.Context(), verify); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type retentionLateWriter struct {
	once     sync.Once
	database string
	mutate   func()
}

func (w *retentionLateWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("phase=verify_state")) && bytes.Contains(p, []byte("database="+w.database)) {
		w.once.Do(w.mutate)
	}
	return len(p), nil
}

func TestOVMRetentionCancellationAndMutation(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, phase := range []string{"convert_ovm_balances", "publish_artifact"} {
			for _, mutate := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/mutate=%t", mode, phase, mutate), func(t *testing.T) {
					f := newOVMFixture(t, nil)
					opts := f.options(t, DBEngineLevelDB, "path", 16)
					opts.TempDB = mode
					opts.OVM.ERC20RetainList = writeRetainList(t, f.holders[2].Hex())
					before := directoryContentDigest(t, f.source)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					writer := ovmPhaseWriter{phase: phase, act: func() {
						if mutate {
							if err := os.WriteFile(opts.OVM.ERC20RetainList, []byte(f.holders[2].Hex()+"\n"), 0600); err != nil {
								t.Fatal(err)
							}
						} else {
							cancel()
						}
					}}
					opts.Progress.Logger = log.NewLogger(log.NewTerminalHandler(&writer, false))
					_, err := Migrate(ctx, opts)
					if mutate {
						if err == nil || !strings.Contains(err.Error(), "retain list changed") {
							t.Fatalf("mutation not caught: %v", err)
						}
					} else if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
					entries, err := os.ReadDir(filepath.Dir(opts.Output))
					if err != nil || len(entries) != 0 {
						t.Fatalf("leaked scratch: %v %v", entries, err)
					}
					opts.Progress = ProgressOptions{}
					if _, err := Migrate(t.Context(), opts); err != nil {
						t.Fatalf("retry after cleanup: %v", err)
					}
					if before != directoryContentDigest(t, f.source) {
						t.Fatal("source changed")
					}
					writer2 := retentionLateWriter{database: filepath.Join(opts.Output, "chaindata"), mutate: func() {
						if err := os.WriteFile(opts.OVM.ERC20RetainList, []byte(f.holders[2].Hex()+" \n"), 0600); err != nil {
							t.Fatal(err)
						}
					}}
					_, err = VerifyOVM(t.Context(), OVMVerifyOptions{TempDB: mode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 4, OVM: opts.OVM, Progress: ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&writer2, false))}})
					if err == nil || !strings.Contains(err.Error(), "retain list changed") {
						t.Fatalf("late verification mutation not caught: %v", err)
					}
					matches, err := filepath.Glob(filepath.Join(filepath.Dir(opts.Output), ".l2state-ovm-verify-*"))
					if err != nil || len(matches) != 0 {
						t.Fatalf("verification scratch leaked: %v %v", matches, err)
					}
				})
			}
		}
	}
}

func TestOVMRetentionDoesNotRepairSource(t *testing.T) {
	for _, kind := range []string{"native", "supply", "ownership", "history"} {
		t.Run(kind, func(t *testing.T) {
			f := newOVMFixture(t, func(a []fixtureAccount) {
				switch kind {
				case "native":
					a[1].balance.SetUint64(1)
				case "supply":
					a[0].storage[common.HexToHash("0x02")] = common.HexToHash("0x01")
				case "ownership":
					a[0].storage[ovmBalanceSlot(common.Address{0xfe})] = common.HexToHash("0x01")
				}
			})
			if kind == "history" {
				editOVMSource(t, f, func(db ethdb.Database) {
					if err := db.Delete(ovmReceiptKey(f.head)); err != nil {
						t.Fatal(err)
					}
				})
			}
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.OVM.ERC20RetainList = writeRetainList(t, f.holders[2].Hex())
			if _, err := Migrate(t.Context(), opts); err == nil {
				t.Fatal("retain list bypassed invalid source")
			}
			assertPathAbsent(t, opts.Output)
		})
	}
}

func TestOVMRetentionIndexFailure(t *testing.T) {
	idx := testAllocIndex(t)
	injected := errors.New("retention index read error")
	counted := &ovmReadCountingDB{Database: idx.db, failure: injected}
	idx.db = counted
	if _, err := readOVMERC20RetainList(t.Context(), strings.NewReader(common.Address{1}.Hex()), idx); !errors.Is(err, injected) {
		t.Fatalf("index failure treated as absence: %v", err)
	}
	if counted.gets != 1 || counted.has != 0 {
		t.Fatal("retention index lookup did not use single Get")
	}
}
