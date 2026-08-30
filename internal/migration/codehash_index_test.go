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

func TestTemporaryCodeHashIndexExactAcrossFlushAndCleanup(t *testing.T) {
	parent := t.TempDir()
	index, err := newTemporaryCodeHashIndex(codeHashIndexOptions{
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
	added, err := index.Add(first)
	if err != nil || !added {
		t.Fatalf("add first hash: added=%t err=%v", added, err)
	}
	added, err = index.Add(first)
	if err != nil || added {
		t.Fatalf("add duplicate hash: added=%t err=%v", added, err)
	}
	for value := uint64(100); value < 20_100; value++ {
		var hash common.Hash
		binary.BigEndian.PutUint64(hash[common.HashLength-8:], value)
		added, err := index.Add(hash)
		if err != nil || !added {
			t.Fatalf("add hash %d: added=%t err=%v", value, added, err)
		}
	}
	if err := index.db.Flush(); err != nil {
		t.Fatalf("flush temporary index: %v", err)
	}
	added, err = index.Add(first)
	if err != nil || added {
		t.Fatalf("find duplicate after flush: added=%t err=%v", added, err)
	}
	missing, err := index.Has(common.HexToHash("0xffff"))
	if err != nil || missing {
		t.Fatalf("look up absent hash: present=%t err=%v", missing, err)
	}
	if err := index.MarkTrieNode(first); err != nil {
		t.Fatal(err)
	}
	if err := index.flushTrieNodes(); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []common.Hash{first, common.HexToHash("0x02")} {
		if err := index.MarkTrieNode(hash); err != nil {
			t.Fatal(err)
		}
	}
	count, err := index.CountTrieNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reachable trie-node count is %d, want 2", count)
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
	if _, err := index.Add(first); err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("closed index accepted a hash: %v", err)
	}
}

func TestTemporaryCodeHashIndexResourceBounds(t *testing.T) {
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
		if got := boundedCodeHashIndexResource(test.configured, test.limit); got != test.want {
			t.Fatalf("bounded resource for %d: have %d want %d", test.configured, got, test.want)
		}
	}
}

func TestTemporaryCodeHashIndexTrieCountHonorsCancellation(t *testing.T) {
	index, err := newTemporaryCodeHashIndex(codeHashIndexOptions{Parent: t.TempDir(), CacheMB: 1, Handles: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := index.Close(); err != nil {
			t.Errorf("close temporary index: %v", err)
		}
	}()
	if err := index.MarkTrieNode(common.HexToHash("0x01")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := index.CountTrieNodes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("count returned %v, want context cancellation", err)
	}
}

func TestTemporaryCodeHashIndexCreationAndRemovalFailClosed(t *testing.T) {
	missingParent := filepath.Join(t.TempDir(), "missing")
	if _, err := newTemporaryCodeHashIndex(codeHashIndexOptions{Parent: missingParent}); err == nil || !strings.Contains(err.Error(), "create temporary code-hash index") {
		t.Fatalf("missing parent did not fail with context: %v", err)
	}
	if err := removeTemporaryCodeHashIndex(filepath.Join(t.TempDir(), "unrelated")); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("unsafe removal path was accepted: %v", err)
	}
}
