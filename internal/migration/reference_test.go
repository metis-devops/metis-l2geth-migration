package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestBundleTrieNodeWriteErrorPropagates(t *testing.T) {
	fixture := buildLegacyFixture(t)
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata, Output: bundleDir, Compression: bundle.CompressionNone, CacheMB: 16, Handles: 16,
	}); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected bundle trie-node write failure")
	_, err := scanBundle(context.Background(), bundleDir, bundleScanOptions{TrieNodes: failingTrieNodeSink{err: injected}}, nil)
	if !errors.Is(err, injected) {
		t.Fatalf("bundle scan returned %v, want %v", err, injected)
	}
}

type failingTrieNodeSink struct{ err error }

func (s failingTrieNodeSink) TrieNode(common.Hash, []byte, common.Hash, []byte) error { return s.err }

func TestPortableImportMatchesGenerateTrieReference(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	for _, compression := range []string{bundle.CompressionNone, bundle.CompressionZstd} {
		bundleDir := filepath.Join(root, "bundle-"+compression)
		exported, err := Export(context.Background(), ExportOptions{
			SourceChaindata: fixture.chaindata, Output: bundleDir, Compression: compression, CacheMB: 16, Handles: 16,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
			t.Run(compression+"/"+scheme, func(t *testing.T) {
				artifact := filepath.Join(root, "direct-"+compression+"-"+scheme)
				if _, err := Import(context.Background(), ImportOptions{
					Bundle: bundleDir, Output: artifact, Scheme: scheme, CacheMB: 16, Handles: 16,
				}); err != nil {
					t.Fatal(err)
				}
				reference := filepath.Join(root, "reference-"+compression+"-"+scheme)
				_ = buildGenerateTrieReference(t, bundleDir, reference, scheme, exported.Manifest.Source)
				assertLogicalDatabaseEqual(t, filepath.Join(artifact, "db"), reference)
			})
		}
	}
}

func buildGenerateTrieReference(t testing.TB, bundleDir, dbPath, scheme string, source bundle.SourceEvidence) BundleResult {
	t.Helper()
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}
	kv, err := pebble.New(dbPath, 16, 16, "generate-trie-reference", false)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	closed := false
	defer func() {
		if !closed {
			if err := disk.Close(); err != nil {
				t.Errorf("close reference database: %v", err)
			}
		}
	}()
	writer := newReferenceFlatWriter(disk)
	result, err := ScanBundle(context.Background(), bundleDir, writer)
	if err != nil {
		writer.Abort()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	stats, err := triedb.GenerateTrie(disk, scheme, result.State.Root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != int64(result.State.Counts.Accounts) || stats.Updated != 0 || stats.Deleted != 0 {
		t.Fatalf("unexpected reference generation stats: %+v", stats)
	}
	if scheme == rawdb.HashScheme {
		if err := removeFlatStateForTest(context.Background(), disk); err != nil {
			t.Fatal(err)
		}
	} else if err := adoptPathStateDatabase(context.Background(), disk, result.State.Root); err != nil {
		t.Fatal(err)
	}
	if err := writeHeadMetadata(disk, source); err != nil {
		t.Fatal(err)
	}
	if err := disk.SyncKeyValue(); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	return result
}

type referenceFlatWriter struct {
	batch  ethdb.Batch
	closed bool
}

func newReferenceFlatWriter(db ethdb.Database) *referenceFlatWriter {
	return &referenceFlatWriter{batch: db.NewBatchWithSize(ethdb.IdealBatchSize)}
}

func (w *referenceFlatWriter) Account(hash common.Hash, account *types.StateAccount, _ []byte) error {
	return w.put(prefixedKey(rawdb.SnapshotAccountPrefix, hash[:]), types.SlimAccountRLP(*account))
}

func (w *referenceFlatWriter) Storage(accountHash, slotHash common.Hash, value []byte) error {
	key := make([]byte, 0, len(rawdb.SnapshotStoragePrefix)+2*common.HashLength)
	key = append(key, rawdb.SnapshotStoragePrefix...)
	key = append(key, accountHash[:]...)
	key = append(key, slotHash[:]...)
	return w.put(key, value)
}

func (w *referenceFlatWriter) Code(_ common.Hash, hash common.Hash, code []byte) error {
	return w.put(prefixedKey(rawdb.CodePrefix, hash[:]), code)
}

func (w *referenceFlatWriter) put(key, value []byte) error {
	if err := w.batch.Put(key, value); err != nil {
		return err
	}
	if w.batch.ValueSize() >= ethdb.IdealBatchSize {
		if err := w.batch.Write(); err != nil {
			return err
		}
		w.batch.Reset()
	}
	return nil
}

func (w *referenceFlatWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.batch.Close()
	return w.batch.Write()
}

func (w *referenceFlatWriter) Abort() {
	if w.closed {
		return
	}
	w.closed = true
	w.batch.Close()
}

func assertLogicalDatabaseEqual(t *testing.T, leftPath, rightPath string) {
	t.Helper()
	left := readLogicalDatabase(t, leftPath, "left")
	right := readLogicalDatabase(t, rightPath, "right")
	if len(left) != len(right) {
		t.Fatalf("logical database entry count differs: left=%d right=%d", len(left), len(right))
	}
	for index := range left {
		if !bytes.Equal(left[index].key, right[index].key) || !bytes.Equal(left[index].value, right[index].value) {
			t.Fatalf("logical database entry %d differs: left=%x=%x right=%x=%x", index,
				left[index].key, left[index].value, right[index].key, right[index].value)
		}
	}
}

type logicalEntry struct {
	key   []byte
	value []byte
}

func readLogicalDatabase(t *testing.T, path, namespace string) []logicalEntry {
	t.Helper()
	kv, err := pebble.New(path, 16, 16, namespace, true)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close %s logical database: %v", namespace, err)
		}
	}()
	it := db.NewIterator(nil, nil)
	defer it.Release()
	var entries []logicalEntry
	for it.Next() {
		entries = append(entries, logicalEntry{key: bytes.Clone(it.Key()), value: bytes.Clone(it.Value())})
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	return entries
}

var _ StateVisitor = (*referenceFlatWriter)(nil)

// removeFlatStateForTest is retained only for GenerateTrie reference fixtures.
// Production import never creates temporary flat state.
func removeFlatStateForTest(ctx context.Context, db ethdb.Database) error {
	for _, target := range []struct {
		prefix    []byte
		keyLength int
	}{
		{prefix: rawdb.SnapshotAccountPrefix, keyLength: len(rawdb.SnapshotAccountPrefix) + common.HashLength},
		{prefix: rawdb.SnapshotStoragePrefix, keyLength: len(rawdb.SnapshotStoragePrefix) + 2*common.HashLength},
	} {
		it := db.NewIterator(target.prefix, nil)
		batch := db.NewBatchWithSize(ethdb.IdealBatchSize)
		for it.Next() {
			if err := ctx.Err(); err != nil {
				it.Release()
				batch.Close()
				return err
			}
			if len(it.Key()) == target.keyLength {
				if err := batch.Delete(append([]byte(nil), it.Key()...)); err != nil {
					it.Release()
					batch.Close()
					return fmt.Errorf("delete reference flat-state key: %w", err)
				}
			}
		}
		if err := it.Error(); err != nil {
			it.Release()
			batch.Close()
			return fmt.Errorf("iterate reference flat state: %w", err)
		}
		it.Release()
		if err := batch.Write(); err != nil {
			batch.Close()
			return fmt.Errorf("flush reference flat-state deletion: %w", err)
		}
		batch.Close()
	}
	return nil
}
