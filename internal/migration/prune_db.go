package migration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
)

// pruneDatabase owns the filesystem lock independently of the DB sessions.
// Reopening for verification never releases the lock to another process.
type pruneDatabase struct {
	path    string
	storage storage.Storage
	db      *leveldb.DB
	options opt.Options
}

func openPruneDatabase(path string, cache, handles int, dryRun bool) (*pruneDatabase, error) {
	// goleveldb creates a missing LOCK even in read-only mode. A dry-run
	// therefore requires the stopped source's existing regular lock file.
	if dryRun {
		info, err := os.Lstat(filepath.Join(path, "LOCK"))
		if err != nil {
			return nil, fmt.Errorf("prune dry-run requires an existing LevelDB LOCK file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("prune dry-run requires a regular LevelDB LOCK file")
		}
	}
	s, err := storage.OpenFile(path, dryRun)
	if err != nil {
		return nil, fmt.Errorf("lock prune database: %w", err)
	}
	p := &pruneDatabase{path: path, storage: pruneManifestStorage{Storage: s, path: path}, options: opt.Options{
		ErrorIfMissing: true, ReadOnly: true, Strict: opt.StrictAll,
		Filter:                 filter.NewBloomFilter(10),
		DisableSeeksCompaction: true, BlockCacheCapacity: cache * opt.MiB, OpenFilesCacheCapacity: handles,
	}}
	if err := p.reopen(true); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return p, nil
}

func (p *pruneDatabase) view() ethdb.Database { return rawdb.NewDatabase(readonlydb.Borrow(p.db)) }

func (p *pruneDatabase) reopen(readOnly bool) error {
	if err := p.closeDB(); err != nil {
		return err
	}
	p.options.ReadOnly = readOnly
	db, err := leveldb.Open(p.storage, &p.options)
	if err != nil {
		return fmt.Errorf("open prune database (read-only=%t, repair disabled): %w", readOnly, err)
	}
	p.db = db
	return nil
}

func (p *pruneDatabase) closeDB() error {
	if p.db == nil {
		return nil
	}
	db := p.db
	p.db = nil
	if err := db.Close(); err != nil {
		return fmt.Errorf("close prune database: %w", err)
	}
	return nil
}

func (p *pruneDatabase) close() error { return errors.Join(p.closeDB(), p.storage.Close()) }

// Ancient files are not opened or modified by pruning. All regular LevelDB
// files are synced after the writer closes; the lock remains held.
func syncPruneFiles(ctx context.Context, path string) error {
	err := walkPruneFiles(path, func(e os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return syncFile(filepath.Join(path, e.Name()))
	})
	if err != nil {
		return err
	}
	return syncDirectory(path)
}

func validatePruneDirectory(path string) (string, error) {
	if path == "" {
		return "", errors.New("chaindata is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if real != abs {
		return "", errors.New("prune paths must not contain symlinks")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("prune path must be a directory")
	}
	return abs, nil
}

func validatePruneEntries(path string) error {
	if err := walkPruneFiles(path, func(os.DirEntry) error { return nil }); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(path, "CURRENT"))
	if err != nil {
		return fmt.Errorf("prune requires an existing LevelDB CURRENT: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("LevelDB CURRENT is not a regular file")
	}
	return nil
}

func walkPruneFiles(path string, visit func(os.DirEntry) error) (retErr error) {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open prune directory: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, dir.Close()) }()
	for {
		entries, err := dir.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == "ancient" && entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("unexpected prune database entry %q", entry.Name())
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
