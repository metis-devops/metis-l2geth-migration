package migration

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

// directKeyScratchSize covers the pinned one-byte rawdb prefix, one owner
// hash, and the maximum 64-byte trie-path capacity. Canonical paths emitted by
// StackTrie are shorter than 64 bytes.
const directKeyScratchSize = 1 + 3*common.HashLength

// directStateWriter persists the trie nodes produced while the legacy source
// is independently rebuilt. Path artifacts also retain the flat state needed
// by geth's path database; hash artifacts never create temporary flat state.
type directStateWriter struct {
	batch      ethdb.Batch
	scheme     string
	flushAt    int
	closed     bool
	keyScratch [directKeyScratchSize]byte
}

// keyBytes assembles a key in writer-owned storage. Target batches copy keys
// before Put or Delete returns, so the next operation can reuse the scratch
// even when the batch is retained for a later flush.
func (w *directStateWriter) keyBytes(prefix, middle, suffix []byte) []byte {
	key := w.keyScratch[:0]
	key = append(key, prefix...)
	key = append(key, middle...)
	key = append(key, suffix...)
	return key
}

func newDirectStateWriter(db ethdb.Database, scheme string) *directStateWriter {
	return newDirectStateWriterWithFlushLimit(db, scheme, ethdb.IdealBatchSize)
}

// newDeferredDirectStateWriter retains every write in its batch until Close.
// It is used for bounded storage probes whose writes must be discardable when
// the trie crosses the partitioning threshold.
func newDeferredDirectStateWriter(db ethdb.Database, scheme string) *directStateWriter {
	return newDirectStateWriterWithFlushLimit(db, scheme, 0)
}

func newDirectStateWriterWithFlushLimit(db ethdb.Database, scheme string, flushAt int) *directStateWriter {
	batch := db.NewBatchWithSize(ethdb.IdealBatchSize)
	return &directStateWriter{
		batch:   batch,
		scheme:  scheme,
		flushAt: flushAt,
	}
}

func (w *directStateWriter) Account(hash common.Hash, account *types.StateAccount, _ []byte) error {
	return w.writeFlatAccount(hash, types.SlimAccountRLP(*account))
}

func (w *directStateWriter) AccountFromBundle(hash common.Hash, _ *types.StateAccount, _, slimRLP []byte) error {
	return w.writeFlatAccount(hash, slimRLP)
}

func (w *directStateWriter) writeFlatAccount(hash common.Hash, slimRLP []byte) error {
	if w.scheme == rawdb.HashScheme {
		return nil
	}
	key := w.keyBytes(rawdb.SnapshotAccountPrefix, hash[:], nil)
	if err := w.batch.Put(key, slimRLP); err != nil {
		return fmt.Errorf("write flat account %s: %w", hash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) Storage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	if w.scheme == rawdb.HashScheme {
		return nil
	}
	key := w.keyBytes(rawdb.SnapshotStoragePrefix, accountHash[:], slotHash[:])
	if err := w.batch.Put(key, valueRLP); err != nil {
		return fmt.Errorf("write flat account %s slot %s: %w", accountHash, slotHash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) Code(_ common.Hash, codeHash common.Hash, code []byte) error {
	key := w.keyBytes(rawdb.CodePrefix, codeHash[:], nil)
	if err := w.batch.Put(key, code); err != nil {
		return fmt.Errorf("write code %s: %w", codeHash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) TrieNode(owner common.Hash, path []byte, hash common.Hash, blob []byte) error {
	key, err := w.trieNodeKey(owner, path, hash)
	if err != nil {
		return err
	}
	if err := w.batch.Put(key, blob); err != nil {
		return fmt.Errorf("write %s trie node owner=%s path=%x hash=%s: %w", w.scheme, owner, path, hash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) DeleteTrieNode(owner common.Hash, path []byte, hash common.Hash) error {
	key, err := w.trieNodeKey(owner, path, hash)
	if err != nil {
		return err
	}
	if err := w.batch.Delete(key); err != nil {
		return fmt.Errorf("delete %s trie node owner=%s path=%x hash=%s: %w", w.scheme, owner, path, hash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) trieNodeKey(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	switch w.scheme {
	case rawdb.HashScheme:
		return w.keyBytes(nil, hash[:], nil), nil
	case rawdb.PathScheme:
		if owner == (common.Hash{}) {
			return w.keyBytes(rawdb.TrieNodeAccountPrefix, path, nil), nil
		}
		return w.keyBytes(rawdb.TrieNodeStoragePrefix, owner[:], path), nil
	default:
		return nil, fmt.Errorf("unknown state scheme %q", w.scheme)
	}
}

func (w *directStateWriter) flushIfNeeded() error {
	if w.flushAt <= 0 || w.batch.ValueSize() < w.flushAt {
		return nil
	}
	if err := w.batch.Write(); err != nil {
		return fmt.Errorf("flush directly generated state: %w", err)
	}
	w.batch.Reset()
	return nil
}

func (w *directStateWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.batch.Close()
	if err := w.batch.Write(); err != nil {
		return fmt.Errorf("flush final directly generated state: %w", err)
	}
	return nil
}

// CloseContext flushes the final batch only while ctx remains active. A
// cancellation immediately before the flush discards the pending batch.
func (w *directStateWriter) CloseContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		w.Abort()
		return err
	}
	return w.Close()
}

// Abort closes the writer without flushing its pending batch. It is
// idempotent so failure and deferred-cleanup paths may both call it.
func (w *directStateWriter) Abort() {
	if w == nil || w.closed {
		return
	}
	w.closed = true
	w.batch.Close()
}

var _ StateVisitor = (*directStateWriter)(nil)
var _ bundleAccountVisitor = (*directStateWriter)(nil)
var _ trieNodeSink = (*directStateWriter)(nil)
