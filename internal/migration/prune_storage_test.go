package migration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/table"
)

func TestPruneDryRunDoesNotCreateMissingLock(t *testing.T) {
	opts, _ := pruneTestOptions(t)
	lock := filepath.Join(opts.Chaindata, "LOCK")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	before := directoryContentDigest(t, opts.Chaindata)
	opts.DryRun = true
	result, err := Prune(context.Background(), opts)
	if !errors.Is(err, os.ErrNotExist) || result.Verified || !strings.Contains(err.Error(), "LOCK") {
		t.Fatalf("missing LOCK accepted: %+v %v", result, err)
	}
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run created LOCK: %v", err)
	}
	if after := directoryContentDigest(t, opts.Chaindata); after != before {
		t.Fatal("rejected dry-run changed source files")
	}
	assertNoPruneTemps(t, filepath.Dir(opts.Chaindata))
}

func TestPruneReadOnlyOpenRequiresRegularLock(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			opts, _ := pruneTestOptions(t)
			lock := filepath.Join(opts.Chaindata, "LOCK")
			if err := os.Remove(lock); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "directory" {
				err = os.Mkdir(lock, 0700)
			} else {
				err = os.Symlink("CURRENT", lock)
			}
			if err != nil {
				t.Fatal(err)
			}
			db, err := openPruneDatabase(opts.Chaindata, 16, 16, true)
			if err == nil {
				if closeErr := db.close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("non-regular LOCK accepted")
			}
			if !strings.Contains(err.Error(), "regular LevelDB LOCK") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestPruneCompactionPreservesBloomFilters(t *testing.T) {
	opts, _ := pruneTestOptions(t)
	mutatePruneFixture(t, opts.Chaindata, func(db ethdb.Database) {
		if err := db.Compact(nil, nil); err != nil {
			t.Fatal(err)
		}
	})
	assertPruneTablesUseBloom(t, opts.Chaindata)
	opts.Compact = true
	result, err := Prune(context.Background(), opts)
	if err != nil || !result.Verified || !result.Compacted || result.Deleted.Keys == 0 {
		t.Fatalf("prune/compact failed: %+v %v", result, err)
	}
	assertPruneTablesUseBloom(t, opts.Chaindata)
	// Reopening must also configure the reader policy to use existing filters.
	db, err := openPruneDatabase(opts.Chaindata, 16, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.close(); err != nil {
			t.Error(err)
		}
	}()
	if db.options.Filter == nil || db.options.Filter.Name() != filter.NewBloomFilter(10).Name() {
		t.Fatal("prune reader does not enable the legacy Bloom filter policy")
	}
}

func assertPruneTablesUseBloom(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".ldb") || strings.HasSuffix(entry.Name(), ".sst") {
			assertPruneTableUsesBloom(t, filepath.Join(path, entry.Name()))
			count++
		}
	}
	if count == 0 {
		t.Fatal("fixture has no SSTs to check")
	}
}

// A matching reader policy is consulted only when an SST actually contains a
// filter block. Returning true keeps the raw internal-key lookup unfiltered;
// the call count proves that the stored Bloom metadata was recognized/used.
type pruneBloomProbe struct {
	filter.Filter
	calls int
}

func (p *pruneBloomProbe) Contains([]byte, []byte) bool { p.calls++; return true }

func assertPruneTableUsesBloom(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	probe := &pruneBloomProbe{Filter: filter.NewBloomFilter(10)}
	// Give the table reader only ReaderAt ownership, so its Release cannot
	// swallow the underlying file-close error checked above.
	reader, err := table.NewReader(struct{ io.ReaderAt }{file}, info.Size(), storage.FileDesc{Type: storage.TypeTable}, nil, nil, &opt.Options{Filter: probe, Strict: opt.StrictAll})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Release()
	iter := reader.NewIterator(nil, nil)
	if !iter.First() {
		iter.Release()
		t.Fatalf("cannot read SST %s: %v", path, iter.Error())
	}
	key, want := bytes.Clone(iter.Key()), bytes.Clone(iter.Value())
	iter.Release()
	gotKey, got, err := reader.Find(key, true, nil)
	if err != nil || !bytes.Equal(gotKey, key) || !bytes.Equal(got, want) {
		t.Fatalf("SST lookup failed: %s %v", path, err)
	}
	if probe.calls == 0 {
		t.Fatalf("SST has no usable legacy Bloom filter: %s", path)
	}
}
