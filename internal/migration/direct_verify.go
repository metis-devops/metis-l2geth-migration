package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// DirectVerifyOptions configures independent legacy-source and artifact verification.
type DirectVerifyOptions struct {
	SourceChaindata string
	Artifact        string
	CacheMB         int
	Handles         int
	Progress        ProgressOptions
}

// VerifyDirect recomputes legacy source evidence and validates a directly migrated artifact.
func VerifyDirect(ctx context.Context, opts DirectVerifyOptions) (result DirectVerificationReport, retErr error) {
	reporter := newProgressReporter("verify", opts.Progress,
		"source", opts.SourceChaindata,
		"artifact", opts.Artifact,
	)
	defer func() {
		attrs := []any{"scheme", result.Scheme}
		if result.RecomputedRoot != (common.Hash{}) {
			attrs = append(attrs,
				"block", result.Source.HeadBefore.BlockNumber,
				"root", result.RecomputedRoot,
			)
		}
		reporter.Finish(retErr, attrs...)
	}()
	if opts.SourceChaindata == "" {
		return DirectVerificationReport{}, errors.New("source chaindata path is required")
	}
	if opts.Artifact == "" {
		return DirectVerificationReport{}, errors.New("artifact path is required for direct verification")
	}
	stored, err := loadDirectVerificationReport(opts.Artifact)
	if err != nil {
		return DirectVerificationReport{}, err
	}
	source, err := openLegacySource(opts.SourceChaindata, opts.CacheMB, opts.Handles, reporter)
	if err != nil {
		return DirectVerificationReport{}, err
	}
	defer func() {
		if err := source.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close legacy source database: %w", err))
		}
	}()
	head, _ := source.Head()
	var (
		visitor      StateVisitor
		progressView progressSnapshot
	)
	if reporter.Enabled() {
		counts := new(progressCounts)
		visitor = newCountingStateVisitor(nil, counts)
		progressView = countProgressSnapshot(counts, &stored.Counts)
	}
	traversePhase := reporter.StartPhase("verify_source_state", progressView,
		"root", head.StateRoot,
	)
	stateResult, traverseErr := source.Traverse(ctx, visitor)
	traversePhase.Finish(traverseErr, "recomputed_root", stateResult.Root)
	if traverseErr != nil {
		return DirectVerificationReport{}, traverseErr
	}
	confirmPhase := reporter.StartPhase("confirm_source_head", nil)
	sourceEvidence, err := source.ConfirmStableAndClose("direct verification")
	confirmPhase.Finish(err,
		"block", sourceEvidence.HeadAfter.BlockNumber,
		"hash", sourceEvidence.HeadAfter.BlockHash,
		"root", sourceEvidence.HeadAfter.StateRoot,
	)
	if err != nil {
		return DirectVerificationReport{}, err
	}
	if !sameSourceEvidence(stored.Source, sourceEvidence) ||
		stored.Counts != stateResult.Counts || stored.RecomputedRoot != stateResult.Root {
		return DirectVerificationReport{}, errors.New("direct artifact report evidence does not match the legacy source")
	}
	dbState, err := verifyDatabase(ctx, filepath.Join(opts.Artifact, "db"), stored.Scheme, sourceEvidence, stateResult, opts.CacheMB, opts.Handles, reporter, "")
	if err != nil {
		return DirectVerificationReport{}, err
	}
	if dbState != stateResult {
		return DirectVerificationReport{}, fmt.Errorf("artifact state result mismatch: database %+v source %+v", dbState, stateResult)
	}
	return newDirectVerificationReport(sourceEvidence, stateResult, stored.Scheme), nil
}

func sameSourceEvidence(left, right bundle.SourceEvidence) bool {
	return left.HeadBefore == right.HeadBefore &&
		left.HeadAfter == right.HeadAfter &&
		bytes.Equal(left.HeaderRLP, right.HeaderRLP)
}
