package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/core/rawdb"
)

// MigrateOptions configures direct migration from legacy l2geth state.
type MigrateOptions struct {
	TempDB          TempDBMode
	SourceChaindata string
	Output          string
	Scheme          string
	DBEngine        string
	CacheMB         int
	Handles         int
	Workers         int
	Progress        ProgressOptions
	OVM             OVMOptions
}

// Migrate directly rebuilds and verifies a state database without creating a bundle.
func Migrate(ctx context.Context, opts MigrateOptions) (result MigrateResult, retErr error) {
	if err := opts.TempDB.validate(); err != nil {
		return result, err
	}
	if err := validateOVMOptions(opts.OVM); err != nil {
		return result, err
	}
	if opts.OVM.Enabled {
		return migrateOVM(ctx, opts)
	}
	workers := normalizeMigrateWorkers(opts.Workers)
	reporter := newProgressReporter("migrate", opts.Progress, "temp_db", opts.TempDB.normalized(),
		"source", opts.SourceChaindata,
		"output", opts.Output,
		"scheme", opts.Scheme, "db_engine", opts.DBEngine, "state_layout", LayoutGeth,
		"workers", workers,
	)
	defer func() {
		attrs := []any{"artifact", result.ArtifactPath}
		if report, ok := result.DirectReport(); ok {
			attrs = append(attrs,
				"block", report.Source.HeadBefore.BlockNumber,
				"root", report.RecomputedRoot,
			)
		}
		reporter.Finish(retErr, attrs...)
	}()
	if err := validateMigrateOptions(opts); err != nil {
		return MigrateResult{}, err
	}
	target, err := targetOptions(opts.DBEngine, opts.Scheme)
	if err != nil {
		return MigrateResult{}, err
	}
	source, err := openLegacySource(opts.SourceChaindata, opts.CacheMB, opts.Handles, reporter)
	if err != nil {
		return MigrateResult{}, err
	}
	defer func() {
		if err := source.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close legacy source database: %w", err))
		}
	}()
	head, _ := source.Head()

	output, err := newAtomicDir(opts.Output)
	if err != nil {
		return MigrateResult{}, err
	}
	defer func() {
		if err := output.Abort(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove partial artifact: %w", err))
		}
	}()
	if err := rejectOutputInsideSource(opts.SourceChaindata, output.Path()); err != nil {
		return MigrateResult{}, err
	}

	dbPath := filepath.Join(output.Path(), artifactDatabaseDirName)
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		return MigrateResult{}, fmt.Errorf("create artifact database directory: %w", err)
	}
	diskKV, err := target.open(dbPath, opts.CacheMB, opts.Handles, false)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("open target database: %w", err)
	}
	disk := rawdb.NewDatabase(diskKV)
	reporter.Info("Target database opened",
		"phase", "prepare_target",
		"status", "completed",
		"path", dbPath,
		"scheme", opts.Scheme, "db_engine", target.engine, "state_layout", target.layout,
	)
	diskClosed := false
	defer func() {
		if !diskClosed {
			if err := disk.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close target database: %w", err))
			}
		}
	}()

	var (
		counts       *progressCounts
		progressView progressSnapshot
	)
	if reporter.Enabled() {
		counts = new(progressCounts)
		progressView = countProgressSnapshot(counts, nil)
	}
	traversePhase := reporter.StartPhase("migrate_state", progressView, "root", head.StateRoot, "workers", workers)
	stateResult, finalWriter, traverseErr := source.migratePartitionedState(ctx, disk, opts.Scheme, workers, counts)
	traversePhase.Finish(traverseErr)
	if traverseErr != nil {
		return MigrateResult{}, traverseErr
	}
	flushPhase := reporter.StartPhase("flush_generated_state", nil, "scheme", opts.Scheme)
	if err := finalWriter.CloseContext(ctx); err != nil {
		flushPhase.Finish(err)
		return MigrateResult{}, err
	}
	flushPhase.Finish(nil)

	confirmPhase := reporter.StartPhase("confirm_source_head", nil)
	sourceEvidence, err := source.ConfirmStableAndClose("migration")
	confirmPhase.Finish(err,
		"block", sourceEvidence.HeadAfter.BlockNumber,
		"hash", sourceEvidence.HeadAfter.BlockHash,
		"root", sourceEvidence.HeadAfter.StateRoot,
	)
	if err != nil {
		return MigrateResult{}, err
	}

	dbState, closed, err := finalizeAndVerifyTarget(ctx, disk, dbPath, opts.Scheme, target, sourceEvidence, stateResult, opts.CacheMB, opts.Handles, reporter, opts.TempDB)
	diskClosed = closed
	if err != nil {
		return MigrateResult{}, err
	}
	if dbState != stateResult {
		return MigrateResult{}, fmt.Errorf("target state result mismatch: database %+v source %+v", dbState, stateResult)
	}
	report := newDirectVerificationReport(sourceEvidence, stateResult, opts.Scheme)
	report.DBEngine, report.StateLayout = target.engine, target.layout
	if err := publishDirectArtifact(ctx, output, report, opts, reporter); err != nil {
		return MigrateResult{}, err
	}
	return newDirectMigrateResult(opts.Output, report), nil
}

func validateMigrateOptions(opts MigrateOptions) error {
	if opts.SourceChaindata == "" {
		return errors.New("source chaindata path is required")
	}
	if opts.Output == "" {
		return errors.New("artifact output path is required")
	}
	if opts.Scheme != rawdb.HashScheme && opts.Scheme != rawdb.PathScheme {
		return fmt.Errorf("scheme must be %q or %q", rawdb.HashScheme, rawdb.PathScheme)
	}
	if opts.Workers > maxMigrateWorkers {
		return fmt.Errorf("workers must not exceed %d", maxMigrateWorkers)
	}
	if _, err := targetOptions(opts.DBEngine, opts.Scheme); err != nil {
		return err
	}
	return rejectOutputInsideSource(opts.SourceChaindata, opts.Output)
}

func normalizeMigrateWorkers(workers int) int {
	return max(workers, minMigrateWorkers)
}

func publishDirectArtifact(ctx context.Context, output *atomicDir, report DirectVerificationReport, opts MigrateOptions, reporter *progressReporter) error {
	return publishArtifact(ctx, output, report, artifactReportCodec[DirectVerificationReport]{
		label: "direct verification report",
		write: func(dir string, report DirectVerificationReport) error {
			_, err := writeDirectVerificationReport(dir, report)
			return err
		},
		load: loadDirectVerificationReport, equal: sameDirectVerificationReport,
	}, func() error { return rejectOutputInsideSource(opts.SourceChaindata, opts.Output) }, reporter)
}
