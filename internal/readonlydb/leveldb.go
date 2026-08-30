package readonlydb

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/ethdb"
	leveldb "github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// ErrReadOnly is returned by every attempted mutation through the adapter.
var ErrReadOnly = errors.New("legacy source database is strictly read-only")

// Database is a deliberately small ethdb.KeyValueStore adapter around
// goleveldb's true read-only mode. It never invokes RecoverFile, compaction or
// any write path when opening a legacy l2geth database.
type Database struct {
	db *leveldb.DB
}

// Open opens a legacy LevelDB in strict read-only mode without recovery.
func Open(path string, cacheMB, handles int) (*Database, error) {
	if cacheMB <= 0 {
		return nil, errors.New("LevelDB cache must be positive")
	}
	if handles <= 0 {
		return nil, errors.New("LevelDB handles must be positive")
	}
	db, err := leveldb.OpenFile(path, &opt.Options{
		ReadOnly:               true,
		OpenFilesCacheCapacity: handles,
		BlockCacheCapacity:     cacheMB * opt.MiB,
		Filter:                 filter.NewBloomFilter(10),
		DisableSeeksCompaction: true,
		Strict:                 opt.StrictAll,
	})
	if err != nil {
		return nil, fmt.Errorf("open legacy LevelDB read-only (automatic recovery disabled): %w", err)
	}
	return &Database{db: db}, nil
}

// Has reports whether key exists in the legacy database.
func (db *Database) Has(key []byte) (bool, error) {
	return db.db.Has(key, nil)
}

// Get returns the value for key from the legacy database.
func (db *Database) Get(key []byte) ([]byte, error) {
	return db.db.Get(key, nil)
}

// Put rejects a write to the legacy database.
func (db *Database) Put([]byte, []byte) error {
	return ErrReadOnly
}

// Delete rejects a deletion from the legacy database.
func (db *Database) Delete([]byte) error {
	return ErrReadOnly
}

// DeleteRange rejects a range deletion from the legacy database.
func (db *Database) DeleteRange([]byte, []byte) error {
	return ErrReadOnly
}

// NewBatch returns a batch that rejects every mutation.
func (db *Database) NewBatch() ethdb.Batch {
	return readOnlyBatch{}
}

// NewBatchWithSize returns a batch that rejects every mutation.
func (db *Database) NewBatchWithSize(int) ethdb.Batch {
	return readOnlyBatch{}
}

// NewIterator returns a read-only iterator over the requested key range.
func (db *Database) NewIterator(prefix, start []byte) ethdb.Iterator {
	rangeSpec := util.BytesPrefix(prefix)
	rangeSpec.Start = append(rangeSpec.Start, start...)
	return db.db.NewIterator(rangeSpec, nil)
}

// Stat returns the underlying LevelDB statistics.
func (db *Database) Stat() (string, error) {
	return db.db.GetProperty("leveldb.stats")
}

// SyncKeyValue is a no-op because the database is read-only.
func (db *Database) SyncKeyValue() error {
	return nil
}

// Compact rejects a compaction request against the legacy database.
func (db *Database) Compact([]byte, []byte) error {
	return ErrReadOnly
}

// Close releases the underlying read-only LevelDB handle.
func (db *Database) Close() error {
	return db.db.Close()
}

type readOnlyBatch struct{}

func (readOnlyBatch) Put([]byte, []byte) error          { return ErrReadOnly }
func (readOnlyBatch) Delete([]byte) error               { return ErrReadOnly }
func (readOnlyBatch) DeleteRange([]byte, []byte) error  { return ErrReadOnly }
func (readOnlyBatch) ValueSize() int                    { return 0 }
func (readOnlyBatch) Write() error                      { return ErrReadOnly }
func (readOnlyBatch) Reset()                            {}
func (readOnlyBatch) Replay(ethdb.KeyValueWriter) error { return ErrReadOnly }
func (readOnlyBatch) Close()                            {}

var _ ethdb.KeyValueStore = (*Database)(nil)
