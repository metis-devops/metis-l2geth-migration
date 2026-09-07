package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	cpebble "github.com/cockroachdb/pebble/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
	goleveldb "github.com/syndtr/goleveldb/leveldb"
)

// StateLayout identifies the target code-key convention independently of its engine.
type StateLayout string

const (
	// LayoutGeth stores contract code under geth's prefixed keys.
	LayoutGeth StateLayout = "geth"
	// LayoutLegacyL2Geth stores contract code under bare hashes.
	LayoutLegacyL2Geth StateLayout = "legacy-l2geth"
)

// UnmarshalJSON distinguishes an omitted old-report field from an invalid explicit value.
func (l *StateLayout) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode state layout: %w", err)
	}
	if value != string(LayoutGeth) && value != string(LayoutLegacyL2Geth) {
		return fmt.Errorf("invalid state layout %q", value)
	}
	*l = StateLayout(value)
	return nil
}

type targetConfig struct {
	engine string
	layout StateLayout
}

func targetOptions(engine, layout, scheme string) (targetConfig, error) {
	if engine == "" {
		engine = "pebble"
	}
	if engine == "pebble" {
		engine = "pebble-v2"
	} else if engine != "leveldb" {
		return targetConfig{}, fmt.Errorf("db-engine must be pebble or leveldb: %q", engine)
	}
	return reportTarget(engine, StateLayout(layout), scheme)
}

func reportTarget(engine string, layout StateLayout, scheme string) (targetConfig, error) {
	if layout == "" {
		layout = LayoutGeth // Existing reports predate this field and always used geth layout.
	}
	if engine != "pebble-v2" && engine != "leveldb" {
		return targetConfig{}, fmt.Errorf("invalid database engine %q", engine)
	}
	if layout != LayoutGeth && layout != LayoutLegacyL2Geth {
		return targetConfig{}, fmt.Errorf("invalid state layout %q", layout)
	}
	if scheme != rawdb.HashScheme && scheme != rawdb.PathScheme {
		return targetConfig{}, fmt.Errorf("invalid state scheme %q", scheme)
	}
	if layout == LayoutLegacyL2Geth && (engine != "leveldb" || scheme != rawdb.HashScheme) {
		return targetConfig{}, errors.New("legacy-l2geth layout requires --db-engine leveldb --scheme hash")
	}
	return targetConfig{engine: engine, layout: layout}, nil
}

func (c targetConfig) open(path string, cacheMB, handles int, readonly bool) (ethdb.KeyValueStore, error) {
	if readonly {
		want := rawdb.DBPebble
		if c.engine == "leveldb" {
			want = rawdb.DBLeveldb
		}
		if actual := rawdb.PreexistingDatabase(path); actual != want {
			return nil, fmt.Errorf("artifact database engine mismatch: found %q, report specifies %q", actual, c.engine)
		}
	}
	if c.engine == "leveldb" {
		if readonly {
			// Match the writable geth adapter's minimum resource allowances.
			return readonlydb.Open(path, max(cacheMB, 16), max(handles, 16))
		}
		// Only this invocation's empty staging directory can reach geth's writable opener.
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("inspect new LevelDB directory: %w", err)
		}
		if len(entries) != 0 {
			return nil, errors.New("new LevelDB target directory is not empty")
		}
		return leveldb.New(path, cacheMB, handles, "l2state/target", false)
	}
	return pebble.New(path, cacheMB, handles, "l2state/target", readonly)
}

func (c targetConfig) readCode(db ethdb.KeyValueReader, hash common.Hash) ([]byte, error) {
	key := hash[:]
	if c.layout != LayoutLegacyL2Geth {
		key = prefixedKey(rawdb.CodePrefix, key)
	}
	code, err := db.Get(key)
	if errors.Is(err, cpebble.ErrNotFound) || errors.Is(err, goleveldb.ErrNotFound) {
		return nil, nil
	}
	return code, err
}

// finalizeLegacyCode runs after every builder has joined and flushed. Staging code
// with a prefix protects code/node aliases from partition-root orphan deletion.
func finalizeLegacyCode(ctx context.Context, disk ethdb.Database) error {
	it := disk.NewIterator(rawdb.CodePrefix, nil)
	defer it.Release()
	batch := disk.NewBatch()
	defer batch.Close()
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, code := it.Key(), it.Value()
		// Hash trie nodes can also start with the byte 'c'. They are 32-byte
		// keys, distinct from the 33-byte prefixed staging-code entries.
		if len(key) == common.HashLength {
			continue
		}
		if len(key) != len(rawdb.CodePrefix)+common.HashLength || len(code) == 0 || !bytes.Equal(crypto.Keccak256(code), key[len(rawdb.CodePrefix):]) {
			return fmt.Errorf("invalid staged legacy code key %x", key)
		}
		hash := key[len(rawdb.CodePrefix):]
		if err := checkSharedLegacyKey(disk, hash, code); err != nil {
			return err
		}
		if err := batch.Put(hash, code); err != nil {
			return fmt.Errorf("write legacy code: %w", err)
		}
		if err := batch.Delete(key); err != nil {
			return fmt.Errorf("remove staged legacy code: %w", err)
		}
		if batch.ValueSize() >= ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return fmt.Errorf("flush legacy code: %w", err)
			}
			batch.Reset()
		}
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("iterate staged legacy code: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return fmt.Errorf("flush final legacy code: %w", err)
	}
	return nil
}

func checkSharedLegacyKey(disk ethdb.KeyValueReader, hash, code []byte) error {
	has, err := disk.Has(hash)
	if err != nil {
		return fmt.Errorf("check legacy code/node alias: %w", err)
	}
	if !has {
		return nil
	}
	existing, err := disk.Get(hash)
	if err != nil {
		return fmt.Errorf("read legacy code/node alias: %w", err)
	}
	if !bytes.Equal(existing, code) {
		return fmt.Errorf("legacy code/node alias has conflicting bytes at %x", hash)
	}
	return nil
}

// syncLevelDBFiles supplies durability that geth's no-op LevelDB SyncKeyValue
// cannot provide. Call only after closing the writer and all of its tasks.
func syncLevelDBFiles(ctx context.Context, path string, syncOne func(string) error) (retErr error) {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open closed LevelDB directory: %w", err)
	}
	defer func() {
		if err := dir.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close LevelDB directory handle: %w", err))
		}
	}()
	for {
		entries, err := dir.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("list closed LevelDB files: %w", err)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat closed LevelDB file: %w", err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("closed LevelDB contains non-regular entry %q", entry.Name())
			}
			if err := syncOne(filepath.Join(path, entry.Name())); err != nil {
				return fmt.Errorf("sync closed LevelDB file: %w", err)
			}
		}
		if errors.Is(err, io.EOF) {
			return ctx.Err()
		}
	}
}
