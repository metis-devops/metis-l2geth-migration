package migration

import (
	"bytes"
	"context"
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"testing"
)

func TestPartitionFinalNodeError(t *testing.T) {
	want := errors.New("injected final node write failure")
	key := make([]byte, 32)
	value := []byte{0x01}
	t.Run("storage", func(t *testing.T) {
		ref := trie.NewStackTrie(nil)
		if err := ref.Update(key, value); err != nil {
			t.Fatal(err)
		}
		root := ref.Hash()
		batch := newTrackingBatch()
		batch.err = want
		writer := &directStateWriter{batch: batch, scheme: "hash"}
		var nodeErr error
		stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
			nodeErr = writer.TrieNode(common.Hash{}, path, hash, blob)
		})
		if err := stack.Update(key, value); err != nil {
			t.Fatal(err)
		}
		if nodeErr != nil {
			t.Fatal("error arrived before finalization")
		}
		m := new(partitionedStateMigrator)
		_, _, err := m.finishProbedStorage(context.Background(), writer, stack, nil, &nodeErr, common.Hash{}, root, bundle.Counts{})
		if !errors.Is(nodeErr, want) {
			t.Fatal("callback was not invoked")
		}
		if !errors.Is(err, want) {
			t.Fatalf("final callback failed, but finalizer returned %v", err)
		}
		writer.Abort()
		if batch.writes != 0 || batch.closes != 1 {
			t.Fatalf("failed finalizer flushed or leaked batch: writes=%d closes=%d", batch.writes, batch.closes)
		}
	})
	t.Run("account", func(t *testing.T) {
		batch := newTrackingBatch()
		batch.err = want
		writer := &directStateWriter{batch: batch, scheme: "hash"}
		var nodeErr error
		result := migratePartitionResult{populated: true}
		stack := trie.NewPartialStackTrie(0, func(path []byte, hash common.Hash, blob []byte) {
			if len(path) == 1 {
				result.rootBlob = bytes.Clone(blob)
			}
			nodeErr = writer.TrieNode(common.Hash{}, path, hash, blob)
		})
		if err := stack.Update(key, value); err != nil {
			t.Fatal(err)
		}
		if nodeErr != nil {
			t.Fatal("error arrived before finalization")
		}
		m := &partitionedStateMigrator{limiter: newMigrateWorkLimiter(2)}
		_, err := m.finishMigrateAccountPartition(context.Background(), 0, writer, stack, &nodeErr, &result)
		if !errors.Is(nodeErr, want) {
			t.Fatal("callback was not invoked")
		}
		if !errors.Is(err, want) {
			t.Fatalf("final callback failed, but finalizer returned %v", err)
		}
		writer.Abort()
		if batch.writes != 0 || batch.closes != 1 {
			t.Fatalf("failed finalizer flushed or leaked batch: writes=%d closes=%d", batch.writes, batch.closes)
		}
	})
}
