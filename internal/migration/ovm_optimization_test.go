package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

func TestOVMLateInputMutation(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMLateInputMutation(t, mode) })
	}
}

func testOVMLateInputMutation(t *testing.T, tempMode TempDBMode) {
	for _, mode := range []string{"publish", "verify"} {
		for _, input := range []string{"code", "witness"} {
			t.Run(mode+"/"+input, func(t *testing.T) {
				f := newOVMFixture(t, nil)
				opts := f.options(t, DBEnginePebble, "hash", 2)
				opts.TempDB = tempMode
				changed := false
				act := func() {
					path, data := f.code, []byte("0x00\n")
					if input == "witness" {
						path, data = f.witness, []byte("not-json\n")
					}
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Error(err)
					}
					changed = true
				}
				var err error
				if mode == "publish" {
					writer := ovmPhaseWriter{phase: "publish_artifact", act: act}
					opts.Progress = ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&writer, false))}
					_, err = Migrate(t.Context(), opts)
					if _, statErr := os.Lstat(opts.Output); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("changed input was published: %v", statErr)
					}
				} else {
					if _, err := Migrate(t.Context(), opts); err != nil {
						t.Fatal(err)
					}
					writer := allocDatabasePhaseWriter{path: filepath.Join(opts.Output, "chaindata"), phase: ovmPhaseWriter{phase: "verify_state", act: act}}
					_, err = VerifyOVM(t.Context(), OVMVerifyOptions{TempDir: filepath.Dir(opts.Output), TempDB: tempMode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM, Progress: ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&writer, false))}})
				}
				if !changed || err == nil || !strings.Contains(err.Error(), "input changed") {
					t.Fatalf("late input mutation: changed=%t err=%v", changed, err)
				}
				entries, err := os.ReadDir(filepath.Dir(opts.Output))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.Name() != filepath.Base(opts.Output) {
						t.Fatalf("scratch leaked: %s", entry.Name())
					}
				}
			})
		}
	}
}

func TestOVMInputDigestRawBytesAndFailures(t *testing.T) {
	data := []byte(" \t{\"a\": \"escaped\\\\bytes\"}\n ")
	digest, err := hashOVMInputReader(t.Context(), bytes.NewReader(data))
	if err != nil || digest != common.Hash(sha256.Sum256(data)) {
		t.Fatalf("raw digest: %s %v", digest, err)
	}
	injected := errors.New("read failure")
	_, err = hashOVMInputReader(t.Context(), &allocReadError{data: "valid bytes", err: injected})
	if !errors.Is(err, injected) {
		t.Fatalf("read failure lost: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader := &ovmCancelHashReader{cancel: cancel}
	_, err = hashOVMInputReader(ctx, reader)
	if !errors.Is(err, context.Canceled) || reader.reads != 1 {
		t.Fatalf("cancellation: %v reads=%d", err, reader.reads)
	}
}

type ovmCancelHashReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *ovmCancelHashReader) Read(p []byte) (int, error) {
	r.reads++
	r.cancel()
	p[0] = ' '
	return 1, nil
}

type ovmReadCountingDB struct {
	ethdb.Database
	gets, has int
	failure   error
}

func (d *ovmReadCountingDB) Get(key []byte) ([]byte, error) {
	d.gets++
	if d.failure != nil {
		return nil, d.failure
	}
	return d.Database.Get(key)
}
func (d *ovmReadCountingDB) Has(key []byte) (bool, error) { d.has++; return d.Database.Has(key) }

func TestOVMIndexSingleReadAndFailures(t *testing.T) {
	index := testAllocIndex(t)
	counted := &ovmReadCountingDB{Database: index.db}
	index.db = counted
	for _, value := range [][]byte{nil, {1}} {
		if err := index.put('f', []byte{1}, value); err != nil {
			t.Fatal(err)
		}
		if err := index.flush(); err != nil {
			t.Fatal(err)
		}
		got, ok, err := index.get('f', []byte{1})
		if err != nil || !ok || !bytes.Equal(got, value) {
			t.Fatalf("present entry: %x %t %v", got, ok, err)
		}
	}
	if _, ok, err := index.get('f', []byte{2}); err != nil || ok {
		t.Fatalf("missing entry: %t %v", ok, err)
	}
	injected := errors.New("index I/O error")
	counted.failure = injected
	if _, _, err := index.get('f', []byte{2}); !errors.Is(err, injected) {
		t.Fatalf("read failure lost: %v", err)
	}
	if counted.gets != 4 || counted.has != 0 {
		t.Fatalf("redundant lookups: get=%d has=%d", counted.gets, counted.has)
	}
}

func TestOVMAccountReaderWindowAndEOAQueries(t *testing.T) {
	f := newOVMFixture(t, nil)
	source, err := openLegacySource(f.source, 16, 16, newProgressReporter("test", ProgressOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	counted := &ovmReadCountingDB{Database: source.db}
	tdb := triedb.NewDatabase(counted, triedb.HashDefaults)
	defer func() {
		if err := tdb.Close(); err != nil {
			t.Error(err)
		}
	}()
	hash := crypto.Keccak256Hash(f.holders[0][:])
	for range ovmAccountReadWindow {
		if _, err := readOVMAccount(tdb, f.root, hash); err != nil {
			t.Fatal(err)
		}
	}
	freshReads := counted.gets
	counted.gets = 0
	reader := ovmAccountReader{db: tdb, root: f.root}
	for n := range 3 * ovmAccountReadWindow {
		account, err := reader.read(hash)
		if err != nil {
			t.Fatal(err)
		}
		if !account.Balance.IsZero() || account.Nonce != 1 {
			t.Fatal("cached account aliased a modified result")
		}
		account.Balance.SetUint64(100)
		if (n+1)%ovmAccountReadWindow == 0 && (reader.trie != nil || reader.reads != 0) {
			t.Fatal("account reader exceeded window")
		}
	}
	if counted.gets >= freshReads {
		t.Fatalf("bounded reuse did not reduce reads: %d vs %d", counted.gets, freshReads)
	}
	absent, err := reader.read(common.Hash{0xaa})
	if err != nil || !absent.Balance.IsZero() || absent.Root != types.EmptyRootHash {
		t.Fatalf("absent account: %+v %v", absent, err)
	}
	index := testAllocIndex(t)
	indexDB := &ovmReadCountingDB{Database: index.db, failure: errors.New("membership lookup failure")}
	index.db = indexDB
	transformer := ovmTransformer{trieDB: tdb, root: f.root, index: index, limiter: newMigrateWorkLimiter(2)}
	batch := ovmBalanceBatch{ctx: t.Context(), transformer: &transformer}
	job := ovmBalanceJob{address: f.holders[0], hash: hash, value: *uint256.NewInt(1)}
	if err := batch.inspect(&job); err != nil || indexDB.gets != 0 {
		t.Fatalf("EOA queried membership: %v calls=%d", err, indexDB.gets)
	}
	job = ovmBalanceJob{address: f.holders[1], hash: crypto.Keccak256Hash(f.holders[1][:]), value: *uint256.NewInt(1)}
	if err := batch.inspect(&job); !errors.Is(err, indexDB.failure) {
		t.Fatalf("contract lookup failure lost: %v", err)
	}
}

func TestOVMVerifyReplaysWithoutFinalScratchTarget(t *testing.T) {
	for _, target := range targetTestCases() {
		t.Run(target.name(), func(t *testing.T) {
			f := newOVMFixture(t, nil)
			opts := f.options(t, target.engine, target.scheme, 4)
			opts.OVM.GenesisAlloc = writeAllocFile(t, fmt.Sprintf(`{"%s":{"nonce":0}}`, f.holders[0]))
			result, err := Migrate(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			before := directoryContentDigest(t, opts.Output)
			checked := false
			hook := ovmPhaseWriter{phase: "replay_converted_state", act: func() {
				matches, err := filepath.Glob(filepath.Join(filepath.Dir(opts.Output), ".l2state-verify-*"))
				if err != nil || len(matches) != 1 {
					t.Fatalf("scratch: %v %v", matches, err)
				}
				if _, err := os.Lstat(filepath.Join(matches[0], artifactDatabaseDirName)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("replay materialized a final target: %v", err)
				}
				checked = true
			}}
			var output bytes.Buffer
			writer := io.MultiWriter(&hook, &output)
			report, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDir: filepath.Dir(opts.Output), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 4, OVM: opts.OVM, Progress: ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(writer, false))}})
			if err != nil {
				t.Fatal(err)
			}
			report.VerifiedAt = requireOVMReport(t, result).VerifiedAt
			if !sameOVMReport(report, *requireOVMReport(t, result)) {
				t.Fatal("validation-only replay changed evidence")
			}
			if !checked || bytes.Contains(output.Bytes(), []byte("phase=build_converted_state")) {
				t.Fatal("verification rebuilt a final artifact")
			}
			if before != directoryContentDigest(t, opts.Output) {
				t.Fatal("verification changed artifact")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cancelHook := ovmPhaseWriter{phase: "replay_converted_state", act: cancel}
			_, err = VerifyOVM(ctx, OVMVerifyOptions{TempDir: filepath.Dir(opts.Output), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 4, OVM: opts.OVM, Progress: ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&cancelHook, false))}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("replay cancellation lost: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(opts.Output), ".l2state-verify-*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("replay leaked scratch: %v %v", matches, err)
			}
		})
	}
}

func TestDirectHashAccountHasNoEncodingAllocations(t *testing.T) {
	writer := &directStateWriter{scheme: rawdb.HashScheme, batch: newTrackingBatch()}
	account := types.NewEmptyStateAccount()
	allocs := testing.AllocsPerRun(1000, func() {
		if err := writer.Account(common.Hash{1}, account, nil); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("discarded slim encoding allocated: %f", allocs)
	}
}

func TestOVMBoundedReadersAcrossTargets(t *testing.T) {
	f := newOVMFixtureSized(t, 1200, nil)
	var alloc strings.Builder
	alloc.WriteByte('{')
	for n, address := range f.holders[:128] {
		if n > 0 {
			alloc.WriteByte(',')
		}
		fmt.Fprintf(&alloc, `"%s":{"nonce":123}`, address)
	}
	alloc.WriteByte('}')
	path := writeAllocFile(t, alloc.String())
	expected := ovmReferenceWithAlloc(t, f, func(s *state.StateDB) {
		for _, address := range f.holders[:128] {
			s.SetNonce(address, 123, tracing.NonceChangeUnspecified)
		}
	})
	before := directoryContentDigest(t, f.source)
	for _, target := range targetTestCases() {
		t.Run(target.name(), func(t *testing.T) {
			workers := 2
			if target.scheme == "path" {
				workers = 16
			}
			opts := f.options(t, target.engine, target.scheme, workers)
			opts.OVM.GenesisAlloc = path
			result, err := Migrate(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if requireOVMReport(t, result).Target.Root != expected {
				t.Fatal("reader reuse changed serial StateDB reference root")
			}
			report, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDir: filepath.Dir(opts.Output), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: workers, OVM: opts.OVM})
			if err != nil {
				t.Fatal(err)
			}
			if report.Target != requireOVMReport(t, result).Target {
				t.Fatal("replay counts or root changed")
			}
		})
	}
	if before != directoryContentDigest(t, f.source) {
		t.Fatal("source changed")
	}
}
