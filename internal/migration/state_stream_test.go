package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

func TestStreamAccountTrieCopiesIteratorDataAndPreservesOrder(t *testing.T) {
	want := []iteratorLeaf{
		{key: iteratorKey(0x01), value: []byte{0xa1, 0xb1}},
		{key: iteratorKey(0x02), value: []byte{0xa2, 0xb2}},
		{key: iteratorKey(0x03), value: []byte{0xa3, 0xb3}},
	}
	nodes := &reusingLeafNodeIterator{leaves: want, index: -1}
	stream := newAccountTrieStream(context.Background(), trie.NewIterator(nodes))
	defer stream.close()
	if err := stream.wait(); err != nil {
		t.Fatal(err)
	}
	var got []accountTrieLeaf
	for leaf := range stream.leaves {
		got = append(got, leaf)
	}
	if len(got) != len(want) {
		t.Fatalf("streamed %d leaves, want %d", len(got), len(want))
	}
	for i := range want {
		wantHash := common.BytesToHash(want[i].key)
		if got[i].accountHash != wantHash || !bytes.Equal(got[i].value, want[i].value) {
			t.Fatalf("leaf %d changed or reordered: have %s=%x want %s=%x", i,
				got[i].accountHash, got[i].value, wantHash, want[i].value)
		}
	}
}

func TestStreamAccountTrieRejectsInvalidKeyLength(t *testing.T) {
	nodes := &reusingLeafNodeIterator{
		leaves: []iteratorLeaf{{key: []byte{0x01}, value: []byte{0x02}}},
		index:  -1,
	}
	stream := newAccountTrieStream(context.Background(), trie.NewIterator(nodes))
	defer stream.close()
	for range stream.leaves {
		t.Fatal("stream delivered an account with an invalid key length")
	}
	if err := stream.wait(); err == nil || err.Error() != "account trie key has length 1" {
		t.Fatalf("stream error is %v, want invalid account key length", err)
	}
}

func TestStreamAccountTriePropagatesIteratorError(t *testing.T) {
	wantErr := errors.New("injected account iterator failure")
	nodes := &reusingLeafNodeIterator{
		leaves:      []iteratorLeaf{{key: iteratorKey(0x01), value: []byte{0x02}}},
		index:       -1,
		terminalErr: wantErr,
	}
	stream := newAccountTrieStream(context.Background(), trie.NewIterator(nodes))
	defer stream.close()
	for range stream.leaves {
	}
	err := stream.wait()
	if !errors.Is(err, wantErr) || err.Error() != "iterate account trie: injected account iterator failure" {
		t.Fatalf("stream error is %v, want wrapped %v", err, wantErr)
	}
}

func TestStreamAccountTrieCancellationUnblocksFullChannel(t *testing.T) {
	entries := make([]iteratorLeaf, accountTrieReadAhead+1)
	for i := range entries {
		entries[i] = iteratorLeaf{key: iteratorKey(byte(i)), value: []byte{byte(i + 1)}}
	}
	lastLeafRead := make(chan struct{})
	nodes := &reusingLeafNodeIterator{
		leaves: entries,
		index:  -1,
		onNext: func(index int) {
			if index == accountTrieReadAhead {
				close(lastLeafRead)
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newAccountTrieStream(ctx, trie.NewIterator(nodes))
	select {
	case <-lastLeafRead:
	case <-time.After(5 * time.Second):
		t.Fatal("account producer did not fill the read-ahead channel")
	}
	cancel()
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- stream.wait()
	}()
	select {
	case err := <-waitResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream returned %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled account producer did not exit")
	}
	count := 0
	for range stream.leaves {
		count++
	}
	if count != accountTrieReadAhead {
		t.Fatalf("stream delivered %d buffered leaves, want %d", count, accountTrieReadAhead)
	}
	stream.close()
	stream.close()
}

func TestStreamStorageTrieCopiesIteratorDataAndPreservesOrder(t *testing.T) {
	accountHash := common.HexToHash("0xa1")
	want := []iteratorLeaf{
		{key: iteratorKey(0x01), value: []byte{0xa1, 0xb1}},
		{key: iteratorKey(0x02), value: []byte{0xa2, 0xb2}},
		{key: iteratorKey(0x03), value: []byte{0xa3, 0xb3}},
	}
	nodes := &reusingLeafNodeIterator{leaves: want, index: -1}
	stream := newStorageTrieStream(context.Background(), accountHash, trie.NewIterator(nodes))
	defer stream.close()
	if err := stream.wait(); err != nil {
		t.Fatal(err)
	}
	var got []storageTrieLeaf
	for leaf := range stream.leaves {
		got = append(got, leaf)
	}
	if len(got) != len(want) {
		t.Fatalf("streamed %d storage leaves, want %d", len(got), len(want))
	}
	for i := range want {
		wantHash := common.BytesToHash(want[i].key)
		if got[i].slotHash != wantHash || !bytes.Equal(got[i].value, want[i].value) {
			t.Fatalf("storage leaf %d changed or reordered: have %s=%x want %s=%x", i,
				got[i].slotHash, got[i].value, wantHash, want[i].value)
		}
	}
}

func TestStreamStorageTrieRejectsInvalidKeyLength(t *testing.T) {
	accountHash := common.HexToHash("0xa1")
	nodes := &reusingLeafNodeIterator{
		leaves: []iteratorLeaf{{key: []byte{0x01}, value: []byte{0x02}}},
		index:  -1,
	}
	stream := newStorageTrieStream(context.Background(), accountHash, trie.NewIterator(nodes))
	defer stream.close()
	for range stream.leaves {
		t.Fatal("stream delivered a storage leaf with an invalid key length")
	}
	want := fmt.Sprintf("account %s storage key has length 1", accountHash)
	if err := stream.wait(); err == nil || err.Error() != want {
		t.Fatalf("stream error is %v, want %q", err, want)
	}
}

func TestStreamStorageTriePropagatesIteratorError(t *testing.T) {
	accountHash := common.HexToHash("0xa1")
	wantErr := errors.New("injected storage iterator failure")
	nodes := &reusingLeafNodeIterator{
		leaves:      []iteratorLeaf{{key: iteratorKey(0x01), value: []byte{0x02}}},
		index:       -1,
		terminalErr: wantErr,
	}
	stream := newStorageTrieStream(context.Background(), accountHash, trie.NewIterator(nodes))
	defer stream.close()
	for range stream.leaves {
	}
	err := stream.wait()
	want := fmt.Sprintf("iterate storage trie for account %s: %s", accountHash, wantErr)
	if !errors.Is(err, wantErr) || err.Error() != want {
		t.Fatalf("stream error is %v, want %q", err, want)
	}
}

func TestStreamStorageTrieCancellationUnblocksFullChannel(t *testing.T) {
	entries := make([]iteratorLeaf, storageTrieReadAhead+1)
	for i := range entries {
		entries[i] = iteratorLeaf{key: iteratorKey(byte(i)), value: []byte{byte(i + 1)}}
	}
	lastLeafRead := make(chan struct{})
	nodes := &reusingLeafNodeIterator{
		leaves: entries,
		index:  -1,
		onNext: func(index int) {
			if index == storageTrieReadAhead {
				close(lastLeafRead)
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newStorageTrieStream(ctx, common.HexToHash("0xa1"), trie.NewIterator(nodes))
	select {
	case <-lastLeafRead:
	case <-time.After(5 * time.Second):
		t.Fatal("storage producer did not fill the read-ahead channel")
	}
	cancel()
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- stream.wait()
	}()
	select {
	case err := <-waitResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stream returned %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled storage producer did not exit")
	}
	count := 0
	for range stream.leaves {
		count++
	}
	if count != storageTrieReadAhead {
		t.Fatalf("stream delivered %d buffered storage leaves, want %d", count, storageTrieReadAhead)
	}
	stream.close()
	stream.close()
}

func TestStoragePipelineConsumerFailureStopsProducer(t *testing.T) {
	entries := make([]iteratorLeaf, storageTriePipelineThreshold+storageTrieReadAhead+1)
	for i := range entries {
		entries[i] = iteratorLeaf{
			key:   iteratorKey(byte(i + 1)),
			value: []byte{byte(i%0x7f + 1)},
		}
	}
	nodes := &reusingLeafNodeIterator{leaves: entries, index: -1}
	wantErr := errors.New("injected pipelined storage consumer failure")
	visitor := &failingStorageVisitor{err: wantErr, failAt: storageTriePipelineThreshold + 1}
	traverser := &stateTraverser{ctx: context.Background(), visitor: visitor}
	err := traverser.consumeStorage(common.HexToHash("0xa1"), trie.NewStackTrie(nil), trie.NewIterator(nodes))
	if !errors.Is(err, wantErr) {
		t.Fatalf("storage pipeline returned %v, want consumer failure %v", err, wantErr)
	}
	if visitor.seen != visitor.failAt {
		t.Fatalf("storage visitor saw %d leaves, want failure at %d", visitor.seen, visitor.failAt)
	}
}

func TestStoragePipelineStartsOnlyBeyondThreshold(t *testing.T) {
	for _, count := range []int{storageTriePipelineThreshold, storageTriePipelineThreshold + 1} {
		t.Run(fmt.Sprintf("slots=%d", count), func(t *testing.T) {
			entries := make([]iteratorLeaf, count)
			for index := range entries {
				entries[index] = iteratorLeaf{key: iteratorKey(byte(index + 1)), value: []byte{byte(index%0x7f + 1)}}
			}
			traverser := &stateTraverser{ctx: context.Background()}
			first, err := traverser.consumeStoragePrefix(
				common.HexToHash("0xa1"), trie.NewStackTrie(nil),
				trie.NewIterator(&reusingLeafNodeIterator{leaves: entries, index: -1}),
			)
			if err != nil {
				t.Fatal(err)
			}
			if traverser.counts.StorageSlots != storageTriePipelineThreshold {
				t.Fatalf("consumed %d prefix slots", traverser.counts.StorageSlots)
			}
			if count == storageTriePipelineThreshold {
				if first != nil {
					t.Fatalf("exact-threshold trie returned an extra leaf: %+v", first)
				}
				return
			}
			if first == nil || first.slotHash != common.BytesToHash(entries[storageTriePipelineThreshold].key) {
				t.Fatalf("first pipelined leaf is %+v", first)
			}
		})
	}
}

func TestStoragePipelineCompletesInOrderAndMatchesSerialRoot(t *testing.T) {
	entries := make([]iteratorLeaf, storageTriePipelineThreshold+storageTrieReadAhead+1)
	wantSlots := make([]common.Hash, 0, len(entries))
	wantStack := trie.NewStackTrie(nil)
	var wantPayloadBytes uint64
	for i := range entries {
		entries[i] = iteratorLeaf{
			key:   iteratorKey(byte(i + 1)),
			value: []byte{byte(i%0x7f + 1)},
		}
		if err := wantStack.Update(entries[i].key, entries[i].value); err != nil {
			t.Fatal(err)
		}
		wantSlots = append(wantSlots, common.BytesToHash(entries[i].key))
		wantPayloadBytes += uint64(len(entries[i].value))
	}
	nodes := &reusingLeafNodeIterator{leaves: entries, index: -1}
	visitor := new(recordingStorageVisitor)
	traverser := &stateTraverser{ctx: context.Background(), visitor: visitor}
	stack := trie.NewStackTrie(nil)
	if err := traverser.consumeStorage(common.HexToHash("0xa1"), stack, trie.NewIterator(nodes)); err != nil {
		t.Fatal(err)
	}
	if have, want := stack.Hash(), wantStack.Hash(); have != want {
		t.Fatalf("pipelined storage root is %s, want serial root %s", have, want)
	}
	if traverser.counts.StorageSlots != uint64(len(entries)) ||
		traverser.counts.Records != uint64(len(entries)) ||
		traverser.counts.PayloadBytes != wantPayloadBytes {
		t.Fatalf("unexpected pipelined storage counts: %+v", traverser.counts)
	}
	if len(visitor.slots) != len(wantSlots) {
		t.Fatalf("storage visitor saw %d slots, want %d", len(visitor.slots), len(wantSlots))
	}
	for i := range wantSlots {
		if visitor.slots[i] != wantSlots[i] {
			t.Fatalf("storage visitor slot %d is %s, want %s", i, visitor.slots[i], wantSlots[i])
		}
	}
}

func TestTraverseStateConsumerFailureStopsAccountStream(t *testing.T) {
	fixture := buildLegacyFixture(t)
	kv, err := gethleveldb.New(fixture.chaindata, 16, 16, "account-stream-failure", true)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	defer func() {
		if err := disk.Close(); err != nil {
			t.Errorf("close account-stream database: %v", err)
		}
	}()
	trieDB := triedb.NewDatabase(disk, triedb.HashDefaults)
	defer func() {
		if err := trieDB.Close(); err != nil {
			t.Errorf("close account-stream trie database: %v", err)
		}
	}()
	scratch := t.TempDir()
	wantErr := errors.New("injected account consumer failure")
	_, _, err = traverseState(context.Background(), disk, trieDB, fixture.root, failingAccountVisitor{err: wantErr}, true, stateTraversalOptions{
		NodeIndex: trieNodeIndexOptions{Parent: scratch, CacheMB: 16, Handles: 16},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("traversal returned %v, want consumer failure %v", err, wantErr)
	}
	assertNoTemporaryTrieNodeIndexes(t, scratch)
}

func TestTraverseStateStorageConsumerFailureStopsStorageStream(t *testing.T) {
	fixture := buildLegacyFixture(t)
	kv, err := gethleveldb.New(fixture.chaindata, 16, 16, "storage-stream-failure", true)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	defer func() {
		if err := disk.Close(); err != nil {
			t.Errorf("close storage-stream database: %v", err)
		}
	}()
	trieDB := triedb.NewDatabase(disk, triedb.HashDefaults)
	defer func() {
		if err := trieDB.Close(); err != nil {
			t.Errorf("close storage-stream trie database: %v", err)
		}
	}()
	scratch := t.TempDir()
	wantErr := errors.New("injected storage consumer failure")
	_, _, err = traverseState(context.Background(), disk, trieDB, fixture.root, &failingStorageVisitor{err: wantErr}, true, stateTraversalOptions{
		NodeIndex: trieNodeIndexOptions{Parent: scratch, CacheMB: 16, Handles: 16},
		ReadCode: func(db ethdb.KeyValueReader, hash common.Hash) ([]byte, error) {
			return db.Get(hash[:])
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("traversal returned %v, want storage consumer failure %v", err, wantErr)
	}
	assertNoTemporaryTrieNodeIndexes(t, scratch)
}

type reusingLeafNodeIterator struct {
	leaves      []iteratorLeaf
	index       int
	key         []byte
	value       []byte
	terminalErr error
	onNext      func(int)
}

type iteratorLeaf struct {
	key   []byte
	value []byte
}

func iteratorKey(lastByte byte) []byte {
	key := make([]byte, common.HashLength)
	key[len(key)-1] = lastByte
	return key
}

func (it *reusingLeafNodeIterator) Next(bool) bool {
	next := it.index + 1
	if next >= len(it.leaves) {
		it.index = len(it.leaves)
		return false
	}
	it.index = next
	it.key = append(it.key[:0], it.leaves[next].key...)
	it.value = append(it.value[:0], it.leaves[next].value...)
	if it.onNext != nil {
		it.onNext(next)
	}
	return true
}

func (it *reusingLeafNodeIterator) Error() error {
	if it.index >= len(it.leaves) {
		return it.terminalErr
	}
	return nil
}

func (*reusingLeafNodeIterator) Hash() common.Hash             { return common.Hash{} }
func (*reusingLeafNodeIterator) Parent() common.Hash           { return common.Hash{} }
func (*reusingLeafNodeIterator) Path() []byte                  { return nil }
func (*reusingLeafNodeIterator) NodeBlob() []byte              { return nil }
func (*reusingLeafNodeIterator) Leaf() bool                    { return true }
func (it *reusingLeafNodeIterator) LeafKey() []byte            { return it.key }
func (it *reusingLeafNodeIterator) LeafBlob() []byte           { return it.value }
func (*reusingLeafNodeIterator) LeafProof() [][]byte           { return nil }
func (*reusingLeafNodeIterator) AddResolver(trie.NodeResolver) {}

type failingAccountVisitor struct {
	err error
}

func (v failingAccountVisitor) Account(common.Hash, *types.StateAccount, []byte) error {
	return v.err
}

func (failingAccountVisitor) Storage(common.Hash, common.Hash, []byte) error { return nil }
func (failingAccountVisitor) Code(common.Hash, common.Hash, []byte) error    { return nil }

type failingStorageVisitor struct {
	err    error
	failAt int
	seen   int
}

func (*failingStorageVisitor) Account(common.Hash, *types.StateAccount, []byte) error {
	return nil
}

func (v *failingStorageVisitor) Storage(common.Hash, common.Hash, []byte) error {
	v.seen++
	if v.failAt == 0 || v.seen == v.failAt {
		return v.err
	}
	return nil
}

func (*failingStorageVisitor) Code(common.Hash, common.Hash, []byte) error { return nil }

type recordingStorageVisitor struct {
	slots []common.Hash
}

func (*recordingStorageVisitor) Account(common.Hash, *types.StateAccount, []byte) error {
	return nil
}

func (v *recordingStorageVisitor) Storage(_ common.Hash, slotHash common.Hash, _ []byte) error {
	v.slots = append(v.slots, slotHash)
	return nil
}

func (*recordingStorageVisitor) Code(common.Hash, common.Hash, []byte) error { return nil }

var _ trie.NodeIterator = (*reusingLeafNodeIterator)(nil)
var _ StateVisitor = failingAccountVisitor{}
var _ StateVisitor = (*failingStorageVisitor)(nil)
var _ StateVisitor = (*recordingStorageVisitor)(nil)
