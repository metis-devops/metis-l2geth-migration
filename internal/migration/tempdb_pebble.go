package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/ethereum/go-ethereum/ethdb"
)

// memoryPebble implements geth's ownership rules over an injected Pebble FS.
// Resource-release failures from void ethdb methods are latched until Close.
// As with other ethdb implementations, owners join jobs and release iterators
// and batches before closing the database.
type memoryPebble struct {
	db         *pebble.DB
	cache      *pebble.Cache
	mu         sync.RWMutex
	closed     bool
	errorsMu   sync.Mutex
	releaseErr error
}

var _ ethdb.KeyValueStore = (*memoryPebble)(nil)

func openMemoryPebble(fs vfs.FS, path string, cacheMB, handles int, readonly bool) (*memoryPebble, error) {
	cacheMB, handles = max(cacheMB, 16), max(handles, 16)
	cache := pebble.NewCache(int64(cacheMB) * 1024 * 1024)
	// Match the pinned geth adapter's data/cache/compaction settings. Only the
	// filesystem and metrics plumbing differ; WAL remains enabled for reopening.
	memtable := min(uint64(cacheMB)*1024*1024/8, uint64((1<<31)<<(^uint(0)>>63)-2))
	options := &pebble.Options{
		FS: fs, Cache: cache, MaxOpenFiles: handles, ReadOnly: readonly,
		MemTableSize: memtable, MemTableStopWritesThreshold: 8,
		CompactionConcurrencyRange: func() (int, int) { return 1, runtime.NumCPU() },
		Levels: [7]pebble.LevelOptions{
			{FilterPolicy: bloom.FilterPolicy(10)}, {FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)}, {FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)}, {FilterPolicy: bloom.FilterPolicy(10)}, {},
		},
		TargetFileSizes: [7]int64{2 << 20, 4 << 20, 8 << 20, 16 << 20, 32 << 20, 64 << 20, 128 << 20},
		WALBytesPerSync: 5 * ethdb.IdealBatchSize, L0CompactionThreshold: 2,
		FormatMajorVersion: pebble.FormatFlushableIngest, Logger: temporaryPebbleLogger{},
	}
	options.Experimental.ReadSamplingMultiplier = -1
	options.Experimental.L0CompactionConcurrency = 1
	options.Experimental.CompactionDebtConcurrency = 1 << 28
	db, err := pebble.Open(path, options)
	if err != nil {
		cache.Unref()
		return nil, fmt.Errorf("open memory Pebble: %w", err)
	}
	return &memoryPebble{db: db, cache: cache}, nil
}

func (d *memoryPebble) latch(err error) {
	if err != nil {
		d.errorsMu.Lock()
		d.releaseErr = errors.Join(d.releaseErr, err)
		d.errorsMu.Unlock()
	}
}
func (d *memoryPebble) Get(key []byte) ([]byte, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, pebble.ErrClosed
	}
	value, closer, err := d.db.Get(key)
	if err != nil {
		return nil, err
	}
	copyValue := bytes.Clone(value)
	return copyValue, closer.Close()
}
func (d *memoryPebble) Has(key []byte) (bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return false, pebble.ErrClosed
	}
	_, closer, err := d.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, closer.Close()
}
func (d *memoryPebble) Put(key, value []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	return d.db.Set(key, value, pebble.NoSync)
}
func (d *memoryPebble) Delete(key []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	return d.db.Delete(key, pebble.NoSync)
}
func (d *memoryPebble) DeleteRange(start, end []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	if end == nil {
		end = ethdb.MaximumKey
	}
	return d.db.DeleteRange(start, end, pebble.NoSync)
}
func (d *memoryPebble) Stat() (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return "", pebble.ErrClosed
	}
	return d.db.Metrics().String(), nil
}
func (d *memoryPebble) SyncKeyValue() (retErr error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	batch := d.db.NewBatch()
	defer func() { retErr = errors.Join(retErr, batch.Close()) }()
	if err := batch.LogData(nil, nil); err != nil {
		return err
	}
	return d.db.Apply(batch, pebble.Sync)
}
func (d *memoryPebble) Compact(start, end []byte) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	if end == nil {
		end = ethdb.MaximumKey
	}
	return d.db.Compact(context.Background(), start, end, true)
}
func (d *memoryPebble) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.latch(d.db.Close())
		d.cache.Unref()
	}
	d.errorsMu.Lock()
	defer d.errorsMu.Unlock()
	return d.releaseErr
}
func (d *memoryPebble) NewBatch() ethdb.Batch { return d.NewBatchWithSize(0) }
func (d *memoryPebble) NewBatchWithSize(size int) ethdb.Batch {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return &memoryPebbleBatch{owner: d, err: pebble.ErrClosed}
	}
	return &memoryPebbleBatch{owner: d, batch: d.db.NewBatchWithSize(size)}
}
func (d *memoryPebble) NewIterator(prefix, start []byte) ethdb.Iterator {
	d.mu.RLock()
	defer d.mu.RUnlock()
	it := &memoryPebbleIterator{owner: d}
	if d.closed {
		it.err = pebble.ErrClosed
		return it
	}
	lower := append(bytes.Clone(prefix), start...)
	upper := bytes.Clone(prefix)
	for len(upper) > 0 && upper[len(upper)-1] == 255 {
		upper = upper[:len(upper)-1]
	}
	if len(upper) > 0 {
		upper[len(upper)-1]++
	} else {
		upper = nil
	}
	it.iter, it.err = d.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	return it
}

type memoryPebbleBatch struct {
	owner *memoryPebble
	batch *pebble.Batch
	size  int
	err   error
}

func (b *memoryPebbleBatch) Put(key, value []byte) error {
	if b.err != nil {
		return b.err
	}
	if err := b.batch.Set(key, value, nil); err != nil {
		return err
	}
	b.size += len(key) + len(value)
	return nil
}
func (b *memoryPebbleBatch) Delete(key []byte) error {
	if b.err != nil {
		return b.err
	}
	if err := b.batch.Delete(key, nil); err != nil {
		return err
	}
	b.size += len(key)
	return nil
}
func (b *memoryPebbleBatch) DeleteRange(start, end []byte) error {
	if b.err != nil {
		return b.err
	}
	if end == nil {
		end = ethdb.MaximumKey
	}
	if err := b.batch.DeleteRange(start, end, nil); err != nil {
		return err
	}
	b.size += len(start) + len(end)
	return nil
}
func (b *memoryPebbleBatch) ValueSize() int { return b.size }
func (b *memoryPebbleBatch) Write() error {
	b.owner.mu.RLock()
	defer b.owner.mu.RUnlock()
	if b.owner.closed {
		return pebble.ErrClosed
	}
	if b.err != nil {
		return b.err
	}
	return b.batch.Commit(pebble.NoSync)
}
func (b *memoryPebbleBatch) Reset() {
	if b.err == nil {
		b.batch.Reset()
		b.size = 0
	}
}
func (b *memoryPebbleBatch) Close() {
	if b.batch != nil {
		b.owner.latch(b.batch.Close())
		b.batch = nil
	}
	b.err = pebble.ErrClosed
}
func (b *memoryPebbleBatch) Replay(w ethdb.KeyValueWriter) error {
	if b.err != nil {
		return b.err
	}
	reader := b.batch.Reader()
	for {
		kind, key, value, ok, err := reader.Next()
		if err != nil || !ok {
			return err
		}
		if err = replayMemoryOperation(w, kind, key, value); err != nil {
			return err
		}
	}
}
func replayMemoryOperation(w ethdb.KeyValueWriter, kind pebble.InternalKeyKind, key, value []byte) error {
	switch kind {
	case pebble.InternalKeyKindSet:
		return w.Put(key, value)
	case pebble.InternalKeyKindDelete:
		return w.Delete(key)
	case pebble.InternalKeyKindRangeDelete:
		deleter, ok := w.(ethdb.KeyValueRangeDeleter)
		if !ok {
			return errors.New("batch replay requires range deletion support")
		}
		return deleter.DeleteRange(key, value)
	default:
		return fmt.Errorf("unsupported batch operation %v", kind)
	}
}

type memoryPebbleIterator struct {
	owner   *memoryPebble
	iter    *pebble.Iterator
	started bool
	err     error
}

func (i *memoryPebbleIterator) Next() bool {
	if i.err != nil || i.iter == nil {
		return false
	}
	if !i.started {
		i.started = true
		return i.iter.First()
	}
	return i.iter.Next()
}
func (i *memoryPebbleIterator) Error() error {
	if i.iter != nil {
		return errors.Join(i.err, i.iter.Error())
	}
	return i.err
}
func (i *memoryPebbleIterator) Key() []byte {
	if i.iter == nil {
		return nil
	}
	return i.iter.Key()
}
func (i *memoryPebbleIterator) Value() []byte {
	if i.iter == nil {
		return nil
	}
	return i.iter.Value()
}
func (i *memoryPebbleIterator) Release() {
	if i.iter != nil {
		i.err = errors.Join(i.err, i.iter.Error(), i.iter.Close())
		i.owner.latch(i.err)
		i.iter = nil
	}
}
