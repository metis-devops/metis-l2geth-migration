package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

func TestDirectStateWriterAbortDiscardsPendingBatch(t *testing.T) {
	batch := newTrackingBatch()
	writer := &directStateWriter{
		batch: batch, recorder: &capturingKeyValueWriter{target: batch}, scheme: rawdb.PathScheme,
	}
	if err := writer.Storage(common.HexToHash("0x01"), common.HexToHash("0x02"), []byte{0x01}); err != nil {
		t.Fatal(err)
	}
	writer.Abort()
	writer.Abort()
	if batch.writes != 0 || batch.closes != 1 {
		t.Fatalf("abort wrote or reclosed batch: writes=%d closes=%d", batch.writes, batch.closes)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if batch.writes != 0 || batch.closes != 1 {
		t.Fatalf("close after abort changed batch: writes=%d closes=%d", batch.writes, batch.closes)
	}
}

func TestDirectStateWriterCloseFlushesOnce(t *testing.T) {
	batch := newTrackingBatch()
	writer := &directStateWriter{
		batch: batch, recorder: &capturingKeyValueWriter{target: batch}, scheme: rawdb.HashScheme,
	}
	if err := writer.Code(common.Hash{}, common.HexToHash("0x01"), []byte{0x60}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if batch.writes != 1 || batch.closes != 1 {
		t.Fatalf("close lifecycle mismatch: writes=%d closes=%d", batch.writes, batch.closes)
	}
}

func TestDirectStateWriterCloseContextAbortsCanceledFlush(t *testing.T) {
	batch := newTrackingBatch()
	writer := &directStateWriter{
		batch: batch, recorder: &capturingKeyValueWriter{target: batch}, scheme: rawdb.HashScheme,
	}
	if err := writer.Code(common.Hash{}, common.HexToHash("0x01"), []byte{0x60}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.CloseContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close returned %v, want cancellation", err)
	}
	if batch.writes != 0 || batch.closes != 1 {
		t.Fatalf("canceled close flushed batch: writes=%d closes=%d", batch.writes, batch.closes)
	}
}

type trackingBatch struct {
	puts   int
	writes int
	closes int
	size   int
	closed bool
}

func newTrackingBatch() *trackingBatch { return new(trackingBatch) }

func (b *trackingBatch) Put(key, value []byte) error {
	b.puts++
	b.size += len(key) + len(value)
	return nil
}

func (b *trackingBatch) Delete(key []byte) error {
	b.size += len(key)
	return nil
}

func (b *trackingBatch) DeleteRange(start, end []byte) error {
	b.size += len(start) + len(end)
	return nil
}

func (b *trackingBatch) ValueSize() int { return b.size }

func (b *trackingBatch) Write() error {
	b.writes++
	return nil
}

func (b *trackingBatch) Reset() { b.size = 0 }

func (b *trackingBatch) Replay(writer ethdb.KeyValueWriter) error { return nil }

func (b *trackingBatch) Close() {
	if !b.closed {
		b.closed = true
		b.closes++
	}
}

var _ ethdb.Batch = (*trackingBatch)(nil)
