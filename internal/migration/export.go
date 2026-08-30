package migration

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

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
		opts.Compression = "zstd"
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
	if opts.SourceChaindata == "" {
		return ExportResult{}, errors.New("source chaindata path is required")
	}
	if opts.Output == "" {
		return ExportResult{}, errors.New("bundle output path is required")
	}
	if err := rejectOutputInsideSource(opts.SourceChaindata, opts.Output); err != nil {
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

	recordWriter, err := bundle.NewWriter(output.Path(), opts.Compression, headBefore, headerRLP)
	if err != nil {
		return ExportResult{}, err
	}
	var (
		visitor      StateVisitor = &recordWriterVisitor{writer: recordWriter, seenCode: make(map[common.Hash]struct{})}
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
	var (
		writerResult bundle.WriterResult
		manifest     bundle.Manifest
	)
	finalizeErr := func() error {
		var err error
		writerResult, err = recordWriter.Close()
		if err != nil {
			return err
		}
		if writerResult.Counts != state.Counts {
			return fmt.Errorf("export record counts mismatch: writer %+v traversal %+v", writerResult.Counts, state.Counts)
		}
		sourceEvidence, err := source.ConfirmStableAndClose("export")
		if err != nil {
			return err
		}
		manifest = bundle.NewManifest(sourceEvidence, state.Counts, bundle.StateFile{
			Name:            writerResult.FileName,
			Compression:     writerResult.Compression,
			Size:            writerResult.Size,
			SHA256:          writerResult.SHA256,
			RecordChainHash: writerResult.RecordChainHash,
		})
		if _, err := bundle.WriteManifest(output.Path(), manifest); err != nil {
			return err
		}
		if err := syncFile(filepath.Join(output.Path(), bundle.ManifestFileName)); err != nil {
			return err
		}
		if _, _, err := bundle.LoadManifest(output.Path()); err != nil {
			return fmt.Errorf("re-open generated manifest: %w", err)
		}
		return nil
	}()
	finalizePhase.Finish(finalizeErr,
		"records", writerResult.Counts.Records,
		"state_file_size", writerResult.Size,
	)
	if finalizeErr != nil {
		return ExportResult{}, finalizeErr
	}
	publishPhase := reporter.StartPhase("publish_bundle", nil, "output", opts.Output)
	if err := rejectOutputInsideSource(opts.SourceChaindata, opts.Output); err != nil {
		publishPhase.Finish(err)
		return ExportResult{}, err
	}
	if err := output.Commit(); err != nil {
		publishPhase.Finish(err)
		return ExportResult{}, err
	}
	publishPhase.Finish(nil)
	return ExportResult{BundlePath: opts.Output, Manifest: manifest}, nil
}

type recordWriterVisitor struct {
	writer   *bundle.Writer
	seenCode map[common.Hash]struct{}
}

func (v *recordWriterVisitor) Account(hash common.Hash, account *types.StateAccount, fullRLP []byte) error {
	if err := v.writer.WriteAccount(hash, fullRLP); err != nil {
		return fmt.Errorf("write account %s: %w", hash, err)
	}
	if common.BytesToHash(account.CodeHash) != types.EmptyCodeHash {
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
	if _, exists := v.seenCode[codeHash]; exists {
		return nil
	}
	if err := v.writer.WriteCode(codeHash, code); err != nil {
		return fmt.Errorf("write account %s code %s: %w", accountHash, codeHash, err)
	}
	v.seenCode[codeHash] = struct{}{}
	return nil
}
