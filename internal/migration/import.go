package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"

	cpebble "github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
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
	dbState, closed, err := buildAndVerifyTarget(ctx, disk, dbPath, opts.Scheme, bundleResult.Manifest.Source, bundleResult.State, opts.CacheMB, opts.Handles, reporter)
	diskClosed = closed
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

func buildAndVerifyTarget(
	ctx context.Context,
	disk ethdb.Database,
	dbPath, scheme string,
	source bundle.SourceEvidence,
	expected StateResult,
	cacheMB, handles int,
	reporter *progressReporter,
) (StateResult, bool, error) {
	var triePercent atomic.Uint64
	triePhase := reporter.StartPhase("generate_trie", percentageProgressSnapshot(&triePercent, true),
		"scheme", scheme,
		"root", expected.Root,
	)
	var (
		stats triedb.GenerateStats
		err   error
	)
	if reporter.Enabled() {
		stats, err = triedb.GenerateTrieWithProgress(disk, scheme, expected.Root, ctx.Done(), &triePercent)
	} else {
		stats, err = triedb.GenerateTrie(disk, scheme, expected.Root, ctx.Done())
	}
	if err != nil {
		generateErr := fmt.Errorf("generate %s state trie: %w", scheme, err)
		if ctxErr := ctx.Err(); ctxErr != nil {
			generateErr = errors.Join(generateErr, ctxErr)
		}
		triePhase.Finish(generateErr)
		return StateResult{}, false, generateErr
	}
	if stats.Scanned != int64(expected.Counts.Accounts) || stats.Updated != 0 || stats.Deleted != 0 {
		reconcileErr := fmt.Errorf("unexpected trie generation reconciliation: scanned=%d updated=%d deleted=%d expected-accounts=%d", stats.Scanned, stats.Updated, stats.Deleted, expected.Counts.Accounts)
		triePhase.Finish(reconcileErr,
			"accounts", stats.Scanned,
			"updated_accounts", stats.Updated,
			"deleted_storage_slots", stats.Deleted,
		)
		return StateResult{}, false, reconcileErr
	}
	triePhase.Finish(nil,
		"accounts", stats.Scanned,
		"updated_accounts", stats.Updated,
		"deleted_storage_slots", stats.Deleted,
	)
	schemePhaseName := "adopt_path_state"
	if scheme == rawdb.HashScheme {
		schemePhaseName = "remove_flat_state"
	}
	schemePhase := reporter.StartPhase(schemePhaseName, nil, "scheme", scheme)
	schemeErr := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scheme == rawdb.HashScheme {
			return removeFlatState(ctx, disk)
		}
		config := *pathdb.Defaults
		config.SnapshotNoBuild = true
		config.EnableStateIndexing = false
		config.TrienodeHistory = -1
		trieDB := triedb.NewDatabase(disk, &triedb.Config{PathDB: &config})
		if err := trieDB.AdoptSyncedState(expected.Root); err != nil {
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
		return StateResult{}, false, schemeErr
	}
	headPhase := reporter.StartPhase("write_head_metadata", nil,
		"block", source.HeadBefore.BlockNumber,
		"hash", source.HeadBefore.BlockHash,
	)
	headErr := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return writeHeadMetadata(disk, source)
	}()
	headPhase.Finish(headErr)
	if headErr != nil {
		return StateResult{}, false, headErr
	}
	diskClosed := false
	finalizePhase := reporter.StartPhase("finalize_database", nil)
	finalizeErr := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := disk.SyncKeyValue(); err != nil {
			return fmt.Errorf("sync target database: %w", err)
		}
		if err := disk.Close(); err != nil {
			return fmt.Errorf("close target database: %w", err)
		}
		diskClosed = true
		ranges := [][2][]byte{{rawdb.CodePrefix, prefixLimit(rawdb.CodePrefix)}}
		if scheme == rawdb.HashScheme {
			ranges = append([][2][]byte{
				{rawdb.SnapshotAccountPrefix, prefixLimit(rawdb.SnapshotAccountPrefix)},
				{rawdb.SnapshotStoragePrefix, prefixLimit(rawdb.SnapshotStoragePrefix)},
			}, ranges...)
		}
		if err := compactPebbleRanges(ctx, dbPath, cacheMB, handles, ranges); err != nil {
			return err
		}
		return syncDirectory(dbPath)
	}()
	finalizePhase.Finish(finalizeErr)
	if finalizeErr != nil {
		return StateResult{}, diskClosed, finalizeErr
	}
	dbState, err := verifyDatabase(ctx, dbPath, scheme, source, expected, cacheMB, handles, reporter)
	if err != nil {
		return StateResult{}, diskClosed, err
	}
	return dbState, diskClosed, nil
}

type flatStateWriter struct {
	batch     ethdb.Batch
	seenCodes map[common.Hash]struct{}
	closed    bool
}

func newFlatStateWriter(db ethdb.Database) *flatStateWriter {
	return &flatStateWriter{batch: db.NewBatch(), seenCodes: make(map[common.Hash]struct{})}
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
	if _, exists := w.seenCodes[codeHash]; exists {
		return nil
	}
	key := prefixedKey(rawdb.CodePrefix, codeHash[:])
	if err := w.batch.Put(key, code); err != nil {
		return fmt.Errorf("write code %s: %w", codeHash, err)
	}
	if err := w.flushIfNeeded(); err != nil {
		return err
	}
	w.seenCodes[codeHash] = struct{}{}
	return nil
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

func removeFlatState(ctx context.Context, db ethdb.Database) error {
	for _, target := range []struct {
		prefix    []byte
		keyLength int
	}{
		{prefix: rawdb.SnapshotAccountPrefix, keyLength: len(rawdb.SnapshotAccountPrefix) + common.HashLength},
		{prefix: rawdb.SnapshotStoragePrefix, keyLength: len(rawdb.SnapshotStoragePrefix) + 2*common.HashLength},
	} {
		if err := deleteKeysWithExactLength(ctx, db, target.prefix, target.keyLength); err != nil {
			return err
		}
		hasEntry, err := hasKeyWithExactLength(ctx, db, target.prefix, target.keyLength)
		if err != nil {
			return err
		}
		if hasEntry {
			return fmt.Errorf("temporary flat state prefix %x is not empty after deletion", target.prefix)
		}
	}
	return nil
}

func deleteKeysWithExactLength(ctx context.Context, db ethdb.Database, prefix []byte, keyLength int) (retErr error) {
	it := db.NewIterator(prefix, nil)
	defer it.Release()
	batch := db.NewBatchWithSize(ethdb.IdealBatchSize)
	defer batch.Close()
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(it.Key()) != keyLength {
			continue
		}
		if err := batch.Delete(append([]byte(nil), it.Key()...)); err != nil {
			return fmt.Errorf("delete temporary flat state key %x: %w", it.Key(), err)
		}
		if batch.ValueSize() >= ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				return fmt.Errorf("flush temporary flat state deletion: %w", err)
			}
			batch.Reset()
		}
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("iterate temporary flat state prefix %x: %w", prefix, err)
	}
	if err := batch.Write(); err != nil {
		return fmt.Errorf("flush final temporary flat state deletion: %w", err)
	}
	return nil
}

func hasKeyWithExactLength(ctx context.Context, db ethdb.Database, prefix []byte, keyLength int) (bool, error) {
	it := db.NewIterator(prefix, nil)
	defer it.Release()
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if len(it.Key()) == keyLength {
			return true, nil
		}
	}
	if err := it.Error(); err != nil {
		return false, fmt.Errorf("inspect temporary flat state prefix %x: %w", prefix, err)
	}
	return false, nil
}

func compactPebbleRanges(ctx context.Context, path string, cacheMB, handles int, ranges [][2][]byte) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	cacheMB = max(cacheMB, 16)
	handles = max(handles, 16)
	cache := cpebble.NewCache(int64(cacheMB) * 1024 * 1024)
	defer cache.Unref()
	memTableSize := cacheMB * 1024 * 1024 / 8
	maxMemTableSize := (1<<31)<<(^uint(0)>>63) - 1
	if memTableSize >= maxMemTableSize {
		memTableSize = maxMemTableSize - 1
	}
	options := &cpebble.Options{
		Cache:                       cache,
		MaxOpenFiles:                handles,
		MemTableSize:                uint64(memTableSize),
		MemTableStopWritesThreshold: 8,
		CompactionConcurrencyRange:  func() (int, int) { return 1, runtime.NumCPU() },
		Levels: [7]cpebble.LevelOptions{
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{FilterPolicy: bloom.FilterPolicy(10)},
			{},
		},
		TargetFileSizes: [7]int64{
			2 * 1024 * 1024,
			4 * 1024 * 1024,
			8 * 1024 * 1024,
			16 * 1024 * 1024,
			32 * 1024 * 1024,
			64 * 1024 * 1024,
			128 * 1024 * 1024,
		},
		Logger:                compactLogger{},
		WALBytesPerSync:       5 * ethdb.IdealBatchSize,
		L0CompactionThreshold: 2,
		FormatMajorVersion:    cpebble.FormatFlushableIngest,
	}
	options.Experimental.ReadSamplingMultiplier = -1
	options.Experimental.L0CompactionConcurrency = 1
	options.Experimental.CompactionDebtConcurrency = 1 << 28
	db, err := cpebble.Open(path, options)
	if err != nil {
		return fmt.Errorf("open target Pebble database for cancellable compaction: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close compacted Pebble database: %w", err))
		}
	}()
	for _, bounds := range ranges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := db.Compact(ctx, bounds[0], bounds[1], true); err != nil {
			return fmt.Errorf("compact target database range %x: %w", bounds[0], err)
		}
	}
	return nil
}

type compactLogger struct{}

func (compactLogger) Infof(string, ...any)  {}
func (compactLogger) Errorf(string, ...any) {}
func (compactLogger) Fatalf(format string, args ...any) {
	panic(fmt.Errorf("fatal: "+format, args...))
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
