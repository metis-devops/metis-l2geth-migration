package migration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/trie"
)

func replaceOVMFixtureLogs(t *testing.T, f ovmFixture, logs []*types.Log) {
	t.Helper()
	r := &types.Receipt{Status: 1, CumulativeGasUsed: 21000, Logs: logs}
	r.Bloom = types.CreateBloom(r)
	f.head.ReceiptHash = types.DeriveSha(types.Receipts{r}, trie.NewStackTrie(nil))
	f.head.Bloom = r.Bloom
	editOVMSource(t, f, func(db ethdb.Database) {
		rawdb.WriteHeader(db, f.head)
		rawdb.WriteCanonicalHash(db, f.head.Hash(), 1)
		rawdb.WriteHeadBlockHash(db, f.head.Hash())
		rawdb.WriteHeadHeaderHash(db, f.head.Hash())
		putOVMTest(t, db, ovmReceiptKey(f.head), encodeOVMTestReceipts(t, logs))
	})
}

func TestOVMOversizeHistoryAndPreimages(t *testing.T) {
	f := newOVMFixture(t, nil)
	logs := []*types.Log{
		ovmTestEvent(ovmTransferTopic, f.holders[1], f.holders[3], 1),
		ovmTestEvent(ovmTransferTopic, f.holders[0], f.holders[3], 1),
		ovmTestEvent(ovmApprovalTopic, f.holders[1], f.holders[3], 0x1234),
		{Address: common.Address{0x33}, Data: bytes.Repeat([]byte{0x42}, 9<<20)},
	}
	replaceOVMFixtureLogs(t, f, logs)
	editOVMSource(t, f, func(db ethdb.Database) {
		for _, address := range f.holders {
			key := append([]byte("secure-key-"), crypto.Keccak256(address[:])...)
			putOVMTest(t, db, key, address[:])
		}
	})
	opts := f.options(t, DBEnginePebble, "hash", 2)
	opts.OVM.StateWitness = ""
	r, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.OVMReport.Target.Root != ovmReferenceRoot(t, f) {
		t.Fatal("oversize history or preimages changed transformation")
	}
	if r.OVMReport.History.Transfers != 2 {
		t.Fatal("unrelated emitter counted")
	}
}

func TestOVMHistoricalBurnAndDestroyedSender(t *testing.T) {
	f := newOVMFixture(t, nil)
	replaceOVMFixtureLogs(t, f, []*types.Log{
		ovmTestEvent(ovmTransferTopic, f.holders[1], common.Address{}, 1),
		ovmTestEvent(ovmTransferTopic, f.holders[5], f.holders[3], 1),
		ovmTestEvent(ovmApprovalTopic, f.holders[1], f.holders[3], 0x1234),
	})
	r, err := Migrate(t.Context(), f.options(t, DBEnginePebble, "hash", 2))
	if err != nil {
		t.Fatal(err)
	}
	if r.OVMReport.Target.Root != ovmReferenceRoot(t, f) {
		t.Fatal("burn qualification or destroyed sender classification is wrong")
	}
}

type ovmPhaseWriter struct {
	phase string
	once  sync.Once
	act   func()
}

func (w *ovmPhaseWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("phase="+w.phase)) && bytes.Contains(p, []byte("status=started")) {
		w.once.Do(w.act)
	}
	return len(p), nil
}

func TestOVMCancellationAtStageBoundaries(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMCancellationAtStageBoundaries(t, mode) })
	}
}

func testOVMCancellationAtStageBoundaries(t *testing.T, tempMode TempDBMode) {
	for _, phase := range []string{"scan_ovm_history", "convert_ovm_balances", "build_converted_state", "publish_artifact"} {
		t.Run(phase, func(t *testing.T) {
			f := newOVMFixture(t, nil)
			opts := f.options(t, DBEnginePebble, "path", 16)
			opts.TempDB = tempMode
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			writer := &ovmPhaseWriter{phase: phase, act: cancel}
			opts.Progress.Logger = log.NewLogger(log.NewTerminalHandler(writer, false))
			_, err := Migrate(ctx, opts)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected stage cancellation, got %v", err)
			}
			if _, err := os.Stat(opts.Output); !os.IsNotExist(err) {
				t.Fatal("cancelled output published")
			}
			opts.Progress = ProgressOptions{}
			if _, err := Migrate(t.Context(), opts); err != nil {
				t.Fatalf("workers/databases were not cleaned up: %v", err)
			}
		})
	}
}

func TestOVMLevelDBSyncFailure(t *testing.T) {
	f := newOVMFixture(t, nil)
	opts := f.options(t, DBEngineLevelDB, "hash", 2)
	var injected bool
	writer := ovmPhaseWriter{phase: "build_converted_state", act: func() {
		partials, err := filepath.Glob(filepath.Join(filepath.Dir(opts.Output), ".artifact.partial-*"))
		if err != nil || len(partials) != 1 {
			t.Fatalf("partial lookup: %v %v", partials, err)
		}
		if err := os.Mkdir(filepath.Join(partials[0], "chaindata", "unexpected-directory"), 0700); err != nil {
			t.Fatal(err)
		}
		injected = true
	}}
	opts.Progress.Logger = log.NewLogger(log.NewTerminalHandler(&writer, false))
	_, err := Migrate(t.Context(), opts)
	if !injected || err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("expected LevelDB file-sync failure, got %v", err)
	}
	if _, err := os.Stat(opts.Output); !os.IsNotExist(err) {
		t.Fatal("unsynced target published")
	}
}

func TestOVMOutputOutsideAncient(t *testing.T) {
	f := newOVMFixture(t, nil)
	ancient := t.TempDir()
	opts := f.options(t, DBEnginePebble, "hash", 2)
	opts.OVM.SourceAncient = ancient
	opts.Output = filepath.Join(ancient, "nested", "artifact")
	_, err := Migrate(t.Context(), opts)
	if err == nil {
		t.Fatal("output inside ancient accepted")
	}
	entries, err := os.ReadDir(ancient)
	if err != nil || len(entries) != 0 {
		t.Fatal("output validation changed ancient before rejection")
	}
}

func TestOVMLongSparseStringPreserved(t *testing.T) {
	// Solidity encodes a 1024-byte all-zero long string with a length word,
	// but every data slot is absent. Nonzero slot count cannot bound its length.
	f := newOVMFixture(t, func(accounts []fixtureAccount) {
		clear(accounts[0].storage)
		accounts[0].storage[common.HexToHash("0x03")] = common.HexToHash("0x0801")
	})
	opts := f.options(t, DBEnginePebble, "path", 2)
	r, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	withArtifactState(t, opts.Output, opts.Scheme, r.OVMReport.Target.Root, true, func(s *state.StateDB) {
		if s.GetState(ovmETHAddress, common.HexToHash("0x03")) != common.HexToHash("0x0801") {
			t.Fatal("long string metadata changed")
		}
	})
}
