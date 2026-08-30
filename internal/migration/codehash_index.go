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
	codeHashIndexTempPrefix = ".l2state-codehash-"
	codeHashIndexMaxCacheMB = 16
	codeHashIndexMaxHandles = 16
	codeHashNamespace       = byte(1)
	trieNodeHashNamespace   = byte(2)
)

type codeHashIndexOptions struct {
	Parent  string
	CacheMB int
	Handles int
}

// temporaryCodeHashIndex is an exact, namespaced operation-local set of code
// and reachable trie-node hashes. It is deliberately disk-backed so memory
// usage does not grow with the number of unique hashes.
type temporaryCodeHashIndex struct {
	db        *cpebble.DB
	cache     *cpebble.Cache
	nodeBatch *cpebble.Batch
	path      string
	closed    bool
}

func newTemporaryCodeHashIndex(opts codeHashIndexOptions) (*temporaryCodeHashIndex, error) {
	path, err := os.MkdirTemp(opts.Parent, codeHashIndexTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("create temporary code-hash index: %w", err)
	}
	cacheMB := boundedCodeHashIndexResource(opts.CacheMB, codeHashIndexMaxCacheMB)
	handles := boundedCodeHashIndexResource(opts.Handles, codeHashIndexMaxHandles)
	cache := cpebble.NewCache(int64(cacheMB) * 1024 * 1024)
	options := &cpebble.Options{
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
		Logger:             compactLogger{},
	}
	options.Experimental.ReadSamplingMultiplier = -1
	db, err := cpebble.Open(path, options)
	if err != nil {
		cache.Unref()
		openErr := fmt.Errorf("open temporary code-hash index: %w", err)
		if removeErr := removeTemporaryCodeHashIndex(path); removeErr != nil {
			openErr = errors.Join(openErr, fmt.Errorf("remove unopened temporary code-hash index: %w", removeErr))
		}
		return nil, openErr
	}
	return &temporaryCodeHashIndex{db: db, cache: cache, nodeBatch: db.NewBatch(), path: path}, nil
}

func boundedCodeHashIndexResource(configured, limit int) int {
	if configured <= 0 {
		return limit
	}
	return min(configured, limit)
}

// Add records hash and reports whether it was absent before this call.
func (i *temporaryCodeHashIndex) Add(hash common.Hash) (bool, error) {
	exists, err := i.Has(hash)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	key := hashIndexKey(codeHashNamespace, hash)
	if err := i.db.Set(key[:], nil, cpebble.NoSync); err != nil {
		return false, fmt.Errorf("store code hash %s in temporary index: %w", hash, err)
	}
	return true, nil
}

// Has reports whether hash was already recorded.
func (i *temporaryCodeHashIndex) Has(hash common.Hash) (bool, error) {
	if i == nil || i.closed {
		return false, errors.New("temporary code-hash index is closed")
	}
	key := hashIndexKey(codeHashNamespace, hash)
	_, closer, err := i.db.Get(key[:])
	if errors.Is(err, cpebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up code hash %s in temporary index: %w", hash, err)
	}
	if err := closer.Close(); err != nil {
		return false, fmt.Errorf("release code hash %s lookup: %w", hash, err)
	}
	return true, nil
}

// MarkTrieNode records a reachable hash-scheme trie node. Unlike Add, it does
// not perform a point lookup first: repeated Sets collapse to one user key when
// CountTrieNodes iterates the index.
func (i *temporaryCodeHashIndex) MarkTrieNode(hash common.Hash) error {
	if i == nil || i.closed {
		return errors.New("temporary code-hash index is closed")
	}
	key := hashIndexKey(trieNodeHashNamespace, hash)
	if err := i.nodeBatch.Set(key[:], nil, nil); err != nil {
		return fmt.Errorf("store reachable trie node %s in temporary index: %w", hash, err)
	}
	if i.nodeBatch.Len() >= ethdb.IdealBatchSize {
		if err := i.flushTrieNodes(); err != nil {
			return err
		}
	}
	return nil
}

// CountTrieNodes flushes pending node markers and counts unique reachable trie
// node hashes while honoring cancellation.
func (i *temporaryCodeHashIndex) CountTrieNodes(ctx context.Context) (count uint64, retErr error) {
	if i == nil || i.closed {
		return 0, errors.New("temporary code-hash index is closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := i.flushTrieNodes(); err != nil {
		return 0, err
	}
	lower := []byte{trieNodeHashNamespace}
	upper := []byte{trieNodeHashNamespace + 1}
	it, err := i.db.NewIterWithContext(ctx, &cpebble.IterOptions{LowerBound: lower, UpperBound: upper})
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

func (i *temporaryCodeHashIndex) flushTrieNodes() error {
	if i.nodeBatch == nil || i.nodeBatch.Len() == 0 {
		return nil
	}
	if err := i.nodeBatch.Commit(cpebble.NoSync); err != nil {
		return fmt.Errorf("flush reachable trie-node index: %w", err)
	}
	i.nodeBatch.Reset()
	return nil
}

func hashIndexKey(namespace byte, hash common.Hash) (key [1 + common.HashLength]byte) {
	key[0] = namespace
	copy(key[1:], hash[:])
	return key
}

// Close closes and removes the exact temporary directory created for the
// index. It is idempotent so callers can safely combine cleanup paths.
func (i *temporaryCodeHashIndex) Close() error {
	if i == nil || i.closed {
		return nil
	}
	i.closed = true
	var closeErr error
	if err := i.nodeBatch.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close reachable trie-node index batch: %w", err))
	}
	if err := i.db.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close temporary code-hash index: %w", err))
	}
	i.cache.Unref()
	if err := removeTemporaryCodeHashIndex(i.path); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("remove temporary code-hash index: %w", err))
	}
	return closeErr
}

func removeTemporaryCodeHashIndex(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(filepath.Base(clean), codeHashIndexTempPrefix) {
		return fmt.Errorf("refusing to remove unexpected temporary code-hash index path %s", clean)
	}
	return os.RemoveAll(clean)
}
