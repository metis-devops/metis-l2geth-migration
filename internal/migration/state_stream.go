package migration

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	accountTrieReadAhead = 64
	storageTrieReadAhead = 64
	// Avoid per-account stream startup when the entire storage trie is small.
	storageTriePipelineThreshold = 64
)

type accountTrieLeaf struct {
	accountHash common.Hash
	value       []byte
}

type storageTrieLeaf struct {
	slotHash common.Hash
	value    []byte
}

type trieLeafStream[T any] struct {
	leaves <-chan T
	result <-chan error
	cancel context.CancelFunc
	joined bool
	err    error
}

type trieLeafProducer[T any] func(context.Context, chan<- T) error

func newTrieLeafStream[T any](ctx context.Context, readAhead int, produce trieLeafProducer[T]) *trieLeafStream[T] {
	streamCtx, cancel := context.WithCancel(ctx)
	leaves := make(chan T, readAhead)
	result := make(chan error, 1)
	go func() {
		result <- produce(streamCtx, leaves)
		close(result)
		close(leaves)
	}()
	return &trieLeafStream[T]{leaves: leaves, result: result, cancel: cancel}
}

func (s *trieLeafStream[T]) wait() error {
	if !s.joined {
		s.err = <-s.result
		s.joined = true
	}
	s.cancel()
	return s.err
}

func (s *trieLeafStream[T]) close() {
	s.cancel()
	_ = s.wait()
}

func newAccountTrieStream(ctx context.Context, accounts *trie.Iterator) *trieLeafStream[accountTrieLeaf] {
	return newTrieLeafStream(ctx, accountTrieReadAhead, func(ctx context.Context, leaves chan<- accountTrieLeaf) error {
		return produceAccountTrie(ctx, accounts, leaves)
	})
}

// produceAccountTrie prefetches account leaves while the caller processes the
// preceding account. After validating its length, the key is copied into a
// value-type hash and the value is cloned because trie iterators reuse both
// buffers when advancing.
func produceAccountTrie(ctx context.Context, accounts *trie.Iterator, leaves chan<- accountTrieLeaf) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !accounts.Next() {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(accounts.Key) != common.HashLength {
			return fmt.Errorf("account trie key has length %d", len(accounts.Key))
		}
		leaf := accountTrieLeaf{
			accountHash: common.Hash(accounts.Key),
			value:       bytes.Clone(accounts.Value),
		}
		select {
		case leaves <- leaf:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if accounts.Err != nil {
		return fmt.Errorf("iterate account trie: %w", accounts.Err)
	}
	return ctx.Err()
}

func newStorageTrieStream(ctx context.Context, accountHash common.Hash, storage *trie.Iterator) *trieLeafStream[storageTrieLeaf] {
	return newTrieLeafStream(ctx, storageTrieReadAhead, func(ctx context.Context, leaves chan<- storageTrieLeaf) error {
		return produceStorageTrie(ctx, accountHash, storage, leaves)
	})
}

// produceStorageTrie preserves the iterator's ascending order while
// prefetching leaves for the sequential StackTrie and visitor consumer.
func produceStorageTrie(ctx context.Context, accountHash common.Hash, storage *trie.Iterator, leaves chan<- storageTrieLeaf) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !storage.Next() {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(storage.Key) != common.HashLength {
			return fmt.Errorf("account %s storage key has length %d", accountHash, len(storage.Key))
		}
		leaf := storageTrieLeaf{
			slotHash: common.Hash(storage.Key),
			value:    bytes.Clone(storage.Value),
		}
		select {
		case leaves <- leaf:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if storage.Err != nil {
		return fmt.Errorf("iterate storage trie for account %s: %w", accountHash, storage.Err)
	}
	return ctx.Err()
}
