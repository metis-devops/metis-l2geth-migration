package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
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
	BundlePath string
	Manifest   bundle.Manifest
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
	openPhase := reporter.StartPhase("open_source", nil, "source", opts.SourceChaindata)
	sourceKV, err := readonlydb.Open(opts.SourceChaindata, opts.CacheMB, opts.Handles)
	openPhase.Finish(err)
	if err != nil {
		return ExportResult{}, err
	}
	sourceDB := rawdb.NewDatabase(sourceKV)
	sourceClosed := false
	defer func() {
		if !sourceClosed {
			if err := sourceDB.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close legacy source database: %w", err))
			}
		}
	}()

	headPhase := reporter.StartPhase("read_source_head", nil)
	headBefore, headerRLP, err := readLegacyHead(sourceDB)
	headPhase.Finish(err,
		"block", headBefore.BlockNumber,
		"hash", headBefore.BlockHash,
		"root", headBefore.StateRoot,
	)
	if err != nil {
		return ExportResult{}, err
	}
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
	trieDB := triedb.NewDatabase(sourceDB, triedb.HashDefaults)
	traversePhase := reporter.StartPhase("export_state", progressView, "root", headBefore.StateRoot)
	state, traverseErr := TraverseState(ctx, sourceDB, trieDB, headBefore.StateRoot, visitor)
	traversePhase.Finish(traverseErr)
	closeTrieErr := trieDB.Close()
	if traverseErr != nil {
		if err := recordWriter.Abort(); err != nil {
			traverseErr = errors.Join(traverseErr, fmt.Errorf("abort state record writer: %w", err))
		}
		return ExportResult{}, traverseErr
	}
	if closeTrieErr != nil {
		err := fmt.Errorf("close source trie database: %w", closeTrieErr)
		if abortErr := recordWriter.Abort(); abortErr != nil {
			err = errors.Join(err, fmt.Errorf("abort state record writer: %w", abortErr))
		}
		return ExportResult{}, err
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
		headAfter, headerRLPAfter, err := readLegacyHead(sourceDB)
		if err != nil {
			return fmt.Errorf("re-read source head: %w", err)
		}
		if headAfter != headBefore || !bytes.Equal(headerRLPAfter, headerRLP) {
			return fmt.Errorf("source head changed during export: before %+v after %+v", headBefore, headAfter)
		}
		closeSourceErr := sourceDB.Close()
		sourceClosed = true
		if closeSourceErr != nil {
			return fmt.Errorf("close legacy source database: %w", closeSourceErr)
		}
		manifest = bundle.NewManifest(bundle.SourceEvidence{
			HeadBefore: headBefore,
			HeadAfter:  headAfter,
			HeaderRLP:  hexutil.Bytes(headerRLP),
		}, state.Counts, bundle.StateFile{
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

func readLegacyHead(db ethdb.Database) (bundle.Head, []byte, error) {
	hash := rawdb.ReadHeadBlockHash(db)
	if hash == (common.Hash{}) {
		return bundle.Head{}, nil, errors.New("legacy database has no LastBlock head")
	}
	number, ok := rawdb.ReadHeaderNumber(db, hash)
	if !ok {
		return bundle.Head{}, nil, fmt.Errorf("legacy head %s has no hash-to-number mapping", hash)
	}
	canonical := rawdb.ReadCanonicalHash(db, number)
	if canonical != hash {
		return bundle.Head{}, nil, fmt.Errorf("legacy LastBlock is not canonical at height %d: head %s canonical %s", number, hash, canonical)
	}
	headerRLP := rawdb.ReadHeaderRLP(db, hash, number)
	if len(headerRLP) == 0 {
		return bundle.Head{}, nil, fmt.Errorf("legacy head header RLP is missing for block %d %s", number, hash)
	}
	var header types.Header
	if err := rlp.DecodeBytes(headerRLP, &header); err != nil {
		return bundle.Head{}, nil, fmt.Errorf("decode legacy header with geth v1.17.5: %w", err)
	}
	if header.Number == nil || !header.Number.IsUint64() {
		return bundle.Head{}, nil, errors.New("legacy head header number is missing or exceeds uint64")
	}
	if header.Number.Uint64() != number {
		return bundle.Head{}, nil, fmt.Errorf("legacy header number mismatch: header %d mapping %d", header.Number.Uint64(), number)
	}
	if header.Hash() != hash {
		return bundle.Head{}, nil, fmt.Errorf("legacy header hash mismatch: header %s LastBlock %s", header.Hash(), hash)
	}
	if header.Root == (common.Hash{}) {
		return bundle.Head{}, nil, errors.New("legacy head state root is empty")
	}
	return bundle.Head{BlockNumber: number, BlockHash: hash, StateRoot: header.Root}, append([]byte(nil), headerRLP...), nil
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

func rejectOutputInsideSource(source, output string) error {
	sourceAbs, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve source path: %w", err)
	}
	sourceAbs, err = filepath.Abs(sourceAbs)
	if err != nil {
		return fmt.Errorf("resolve absolute source path: %w", err)
	}
	outputAbs, err := resolvePathWithMissing(output)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	rel, err := filepath.Rel(sourceAbs, outputAbs)
	if err != nil {
		return fmt.Errorf("compare source and output paths: %w", err)
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("bundle output must not be inside the source chaindata directory")
	}
	sourceInfo, err := os.Stat(sourceAbs)
	if err != nil {
		return fmt.Errorf("stat source path: %w", err)
	}
	for probe := outputAbs; ; probe = filepath.Dir(probe) {
		info, statErr := os.Stat(probe)
		switch {
		case statErr == nil && os.SameFile(sourceInfo, info):
			return errors.New("bundle output aliases the source chaindata directory")
		case statErr == nil:
		case errors.Is(statErr, os.ErrNotExist):
		default:
			return fmt.Errorf("inspect output ancestor %s: %w", probe, statErr)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
	}
	return nil
}
