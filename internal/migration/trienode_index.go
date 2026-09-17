package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	cpebble "github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
)

const (
	trieNodeIndexTempPrefix = ".l2state-trienodes-"
	trieNodeIndexMaxCacheMB = 16
	trieNodeIndexMaxHandles = 16
)

type trieNodeIndexOptions struct {
	Mode    TempDBMode
	Parent  string
	CacheMB int
	Handles int
}

// temporaryTrieNodeIndex is an exact operation-local set of reachable
// hash-scheme trie-node hashes. Disk storage is the default; explicitly selected
// memory storage retains Pebble files until the index is closed.
type temporaryTrieNodeIndex struct {
	db        *cpebble.DB
	cache     *cpebble.Cache
	nodeBatch *cpebble.Batch
	path      string
	closed    bool
	storage   *temporaryStorage
}

func newTemporaryTrieNodeIndex(opts trieNodeIndexOptions) (*temporaryTrieNodeIndex, error) {
	if err := opts.Mode.validate(); err != nil {
		return nil, err
	}
	storage := newTemporaryStorage(opts.Mode)
	path := "/" + trieNodeIndexTempPrefix + "index"
	var err error
	if storage.fs == nil {
		path, err = os.MkdirTemp(opts.Parent, trieNodeIndexTempPrefix)
	}
	if err != nil {
		return nil, fmt.Errorf("create temporary trie-node index: %w", err)
	}
	cacheMB := boundedTrieNodeIndexResource(opts.CacheMB, trieNodeIndexMaxCacheMB)
	handles := boundedTrieNodeIndexResource(opts.Handles, trieNodeIndexMaxHandles)
	cache := cpebble.NewCache(int64(cacheMB) * 1024 * 1024)
	options := &cpebble.Options{
		FS:                          storage.fs,
		Cache:                       cache,
		MaxOpenFiles:                handles,
		MemTableSize:                uint64(cacheMB * 1024 * 1024 / 4),
		MemTableStopWritesThreshold: 2,
		DisableWAL:                  true,
		NoSyncOnClose:               true,
		CompactionConcurrencyRange:  func() (int, int) { return 1, 1 },
		Levels: [7]cpebble.LevelOptions{
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{},
		},
		FormatMajorVersion: cpebble.FormatFlushableIngest,
		Logger:             temporaryPebbleLogger{},
	}
	options.Experimental.ReadSamplingMultiplier = -1
	db, err := cpebble.Open(path, options)
	if err != nil {
		cache.Unref()
		openErr := fmt.Errorf("open temporary trie-node index: %w", err)
		if removeErr := storage.remove(path); removeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("remove unopened temporary trie-node index: %w", removeErr))
		}
		return nil, openErr
	}
	return &temporaryTrieNodeIndex{db: db, cache: cache, nodeBatch: db.NewBatch(), path: path, storage: storage}, nil
}

func boundedTrieNodeIndexResource(configured, limit int) int {
	if configured <= 0 {
		return limit
	}
	return min(configured, limit)
}

// Mark records a reachable hash-scheme trie node. Repeated Sets collapse to
// one user key when Count iterates the index.
func (i *temporaryTrieNodeIndex) Mark(hash common.Hash) error {
	if i == nil || i.closed {
		return errors.New("temporary trie-node index is closed")
	}
	if err := i.nodeBatch.Set(hash[:], nil, nil); err != nil {
		return fmt.Errorf("store reachable trie node %s in temporary index: %w", hash, err)
	}
	if i.nodeBatch.Len() >= ethdb.IdealBatchSize {
		if err := i.flush(); err != nil {
			return err
		}
	}
	return nil
}

// Count flushes pending node markers and counts unique reachable trie-node
// hashes while honoring cancellation.
func (i *temporaryTrieNodeIndex) Count(ctx context.Context) (count uint64, retErr error) {
	if i == nil || i.closed {
		return 0, errors.New("temporary trie-node index is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := i.flush(); err != nil {
		return 0, err
	}
	it, err := i.db.NewIterWithContext(ctx, &cpebble.IterOptions{})
	if err != nil {
		return 0, fmt.Errorf("open reachable trie-node index iterator: %w", err)
	}
	defer func() {
		if err := it.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close reachable trie-node index iterator: %w", err))
		}
	}()
	for valid := it.First(); valid; valid = it.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		count++
	}
	if err := it.Error(); err != nil {
		return 0, fmt.Errorf("iterate reachable trie-node index: %w", err)
	}
	return count, nil
}

func (i *temporaryTrieNodeIndex) flush() error {
	if i.nodeBatch == nil || i.nodeBatch.Len() == 0 {
		return nil
	}
	if err := i.nodeBatch.Commit(cpebble.NoSync); err != nil {
		return fmt.Errorf("flush reachable trie-node index: %w", err)
	}
	i.nodeBatch.Reset()
	return nil
}

// Has queries an index after Count has flushed all markers and traversal joined.
func (i *temporaryTrieNodeIndex) Has(hash common.Hash) (bool, error) {
	if i == nil || i.closed {
		return false, errors.New("temporary trie-node index is closed")
	}
	_, closer, err := i.db.Get(hash[:])
	if errors.Is(err, cpebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read reachable trie-node index: %w", err)
	}
	if err := closer.Close(); err != nil {
		return false, fmt.Errorf("close reachable trie-node index value: %w", err)
	}
	return true, nil
}

// Close closes and removes the exact temporary directory created for the
// index. It is idempotent so callers can safely combine cleanup paths.
func (i *temporaryTrieNodeIndex) Close() error {
	if i == nil || i.closed {
		return nil
	}
	i.closed = true
	var closeErr error
	if err := i.nodeBatch.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close reachable trie-node index batch: %w", err))
	}
	if err := i.db.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close temporary trie-node index: %w", err))
	}
	i.cache.Unref()
	if err := i.remove(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("remove temporary trie-node index: %w", err))
	}
	return closeErr
}

func (i *temporaryTrieNodeIndex) remove() error {
	if i.storage.fs != nil {
		return i.storage.remove(i.path)
	}
	return removeTemporaryTrieNodeIndex(i.path)
}

func removeTemporaryTrieNodeIndex(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(filepath.Base(clean), trieNodeIndexTempPrefix) {
		return fmt.Errorf("refusing to remove unexpected temporary trie-node index path %s", clean)
	}
	return os.RemoveAll(clean)
}

type temporaryPebbleLogger struct{}

func (temporaryPebbleLogger) Infof(string, ...any)  {}
func (temporaryPebbleLogger) Errorf(string, ...any) {}
func (temporaryPebbleLogger) Fatalf(format string, args ...any) {
	panic(fmt.Errorf("fatal: "+format, args...))
}
