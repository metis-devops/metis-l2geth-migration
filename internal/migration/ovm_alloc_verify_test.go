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
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

func TestOVMGenesisAllocVerificationTampering(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMGenesisAllocVerificationTampering(t, mode) })
	}
}

func testOVMGenesisAllocVerificationTampering(t *testing.T, tempMode TempDBMode) {
	f := newOVMFixture(t, nil)
	opts := f.options(t, DBEnginePebble, "hash", 4)
	opts.TempDB = tempMode
	opts.OVM.GenesisAlloc = writeAllocFile(t, fmt.Sprintf(`{"%s":{"balance":123}}`, f.holders[0]))
	result, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	verify := OVMVerifyOptions{TempDB: tempMode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}
	original, err := json.Marshal(result.OVMReport)
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(opts.Output, VerificationFileName)
	for _, kind := range []string{"missing_input", "different_input", "missing_evidence", "missing_both", "converted_root", "converted_counts", "null_evidence", "unknown_field", "bad_digest", "null_counts"} {
		t.Run(kind, func(t *testing.T) {
			v := verify
			var data map[string]any
			if err := json.Unmarshal(original, &data); err != nil {
				t.Fatal(err)
			}
			alloc := data["genesis_alloc"].(map[string]any)
			switch kind {
			case "missing_input":
				v.OVM.GenesisAlloc = ""
			case "different_input":
				v.OVM.GenesisAlloc = writeAllocFile(t, `{}`)
			case "missing_evidence":
				delete(data, "genesis_alloc")
			case "missing_both":
				delete(data, "genesis_alloc")
				v.OVM.GenesisAlloc = ""
			case "converted_root":
				alloc["converted_state"].(map[string]any)["state_root"] = common.HexToHash("0x01")
			case "converted_counts":
				alloc["converted_state"].(map[string]any)["counts"].(map[string]any)["payload_bytes"] = float64(result.OVMReport.GenesisAlloc.Converted.Counts.PayloadBytes + 1)
			case "null_evidence":
				data["genesis_alloc"] = nil
			case "unknown_field":
				alloc["ignored"] = true
			case "bad_digest":
				alloc["file_sha256"] = "0x01"
			case "null_counts":
				alloc["converted_state"].(map[string]any)["counts"] = nil
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(reportPath, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyOVM(t.Context(), v); err == nil {
				t.Fatal("tampered evidence/input accepted")
			}
		})
	}
	if err := os.WriteFile(reportPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	kv, err := openTestTargetKV(filepath.Join(opts.Output, "chaindata"), 16, 16, "alloc-tamper", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	rawdb.WriteCode(db, crypto.Keccak256Hash([]byte{0x55}), []byte{0x55})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyOVM(t.Context(), verify); err == nil {
		t.Fatal("orphan target code accepted")
	}

	plain := f.options(t, DBEnginePebble, "hash", 2)
	plain.TempDB = tempMode
	if _, err := Migrate(t.Context(), plain); err != nil {
		t.Fatal(err)
	}
	verify.Artifact = plain.Output
	if _, err := VerifyOVM(t.Context(), verify); err == nil {
		t.Fatal("unrecorded alloc input accepted")
	}
}

func TestOVMGenesisAllocCancellationAndMutationCleanup(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMGenesisAllocCancellationAndMutationCleanup(t, mode) })
	}
}

func testOVMGenesisAllocCancellationAndMutationCleanup(t *testing.T, tempMode TempDBMode) {
	for _, phase := range []string{"apply_genesis_alloc", "build_converted_state", "publish_artifact"} {
		for _, mutation := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mutate=%t", phase, mutation), func(t *testing.T) {
				f := newOVMFixture(t, nil)
				before := directoryContentDigest(t, f.source)
				opts := f.options(t, DBEngineLevelDB, "path", 16)
				opts.TempDB = tempMode
				data := fmt.Sprintf(`{"%s":{"nonce":42}}`, f.holders[0])
				opts.OVM.GenesisAlloc = writeAllocFile(t, data)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				writer := ovmPhaseWriter{phase: phase, act: func() {
					if mutation {
						if err := os.WriteFile(opts.OVM.GenesisAlloc, []byte(data+" "), 0600); err != nil {
							t.Error(err)
						}
					} else {
						cancel()
					}
				}}
				opts.Progress.Logger = log.NewLogger(log.NewTerminalHandler(&writer, false))
				_, err := Migrate(ctx, opts)
				if mutation {
					if err == nil || !strings.Contains(err.Error(), "GenesisAlloc input changed") {
						t.Fatalf("mutation: %v", err)
					}
				} else if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
				entries, err := os.ReadDir(filepath.Dir(opts.Output))
				if err != nil || len(entries) != 0 {
					t.Fatalf("unpublished scratch was not cleaned: %v %v", entries, err)
				}
				opts.Progress = ProgressOptions{}
				if _, err := Migrate(t.Context(), opts); err != nil {
					t.Fatalf("retry failed: %v", err)
				}
				if before != directoryContentDigest(t, f.source) {
					t.Fatal("source changed")
				}
			})
		}
	}
}

func TestOVMGenesisAllocDoesNotRepairSource(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMGenesisAllocDoesNotRepairSource(t, mode) })
	}
}

func testOVMGenesisAllocDoesNotRepairSource(t *testing.T, tempMode TempDBMode) {
	for _, kind := range []string{"native", "supply", "ownership"} {
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
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.TempDB = tempMode
			opts.OVM.GenesisAlloc = writeAllocFile(t, fmt.Sprintf(`{"%s":{"balance":0},"%s":{"balance":0}}`, f.holders[0], common.Address{0xfe}))
			if _, err := Migrate(t.Context(), opts); err == nil {
				t.Fatal("alloc repaired invalid source or provided ownership witness")
			}
		})
	}
}

func TestOVMBalanceClassifiesCodeAtOriginalHead(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMBalanceClassifiesCodeAtOriginalHead(t, mode) })
	}
}

func testOVMBalanceClassifiesCodeAtOriginalHead(t *testing.T, tempMode TempDBMode) {
	f := newOVMFixture(t, nil)
	s, err := openLegacySource(f.source, 16, 16, newProgressReporter("test", ProgressOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	tdb := triedb.NewDatabase(s.db, triedb.HashDefaults)
	defer func() {
		if err := tdb.Close(); err != nil {
			t.Error(err)
		}
	}()
	transformer := ovmTransformer{trieDB: tdb, root: f.root, index: testAllocIndex(t), limiter: newMigrateWorkLimiter(2)}
	batch := ovmBalanceBatch{ctx: t.Context(), transformer: &transformer}
	for n, contract := range map[int]bool{0: false, 1: true, 2: true, 4: false, 7: true} {
		job := ovmBalanceJob{address: f.holders[n], hash: crypto.Keccak256Hash(f.holders[n][:]), value: *uint256.NewInt(1)}
		if err := batch.inspect(&job); err != nil {
			t.Fatal(err)
		}
		if job.contract != contract {
			t.Fatalf("holder %d contract=%t want %t", n, job.contract, contract)
		}
	}
}

type allocFailBatch struct {
	ethdb.Batch
	failure error
}

func (b allocFailBatch) Write() error { return b.failure }

func TestOVMGenesisAllocPropagatesIndexFlushFailure(t *testing.T) {
	idx := testAllocIndex(t)
	injected := errors.New("alloc flush failure")
	idx.batch = allocFailBatch{Batch: idx.batch, failure: injected}
	if _, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, `{"0000000000000000000000000000000000000001":{"balance":0}}`), idx); !errors.Is(err, injected) {
		t.Fatalf("flush error swallowed: %v", err)
	}
}

func TestOVMGenesisAllocRawEncodingBounds(t *testing.T) {
	// Maximal code remains accepted, including JSON escapes across reader chunks.
	for _, escaped := range []bool{false, true} {
		code := strings.Repeat("00", ovmAllocMaxCode)
		if escaped {
			code = strings.Repeat(`\u0030`, 2*ovmAllocMaxCode)
		}
		data := `{"0000000000000000000000000000000000000001":{"code":"0x` + code + `"}}`
		if _, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, data), testAllocIndex(t)); err != nil {
			t.Fatal(err)
		}
	}
	data := `{"0000000000000000000000000000000000000001":{"balance":"0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","nonce":"0xffffffffffffffff"}}`
	if _, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, data), testAllocIndex(t)); err != nil {
		t.Fatal(err)
	}
	// A token length limit must stop an unbounded stream instead of reading to EOF.
	r := &allocCountingReader{Reader: bytes.NewReader([]byte(`"` + strings.Repeat("a", 20<<20) + `"`))}
	decoder := json.NewDecoder(&ovmAllocReader{ctx: t.Context(), r: r})
	if _, err := decoder.Token(); err == nil || r.bytes > 13<<20 {
		t.Fatalf("token was not bounded: read=%d err=%v", r.bytes, err)
	}
}

type allocCountingReader struct {
	*bytes.Reader
	bytes int
}

type allocDatabasePhaseWriter struct {
	path  string
	phase ovmPhaseWriter
}

func (w *allocDatabasePhaseWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(w.path)) {
		return w.phase.Write(p)
	}
	return len(p), nil
}

func TestOVMGenesisAllocMutationDuringActualArtifactVerification(t *testing.T) {
	f := newOVMFixture(t, nil)
	opts := f.options(t, DBEnginePebble, "hash", 2)
	opts.OVM.GenesisAlloc = writeAllocFile(t, `{}`)
	if _, err := Migrate(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	writer := allocDatabasePhaseWriter{path: filepath.Join(opts.Output, "chaindata"), phase: ovmPhaseWriter{phase: "verify_state", act: func() {
		if err := os.WriteFile(opts.OVM.GenesisAlloc, []byte(`{} `), 0600); err != nil {
			t.Error(err)
		}
	}}}
	v := OVMVerifyOptions{SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM,
		Progress: ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&writer, false))}}
	if _, err := VerifyOVM(t.Context(), v); err == nil || !strings.Contains(err.Error(), "GenesisAlloc input changed") {
		t.Fatalf("input change during actual artifact verification was missed: %v", err)
	}
}

func (r *allocCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

type allocObserveBatch struct {
	ethdb.Batch
	observe func()
}

func (b allocObserveBatch) Write() error { b.observe(); return b.Batch.Write() }

func TestOVMGenesisAllocPendingKeysBounded(t *testing.T) {
	var data strings.Builder
	data.WriteString(`{"0000000000000000000000000000000000000001":{"storage":{`)
	for n := range 10000 {
		if n > 0 {
			data.WriteByte(',')
		}
		fmt.Fprintf(&data, `"%064x":"01"`, n)
	}
	data.WriteString(`}}}`)
	idx := testAllocIndex(t)
	l := ovmAllocLoader{ctx: t.Context(), decoder: json.NewDecoder(strings.NewReader(data.String())), index: idx, pending: make(map[string]struct{})}
	var writes, peak int
	idx.batch = allocObserveBatch{Batch: idx.batch, observe: func() { writes++; peak = max(peak, len(l.pending)) }}
	if err := l.object(l.account); err != nil {
		t.Fatal(err)
	}
	if writes < 3 || peak > ethdb.IdealBatchSize/33+1 || len(l.pending) > ethdb.IdealBatchSize/33+1 {
		t.Fatalf("unbounded duplicate tracking: writes=%d peak=%d remaining=%d", writes, peak, len(l.pending))
	}
}

type allocCancelIndex struct {
	ethdb.Database
	cancel context.CancelFunc
	reads  int
}

func (db *allocCancelIndex) NewIterator(prefix, start []byte) ethdb.Iterator {
	it := db.Database.NewIterator(prefix, start)
	if len(prefix) != 0 && prefix[0] == ovmAllocStoragePrefix {
		return &allocCancelIterator{Iterator: it, index: db}
	}
	return it
}

type allocCancelIterator struct {
	ethdb.Iterator
	index *allocCancelIndex
}

func (it *allocCancelIterator) Next() bool {
	ok := it.Iterator.Next()
	it.index.reads++
	if it.index.reads == 100 {
		it.index.cancel()
	}
	return ok
}

func TestOVMGenesisAllocCancelsDuringStorageMerge(t *testing.T) {
	address := common.Address{0x88}
	var fields strings.Builder
	for n := range 1000 {
		if n > 0 {
			fields.WriteByte(',')
		}
		fmt.Fprintf(&fields, `"%064x":"01"`, n)
	}
	idx := testAllocIndex(t)
	if _, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, fmt.Sprintf(`{"%s":{"storage":{%s}}}`, address, fields.String())), idx); err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	root := writeOVMFixtureState(t, db, []fixtureAccount{{address: address, balance: new(uint256.Int)}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tracked := &allocCancelIndex{Database: idx.db, cancel: cancel}
	idx.db = tracked
	if _, err := applyOVMGenesisAlloc(ctx, db, root, idx); !errors.Is(err, context.Canceled) || tracked.reads != 100 {
		t.Fatalf("storage cancellation: reads=%d err=%v", tracked.reads, err)
	}
}
