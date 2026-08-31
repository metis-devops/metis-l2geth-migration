package migration

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

// directStateWriter persists the trie nodes produced while the legacy source
// is independently rebuilt. Path artifacts also retain the flat state needed
// by geth's path database; hash artifacts never create temporary flat state.
type directStateWriter struct {
	batch    ethdb.Batch
	recorder *capturingKeyValueWriter
	scheme   string
	closed   bool
}

func newDirectStateWriter(db ethdb.Database, scheme string) *directStateWriter {
	batch := db.NewBatchWithSize(ethdb.IdealBatchSize)
	return &directStateWriter{
		batch:    batch,
		recorder: &capturingKeyValueWriter{target: batch},
		scheme:   scheme,
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
	key := prefixedKey(rawdb.SnapshotAccountPrefix, hash[:])
	if err := w.batch.Put(key, slimRLP); err != nil {
		return fmt.Errorf("write flat account %s: %w", hash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) Storage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	if w.scheme == rawdb.HashScheme {
		return nil
	}
	key := make([]byte, 0, len(rawdb.SnapshotStoragePrefix)+2*common.HashLength)
	key = append(key, rawdb.SnapshotStoragePrefix...)
	key = append(key, accountHash[:]...)
	key = append(key, slotHash[:]...)
	if err := w.batch.Put(key, valueRLP); err != nil {
		return fmt.Errorf("write flat account %s slot %s: %w", accountHash, slotHash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) Code(_ common.Hash, codeHash common.Hash, code []byte) error {
	key := prefixedKey(rawdb.CodePrefix, codeHash[:])
	if err := w.batch.Put(key, code); err != nil {
		return fmt.Errorf("write code %s: %w", codeHash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) TrieNode(owner common.Hash, path []byte, hash common.Hash, blob []byte) error {
	rawdb.WriteTrieNode(w.recorder, owner, path, hash, blob, w.scheme)
	if err := w.recorder.Err(); err != nil {
		return fmt.Errorf("write %s trie node owner=%s path=%x hash=%s: %w", w.scheme, owner, path, hash, err)
	}
	return w.flushIfNeeded()
}

func (w *directStateWriter) flushIfNeeded() error {
	if err := w.recorder.Err(); err != nil {
		return err
	}
	if w.batch.ValueSize() < ethdb.IdealBatchSize {
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
	if err := w.recorder.Err(); err != nil {
		return err
	}
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

// capturingKeyValueWriter lets rawdb's pinned trie-layout helper remain the
// single source of truth without allowing its log.Crit error path to terminate
// the process. The original error is returned by the enclosing operation.
type capturingKeyValueWriter struct {
	target ethdb.KeyValueWriter
	err    error
}

func (w *capturingKeyValueWriter) Put(key, value []byte) error {
	if w.err == nil {
		w.err = w.target.Put(key, value)
	}
	return nil
}

func (w *capturingKeyValueWriter) Delete(key []byte) error {
	if w.err == nil {
		w.err = w.target.Delete(key)
	}
	return nil
}

func (w *capturingKeyValueWriter) Err() error {
	return w.err
}

var _ StateVisitor = (*directStateWriter)(nil)
var _ bundleAccountVisitor = (*directStateWriter)(nil)
var _ trieNodeSink = (*directStateWriter)(nil)
var _ ethdb.KeyValueWriter = (*capturingKeyValueWriter)(nil)
