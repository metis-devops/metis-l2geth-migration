package migration

// Frozen test-only serial prune reference captured before the performance refactor.
// Keep this independent of the optimized traversal, writer and scan pipeline.

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// serialKeepWriter records physical reads, not reconstructed trie nodes. The
// account and storage iterators may read concurrently.
type serialKeepWriter struct {
	mu      sync.Mutex
	db      ethdb.Database
	batch   ethdb.Batch
	pending map[common.Hash][]byte
}

func serialNewPruneKeepWriter(db ethdb.Database) *serialKeepWriter {
	return &serialKeepWriter{db: db, batch: db.NewBatch(), pending: make(map[common.Hash][]byte)}
}

func (w *serialKeepWriter) put(key, value []byte) error {
	if len(key) != common.HashLength || crypto.Keccak256Hash(value) != common.BytesToHash(key) {
		return fmt.Errorf("prune state hash does not match key %x", key)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	hash := common.BytesToHash(key)
	if previous, ok := w.pending[hash]; ok {
		return serialComparePruneValue(key, previous, value)
	}
	exists, err := w.db.Has(key)
	if err != nil {
		return err
	}
	if exists {
		previous, err := w.db.Get(key)
		if err != nil {
			return err
		}
		return serialComparePruneValue(key, previous, value)
	}
	if err := w.batch.Put(key, value); err != nil {
		return err
	}
	w.pending[hash] = bytes.Clone(value)
	if w.batch.ValueSize() >= ethdb.IdealBatchSize {
		return w.flush()
	}
	return nil
}

func serialComparePruneValue(key, a, b []byte) error {
	if !bytes.Equal(a, b) {
		return fmt.Errorf("prune keep value differs at key %x", key)
	}
	return nil
}

func (w *serialKeepWriter) flush() error {
	if err := w.batch.Write(); err != nil {
		return fmt.Errorf("write prune keep batch: %w", err)
	}
	w.batch.Reset()
	clear(w.pending)
	return nil
}

func (w *serialKeepWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.batch.Close()
	return w.flush()
}

type serialRecordingDB struct {
	ethdb.Database
	keep *serialKeepWriter
}

func (d *serialRecordingDB) Get(key []byte) ([]byte, error) {
	value, err := d.Database.Get(key)
	if err == nil && len(key) == common.HashLength {
		err = d.keep.put(key, value)
	}
	return value, err
}

func serialTraversePruneState(ctx context.Context, db ethdb.Database, head bundle.Head) (StateResult, error) {
	source := &legacySource{db: db, head: head}
	return source.Traverse(ctx, nil)
}
