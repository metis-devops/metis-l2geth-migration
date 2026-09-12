package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
)

// pruneKeepWriter records physical reads in independent key-owned batches.
// All producers have joined before close/abort; errors are latched even if a
// trie reader discards a Get error.
type pruneKeepWriter struct {
	shards   []pruneKeepShard
	failure  pruneKeepFailure
	cancel   context.CancelFunc
	progress *pruneScanProgress
}

type pruneKeepError struct{ err error }
type pruneKeepFailure struct {
	cause atomic.Pointer[pruneKeepError]
}

func (f *pruneKeepFailure) record(err error) bool {
	return f.cause.CompareAndSwap(nil, &pruneKeepError{err: err})
}
func (f *pruneKeepFailure) load() error {
	if failure := f.cause.Load(); failure != nil {
		return failure.err
	}
	return nil
}

type pruneKeepShard struct {
	mu      sync.Mutex
	batch   ethdb.Batch
	pending map[common.Hash][]byte
}

func newPruneKeepWriter(db ethdb.Database, workers int, cancel context.CancelFunc) *pruneKeepWriter {
	w := &pruneKeepWriter{shards: make([]pruneKeepShard, workers), cancel: cancel}
	for i := range w.shards {
		w.shards[i].batch = db.NewBatchWithSize(ethdb.IdealBatchSize)
		w.shards[i].pending = make(map[common.Hash][]byte)
	}
	return w
}

func (w *pruneKeepWriter) fail(err error) error {
	if err != nil && w.failure.record(err) {
		w.cancel()
	}
	return err
}

func (w *pruneKeepWriter) put(key, value []byte) error {
	if err := w.failure.load(); err != nil {
		return err
	}
	if len(key) != common.HashLength || crypto.Keccak256Hash(value) != common.BytesToHash(key) {
		return w.fail(fmt.Errorf("prune state hash does not match key %x", key))
	}
	hash := common.BytesToHash(key)
	shard := &w.shards[int(hash[0])%len(w.shards)]
	err := w.fail(shard.put(hash, value))
	if err == nil && w.progress != nil {
		w.progress.keys.Add(1)
		w.progress.bytes.Add(uint64(len(key) + len(value)))
	}
	return err
}

func (s *pruneKeepShard) put(hash common.Hash, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.pending[hash]; ok {
		return comparePruneValue(hash[:], previous, value)
	}
	if err := s.batch.Put(hash[:], value); err != nil {
		return err
	}
	s.pending[hash] = bytes.Clone(value)
	if s.batch.ValueSize() >= ethdb.IdealBatchSize {
		return s.flush()
	}
	return nil
}

func comparePruneValue(key, a, b []byte) error {
	if !bytes.Equal(a, b) {
		return fmt.Errorf("prune keep value differs at key %x", key)
	}
	return nil
}

func (s *pruneKeepShard) flush() error {
	if err := s.batch.Write(); err != nil {
		return fmt.Errorf("write prune keep batch: %w", err)
	}
	s.batch.Reset()
	clear(s.pending)
	return nil
}

func (w *pruneKeepWriter) close(ctx context.Context) error {
	defer w.abort()
	if err := w.failure.load(); err != nil {
		return err
	}
	for i := range w.shards {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.shards[i].flush(); err != nil {
			return w.fail(err)
		}
	}
	return nil
}

func (w *pruneKeepWriter) abort() {
	for i := range w.shards {
		w.shards[i].batch.Close()
	}
}

type pruneRecordingDB struct {
	ethdb.Database
	keep *pruneKeepWriter
}

func (d *pruneRecordingDB) Get(key []byte) ([]byte, error) {
	value, err := d.Database.Get(key)
	if err == nil && len(key) == common.HashLength {
		err = d.keep.put(key, value)
	}
	if err != nil {
		return nil, err
	}
	return value, nil
}

func readPruneGenesis(db ethdb.Database) (common.Hash, []byte, error) {
	hash := rawdb.ReadCanonicalHash(db, 0)
	if hash == (common.Hash{}) {
		return common.Hash{}, nil, errors.New("prune requires the genesis canonical mapping in LevelDB")
	}
	blob := rawdb.ReadHeaderRLP(db, hash, 0)
	if crypto.Keccak256Hash(blob) != hash {
		return common.Hash{}, nil, errors.New("invalid or missing genesis header")
	}
	var h types.Header
	if err := rlp.DecodeBytes(blob, &h); err != nil {
		return common.Hash{}, nil, fmt.Errorf("decode prune genesis: %w", err)
	}
	if h.Number == nil || h.Number.Sign() != 0 || h.Root == (common.Hash{}) {
		return common.Hash{}, nil, errors.New("invalid genesis number or state root")
	}
	if h.Root == types.EmptyRootHash {
		return h.Root, nil, nil
	}
	node, err := db.Get(h.Root[:])
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("read genesis root node: %w", err)
	}
	if crypto.Keccak256Hash(node) != h.Root {
		return common.Hash{}, nil, errors.New("genesis root node hash mismatch")
	}
	// Opening the trie validates the root encoding without requiring its descendants.
	if err := validatePruneGenesisRoot(db, h.Root); err != nil {
		return common.Hash{}, nil, err
	}
	return h.Root, node, nil
}
