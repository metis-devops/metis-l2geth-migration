package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/cockroachdb/pebble/v2/vfs/errorfs"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/log"
)

func TestMemoryPebbleOwnershipReopenAndRanges(t *testing.T) {
	storage := newTemporaryStorage(TempDBMemory)
	path := filepath.Join(t.TempDir(), "never-on-disk")
	db, err := storage.create(path, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	key, value := []byte("prefix/a"), []byte("original")
	if err := db.Put(key, value); err != nil {
		t.Fatal(err)
	}
	key[0] = 'X'
	value[0] = 'X'
	got, err := db.Get([]byte("prefix/a"))
	if err != nil || string(got) != "original" {
		t.Fatalf("owned data: %s %v", got, err)
	}
	got[0] = 'X'
	batch := db.NewBatchWithSize(1024)
	for _, k := range []string{"prefix/b", "prefix/c", "other", "\xff\xff"} {
		if err := batch.Put([]byte(k), []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Delete([]byte("other")); err != nil {
		t.Fatal(err)
	}
	if err := batch.DeleteRange([]byte("prefix/c"), []byte("prefix/d")); err != nil {
		t.Fatal(err)
	}
	ref := memorydb.New()
	if err := batch.Replay(ref); err != nil {
		t.Fatal(err)
	}
	if ok, err := ref.Has([]byte("prefix/c")); err != nil || ok {
		t.Fatalf("replay: %t %v", ok, err)
	}
	if err := ref.Close(); err != nil {
		t.Fatal(err)
	}
	if err := batch.Write(); err != nil {
		t.Fatal(err)
	}
	batch.Reset()
	batch.Close()
	batch.Close()
	prefix := make([]byte, 7, 20)
	copy(prefix, "prefix/")
	prefixBacking := prefix[:cap(prefix)]
	before := bytes.Clone(prefixBacking)
	it := db.NewIterator(prefix, []byte("b"))
	if !it.Next() || string(it.Key()) != "prefix/b" || it.Next() || it.Error() != nil {
		t.Fatal("prefix/start iteration")
	}
	it.Release()
	it.Release()
	if !bytes.Equal(before, prefixBacking) {
		t.Fatal("iterator mutated caller prefix buffer")
	}
	it = db.NewIterator([]byte{255}, nil)
	if !it.Next() || !bytes.Equal(it.Key(), []byte{255, 255}) || it.Next() {
		t.Fatal("all-ff prefix")
	}
	it.Release()
	if err := db.SyncKeyValue(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Get([]byte("prefix/a")); !errors.Is(err, pebble.ErrClosed) {
		t.Fatalf("closed: %v", err)
	}
	ro, err := storage.open(path, 16, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err = ro.Get([]byte("prefix/a"))
	if err != nil || string(got) != "original" {
		t.Fatalf("reopened: %s %v", got, err)
	}
	if err := ro.Put([]byte("bad"), nil); !errors.Is(err, pebble.ErrReadOnly) {
		t.Fatalf("readonly write: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = storage.open(path, 16, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRange([]byte("prefix/"), nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.Has([]byte("prefix/a")); err != nil || ok {
		t.Fatalf("range deletion: %t %v", ok, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("memory DB wrote to disk: %v", err)
	}
	if err := storage.remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.open(path, 16, 16, true); err == nil {
		t.Fatal("deleted memory db reopened")
	}
}

func TestMemoryPebbleConcurrentWritesAndReleaseErrors(t *testing.T) {
	db, err := openMemoryPebble(vfs.NewMem(), "db", 16, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Go(func() {
			b := db.NewBatch()
			defer b.Close()
			for i := range 100 {
				if err := b.Put([]byte{byte(n), byte(i)}, []byte{byte(i)}); err != nil {
					t.Error(err)
					return
				}
			}
			if err := b.Write(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	it := db.NewIterator(nil, nil)
	count := 0
	for it.Next() {
		count++
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	it.Release()
	if count != 800 {
		t.Fatalf("count: %d", count)
	}
	injected := errors.New("release failure")
	db.latch(injected)
	if err := db.Close(); !errors.Is(err, injected) {
		t.Fatalf("release error lost: %v", err)
	}
	it = db.NewIterator(nil, nil)
	if it.Next() || !errors.Is(it.Error(), pebble.ErrClosed) {
		t.Fatal("closed iterator")
	}
	it.Release()
}

func TestMemoryTrieIndexNoPhysicalDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "nonexistent")
	idx, err := newTemporaryTrieNodeIndex(trieNodeIndexOptions{Mode: TempDBMemory, Parent: parent, CacheMB: 1, Handles: 1})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for n := range 20000 {
			var hash common.Hash
			binary.BigEndian.PutUint64(hash[24:], uint64(n))
			if err := idx.Mark(hash); err != nil {
				t.Fatal(err)
			}
		}
	}
	count, err := idx.Count(t.Context())
	if err != nil || count != 20000 {
		t.Fatalf("count: %d %v", count, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := idx.Count(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("physical temp path created")
	}
}

func TestOVMMemoryScratchAndCancellation(t *testing.T) {
	for _, cancelAtPhase := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[cancelAtPhase], func(t *testing.T) {
			f := newOVMFixture(t, nil)
			opts := f.options(t, DBEnginePebble, "hash", 4)
			opts.TempDB = TempDBMemory
			before := directoryContentDigest(t, f.source)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			visited := false
			writer := ovmPhaseWriter{phase: "convert_ovm_balances", act: func() {
				visited = true
				matches, err := filepath.Glob(filepath.Join(filepath.Dir(opts.Output), "."+filepath.Base(opts.Output)+".partial-*", ".ovm-work"))
				if err != nil || len(matches) != 1 {
					t.Errorf("scratch discovery: %v %v", matches, err)
				}
				for _, path := range matches {
					entries, err := os.ReadDir(path)
					if err != nil || len(entries) != 0 {
						t.Errorf("physical memory scratch: %v %v", entries, err)
					}
				}
				if cancelAtPhase {
					cancel()
				}
			}}
			opts.Progress = ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&writer, false))}
			_, err := Migrate(ctx, opts)
			if !visited {
				t.Fatal("conversion phase not reached")
			}
			if cancelAtPhase {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if before != directoryContentDigest(t, f.source) {
				t.Fatal("source changed")
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

func oppositeTempMode(mode TempDBMode) TempDBMode {
	if mode == TempDBMemory {
		return TempDBDisk
	}
	return TempDBMemory
}

func TestMemoryOriginalIndependentReopenRejectsCorruption(t *testing.T) {
	for _, kind := range []string{"missing-root", "corrupt-root", "orphan", "missing-code"} {
		t.Run(kind, func(t *testing.T) {
			f := newOVMFixture(t, nil)
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.TempDB = TempDBMemory
			w := ovmWork{ctx: t.Context(), opts: opts, path: filepath.Join(t.TempDir(), "scratch"), storage: newTemporaryStorage(TempDBMemory), reporter: newProgressReporter("test", ProgressOptions{}), limiter: newMigrateWorkLimiter(2)}
			t.Cleanup(func() {
				if err := w.close(); err != nil {
					t.Error(err)
				}
			})
			if err := w.prepare(); err != nil {
				t.Fatal(err)
			}
			if err := w.migrateOriginalAndHistory(); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "missing-root":
				err = w.base.Delete(w.original.Root[:])
			case "corrupt-root":
				err = w.base.Put(w.original.Root[:], []byte{0xc0})
			case "orphan":
				blob := []byte{0xc0}
				hash := crypto.Keccak256Hash(blob)
				err = w.base.Put(hash[:], blob)
			case "missing-code":
				it := w.base.NewIterator(rawdb.CodePrefix, nil)
				if !it.Next() {
					t.Fatal("fixture has no code")
				}
				key := bytes.Clone(it.Key())
				it.Release()
				err = w.base.Delete(key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := w.reopenOriginal(); err == nil {
				t.Fatal("corrupt original accepted after independent reopening")
			}
		})
	}
}

func TestTempDBInvalidOptionsFailBeforeOpening(t *testing.T) {
	invalid := TempDBMode("unknown")
	errorsToCheck := []error{}
	_, err := Migrate(t.Context(), MigrateOptions{TempDB: invalid})
	errorsToCheck = append(errorsToCheck, err)
	_, err = Import(t.Context(), ImportOptions{TempDB: invalid})
	errorsToCheck = append(errorsToCheck, err)
	_, err = Verify(t.Context(), VerifyOptions{TempDB: invalid})
	errorsToCheck = append(errorsToCheck, err)
	_, err = VerifyDirect(t.Context(), DirectVerifyOptions{TempDB: invalid})
	errorsToCheck = append(errorsToCheck, err)
	_, err = VerifyOVM(t.Context(), OVMVerifyOptions{TempDB: invalid})
	errorsToCheck = append(errorsToCheck, err)
	for _, err := range errorsToCheck {
		if err == nil || !strings.Contains(err.Error(), "temp-db") {
			t.Fatalf("invalid temp mode: %v", err)
		}
	}
}

func TestMemoryPebbleReadAndIteratorErrors(t *testing.T) {
	var fail atomic.Bool
	fs := errorfs.Wrap(vfs.NewMem(), errorfs.InjectorFunc(func(op errorfs.Op) error {
		if fail.Load() && op.Kind == errorfs.OpFileReadAt && strings.HasSuffix(op.Path, ".sst") {
			return errorfs.ErrInjected
		}
		return nil
	}))
	db, err := openMemoryPebble(fs, "db", 16, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	// Force the value into an SST; the reopened reader has an empty block cache.
	if err := db.db.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openMemoryPebble(fs, "db", 16, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if _, err := db.Get([]byte("key")); !errors.Is(err, errorfs.ErrInjected) {
		t.Fatalf("read error lost: %v", err)
	}
	if _, err := db.Has([]byte("key")); !errors.Is(err, errorfs.ErrInjected) {
		t.Fatalf("Has treated failure as absence: %v", err)
	}
	it := db.NewIterator(nil, nil)
	if it.Next() || !errors.Is(it.Error(), errorfs.ErrInjected) {
		t.Fatalf("iterator error lost: %v", it.Error())
	}
	it.Release()
	fail.Store(false)
	if err := db.Close(); !errors.Is(err, errorfs.ErrInjected) {
		t.Fatalf("released iterator error not latched: %v", err)
	}
}

type memoryCloseFailureFS struct {
	vfs.FS
	fail atomic.Bool
}
type memoryCloseFailureFile struct {
	vfs.File
	owner *memoryCloseFailureFS
}

func (f *memoryCloseFailureFS) OpenDir(path string) (vfs.File, error) {
	file, err := f.FS.OpenDir(path)
	if err != nil {
		return nil, err
	}
	return &memoryCloseFailureFile{File: file, owner: f}, nil
}
func (f *memoryCloseFailureFile) Close() error {
	err := f.File.Close()
	if f.owner.fail.Load() {
		return errors.Join(err, errorfs.ErrInjected)
	}
	return err
}
func TestMemoryPebbleCloseFailure(t *testing.T) {
	fs := &memoryCloseFailureFS{FS: vfs.NewMem()}
	db, err := openMemoryPebble(fs, "db", 16, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	fs.fail.Store(true)
	if err := db.Close(); !errors.Is(err, errorfs.ErrInjected) {
		t.Fatalf("close error lost: %v", err)
	}
}
