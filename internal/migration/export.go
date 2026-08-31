package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// ExportOptions configures a legacy LevelDB state export.
type ExportOptions struct {
	SourceChaindata string
	Output          string
	Compression     string
	CacheMB         int
	Handles         int
	Progress        ProgressOptions
}

// ExportResult identifies a published bundle and its manifest.
type ExportResult struct {
	BundlePath string          `json:"bundle"`
	Manifest   bundle.Manifest `json:"manifest"`
}

// Export writes the latest canonical legacy state into a new audited bundle.
func Export(ctx context.Context, opts ExportOptions) (result ExportResult, retErr error) {
	if opts.Compression == "" {
		opts.Compression = bundle.CompressionZstd
	}
	reporter := newProgressReporter("export", opts.Progress,
		"source", opts.SourceChaindata,
		"output", opts.Output,
		"compression", opts.Compression,
	)
	defer func() {
		attrs := []any{"bundle", result.BundlePath}
		if result.BundlePath != "" {
			attrs = append(attrs,
				"block", result.Manifest.Source.HeadBefore.BlockNumber,
				"root", result.Manifest.Source.HeadBefore.StateRoot,
			)
		}
		reporter.Finish(retErr, attrs...)
	}()
	if err := validateExportOptions(opts); err != nil {
		return ExportResult{}, err
	}
	source, err := openLegacySource(opts.SourceChaindata, opts.CacheMB, opts.Handles, reporter)
	if err != nil {
		return ExportResult{}, err
	}
	defer func() {
		if err := source.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close legacy source database: %w", err))
		}
	}()
	headBefore, headerRLP := source.Head()
	output, err := newAtomicDir(opts.Output)
	if err != nil {
		return ExportResult{}, err
	}
	defer func() {
		if err := output.Abort(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove partial bundle: %w", err))
		}
	}()
	if err := rejectOutputInsideSource(opts.SourceChaindata, output.Path()); err != nil {
		return ExportResult{}, err
	}

	recordWriter, err := bundle.NewWriter(ctx, output.Path(), opts.Compression, headBefore, headerRLP)
	if err != nil {
		return ExportResult{}, err
	}
	var (
		visitor      StateVisitor = &recordWriterVisitor{writer: recordWriter}
		progressView progressSnapshot
	)
	if reporter.Enabled() {
		counts := new(progressCounts)
		visitor = newCountingStateVisitor(visitor, counts)
		progressView = countProgressSnapshot(counts, nil)
	}
	traversePhase := reporter.StartPhase("export_state", progressView, "root", headBefore.StateRoot)
	state, traverseErr := source.Traverse(ctx, visitor)
	traversePhase.Finish(traverseErr)
	if traverseErr != nil {
		if err := recordWriter.Abort(); err != nil {
			traverseErr = errors.Join(traverseErr, fmt.Errorf("abort state record writer: %w", err))
		}
		return ExportResult{}, traverseErr
	}
	finalizePhase := reporter.StartPhase("finalize_bundle", nil, "output", opts.Output)
	manifest, writerResult, finalizeErr := finalizeExportBundle(ctx, source, recordWriter, state, output.Path())
	finalizePhase.Finish(finalizeErr,
		"records", writerResult.Counts.Records,
		"state_file_size", writerResult.Size,
	)
	if finalizeErr != nil {
		return ExportResult{}, finalizeErr
	}
	if err := publishExportBundle(ctx, output, opts, reporter); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{BundlePath: opts.Output, Manifest: manifest}, nil
}

func validateExportOptions(opts ExportOptions) error {
	if opts.SourceChaindata == "" {
		return errors.New("source chaindata path is required")
	}
	if opts.Output == "" {
		return errors.New("bundle output path is required")
	}
	return rejectOutputInsideSource(opts.SourceChaindata, opts.Output)
}

func finalizeExportBundle(ctx context.Context, source *legacySource, writer *bundle.Writer, state StateResult, outputPath string) (bundle.Manifest, bundle.WriterResult, error) {
	if err := ctx.Err(); err != nil {
		if abortErr := writer.Abort(); abortErr != nil {
			return bundle.Manifest{}, bundle.WriterResult{}, errors.Join(err, abortErr)
		}
		return bundle.Manifest{}, bundle.WriterResult{}, err
	}
	writerResult, err := writer.Close()
	if err != nil {
		return bundle.Manifest{}, bundle.WriterResult{}, err
	}
	if writerResult.Counts != state.Counts {
		return bundle.Manifest{}, writerResult, fmt.Errorf("export record counts mismatch: writer %+v traversal %+v", writerResult.Counts, state.Counts)
	}
	if err := ctx.Err(); err != nil {
		return bundle.Manifest{}, writerResult, err
	}
	sourceEvidence, err := source.ConfirmStableAndClose("export")
	if err != nil {
		return bundle.Manifest{}, writerResult, err
	}
	manifest := bundle.NewManifest(sourceEvidence, state.Counts, bundle.StateFile{
		Name: writerResult.FileName, Compression: writerResult.Compression, Size: writerResult.Size,
		RecordPayloadBytes: writerResult.RecordPayloadBytes,
		SHA256:             writerResult.SHA256, RecordChainHash: writerResult.RecordChainHash,
	})
	if _, err := bundle.WriteManifest(outputPath, manifest); err != nil {
		return bundle.Manifest{}, writerResult, err
	}
	if err := syncFile(filepath.Join(outputPath, bundle.ManifestFileName)); err != nil {
		return bundle.Manifest{}, writerResult, err
	}
	stored, _, err := bundle.LoadManifest(outputPath)
	if err != nil {
		return bundle.Manifest{}, writerResult, fmt.Errorf("re-open generated manifest: %w", err)
	}
	if !sameManifest(stored, manifest) {
		return bundle.Manifest{}, writerResult, errors.New("re-opened manifest does not match generated manifest")
	}
	return manifest, writerResult, nil
}

func sameManifest(left, right bundle.Manifest) bool {
	return left.Format == right.Format && left.Version == right.Version && left.CreatedAt.Equal(right.CreatedAt) &&
		left.ToolVersion == right.ToolVersion && left.GethVersion == right.GethVersion && left.GethCommit == right.GethCommit &&
		sameSourceEvidence(left.Source, right.Source) && left.Counts == right.Counts && left.StateFile == right.StateFile &&
		slices.Equal(left.SupportedSchemes, right.SupportedSchemes)
}

func publishExportBundle(ctx context.Context, output *atomicDir, opts ExportOptions, reporter *progressReporter) error {
	phase := reporter.StartPhase("publish_bundle", nil, "output", opts.Output)
	if err := ctx.Err(); err != nil {
		phase.Finish(err)
		return err
	}
	if err := rejectOutputInsideSource(opts.SourceChaindata, opts.Output); err != nil {
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

type recordWriterVisitor struct {
	writer *bundle.Writer
}

func (v *recordWriterVisitor) Account(hash common.Hash, account *types.StateAccount, fullRLP []byte) error {
	payload, err := bundle.EncodeAccount(account)
	if err != nil {
		return fmt.Errorf("encode account %s: %w", hash, err)
	}
	if err := v.writer.WriteAccount(hash, payload, uint64(len(fullRLP))); err != nil {
		return fmt.Errorf("write account %s: %w", hash, err)
	}
	if !bytes.Equal(account.CodeHash, types.EmptyCodeHash[:]) {
		if err := v.writer.CountCodeReference(); err != nil {
			return fmt.Errorf("count account %s code reference: %w", hash, err)
		}
	}
	return nil
}

func (v *recordWriterVisitor) Storage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	if err := v.writer.WriteStorage(accountHash, slotHash, valueRLP); err != nil {
		return fmt.Errorf("write account %s slot %s: %w", accountHash, slotHash, err)
	}
	return nil
}

func (v *recordWriterVisitor) Code(accountHash, codeHash common.Hash, code []byte) error {
	if err := v.writer.WriteCode(codeHash, code); err != nil {
		return fmt.Errorf("write account %s code %s: %w", accountHash, codeHash, err)
	}
	return nil
}
