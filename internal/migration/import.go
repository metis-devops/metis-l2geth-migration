package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
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
	ArtifactPath string             `json:"artifact"`
	Report       VerificationReport `json:"verification"`
}

// Import rebuilds and verifies a state database from a bundle.
func Import(ctx context.Context, opts ImportOptions) (result ImportResult, retErr error) {
	reporter := newProgressReporter("import", opts.Progress, "bundle", opts.Bundle, "output", opts.Output, "scheme", opts.Scheme)
	defer func() {
		attrs := []any{"artifact", result.ArtifactPath}
		if result.ArtifactPath != "" {
			attrs = append(attrs, "block", result.Report.Head.BlockNumber, "root", result.Report.RecomputedRoot)
		}
		reporter.Finish(retErr, attrs...)
	}()
	if err := validateImportOptions(opts); err != nil {
		return ImportResult{}, err
	}
	if err := rejectOutputInsideBundle(opts.Bundle, opts.Output); err != nil {
		return ImportResult{}, err
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
	reporter.Info("Target database opened", "phase", "prepare_target", "status", "completed", "path", dbPath, "scheme", opts.Scheme)
	diskClosed := false
	defer func() {
		if !diskClosed {
			if err := disk.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close target database: %w", err))
			}
		}
	}()

	sink := newDirectStateWriter(disk, opts.Scheme)
	bundleResult, err := scanBundle(ctx, opts.Bundle, bundleScanOptions{Visitor: sink, TrieNodes: sink, BorrowRecords: true}, reporter)
	if err != nil {
		sink.Abort()
		return ImportResult{}, err
	}
	flushPhase := reporter.StartPhase("flush_generated_state", nil, "scheme", opts.Scheme)
	if err := sink.CloseContext(ctx); err != nil {
		flushPhase.Finish(err)
		return ImportResult{}, err
	}
	flushPhase.Finish(nil)

	dbState, closed, err := finalizeAndVerifyTarget(ctx, disk, dbPath, opts.Scheme, bundleResult.Manifest.Source, bundleResult.State, opts.CacheMB, opts.Handles, reporter)
	diskClosed = closed
	if err != nil {
		return ImportResult{}, err
	}
	if dbState != bundleResult.State {
		return ImportResult{}, fmt.Errorf("target state result mismatch: database %+v bundle %+v", dbState, bundleResult.State)
	}
	report := newVerificationReport(bundleResult, opts.Scheme)
	if err := publishImportedArtifact(ctx, output, report, opts.Output, reporter); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{ArtifactPath: opts.Output, Report: report}, nil
}

func validateImportOptions(opts ImportOptions) error {
	if opts.Bundle == "" {
		return errors.New("bundle path is required")
	}
	if opts.Output == "" {
		return errors.New("artifact output path is required")
	}
	if opts.Scheme != rawdb.HashScheme && opts.Scheme != rawdb.PathScheme {
		return fmt.Errorf("scheme must be %q or %q", rawdb.HashScheme, rawdb.PathScheme)
	}
	return nil
}

func publishImportedArtifact(ctx context.Context, output *atomicDir, report VerificationReport, final string, reporter *progressReporter) error {
	phase := reporter.StartPhase("publish_artifact", nil, "output", final)
	if err := ctx.Err(); err != nil {
		phase.Finish(err)
		return err
	}
	if _, err := writeVerificationReport(output.Path(), report); err != nil {
		phase.Finish(err)
		return err
	}
	stored, err := loadVerificationReport(output.Path())
	if err != nil {
		phase.Finish(err)
		return fmt.Errorf("re-open generated verification report: %w", err)
	}
	if stored != report {
		err := errors.New("re-opened verification report does not match generated report")
		phase.Finish(err)
		return err
	}
	if err := ctx.Err(); err != nil {
		phase.Finish(err)
		return err
	}
	if err := output.Commit(); err != nil {
		phase.Finish(err)
		return err
	}
	phase.Finish(nil)
	return nil
}

func finalizeAndVerifyTarget(
	ctx context.Context,
	disk ethdb.Database,
	dbPath, scheme string,
	source bundle.SourceEvidence,
	expected StateResult,
	cacheMB, handles int,
	reporter *progressReporter,
) (StateResult, bool, error) {
	if err := adoptPathState(ctx, disk, scheme, expected.Root, reporter); err != nil {
		return StateResult{}, false, err
	}
	if err := persistHeadMetadata(ctx, disk, source, reporter); err != nil {
		return StateResult{}, false, err
	}
	diskClosed, err := finalizeTargetDatabase(ctx, disk, dbPath, reporter)
	if err != nil {
		return StateResult{}, diskClosed, err
	}
	state, err := verifyDatabase(ctx, dbPath, scheme, source, expected, cacheMB, handles, reporter, filepath.Dir(dbPath))
	if err != nil {
		return StateResult{}, diskClosed, err
	}
	return state, diskClosed, nil
}

func adoptPathState(ctx context.Context, disk ethdb.Database, scheme string, root common.Hash, reporter *progressReporter) error {
	if scheme == rawdb.HashScheme {
		return ctx.Err()
	}
	phase := reporter.StartPhase("adopt_path_state", nil, "scheme", scheme)
	err := adoptPathStateDatabase(ctx, disk, root)
	phase.Finish(err)
	return err
}

func adoptPathStateDatabase(ctx context.Context, disk ethdb.Database, root common.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	config := *pathdb.Defaults
	config.SnapshotNoBuild = true
	config.EnableStateIndexing = false
	config.TrienodeHistory = -1
	trieDB := triedb.NewDatabase(disk, &triedb.Config{PathDB: &config})
	if err := trieDB.AdoptSyncedState(root); err != nil {
		return closeTrieDBWithError(trieDB, fmt.Errorf("adopt generated path state: %w", err))
	}
	if !trieDB.SnapshotCompleted() {
		return closeTrieDBWithError(trieDB, errors.New("path snapshot is not marked complete after adoption"))
	}
	if err := trieDB.Close(); err != nil {
		return fmt.Errorf("close adopted path trie database: %w", err)
	}
	return nil
}

func closeTrieDBWithError(trieDB *triedb.Database, original error) error {
	if err := trieDB.Close(); err != nil {
		return errors.Join(original, fmt.Errorf("close path trie database: %w", err))
	}
	return original
}

func persistHeadMetadata(ctx context.Context, disk ethdb.Database, source bundle.SourceEvidence, reporter *progressReporter) error {
	phase := reporter.StartPhase("write_head_metadata", nil, "block", source.HeadBefore.BlockNumber, "hash", source.HeadBefore.BlockHash)
	if err := ctx.Err(); err != nil {
		phase.Finish(err)
		return err
	}
	err := writeHeadMetadata(disk, source)
	phase.Finish(err)
	return err
}

func finalizeTargetDatabase(ctx context.Context, disk ethdb.Database, dbPath string, reporter *progressReporter) (closed bool, retErr error) {
	phase := reporter.StartPhase("finalize_database", nil)
	defer func() { phase.Finish(retErr) }()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := disk.SyncKeyValue(); err != nil {
		return false, fmt.Errorf("sync target database: %w", err)
	}
	if err := disk.Close(); err != nil {
		return false, fmt.Errorf("close target database: %w", err)
	}
	closed = true
	if err := syncDirectory(dbPath); err != nil {
		return true, err
	}
	return true, nil
}
