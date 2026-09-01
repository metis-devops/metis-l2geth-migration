package migration

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	gethpebble "github.com/ethereum/go-ethereum/ethdb/pebble"
)

func TestDirectStateWriterFlatKeysMatchRawDB(t *testing.T) {
	accountHash := common.HexToHash("0xa1")
	slotHash := common.HexToHash("0xb2")
	codeHash := common.HexToHash("0xc3")
	value := []byte{0x01, 0x02}
	tests := []struct {
		name      string
		reference func(ethdb.KeyValueWriter)
		write     func(*directStateWriter) error
	}{
		{
			name: "account",
			reference: func(writer ethdb.KeyValueWriter) {
				rawdb.WriteAccountSnapshot(writer, accountHash, value)
			},
			write: func(writer *directStateWriter) error {
				return writer.writeFlatAccount(accountHash, value)
			},
		},
		{
			name: "storage",
			reference: func(writer ethdb.KeyValueWriter) {
				rawdb.WriteStorageSnapshot(writer, accountHash, slotHash, value)
			},
			write: func(writer *directStateWriter) error {
				return writer.Storage(accountHash, slotHash, value)
			},
		},
		{
			name: "code",
			reference: func(writer ethdb.KeyValueWriter) {
				rawdb.WriteCode(writer, codeHash, value)
			},
			write: func(writer *directStateWriter) error {
				return writer.Code(accountHash, codeHash, value)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected := newCapturedKeyValueWriter()
			test.reference(expected)
			batch := newCapturedBatch()
			writer := &directStateWriter{batch: batch, scheme: rawdb.PathScheme}
			if err := test.write(writer); err != nil {
				t.Fatal(err)
			}
			assertCapturedOperationsEqual(t, batch.operations, expected.operations)
		})
	}
}

func TestDirectStateWriterTrieKeysMatchRawDB(t *testing.T) {
	owners := []common.Hash{{}, common.HexToHash("0xa1")}
	paths := [][]byte{
		nil,
		{0x01, 0x0f, 0x03},
		bytes.Repeat([]byte{0x0f}, 2*common.HashLength-1),
	}
	hash := common.HexToHash("0xb2")
	blob := []byte{0xf8, 0x01}
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		for ownerIndex, owner := range owners {
			for pathIndex, path := range paths {
				name := fmt.Sprintf("%s/owner=%d/path=%d", scheme, ownerIndex, pathIndex)
				t.Run(name, func(t *testing.T) {
					for _, deleting := range []bool{false, true} {
						expected := newCapturedKeyValueWriter()
						batch := newCapturedBatch()
						writer := &directStateWriter{batch: batch, scheme: scheme}
						var err error
						if deleting {
							rawdb.DeleteTrieNode(expected, owner, path, hash, scheme)
							err = writer.DeleteTrieNode(owner, path, hash)
						} else {
							rawdb.WriteTrieNode(expected, owner, path, hash, blob, scheme)
							err = writer.TrieNode(owner, path, hash, blob)
						}
						if err != nil {
							t.Fatal(err)
						}
						assertCapturedOperationsEqual(t, batch.operations, expected.operations)
					}
				})
			}
		}
	}
}

func TestDeferredDirectStateWriterCopiesReusedKeys(t *testing.T) {
	kv, err := gethpebble.New(t.TempDir(), 16, 16, "direct-writer-reused-keys", false)
	if err != nil {
		t.Fatal(err)
	}
	target := rawdb.NewDatabase(kv)
	t.Cleanup(func() {
		if err := target.Close(); err != nil {
			t.Errorf("close target: %v", err)
		}
	})
	reference := rawdb.NewDatabase(memorydb.New())
	t.Cleanup(func() {
		if err := reference.Close(); err != nil {
			t.Errorf("close reference: %v", err)
		}
	})

	writer := newDeferredDirectStateWriter(target, rawdb.PathScheme)
	accountA := common.HexToHash("0xa1")
	accountB := common.HexToHash("0xa2")
	slotA := common.HexToHash("0xb1")
	slotB := common.HexToHash("0xb2")
	slotC := common.HexToHash("0xb3")
	codeA := common.HexToHash("0xc1")
	codeB := common.HexToHash("0xc2")
	accountPathA := []byte{0x01}
	accountPathB := []byte{0x02, 0x03}
	storagePathA := []byte{0x04}
	storagePathB := []byte{0x05, 0x06}
	deletedPath := []byte{0x07}
	nodeA := common.HexToHash("0xd1")
	nodeB := common.HexToHash("0xd2")

	writes := []struct {
		direct    func() error
		reference func()
	}{
		{func() error { return writer.writeFlatAccount(accountA, []byte{0x01}) }, func() { rawdb.WriteAccountSnapshot(reference, accountA, []byte{0x01}) }},
		{func() error { return writer.writeFlatAccount(accountB, []byte{0x02}) }, func() { rawdb.WriteAccountSnapshot(reference, accountB, []byte{0x02}) }},
		{func() error { return writer.Storage(accountA, slotA, []byte{0x11}) }, func() { rawdb.WriteStorageSnapshot(reference, accountA, slotA, []byte{0x11}) }},
		{func() error { return writer.Storage(accountA, slotB, []byte{0x12}) }, func() { rawdb.WriteStorageSnapshot(reference, accountA, slotB, []byte{0x12}) }},
		{func() error { return writer.Storage(accountB, slotC, []byte{0x13}) }, func() { rawdb.WriteStorageSnapshot(reference, accountB, slotC, []byte{0x13}) }},
		{func() error { return writer.Code(accountA, codeA, []byte{0x21}) }, func() { rawdb.WriteCode(reference, codeA, []byte{0x21}) }},
		{func() error { return writer.Code(accountB, codeB, []byte{0x22}) }, func() { rawdb.WriteCode(reference, codeB, []byte{0x22}) }},
		{func() error { return writer.TrieNode(common.Hash{}, accountPathA, nodeA, []byte{0x31}) }, func() {
			rawdb.WriteTrieNode(reference, common.Hash{}, accountPathA, nodeA, []byte{0x31}, rawdb.PathScheme)
		}},
		{func() error { return writer.TrieNode(common.Hash{}, accountPathB, nodeB, []byte{0x32}) }, func() {
			rawdb.WriteTrieNode(reference, common.Hash{}, accountPathB, nodeB, []byte{0x32}, rawdb.PathScheme)
		}},
		{func() error { return writer.TrieNode(accountA, storagePathA, nodeA, []byte{0x41}) }, func() { rawdb.WriteTrieNode(reference, accountA, storagePathA, nodeA, []byte{0x41}, rawdb.PathScheme) }},
		{func() error { return writer.TrieNode(accountB, storagePathB, nodeB, []byte{0x42}) }, func() { rawdb.WriteTrieNode(reference, accountB, storagePathB, nodeB, []byte{0x42}, rawdb.PathScheme) }},
		{func() error { return writer.TrieNode(accountA, deletedPath, nodeA, []byte{0x43}) }, func() { rawdb.WriteTrieNode(reference, accountA, deletedPath, nodeA, []byte{0x43}, rawdb.PathScheme) }},
		{func() error { return writer.DeleteTrieNode(accountA, deletedPath, nodeA) }, func() { rawdb.DeleteTrieNode(reference, accountA, deletedPath, nodeA, rawdb.PathScheme) }},
	}
	for index, write := range writes {
		if err := write.direct(); err != nil {
			t.Fatalf("direct write %d: %v", index, err)
		}
		write.reference()
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	assertKeyValueDatabasesEqual(t, target, reference)
}

func TestDirectStateWriterColdKeysUseEmbeddedScratch(t *testing.T) {
	account := common.HexToHash("0xa1")
	slot := common.HexToHash("0xb2")
	hash := common.HexToHash("0xc3")
	maxPath := bytes.Repeat([]byte{0x0f}, 2*common.HashLength-1)
	tests := []struct {
		name           string
		prefix, middle []byte
		suffix         []byte
	}{
		{"flat-account", rawdb.SnapshotAccountPrefix, account[:], nil},
		{"flat-storage", rawdb.SnapshotStoragePrefix, account[:], slot[:]},
		{"code", rawdb.CodePrefix, hash[:], nil},
		{"hash-trie", nil, hash[:], nil},
		{"account-path-trie", rawdb.TrieNodeAccountPrefix, maxPath, nil},
		{"storage-path-trie", rawdb.TrieNodeStoragePrefix, account[:], maxPath},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(1_000, func() {
				var writer directStateWriter
				key := writer.keyBytes(test.prefix, test.middle, test.suffix)
				if len(key) == 0 || &key[0] != &writer.keyScratch[0] {
					panic("key did not use embedded scratch")
				}
			})
			if allocs != 0 {
				t.Fatalf("cold key construction allocated %.2f objects per call", allocs)
			}
		})
	}
}

func TestDirectStateWriterKeyWritesDoNotAllocate(t *testing.T) {
	account := common.HexToHash("0xa1")
	otherAccount := common.HexToHash("0xa2")
	slot := common.HexToHash("0xb2")
	hash := common.HexToHash("0xc3")
	value := []byte{0x01}
	path := []byte{0x01, 0x02}
	flatStorageOwner := account
	storageTrieOwner := account
	tests := []struct {
		name   string
		scheme string
		run    func(*directStateWriter) error
	}{
		{"flat-account", rawdb.PathScheme, func(writer *directStateWriter) error { return writer.writeFlatAccount(account, value) }},
		{"flat-storage", rawdb.PathScheme, func(writer *directStateWriter) error {
			if flatStorageOwner == account {
				flatStorageOwner = otherAccount
			} else {
				flatStorageOwner = account
			}
			return writer.Storage(flatStorageOwner, slot, value)
		}},
		{"code", rawdb.PathScheme, func(writer *directStateWriter) error { return writer.Code(account, hash, value) }},
		{"hash-trie-put", rawdb.HashScheme, func(writer *directStateWriter) error { return writer.TrieNode(account, path, hash, value) }},
		{"hash-trie-delete", rawdb.HashScheme, func(writer *directStateWriter) error { return writer.DeleteTrieNode(account, path, hash) }},
		{"account-path-trie", rawdb.PathScheme, func(writer *directStateWriter) error { return writer.TrieNode(common.Hash{}, path, hash, value) }},
		{"storage-path-trie", rawdb.PathScheme, func(writer *directStateWriter) error {
			if storageTrieOwner == account {
				storageTrieOwner = otherAccount
			} else {
				storageTrieOwner = account
			}
			return writer.TrieNode(storageTrieOwner, path, hash, value)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer := &directStateWriter{batch: newTrackingBatch(), scheme: test.scheme}
			allocs := testing.AllocsPerRun(1_000, func() {
				if err := test.run(writer); err != nil {
					panic(err)
				}
			})
			if allocs != 0 {
				t.Fatalf("reused key write allocated %.2f objects per call", allocs)
			}
		})
	}
}

func TestDirectStateWriterHashSchemeDoesNotWriteFlatKeys(t *testing.T) {
	batch := newTrackingBatch()
	writer := &directStateWriter{batch: batch, scheme: rawdb.HashScheme}
	if err := writer.writeFlatAccount(common.HexToHash("0xa1"), []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Storage(common.HexToHash("0xa1"), common.HexToHash("0xb2"), []byte{0x02}); err != nil {
		t.Fatal(err)
	}
	if batch.puts != 0 || batch.size != 0 {
		t.Fatalf("hash writer queued flat state: puts=%d size=%d", batch.puts, batch.size)
	}
}

type capturedKeyOperation struct {
	delete bool
	key    []byte
	value  []byte
}

type capturedKeyValueWriter struct {
	operations []capturedKeyOperation
}

func newCapturedKeyValueWriter() *capturedKeyValueWriter {
	return new(capturedKeyValueWriter)
}

func (w *capturedKeyValueWriter) Put(key, value []byte) error {
	w.operations = append(w.operations, capturedKeyOperation{key: bytes.Clone(key), value: bytes.Clone(value)})
	return nil
}

func (w *capturedKeyValueWriter) Delete(key []byte) error {
	w.operations = append(w.operations, capturedKeyOperation{delete: true, key: bytes.Clone(key)})
	return nil
}

type capturedBatch struct {
	*capturedKeyValueWriter
	size int
}

func newCapturedBatch() *capturedBatch {
	return &capturedBatch{capturedKeyValueWriter: newCapturedKeyValueWriter()}
}

func (b *capturedBatch) Put(key, value []byte) error {
	b.size += len(key) + len(value)
	return b.capturedKeyValueWriter.Put(key, value)
}

func (b *capturedBatch) Delete(key []byte) error {
	b.size += len(key)
	return b.capturedKeyValueWriter.Delete(key)
}

func (b *capturedBatch) DeleteRange(start, end []byte) error {
	b.size += len(start) + len(end)
	return nil
}

func (b *capturedBatch) ValueSize() int { return b.size }
func (b *capturedBatch) Write() error   { return nil }
func (b *capturedBatch) Reset()         { b.size = 0 }
func (b *capturedBatch) Close()         {}

func (b *capturedBatch) Replay(writer ethdb.KeyValueWriter) error {
	for _, operation := range b.operations {
		var err error
		if operation.delete {
			err = writer.Delete(operation.key)
		} else {
			err = writer.Put(operation.key, operation.value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func assertCapturedOperationsEqual(t *testing.T, have, want []capturedKeyOperation) {
	t.Helper()
	if len(have) != len(want) {
		t.Fatalf("operation count differs: have %d want %d", len(have), len(want))
	}
	for index := range want {
		if have[index].delete != want[index].delete ||
			!bytes.Equal(have[index].key, want[index].key) ||
			!bytes.Equal(have[index].value, want[index].value) {
			t.Fatalf("operation %d differs: have %+v want %+v", index, have[index], want[index])
		}
	}
}

func assertKeyValueDatabasesEqual(t *testing.T, have, want ethdb.Database) {
	t.Helper()
	haveEntries := collectDatabaseEntries(t, have)
	wantEntries := collectDatabaseEntries(t, want)
	if len(haveEntries) != len(wantEntries) {
		t.Fatalf("database entry count differs: have %d want %d", len(haveEntries), len(wantEntries))
	}
	for index := range wantEntries {
		if !bytes.Equal(haveEntries[index].key, wantEntries[index].key) ||
			!bytes.Equal(haveEntries[index].value, wantEntries[index].value) {
			t.Fatalf("database entry %d differs: have %x=%x want %x=%x", index,
				haveEntries[index].key, haveEntries[index].value,
				wantEntries[index].key, wantEntries[index].value)
		}
	}
}

func collectDatabaseEntries(t *testing.T, db ethdb.Database) []logicalEntry {
	t.Helper()
	iterator := db.NewIterator(nil, nil)
	defer iterator.Release()
	var entries []logicalEntry
	for iterator.Next() {
		entries = append(entries, logicalEntry{key: bytes.Clone(iterator.Key()), value: bytes.Clone(iterator.Value())})
	}
	if err := iterator.Error(); err != nil {
		t.Fatal(err)
	}
	return entries
}

var _ ethdb.Batch = (*capturedBatch)(nil)
