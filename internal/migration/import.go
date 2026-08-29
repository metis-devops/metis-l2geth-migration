package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

// ImportOptions configures a bundle import into a hash- or path-scheme database.
type ImportOptions struct {
	Bundle   string
	Output   string
	Scheme   string
	CacheMB  int
	Handles  int
	Progress ProgressOptions
}

// ImportResult identifies a published state artifact and its verification report.
type ImportResult struct {
	ArtifactPath string
	Report       VerificationReport
}

// Import rebuilds and verifies a state database from a bundle.
func Import(ctx context.Context, opts ImportOptions) (result ImportResult, retErr error) {
	reporter := newProgressReporter("import", opts.Progress,
		"bundle", opts.Bundle,
		"output", opts.Output,
		"scheme", opts.Scheme,
	)
	defer func() {
		attrs := []any{"artifact", result.ArtifactPath}
		if result.ArtifactPath != "" {
			attrs = append(attrs,
				"block", result.Report.Head.BlockNumber,
				"root", result.Report.RecomputedRoot,
			)
		}
		reporter.Finish(retErr, attrs...)
	}()
	if opts.Bundle == "" {
		return ImportResult{}, errors.New("bundle path is required")
	}
	if opts.Output == "" {
		return ImportResult{}, errors.New("artifact output path is required")
	}
	if opts.Scheme != rawdb.HashScheme && opts.Scheme != rawdb.PathScheme {
		return ImportResult{}, fmt.Errorf("scheme must be %q or %q", rawdb.HashScheme, rawdb.PathScheme)
	}
	output, err := newAtomicDir(opts.Output)
	if err != nil {
		return ImportResult{}, err
	}
	defer func() {
		if err := output.Abort(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove partial artifact: %w", err))
		}
	}()
	dbPath := filepath.Join(output.Path(), "db")
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		return ImportResult{}, fmt.Errorf("create artifact database directory: %w", err)
	}
	diskKV, err := pebble.New(dbPath, opts.CacheMB, opts.Handles, "l2state/import", false)
	if err != nil {
		return ImportResult{}, fmt.Errorf("open target Pebble database: %w", err)
	}
	disk := rawdb.NewDatabase(diskKV)
	reporter.Info("Target database opened",
		"phase", "prepare_target",
		"status", "completed",
		"path", dbPath,
		"scheme", opts.Scheme,
	)
	diskClosed := false
	defer func() {
		if !diskClosed {
			if err := disk.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close target database: %w", err))
			}
		}
	}()

	sink := newFlatStateWriter(disk)
	bundleResult, err := scanBundle(ctx, opts.Bundle, sink, reporter)
	if err != nil {
		if closeErr := sink.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close flat-state writer: %w", closeErr))
		}
		return ImportResult{}, err
	}
	flushPhase := reporter.StartPhase("flush_flat_state", nil)
	if err := sink.Close(); err != nil {
		flushPhase.Finish(err)
		return ImportResult{}, err
	}
	flushPhase.Finish(nil)
	var triePercent atomic.Uint64
	triePhase := reporter.StartPhase("generate_trie", percentageProgressSnapshot(&triePercent, true),
		"scheme", opts.Scheme,
		"root", bundleResult.State.Root,
	)
	var stats triedb.GenerateStats
	if reporter.Enabled() {
		stats, err = triedb.GenerateTrieWithProgress(disk, opts.Scheme, bundleResult.State.Root, ctx.Done(), &triePercent)
	} else {
		stats, err = triedb.GenerateTrie(disk, opts.Scheme, bundleResult.State.Root, ctx.Done())
	}
	if err != nil {
		generateErr := fmt.Errorf("generate %s state trie: %w", opts.Scheme, err)
		if ctxErr := ctx.Err(); ctxErr != nil {
			generateErr = errors.Join(generateErr, ctxErr)
		}
		triePhase.Finish(generateErr)
		return ImportResult{}, generateErr
	}
	if stats.Scanned != int64(bundleResult.State.Counts.Accounts) || stats.Updated != 0 || stats.Deleted != 0 {
		reconcileErr := fmt.Errorf("unexpected trie generation reconciliation: scanned=%d updated=%d deleted=%d expected-accounts=%d", stats.Scanned, stats.Updated, stats.Deleted, bundleResult.State.Counts.Accounts)
		triePhase.Finish(reconcileErr,
			"accounts", stats.Scanned,
			"updated_accounts", stats.Updated,
			"deleted_storage_slots", stats.Deleted,
		)
		return ImportResult{}, reconcileErr
	}
	triePhase.Finish(nil,
		"accounts", stats.Scanned,
		"updated_accounts", stats.Updated,
		"deleted_storage_slots", stats.Deleted,
	)
	schemePhaseName := "adopt_path_state"
	if opts.Scheme == rawdb.HashScheme {
		schemePhaseName = "remove_flat_state"
	}
	schemePhase := reporter.StartPhase(schemePhaseName, nil, "scheme", opts.Scheme)
	schemeErr := func() error {
		if opts.Scheme == rawdb.HashScheme {
			return removeFlatState(disk)
		}
		config := *pathdb.Defaults
		config.SnapshotNoBuild = true
		config.EnableStateIndexing = false
		config.TrienodeHistory = -1
		trieDB := triedb.NewDatabase(disk, &triedb.Config{PathDB: &config})
		if err := trieDB.AdoptSyncedState(bundleResult.State.Root); err != nil {
			adoptErr := fmt.Errorf("adopt generated path state: %w", err)
			if closeErr := trieDB.Close(); closeErr != nil {
				adoptErr = errors.Join(adoptErr, fmt.Errorf("close path trie database: %w", closeErr))
			}
			return adoptErr
		}
		if !trieDB.SnapshotCompleted() {
			snapshotErr := errors.New("path snapshot is not marked complete after adoption")
			if closeErr := trieDB.Close(); closeErr != nil {
				snapshotErr = errors.Join(snapshotErr, fmt.Errorf("close path trie database: %w", closeErr))
			}
			return snapshotErr
		}
		if err := trieDB.Close(); err != nil {
			return fmt.Errorf("close adopted path trie database: %w", err)
		}
		return nil
	}()
	schemePhase.Finish(schemeErr)
	if schemeErr != nil {
		return ImportResult{}, schemeErr
	}
	finalizePhase := reporter.StartPhase("finalize_database", nil)
	finalizeErr := func() error {
		if err := disk.Compact(rawdb.CodePrefix, prefixLimit(rawdb.CodePrefix)); err != nil {
			return fmt.Errorf("compact contract code range: %w", err)
		}
		if err := disk.SyncKeyValue(); err != nil {
			return fmt.Errorf("sync target database: %w", err)
		}
		if err := disk.Close(); err != nil {
			return fmt.Errorf("close target database: %w", err)
		}
		diskClosed = true
		return nil
	}()
	finalizePhase.Finish(finalizeErr)
	if finalizeErr != nil {
		return ImportResult{}, finalizeErr
	}

	dbState, err := verifyDatabase(ctx, dbPath, opts.Scheme, bundleResult.Manifest, opts.CacheMB, opts.Handles, reporter)
	if err != nil {
		return ImportResult{}, err
	}
	if dbState != bundleResult.State {
		return ImportResult{}, fmt.Errorf("target state result mismatch: database %+v bundle %+v", dbState, bundleResult.State)
	}
	report := newVerificationReport(bundleResult, opts.Scheme)
	publishPhase := reporter.StartPhase("publish_artifact", nil, "output", opts.Output)
	if _, err := writeVerificationReport(output.Path(), report); err != nil {
		publishPhase.Finish(err)
		return ImportResult{}, err
	}
	if err := output.Commit(); err != nil {
		publishPhase.Finish(err)
		return ImportResult{}, err
	}
	publishPhase.Finish(nil)
	return ImportResult{ArtifactPath: opts.Output, Report: report}, nil
}

type flatStateWriter struct {
	db     ethdb.Database
	batch  ethdb.Batch
	closed bool
}

func newFlatStateWriter(db ethdb.Database) *flatStateWriter {
	return &flatStateWriter{db: db, batch: db.NewBatch()}
}

func (w *flatStateWriter) Account(hash common.Hash, account *types.StateAccount, _ []byte) error {
	key := prefixedKey(rawdb.SnapshotAccountPrefix, hash[:])
	if err := w.batch.Put(key, types.SlimAccountRLP(*account)); err != nil {
		return fmt.Errorf("write flat account %s: %w", hash, err)
	}
	return w.flushIfNeeded()
}

func (w *flatStateWriter) Storage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	key := make([]byte, 0, len(rawdb.SnapshotStoragePrefix)+2*common.HashLength)
	key = append(key, rawdb.SnapshotStoragePrefix...)
	key = append(key, accountHash[:]...)
	key = append(key, slotHash[:]...)
	if err := w.batch.Put(key, valueRLP); err != nil {
		return fmt.Errorf("write flat account %s slot %s: %w", accountHash, slotHash, err)
	}
	return w.flushIfNeeded()
}

func (w *flatStateWriter) Code(_ common.Hash, codeHash common.Hash, code []byte) error {
	key := prefixedKey(rawdb.CodePrefix, codeHash[:])
	if err := w.batch.Put(key, code); err != nil {
		return fmt.Errorf("write code %s: %w", codeHash, err)
	}
	return w.flushIfNeeded()
}

func (w *flatStateWriter) flushIfNeeded() error {
	if w.batch.ValueSize() < ethdb.IdealBatchSize {
		return nil
	}
	if err := w.batch.Write(); err != nil {
		return fmt.Errorf("flush imported flat state: %w", err)
	}
	w.batch.Reset()
	return nil
}

func (w *flatStateWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.batch.Close()
	if err := w.batch.Write(); err != nil {
		return fmt.Errorf("flush final imported flat state: %w", err)
	}
	return nil
}

func removeFlatState(db ethdb.Database) error {
	ranges := [][2][]byte{
		{rawdb.SnapshotAccountPrefix, prefixLimit(rawdb.SnapshotAccountPrefix)},
		{rawdb.SnapshotStoragePrefix, prefixLimit(rawdb.SnapshotStoragePrefix)},
	}
	for _, bounds := range ranges {
		if err := db.DeleteRange(bounds[0], bounds[1]); err != nil {
			return fmt.Errorf("delete temporary flat state range %x: %w", bounds[0], err)
		}
		if err := db.Compact(bounds[0], bounds[1]); err != nil {
			return fmt.Errorf("compact temporary flat state range %x: %w", bounds[0], err)
		}
		it := db.NewIterator(bounds[0], nil)
		hasEntry := it.Next()
		iterErr := it.Error()
		it.Release()
		if iterErr != nil {
			return fmt.Errorf("inspect removed flat state range %x: %w", bounds[0], iterErr)
		}
		if hasEntry {
			return fmt.Errorf("temporary flat state range %x is not empty after deletion", bounds[0])
		}
	}
	return nil
}

func prefixLimit(prefix []byte) []byte {
	limit := append([]byte(nil), prefix...)
	for i := len(limit) - 1; i >= 0; i-- {
		if limit[i] != 0xff {
			limit[i]++
			return limit[:i+1]
		}
	}
	return nil
}

func prefixedKey(prefix, suffix []byte) []byte {
	key := make([]byte, 0, len(prefix)+len(suffix))
	key = append(key, prefix...)
	key = append(key, suffix...)
	return key
}
