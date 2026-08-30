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

type legacySource struct {
	db        ethdb.Database
	head      bundle.Head
	headerRLP []byte
	closed    bool
}

func openLegacySource(path string, cacheMB, handles int, reporter *progressReporter) (*legacySource, error) {
	openPhase := reporter.StartPhase("open_source", nil, "source", path)
	sourceKV, err := readonlydb.Open(path, cacheMB, handles)
	openPhase.Finish(err)
	if err != nil {
		return nil, err
	}
	db := rawdb.NewDatabase(sourceKV)
	headPhase := reporter.StartPhase("read_source_head", nil)
	head, headerRLP, err := readLegacyHead(db)
	headPhase.Finish(err,
		"block", head.BlockNumber,
		"hash", head.BlockHash,
		"root", head.StateRoot,
	)
	if err != nil {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close legacy source database: %w", closeErr))
		}
		return nil, err
	}
	return &legacySource{db: db, head: head, headerRLP: headerRLP}, nil
}

func (s *legacySource) Head() (bundle.Head, []byte) {
	return s.head, append([]byte(nil), s.headerRLP...)
}

func (s *legacySource) Traverse(ctx context.Context, visitor StateVisitor, indexOpts codeHashIndexOptions) (result StateResult, retErr error) {
	trieDB := triedb.NewDatabase(s.db, triedb.HashDefaults)
	defer func() {
		if err := trieDB.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close source trie database: %w", err))
		}
	}()
	result, _, err := traverseState(ctx, s.db, trieDB, s.head.StateRoot, visitor, false, indexOpts)
	return result, err
}

func (s *legacySource) ConfirmStableAndClose(operation string) (evidence bundle.SourceEvidence, retErr error) {
	headAfter, headerRLPAfter, err := readLegacyHead(s.db)
	if err != nil {
		retErr = fmt.Errorf("re-read source head: %w", err)
	} else if headAfter != s.head || !bytes.Equal(headerRLPAfter, s.headerRLP) {
		retErr = fmt.Errorf("source head changed during %s: before %+v after %+v", operation, s.head, headAfter)
	} else {
		evidence = bundle.SourceEvidence{
			HeadBefore: s.head,
			HeadAfter:  headAfter,
			HeaderRLP:  hexutil.Bytes(append([]byte(nil), s.headerRLP...)),
		}
	}
	if err := s.Close(); err != nil {
		retErr = errors.Join(retErr, fmt.Errorf("close legacy source database: %w", err))
	}
	return evidence, retErr
}

func (s *legacySource) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
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
		return errors.New("output must not be inside the source chaindata directory")
	}
	sourceInfo, err := os.Stat(sourceAbs)
	if err != nil {
		return fmt.Errorf("stat source path: %w", err)
	}
	for probe := outputAbs; ; probe = filepath.Dir(probe) {
		info, statErr := os.Stat(probe)
		switch {
		case statErr == nil && os.SameFile(sourceInfo, info):
			return errors.New("output aliases the source chaindata directory")
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
