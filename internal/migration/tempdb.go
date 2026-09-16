package migration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

// TempDBMode selects storage for operation-local databases, never artifacts.
type TempDBMode string

const (
	// TempDBDisk is the default, bounded-cache disk-backed mode.
	TempDBDisk TempDBMode = "disk"
	// TempDBMemory keeps Pebble files in RAM for the lifetime of an operation.
	TempDBMemory TempDBMode = "memory"
)

func (m TempDBMode) normalized() TempDBMode {
	if m == "" {
		return TempDBDisk
	}
	return m
}

func (m TempDBMode) validate() error {
	if m.normalized() != TempDBDisk && m != TempDBMemory {
		return fmt.Errorf("temp-db must be disk or memory: %q", m)
	}
	return nil
}

// temporaryStorage owns the filesystem across close/read-only reopen/write
// reopen. Handles belong to the caller and must close before removing files.
type temporaryStorage struct {
	fs vfs.FS // nil keeps the existing physical disk implementation
}

func newTemporaryStorage(mode TempDBMode) *temporaryStorage {
	s := new(temporaryStorage)
	if mode == TempDBMemory {
		s.fs = vfs.NewMem()
	}
	return s
}

func (s *temporaryStorage) open(path string, cache, handles int, readonly bool) (ethdb.Database, error) {
	if s.fs != nil {
		kv, err := openMemoryPebble(s.fs, filepath.ToSlash(path), cache, handles, readonly)
		if err != nil {
			return nil, err
		}
		return rawdb.NewDatabase(kv), nil
	}
	kv, err := (targetConfig{engine: "pebble-v2", layout: LayoutGeth}).open(path, cache, handles, readonly)
	if err != nil {
		return nil, err
	}
	return rawdb.NewDatabase(kv), nil
}

func (s *temporaryStorage) create(path string, cache, handles int) (ethdb.Database, error) {
	if s.fs == nil {
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, err
		}
	} else if err := s.fs.MkdirAll(filepath.ToSlash(path), 0700); err != nil {
		return nil, err
	}
	return s.open(path, cache, handles, false)
}

func (s *temporaryStorage) remove(path string) error {
	if s.fs == nil {
		return os.RemoveAll(path)
	}
	return s.fs.RemoveAll(filepath.ToSlash(path))
}

// temporaryStorageObserverKey permits operation-scoped instrumentation without
// global hooks or retaining files after an operation. Observers must be safe for
// concurrent registrations. Production callers leave it unset.
type temporaryStorageObserverKey struct{}

func observeTemporaryStorage(ctx context.Context, storage *temporaryStorage) {
	if observe, ok := ctx.Value(temporaryStorageObserverKey{}).(func(vfs.FS)); ok && storage.fs != nil {
		observe(storage.fs)
	}
}
