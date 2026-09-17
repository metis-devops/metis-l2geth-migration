package migration

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

func TestOVMWrappedRuntimeAndContinuation(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMWrappedRuntimeAndContinuation(t, mode) })
	}
}

func testOVMWrappedRuntimeAndContinuation(t *testing.T, tempMode TempDBMode) {
	f := newOVMFixture(t, nil)
	for _, engine := range []string{DBEnginePebble, DBEngineLevelDB} {
		for _, scheme := range []string{"hash", "path"} {
			t.Run(engine+"/"+scheme, func(t *testing.T) {
				opts := f.options(t, engine, scheme, 2)
				opts.TempDB = tempMode
				result, err := Migrate(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				withArtifactState(t, opts.Output, scheme, result.OVMReport.Target.Root, true, func(s *state.StateDB) {
					call := func(sender common.Address, value int64, method string, args ...[]byte) []byte {
						input := append([]byte(nil), crypto.Keccak256([]byte(method))[:4]...)
						for _, arg := range args {
							input = append(input, common.LeftPadBytes(arg, 32)...)
						}
						output, _, err := runtime.Call(ovmETHAddress, input, &runtime.Config{State: s, Origin: sender, Value: big.NewInt(value), GasLimit: 1_000_000})
						if err != nil {
							t.Fatalf("%s: %v", method, err)
						}
						return output
					}
					read := func(method string, args ...[]byte) uint64 {
						return new(big.Int).SetBytes(call(f.holders[0], 0, method, args...)).Uint64()
					}
					if read("totalSupply()") != 99 || read("balanceOf(address)", f.holders[1][:]) != 22 || read("balanceOf(address)", f.holders[0][:]) != 0 {
						t.Fatal("migrated ERC20 queries disagree")
					}
					if read("allowance(address,address)", f.holders[1][:], f.holders[3][:]) != 0x1234 {
						t.Fatal("allowance changed")
					}
					call(f.holders[0], 5, "deposit()")
					if read("totalSupply()") != 104 || s.GetBalance(ovmETHAddress).Uint64() != 104 || read("balanceOf(address)", f.holders[0][:]) != 5 {
						t.Fatal("deposit backing mismatch")
					}
					call(f.holders[0], 0, "withdraw(uint256)", []byte{3})
					if read("totalSupply()") != 101 || s.GetBalance(ovmETHAddress).Uint64() != 101 || s.GetBalance(f.holders[0]).Uint64() != 9 {
						t.Fatal("withdraw backing mismatch")
					}
					call(f.holders[1], 0, "transfer(address,uint256)", f.holders[0][:], []byte{2})
					if read("balanceOf(address)", f.holders[0][:]) != 4 || read("balanceOf(address)", f.holders[1][:]) != 20 {
						t.Fatal("transfer mismatch")
					}
				})
				root := commitOVMContinuation(t, opts, result.OVMReport.Target.Root, f.holders[0])
				withArtifactState(t, opts.Output, scheme, root, true, func(s *state.StateDB) {
					if s.GetBalance(f.holders[0]).Uint64() != 12 {
						t.Fatal("subsequent state commit was not persisted")
					}
				})
			})
		}
	}
}

func commitOVMContinuation(t *testing.T, opts MigrateOptions, root common.Hash, address common.Address) common.Hash {
	t.Helper()
	kv, err := openTestTargetKV(filepath.Join(opts.Output, "chaindata"), 32, 32, "continue", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	tdb := triedb.NewDatabase(db, trieConfig(opts.Scheme, false))
	s, err := state.New(root, state.NewDatabase(tdb, state.NewCodeDB(db)))
	if err != nil {
		t.Fatal(err)
	}
	s.SetBalance(address, uint256.NewInt(12), tracing.BalanceChangeUnspecified)
	root, err = s.Commit(3, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestOVMReportStrictness(t *testing.T) {
	f := newOVMFixture(t, nil)
	result, err := Migrate(t.Context(), f.options(t, DBEnginePebble, "hash", 2))
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(result.OVMReport)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"version", func(m map[string]any) { m["version"] = 2 }},
		{"commit", func(m map[string]any) { m["geth_commit"] = "old" }},
		{"geth_number", func(m map[string]any) { m["geth_version"] = 17 }},
		{"layout_missing", func(m map[string]any) { delete(m, "state_layout") }},
		{"code_hash", func(m map[string]any) { m["wrapped_code_hash"] = "abc" }},
		{"header_missing", func(m map[string]any) { delete(m["checkpoint"].(map[string]any), "header_rlp") }},
		{"balance_missing", func(m map[string]any) { delete(m["balances"].(map[string]any), "remaining_supply") }},
		{"range", func(m map[string]any) { m["history"].(map[string]any)["first_block"] = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(original, &m); err != nil {
				t.Fatal(err)
			}
			tc.change(m)
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			r, err := strictio.DecodeJSON[OVMVerificationReport](data, "test")
			if err == nil {
				err = r.Validate()
			}
			if err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
	for _, value := range []any{nil, "", "future-version"} {
		var m map[string]any
		if err := json.Unmarshal(original, &m); err != nil {
			t.Fatal(err)
		}
		m["geth_version"] = value
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		r, err := strictio.DecodeJSON[OVMVerificationReport](data, "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOVMVerifyRejectsTampering(t *testing.T) {
	for _, kind := range []string{"code_input", "witness_input", "body", "orphan_code", "orphan_node", "report_supply", "extra_file"} {
		t.Run(kind, func(t *testing.T) {
			f := newOVMFixture(t, nil)
			opts := f.options(t, DBEnginePebble, "hash", 2)
			result, err := Migrate(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "code_input":
				if err := os.WriteFile(f.code, []byte("0x00"), 0600); err != nil {
					t.Fatal(err)
				}
			case "witness_input":
				fh, err := os.OpenFile(f.witness, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fh.WriteString("{\"type\":\"address\",\"address\":\"0x0000000000000000000000000000000000000000\"}\n"); err != nil {
					t.Fatal(err)
				}
				if err := fh.Close(); err != nil {
					t.Fatal(err)
				}
			case "extra_file":
				if err := os.WriteFile(filepath.Join(opts.Output, "extra"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "report_supply":
				r := *result.OVMReport
				r.Balances.MigratedNative.SubUint64(r.Balances.MigratedNative, 1)
				r.Balances.RemainingSupply.AddUint64(r.Balances.RemainingSupply, 1)
				if err := writeOVMReport(opts.Output, r); err != nil {
					t.Fatal(err)
				}
			default:
				kv, err := openTestTargetKV(filepath.Join(opts.Output, "chaindata"), 16, 16, "tamper", false)
				if err != nil {
					t.Fatal(err)
				}
				db := rawdb.NewDatabase(kv)
				switch kind {
				case "body":
					for key := range ovmBodyMetadata(result.OVMReport.Checkpoint) {
						if err := db.Delete([]byte(key)); err != nil {
							t.Fatal(err)
						}
					}
				case "orphan_code":
					rawdb.WriteCode(db, crypto.Keccak256Hash([]byte{0x33}), []byte{0x33})
				default:
					blob := []byte{0xc2, 0x20, 0x01}
					h := crypto.Keccak256Hash(blob)
					putOVMTest(t, db, h[:], blob)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			_, err = VerifyOVM(t.Context(), OVMVerifyOptions{SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM})
			if err == nil {
				t.Fatal("tampered artifact accepted")
			}
		})
	}
}

func TestOVMZeroSupply(t *testing.T) {
	f := newOVMFixture(t, func(a []fixtureAccount) {
		for key := range a[0].storage {
			delete(a[0].storage, key)
		}
	})
	r, err := Migrate(t.Context(), f.options(t, DBEnginePebble, "path", 2))
	if err != nil {
		t.Fatal(err)
	}
	if !r.OVMReport.Balances.RemainingSupply.IsZero() || !r.OVMReport.Balances.MigratedNative.IsZero() {
		t.Fatal("nonzero migrated supply")
	}
}

func TestOVMCheckpointOverflow(t *testing.T) {
	for _, timestamp := range []bool{false, true} {
		h := &types.Header{Number: new(big.Int), Time: 0, Root: types.EmptyRootHash, Difficulty: new(big.Int)}
		if timestamp {
			h.Time = math.MaxUint64
		} else {
			h.Number.SetUint64(math.MaxUint64)
		}
		blob, err := rlp.EncodeToBytes(h)
		if err != nil {
			t.Fatal(err)
		}
		head := bundle.Head{BlockNumber: h.Number.Uint64(), BlockHash: h.Hash(), StateRoot: h.Root}
		if _, err := ovmCheckpoint(bundle.SourceEvidence{HeadBefore: head, HeadAfter: head, HeaderRLP: blob}, h.Root); err == nil {
			t.Fatal("overflow accepted")
		}
	}
}

func TestOVMReceiptFormats(t *testing.T) {
	log := ovmTestEvent(ovmTransferTopic, common.Address{1}, common.Address{2}, 0)
	want := &types.Receipt{Status: 1, CumulativeGasUsed: 21000, Logs: []*types.Log{log}}
	want.Bloom = types.CreateBloom(want)
	for _, fields := range [][]any{
		{[]byte{1}, uint64(21000), []*types.Log{log}, new(big.Int), new(big.Int), new(big.Int), "1.5"},
		{[]byte{1}, uint64(21000), common.Hash{}, common.Address{}, []*types.Log{log}, uint64(21000)},
		{[]byte{1}, uint64(21000), want.Bloom, common.Hash{}, common.Address{}, []*types.Log{log}, uint64(21000)},
	} {
		blob, err := rlp.EncodeToBytes(fields)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeOVMReceipt(blob)
		if err != nil {
			t.Fatal(err)
		}
		a, err := rlp.EncodeToBytes(got)
		if err != nil {
			t.Fatal(err)
		}
		b, err := rlp.EncodeToBytes(want)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("legacy consensus receipt changed")
		}
	}
	for _, blob := range [][]byte{{0xc0}, {0x01}, append(encodeOVMTestReceipts(t, []*types.Log{log}), 0)} {
		if _, err := decodeOVMReceipt(blob); err == nil {
			t.Fatal("invalid receipt accepted")
		}
	}
	idxDB := rawdb.NewMemoryDatabase()
	defer func() {
		if err := idxDB.Close(); err != nil {
			t.Error(err)
		}
	}()
	idx := newOVMIndex(idxDB)
	defer idx.batch.Close()
	s := ovmHistoryScanner{index: idx}
	for _, mutate := range []func(*types.Log){func(l *types.Log) { l.Topics = l.Topics[:2] }, func(l *types.Log) { l.Data = nil }, func(l *types.Log) { l.Topics[1][0] = 1 }} {
		l := ovmTestEvent(ovmTransferTopic, common.Address{1}, common.Address{2}, 0)
		mutate(l)
		if err := s.event(0, 0, 0, l); err == nil {
			t.Fatal("malformed event accepted")
		}
	}
}
