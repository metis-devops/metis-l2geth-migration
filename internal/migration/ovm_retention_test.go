package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/holiman/uint256"
)

func writeRetainList(t testing.TB, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retain.txt")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRetentionIndex(t testing.TB, mode TempDBMode) *ovmIndex {
	t.Helper()
	storage := newTemporaryStorage(mode)
	path := filepath.Join(t.TempDir(), "index")
	db, err := storage.create(path, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	index := newOVMIndex(db)
	t.Cleanup(func() {
		index.batch.Close()
		if err := errors.Join(db.Close(), storage.remove(path)); err != nil {
			t.Error(err)
		}
	})
	return index
}

func TestOVMRetentionInput(t *testing.T) {
	address := common.HexToAddress("0xabcdefabcdefabcdefabcdefabcdefabcdefabcd")
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) {
			for _, data := range []string{"", address.Hex(), " \t" + address.Hex() + " \r\n", strings.Repeat(" ", 4053) + address.Hex(), strings.Repeat(" ", 4053) + address.Hex() + "\n"} {
				idx := testRetentionIndex(t, mode)
				digest, err := readOVMERC20RetainList(t.Context(), iotest.OneByteReader(strings.NewReader(data)), idx)
				if err != nil || digest != common.Hash(sha256.Sum256([]byte(data))) {
					t.Fatalf("valid input/digest: %v %s", err, digest)
				}
				_, present, err := idx.get(ovmERC20RetainPrefix, address[:])
				if err != nil || present != (data != "") {
					t.Fatalf("membership: %t %v", present, err)
				}
				if data != "" {
					slot := ovmStorageHash(ovmBalanceSlot(address))
					v, ok, err := idx.get('b', slot[:])
					if err != nil || !ok || !bytes.Equal(v, address[:]) {
						t.Fatal("list did not identify balance slot")
					}
					if _, ok, err := idx.get('f', address[:]); err != nil || ok {
						t.Fatal("manual eligibility forged history")
					}
				}
			}
			for name, data := range map[string]string{
				"blank": "\n", "whitespace": " \t\r\n", "short": "0x01", "unprefixed": address.Hex()[2:], "upper-prefix": "0X" + address.Hex()[2:], "bad-hex": "0x" + strings.Repeat("z", 40), "comment": "# keep this\n", "json": fmt.Sprintf("[%q]", address), "amount": address.Hex() + " 10", "too-long": strings.Repeat(" ", 4054) + address.Hex(), "duplicate": address.Hex() + "\n" + address.Hex(), "case-alias": strings.ToLower(address.Hex()) + "\n0x" + strings.ToUpper(address.Hex()[2:]), "ovm": ovmETHAddress.Hex(),
			} {
				t.Run(name, func(t *testing.T) {
					if _, err := readOVMERC20RetainList(t.Context(), strings.NewReader(data), testRetentionIndex(t, mode)); err == nil {
						t.Fatal("malformed list accepted")
					}
				})
			}
			var large strings.Builder
			for n := range 5000 {
				fmt.Fprintln(&large, common.BigToAddress(big.NewInt(int64(n+1))).Hex())
			}
			idx := testRetentionIndex(t, mode)
			if _, err := readOVMERC20RetainList(t.Context(), strings.NewReader(large.String()), idx); err != nil {
				t.Fatal(err)
			}
			if _, err := readOVMERC20RetainList(t.Context(), strings.NewReader(common.BigToAddress(big.NewInt(1)).Hex()), idx); err == nil {
				t.Fatal("duplicate across flushed batches accepted")
			}
		})
	}
}

func TestOVMRetentionInputFailures(t *testing.T) {
	address := common.Address{1}
	injected := errors.New("retention read failure")
	idx := testAllocIndex(t)
	if _, err := readOVMERC20RetainList(t.Context(), io.MultiReader(strings.NewReader(address.Hex()+"\n"), iotest.ErrReader(injected)), idx); !errors.Is(err, injected) {
		t.Fatalf("lost read error: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readOVMERC20RetainList(ctx, strings.NewReader(""), testAllocIndex(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	idx = testAllocIndex(t)
	idx.batch = allocFailBatch{Batch: idx.batch, failure: injected}
	if _, err := readOVMERC20RetainList(t.Context(), strings.NewReader(address.Hex()), idx); !errors.Is(err, injected) {
		t.Fatalf("lost flush error: %v", err)
	}
	file := writeRetainList(t, address.Hex())
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, t.TempDir()} {
		if _, err := loadOVMERC20RetainList(t.Context(), path, testAllocIndex(t)); err == nil {
			t.Fatal("non-regular input accepted")
		}
	}
}

func retainedPoolReference(f ovmFixture) func(*state.StateDB) {
	return func(s *state.StateDB) {
		s.SetBalance(f.holders[2], new(uint256.Int), tracing.BalanceChangeUnspecified)
		s.SetState(ovmETHAddress, ovmBalanceSlot(f.holders[2]), common.BigToHash(big.NewInt(33)))
		s.SetState(ovmETHAddress, common.HexToHash("0x02"), common.BigToHash(big.NewInt(132)))
		s.SetBalance(ovmETHAddress, uint256.NewInt(132), tracing.BalanceChangeUnspecified)
	}
}

func TestOVMRetentionAllTargets(t *testing.T) {
	f := newOVMFixture(t, nil)
	// Holder 2 has neither incoming nor outgoing history. Its list entry is
	// the only source of its address, not merely a second copy of a witness.
	witness, err := os.ReadFile(f.witness)
	if err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("{\"type\":\"address\",\"address\":%q}\n", f.holders[2].Hex())
	if err := os.WriteFile(f.witness, bytes.ReplaceAll(witness, []byte(line), nil), 0600); err != nil {
		t.Fatal(err)
	}
	list := writeRetainList(t, fmt.Sprintf("%s\n%s\n%s\n", f.holders[2], f.holders[1], f.holders[7]))
	before := directoryContentDigest(t, f.source)
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, tc := range targetTestCases() {
			t.Run(string(mode)+"/"+tc.name(), func(t *testing.T) {
				opts := f.options(t, tc.engine, tc.scheme, 4)
				opts.TempDB = mode
				opts.OVM.ERC20RetainList = list
				result, err := Migrate(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				r := result.OVMReport
				var reference []logicalEntry
				expected := ovmReferenceWithAllocDB(t, f, retainedPoolReference(f), func(db ethdb.Database, root common.Hash) {
					reference = allocReferenceInventory(t, db, root, tc.scheme, r.Checkpoint)
				})
				if r.Target.Root != expected || r.Balances.RemainingSupply.Uint64() != 132 || r.Balances.MigratedNative.Uint64() != 176 || r.Balances.RetainedContracts != 2 || r.ERC20Retention == nil {
					t.Fatalf("wrong retained state: %+v", r)
				}
				if actual := readLogicalDatabase(t, filepath.Join(opts.Output, "chaindata"), "retain"); !reflect.DeepEqual(actual, reference) {
					t.Fatal("retained inventory differs from serial StateDB/GenerateTrie")
				}
				other := TempDBMemory
				if mode == TempDBMemory {
					other = TempDBDisk
				}
				if _, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDB: other, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}); err != nil {
					t.Fatal(err)
				}
				withArtifactState(t, opts.Output, tc.scheme, expected, true, func(s *state.StateDB) {
					input := append(crypto.Keccak256([]byte("transfer(address,uint256)"))[:4], common.LeftPadBytes(f.holders[0][:], 32)...)
					input = append(input, common.LeftPadBytes([]byte{5}, 32)...)
					if _, _, err := runtime.Call(ovmETHAddress, input, &runtime.Config{State: s, Origin: f.holders[2], GasLimit: 1_000_000}); err != nil {
						t.Fatal(err)
					}
					if s.GetState(ovmETHAddress, ovmBalanceSlot(f.holders[2])).Big().Uint64() != 28 {
						t.Fatal("manually retained ERC20 cannot be transferred")
					}
				})
				next := commitOVMContinuation(t, opts, expected, f.holders[0])
				withArtifactState(t, opts.Output, tc.scheme, next, true, func(s *state.StateDB) {
					if s.GetState(ovmETHAddress, ovmBalanceSlot(f.holders[2])).Big().Uint64() != 33 || !s.GetBalance(f.holders[2]).IsZero() {
						t.Fatal("retained state did not survive subsequent commit")
					}
				})
			})
		}
	}
	if before != directoryContentDigest(t, f.source) {
		t.Fatal("source changed")
	}
}

func TestOVMRetentionValidationAndAlloc(t *testing.T) {
	f := newOVMFixture(t, nil)
	for name, address := range map[string]common.Address{"EOA": f.holders[0], "absent-positive": f.holders[4], "destroyed": f.holders[5], "EOA-zero": f.holders[6], "absent-zero": {0xee}} {
		t.Run(name, func(t *testing.T) {
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.OVM.ERC20RetainList = writeRetainList(t, address.Hex())
			opts.OVM.GenesisAlloc = writeAllocFile(t, fmt.Sprintf(`{"%s":{"code":"0x00"}}`, address))
			if _, err := Migrate(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "source head") {
				t.Fatalf("alloc repaired invalid source classification: %v", err)
			}
			assertPathAbsent(t, opts.Output)
		})
	}
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) {
			opts := f.options(t, DBEnginePebble, "path", 4)
			opts.TempDB = mode
			opts.OVM.ERC20RetainList = writeRetainList(t, f.holders[2].Hex())
			opts.OVM.GenesisAlloc = writeAllocFile(t, fmt.Sprintf(`{"%s":{"code":"0x","balance":7}}`, f.holders[2]))
			r, err := Migrate(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			converted := ovmReferenceWithAlloc(t, f, retainedPoolReference(f))
			expected := ovmReferenceWithAlloc(t, f, func(s *state.StateDB) {
				retainedPoolReference(f)(s)
				s.SetCode(f.holders[2], nil, tracing.CodeChangeUnspecified)
				s.SetBalance(f.holders[2], uint256.NewInt(7), tracing.BalanceChangeUnspecified)
			})
			if r.OVMReport.Target.Root != expected || r.OVMReport.GenesisAlloc.Converted.Root != converted || r.OVMReport.Balances.RemainingSupply.Uint64() != 132 {
				t.Fatal("alloc affected conversion-stage classification/evidence")
			}
			if _, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDB: mode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOVMRetentionWithOnlyZeroTransfer(t *testing.T) {
	f := newOVMFixture(t, nil)
	replaceOVMFixtureLogs(t, f, []*types.Log{
		ovmTestEvent(ovmTransferTopic, f.holders[1], f.holders[3], 0),
		ovmTestEvent(ovmTransferTopic, f.holders[0], f.holders[3], 1),
		ovmTestEvent(ovmApprovalTopic, f.holders[1], f.holders[3], 0x1234),
	})
	plain, err := Migrate(t.Context(), f.options(t, DBEnginePebble, "hash", 2))
	if err != nil {
		t.Fatal(err)
	}
	opts := f.options(t, DBEnginePebble, "hash", 2)
	opts.OVM.ERC20RetainList = writeRetainList(t, f.holders[1].Hex())
	listed, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if plain.OVMReport.Balances.RemainingSupply.Uint64() != 77 || listed.OVMReport.Target.Root != ovmReferenceRoot(t, f) || listed.OVMReport.History != plain.OVMReport.History {
		t.Fatal("manual policy changed history evidence or zero events granted eligibility")
	}
}
