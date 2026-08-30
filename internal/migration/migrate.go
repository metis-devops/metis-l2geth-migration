package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
)

// MigrateOptions configures direct migration from legacy l2geth state.
type MigrateOptions struct {
	SourceChaindata string
	Output          string
	Scheme          string
	CacheMB         int
	Handles         int
	Progress        ProgressOptions
}

// MigrateResult identifies a directly migrated artifact and its verification report.
type MigrateResult struct {
	ArtifactPath string                   `json:"artifact"`
	Report       DirectVerificationReport `json:"verification"`
}

// Migrate directly rebuilds and verifies a state database without creating a bundle.
func Migrate(ctx context.Context, opts MigrateOptions) (result MigrateResult, retErr error) {
	reporter := newProgressReporter("migrate", opts.Progress,
		"source", opts.SourceChaindata,
		"output", opts.Output,
		"scheme", opts.Scheme,
	)
	defer func() {
		attrs := []any{"artifact", result.ArtifactPath}
		if result.ArtifactPath != "" {
			attrs = append(attrs,
				"block", result.Report.Source.HeadBefore.BlockNumber,
				"root", result.Report.RecomputedRoot,
			)
		}
		reporter.Finish(retErr, attrs...)
	}()
	if opts.SourceChaindata == "" {
		return MigrateResult{}, errors.New("source chaindata path is required")
	}
	if opts.Output == "" {
		return MigrateResult{}, errors.New("artifact output path is required")
	}
	if opts.Scheme != rawdb.HashScheme && opts.Scheme != rawdb.PathScheme {
		return MigrateResult{}, fmt.Errorf("scheme must be %q or %q", rawdb.HashScheme, rawdb.PathScheme)
	}
	if err := rejectOutputInsideSource(opts.SourceChaindata, opts.Output); err != nil {
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

	dbPath := filepath.Join(output.Path(), "db")
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		return MigrateResult{}, fmt.Errorf("create artifact database directory: %w", err)
	}
	diskKV, err := pebble.New(dbPath, opts.CacheMB, opts.Handles, "l2state/migrate", false)
	if err != nil {
		return MigrateResult{}, fmt.Errorf("open target Pebble database: %w", err)
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
	var (
		visitor      StateVisitor = sink
		progressView progressSnapshot
	)
	if reporter.Enabled() {
		counts := new(progressCounts)
		visitor = newCountingStateVisitor(visitor, counts)
		progressView = countProgressSnapshot(counts, nil)
	}
	traversePhase := reporter.StartPhase("migrate_state", progressView, "root", head.StateRoot)
	stateResult, traverseErr := source.Traverse(ctx, visitor)
	traversePhase.Finish(traverseErr)
	if traverseErr != nil {
		if closeErr := sink.Close(); closeErr != nil {
			traverseErr = errors.Join(traverseErr, fmt.Errorf("close flat-state writer: %w", closeErr))
		}
		return MigrateResult{}, traverseErr
	}
	flushPhase := reporter.StartPhase("flush_flat_state", nil)
	if err := sink.Close(); err != nil {
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

	dbState, closed, err := buildAndVerifyTarget(ctx, disk, dbPath, opts.Scheme, sourceEvidence, stateResult, opts.CacheMB, opts.Handles, reporter)
	diskClosed = closed
	if err != nil {
		return MigrateResult{}, err
	}
	if dbState != stateResult {
		return MigrateResult{}, fmt.Errorf("target state result mismatch: database %+v source %+v", dbState, stateResult)
	}
	report := newDirectVerificationReport(sourceEvidence, stateResult, opts.Scheme)
	publishPhase := reporter.StartPhase("publish_artifact", nil, "output", opts.Output)
	if _, err := writeDirectVerificationReport(output.Path(), report); err != nil {
		publishPhase.Finish(err)
		return MigrateResult{}, err
	}
	if err := rejectOutputInsideSource(opts.SourceChaindata, opts.Output); err != nil {
		publishPhase.Finish(err)
		return MigrateResult{}, err
	}
	if err := output.Commit(); err != nil {
		publishPhase.Finish(err)
		return MigrateResult{}, err
	}
	publishPhase.Finish(nil)
	return MigrateResult{ArtifactPath: opts.Output, Report: report}, nil
}
