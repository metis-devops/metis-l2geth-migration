package migration

import (
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
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
	goleveldb "github.com/syndtr/goleveldb/leveldb"
)

const (
	// DBEnginePebble identifies Pebble v2 in options, internal configuration and reports.
	DBEnginePebble = "pebble"
	// DBEngineLevelDB is the LevelDB engine name used in options and reports.
	DBEngineLevelDB = "leveldb"
)

// StateLayout identifies the target code-key convention independently of its engine.
type StateLayout string

const (
	// LayoutGeth stores contract code under geth's prefixed keys.
	LayoutGeth StateLayout = "geth"
)

// UnmarshalJSON distinguishes an omitted old-report field from an invalid explicit value.
func (l *StateLayout) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode state layout: %w", err)
	}
	if value != string(LayoutGeth) {
		return fmt.Errorf("invalid state layout %q", value)
	}
	*l = StateLayout(value)
	return nil
}

type targetConfig struct {
	engine string
	layout StateLayout
}

func targetOptions(engine, scheme string) (targetConfig, error) {
	if engine == "" {
		engine = DBEnginePebble
	}
	if engine != DBEnginePebble && engine != DBEngineLevelDB {
		return targetConfig{}, fmt.Errorf("db-engine must be %s or %s: %q", DBEnginePebble, DBEngineLevelDB, engine)
	}
	return reportTarget(engine, LayoutGeth, scheme)
}

func reportTarget(engine string, layout StateLayout, scheme string) (targetConfig, error) {
	if layout == "" {
		layout = LayoutGeth // Existing reports predate this field and always used geth layout.
	}
	if engine != DBEnginePebble && engine != DBEngineLevelDB {
		return targetConfig{}, fmt.Errorf("invalid database engine %q", engine)
	}
	if layout != LayoutGeth {
		return targetConfig{}, fmt.Errorf("invalid state layout %q", layout)
	}
	if scheme != rawdb.HashScheme && scheme != rawdb.PathScheme {
		return targetConfig{}, fmt.Errorf("invalid state scheme %q", scheme)
	}
	return targetConfig{engine: engine, layout: layout}, nil
}

func (c targetConfig) open(path string, cacheMB, handles int, readonly bool) (ethdb.KeyValueStore, error) {
	if readonly {
		want := rawdb.DBPebble
		if c.engine == DBEngineLevelDB {
			want = rawdb.DBLeveldb
		}
		if actual := rawdb.PreexistingDatabase(path); actual != want {
			return nil, fmt.Errorf("artifact database engine mismatch: found %q, report specifies %q", actual, c.engine)
		}
	}
	if c.engine == DBEngineLevelDB {
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
	key := prefixedKey(rawdb.CodePrefix, hash[:])
	code, err := db.Get(key)
	if errors.Is(err, cpebble.ErrNotFound) || errors.Is(err, goleveldb.ErrNotFound) {
		return nil, nil
	}
	return code, err
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
