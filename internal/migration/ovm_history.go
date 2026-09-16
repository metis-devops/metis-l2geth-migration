package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"path/filepath"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	ovmTransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	ovmApprovalTopic = crypto.Keccak256Hash([]byte("Approval(address,address,uint256)"))
)

// OVMHistoryEvidence commits to the complete canonical header/receipt scan.
type OVMHistoryEvidence struct {
	FirstBlock    uint64      `json:"first_block"`
	LastBlock     uint64      `json:"last_block"`
	Blocks        uint64      `json:"blocks"`
	Transfers     uint64      `json:"transfers"`
	Approvals     uint64      `json:"approvals"`
	HistorySHA256 common.Hash `json:"history_sha256"`
	EventsSHA256  common.Hash `json:"events_sha256"`
}

type ovmHistoryScanner struct {
	source   *legacySource
	ancient  *legacyAncient
	index    *ovmIndex
	history  hash.Hash
	events   hash.Hash
	evidence OVMHistoryEvidence
	previous common.Hash
}

func scanOVMHistory(ctx context.Context, source *legacySource, opts MigrateOptions, index *ovmIndex, limiter *migrateWorkLimiter, reporter *progressReporter) (evidence OVMHistoryEvidence, retErr error) {
	var completed atomic.Uint64
	phase := reporter.StartPhase("scan_ovm_history", counterProgressSnapshot(&completed, "blocks", "blocks_per_second"))
	defer func() { phase.Finish(retErr) }()
	path := opts.OVM.SourceAncient
	if path == "" {
		path = filepath.Join(opts.SourceChaindata, "ancient")
	}
	ancient, err := openLegacyAncient(path, opts.OVM.SourceAncient != "")
	if err != nil {
		return evidence, err
	}
	defer func() { retErr = errors.Join(retErr, ancient.Close()) }()
	s := ovmHistoryScanner{source: source, ancient: ancient, index: index, history: sha256.New(), events: sha256.New()}
	s.history.Write([]byte("metis-l2state-ovm-history/v1"))
	s.events.Write([]byte("metis-l2state-ovm-events/v1"))
	// Fast sync can freeze blocks beyond the executed LastBlock. Only the
	// canonical prefix ending at the selected head contributes evidence.
	if err := s.scanParallel(ctx, opts, limiter, &completed); err != nil {
		return evidence, err
	}
	if s.previous != source.head.BlockHash {
		return evidence, errors.New("OVM history does not terminate at selected head")
	}
	if err := index.flush(); err != nil {
		return evidence, err
	}
	s.evidence.LastBlock = source.head.BlockNumber
	copy(s.evidence.HistorySHA256[:], s.history.Sum(nil))
	copy(s.evidence.EventsSHA256[:], s.events.Sum(nil))
	return s.evidence, nil
}

func optionalHistoryKV(db ethdb.Database, key []byte) ([]byte, error) {
	ok, err := db.Has(key)
	if err != nil || !ok {
		return nil, err
	}
	value, err := db.Get(key)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return nil, errors.New("hot canonical history record is empty")
	}
	return value, nil
}

func (s *ovmHistoryScanner) read(table int, number uint64, key []byte) ([]byte, error) {
	hot, cold, err := s.readCopies(table, number, key)
	if err != nil {
		return nil, err
	}
	return mergeLegacyHistoryValue(hot, cold)
}

func (s *ovmHistoryScanner) readCopies(table int, number uint64, key []byte) ([]byte, []byte, error) {
	hot, err := optionalHistoryKV(s.source.db, key)
	if err != nil {
		return nil, nil, err
	}
	cold, err := s.ancient.read(table, number)
	if err != nil {
		return nil, nil, err
	}
	if s.ancient != nil && number < s.ancient.count && len(cold) == 0 {
		return nil, nil, errors.New("ancient canonical history record is empty")
	}
	return hot, cold, nil
}

func (s *ovmHistoryScanner) readReceipts(number uint64, key []byte) (selected, alternate []byte, err error) {
	hot, cold, err := s.readCopies(2, number, key)
	if err != nil {
		return nil, nil, err
	}
	if len(hot) != 0 && len(cold) != 0 && !bytes.Equal(hot, cold) {
		// Freezing can re-encode old receipts. The worker authenticates both
		// copies against the header instead of comparing their storage bytes.
		// Keep the cold encoding as the deterministic history-digest input.
		return cold, hot, nil
	}
	selected, err = mergeLegacyHistoryValue(hot, cold)
	return selected, nil, err
}

func (s *ovmHistoryScanner) readBlock(ctx context.Context, number uint64) (*ovmHistoryBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], number)
	hashBytes, err := s.read(0, number, append(append([]byte{'h'}, num[:]...), 'n'))
	if err != nil {
		return nil, err
	}
	if len(hashBytes) != 32 {
		return nil, errors.New("canonical hash has invalid length")
	}
	headerKey := append(append([]byte{'h'}, num[:]...), hashBytes...)
	blob, err := s.read(1, number, headerKey)
	if err != nil {
		return nil, err
	}
	var header types.Header
	if err := rlp.DecodeBytes(blob, &header); err != nil {
		return nil, fmt.Errorf("decode canonical header: %w", err)
	}
	if header.Number == nil || !header.Number.IsUint64() || header.Number.Uint64() != number || !bytes.Equal(crypto.Keccak256(blob), hashBytes) {
		return nil, errors.New("canonical header hash or number mismatch")
	}
	if header.ParentHash != s.previous {
		return nil, errors.New("canonical header parent mismatch")
	}
	if number == s.source.head.BlockNumber && !bytes.Equal(blob, s.source.headerRLP) {
		return nil, errors.New("selected head header changed")
	}
	receiptKey := append(append([]byte{'r'}, num[:]...), hashBytes...)
	raw, alternate, err := s.readReceipts(number, receiptKey)
	if err != nil {
		return nil, fmt.Errorf("read receipts: %w", err)
	}
	s.previous = common.Hash(hashBytes)
	return &ovmHistoryBlock{number: number, hash: common.Hash(hashBytes), header: header, headerRLP: blob, raw: raw, alternateReceipts: alternate}, nil
}

func (b *ovmHistoryBlock) decode(ctx context.Context) error {
	receipts, err := decodeOVMReceiptsContext(ctx, b.raw)
	if err != nil {
		return err
	}
	if err := validateOVMReceiptCommitments(&b.header, receipts); err != nil {
		return err
	}
	if len(b.alternateReceipts) != 0 {
		alternate, err := decodeOVMReceiptsContext(ctx, b.alternateReceipts)
		if err != nil {
			return fmt.Errorf("decode overlapping hot receipts: %w", err)
		}
		if err := validateOVMReceiptCommitments(&b.header, alternate); err != nil {
			return fmt.Errorf("overlapping hot receipts disagree with canonical header: %w", err)
		}
	}
	b.receipts = receipts
	return nil
}

func validateOVMReceiptCommitments(header *types.Header, receipts types.Receipts) error {
	if types.DeriveSha(receipts, trie.NewStackTrie(nil)) != header.ReceiptHash {
		return errors.New("legacy receipt root mismatch")
	}
	if (len(receipts) == 0) != (header.TxHash == types.EmptyTxsHash) {
		return errors.New("legacy receipt presence disagrees with transaction root")
	}
	var gas uint64
	var bloom types.Bloom
	for _, receipt := range receipts {
		gas = receipt.CumulativeGasUsed
		for n := range bloom {
			bloom[n] |= receipt.Bloom[n]
		}
	}
	if gas != header.GasUsed || bloom != header.Bloom {
		return errors.New("legacy receipts disagree with header gas or bloom")
	}
	return nil
}

func (s *ovmHistoryScanner) acceptBlock(ctx context.Context, b *ovmHistoryBlock) error {
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], b.number)
	ovmHashFrame(s.history, num[:], b.hash[:], b.headerRLP, b.raw)
	for receiptIndex, receipt := range b.receipts {
		for logIndex, log := range receipt.Logs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.event(b.number, uint64(receiptIndex), uint64(logIndex), log); err != nil {
				return err
			}
		}
	}
	s.evidence.Blocks++
	return nil
}

func (s *ovmHistoryScanner) event(block, receipt, position uint64, log *types.Log) error {
	if log.Address != ovmETHAddress || len(log.Topics) == 0 {
		return nil
	}
	topic := log.Topics[0]
	if topic != ovmTransferTopic && topic != ovmApprovalTopic {
		return nil
	}
	if len(log.Topics) != 3 || len(log.Data) != 32 || !bytes.Equal(log.Topics[1][:12], make([]byte, 12)) || !bytes.Equal(log.Topics[2][:12], make([]byte, 12)) {
		return errors.New("malformed OVM ERC20 event")
	}
	from, to := common.BytesToAddress(log.Topics[1][12:]), common.BytesToAddress(log.Topics[2][12:])
	if err := s.index.address(from); err != nil {
		return err
	}
	if err := s.index.address(to); err != nil {
		return err
	}
	if topic == ovmTransferTopic {
		// Zero-value events still discover addresses and commit to history, but
		// only a positive transfer grants the sender retention eligibility.
		if common.BytesToHash(log.Data) != (common.Hash{}) {
			if err := s.index.put('f', from[:], []byte{1}); err != nil {
				return err
			}
		}
		s.evidence.Transfers++
	} else {
		if err := s.index.allowance(from, to); err != nil {
			return err
		}
		s.evidence.Approvals++
	}
	var coordinate [24]byte
	binary.BigEndian.PutUint64(coordinate[:8], block)
	binary.BigEndian.PutUint64(coordinate[8:16], receipt)
	binary.BigEndian.PutUint64(coordinate[16:], position)
	ovmHashFrame(s.events, coordinate[:], topic[:], from[:], to[:], log.Data)
	return nil
}

func ovmHashFrame(h hash.Hash, values ...[]byte) {
	for _, value := range values {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write(value)
	}
}
