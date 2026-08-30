package migration

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestTemporaryTrieNodeIndexExactAcrossFlushAndCleanup(t *testing.T) {
	parent := t.TempDir()
	index, err := newTemporaryTrieNodeIndex(trieNodeIndexOptions{
		Parent: parent, CacheMB: 1, Handles: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := index.path
	closed := false
	defer func() {
		if !closed {
			if err := index.Close(); err != nil {
				t.Errorf("close temporary index: %v", err)
			}
		}
	}()

	first := common.HexToHash("0x01")
	if err := index.Mark(first); err != nil {
		t.Fatal(err)
	}
	for value := uint64(100); value < 20_100; value++ {
		var hash common.Hash
		binary.BigEndian.PutUint64(hash[common.HashLength-8:], value)
		if err := index.Mark(hash); err != nil {
			t.Fatalf("mark hash %d: %v", value, err)
		}
	}
	if err := index.flush(); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []common.Hash{first, common.HexToHash("0x02")} {
		if err := index.Mark(hash); err != nil {
			t.Fatal(err)
		}
	}
	count, err := index.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 20_002 {
		t.Fatalf("reachable trie-node count is %d, want 20002", count)
	}

	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary index survived close: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := index.Mark(first); err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("closed index accepted a hash: %v", err)
	}
}

func TestTemporaryTrieNodeIndexResourceBounds(t *testing.T) {
	for _, test := range []struct {
		configured int
		limit      int
		want       int
	}{
		{configured: 0, limit: 16, want: 16},
		{configured: -1, limit: 16, want: 16},
		{configured: 1, limit: 16, want: 1},
		{configured: 16, limit: 16, want: 16},
		{configured: 64, limit: 16, want: 16},
	} {
		if got := boundedTrieNodeIndexResource(test.configured, test.limit); got != test.want {
			t.Fatalf("bounded resource for %d: have %d want %d", test.configured, got, test.want)
		}
	}
}

func TestTemporaryTrieNodeIndexCountHonorsCancellation(t *testing.T) {
	index, err := newTemporaryTrieNodeIndex(trieNodeIndexOptions{Parent: t.TempDir(), CacheMB: 1, Handles: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := index.Close(); err != nil {
			t.Errorf("close temporary index: %v", err)
		}
	}()
	if err := index.Mark(common.HexToHash("0x01")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := index.Count(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("count returned %v, want context cancellation", err)
	}
}

func TestTemporaryTrieNodeIndexCreationAndRemovalFailClosed(t *testing.T) {
	missingParent := filepath.Join(t.TempDir(), "missing")
	if _, err := newTemporaryTrieNodeIndex(trieNodeIndexOptions{Parent: missingParent}); err == nil || !strings.Contains(err.Error(), "create temporary trie-node index") {
		t.Fatalf("missing parent did not fail with context: %v", err)
	}
	if err := removeTemporaryTrieNodeIndex(filepath.Join(t.TempDir(), "unrelated")); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("unsafe removal path was accepted: %v", err)
	}
}
