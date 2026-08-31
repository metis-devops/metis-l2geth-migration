package bundle

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/klauspost/compress/zstd"
)

var streamMagic = [8]byte{'L', '2', 'S', 'T', 'A', 'T', 'E', '1'}

const (
	recordEnd byte = 0
	// RecordAccount identifies an account-leaf record.
	RecordAccount byte = 1
	// RecordStorage identifies a storage-leaf record.
	RecordStorage byte = 2
	// RecordCode identifies a contract-code record.
	RecordCode byte = 3

	maxAccountPayload = 110
	maxStoragePayload = 33
	maxCodePayload    = 128 << 20
	ioChunkSize       = 256 << 10
)

const recordChainDomain = "metis-l2state-record-chain/v3"

// Record is one decoded account, storage, or code entry from the record stream.
type Record struct {
	Type        byte
	AccountHash common.Hash
	SubHash     common.Hash
	Payload     []byte
}

// WriterResult summarizes a completed record stream and its integrity evidence.
type WriterResult struct {
	Counts             Counts
	RecordPayloadBytes uint64
	FileName           string
	Compression        string
	Size               int64
	SHA256             common.Hash
	RecordChainHash    common.Hash
}

// Writer emits a deterministic, integrity-protected state record stream.
type Writer struct {
	ctx         context.Context
	file        *os.File
	encoder     *zstd.Encoder
	buffer      *bufio.Writer
	fileHash    hash.Hash
	chain       common.Hash
	chainHasher recordChainHasher
	counts      Counts
	recordBytes uint64
	fileName    string
	compression string
	closed      bool
	header      [1 + 2*common.HashLength + 8]byte
}

// NewWriter creates a record stream in dir using the requested compression.
func NewWriter(ctx context.Context, dir, compression string, head Head, headerRLP []byte) (*Writer, error) {
	if ctx == nil {
		return nil, errors.New("state record writer context is nil")
	}
	var fileName string
	switch compression {
	case CompressionZstd:
		fileName = RecordsFileZstd
	case CompressionNone:
		fileName = RecordsFileRaw
	default:
		return nil, fmt.Errorf("unsupported compression %q", compression)
	}
	path := filepath.Join(dir, fileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create state records: %w", err)
	}
	fileHash := sha256.New()
	destination := io.MultiWriter(file, fileHash)
	writer := &Writer{
		ctx:         ctx,
		file:        file,
		fileHash:    fileHash,
		chain:       initialChainHash(head, headerRLP),
		fileName:    fileName,
		compression: compression,
	}
	if compression == CompressionZstd {
		encoder, err := newZstdEncoder(destination)
		if err != nil {
			createErr := fmt.Errorf("create zstd writer: %w", err)
			if closeErr := file.Close(); closeErr != nil {
				createErr = errors.Join(createErr, fmt.Errorf("close state records after zstd setup failure: %w", closeErr))
			}
			return nil, createErr
		}
		writer.encoder = encoder
		writer.buffer = bufio.NewWriterSize(encoder, 256*1024)
	} else {
		writer.buffer = bufio.NewWriterSize(destination, 256*1024)
	}
	if _, err := writer.buffer.Write(streamMagic[:]); err != nil {
		writeErr := fmt.Errorf("write state stream header: %w", err)
		if abortErr := writer.Abort(); abortErr != nil {
			writeErr = errors.Join(writeErr, fmt.Errorf("abort state stream: %w", abortErr))
		}
		return nil, writeErr
	}
	return writer, nil
}

// CountCodeReference records an account's reference to contract code separately
// from the number of unique code records.
func (w *Writer) CountCodeReference() error {
	if w.closed {
		return errors.New("state record writer is closed")
	}
	w.counts.CodeReferences++
	return nil
}

// WriteAccount appends a slim account payload while counting the expanded
// consensus account RLP bytes.
func (w *Writer) WriteAccount(accountHash common.Hash, accountRLP []byte, consensusPayloadBytes uint64) error {
	if uint64(len(accountRLP)) > consensusPayloadBytes {
		return fmt.Errorf("slim account payload is %d bytes, expanded consensus payload is %d", len(accountRLP), consensusPayloadBytes)
	}
	return w.writeRecord(RecordAccount, accountHash[:], nil, accountRLP, consensusPayloadBytes)
}

// WriteStorage appends a storage leaf for an account to the record stream.
func (w *Writer) WriteStorage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	return w.writeRecord(RecordStorage, accountHash[:], slotHash[:], valueRLP, uint64(len(valueRLP)))
}

// WriteCode appends contract code to the record stream.
func (w *Writer) WriteCode(codeHash common.Hash, code []byte) error {
	return w.writeRecord(RecordCode, codeHash[:], nil, code, uint64(len(code)))
}

func (w *Writer) writeRecord(typ byte, key, subkey, payload []byte, consensusPayloadBytes uint64) error {
	if w.closed {
		return errors.New("state record writer is closed")
	}
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := validatePayloadLength(typ, uint64(len(payload))); err != nil {
		return err
	}
	keyLen := len(key) + len(subkey)
	expectedKeyLen, err := recordKeyLength(typ)
	if err != nil {
		return err
	}
	if keyLen != expectedKeyLen {
		return fmt.Errorf("record type %d key has length %d, want %d", typ, keyLen, expectedKeyLen)
	}
	header := w.header[:1+keyLen+8]
	header[0] = typ
	n := copy(header[1:], key)
	copy(header[1+n:], subkey)
	binary.BigEndian.PutUint64(header[len(header)-8:], uint64(len(payload)))
	if _, err := w.buffer.Write(header); err != nil {
		return fmt.Errorf("write record header: %w", err)
	}
	w.chainHasher.Start(&w.chain, header)
	for offset := 0; offset < len(payload); {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		end := min(offset+ioChunkSize, len(payload))
		chunk := payload[offset:end]
		if _, err := w.buffer.Write(chunk); err != nil {
			return fmt.Errorf("write record payload: %w", err)
		}
		w.chainHasher.Write(chunk)
		offset = end
	}
	w.chainHasher.SumInto(&w.chain)
	w.counts.Records++
	w.counts.PayloadBytes += consensusPayloadBytes
	w.recordBytes += uint64(len(payload))
	switch typ {
	case RecordAccount:
		w.counts.Accounts++
	case RecordStorage:
		w.counts.StorageSlots++
	case RecordCode:
		w.counts.CodeRecords++
	default:
		return fmt.Errorf("unknown record type %d", typ)
	}
	return nil
}

// Close finalizes the stream and returns its counts and integrity evidence.
func (w *Writer) Close() (WriterResult, error) {
	if w.closed {
		return WriterResult{}, errors.New("state record writer is already closed")
	}
	w.closed = true
	var closeErr error
	if err := w.buffer.WriteByte(recordEnd); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("write stream terminator: %w", err))
	}
	if err := w.buffer.Flush(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("flush state stream: %w", err))
	}
	if w.encoder != nil {
		if err := w.encoder.Close(); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close zstd stream: %w", err))
		}
	}
	if err := w.file.Sync(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("sync state stream: %w", err))
	}
	info, err := w.file.Stat()
	if err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("stat state stream: %w", err))
	}
	if err := w.file.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close state stream: %w", err))
	}
	if closeErr != nil {
		return WriterResult{}, closeErr
	}
	return WriterResult{
		Counts:             w.counts,
		RecordPayloadBytes: w.recordBytes,
		FileName:           w.fileName,
		Compression:        w.compression,
		Size:               info.Size(),
		SHA256:             common.BytesToHash(w.fileHash.Sum(nil)),
		RecordChainHash:    w.chain,
	}, nil
}

// Abort closes an unfinished stream without finalizing it.
func (w *Writer) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	var abortErr error
	if w.encoder != nil {
		if err := w.encoder.Close(); err != nil {
			abortErr = errors.Join(abortErr, fmt.Errorf("close zstd stream: %w", err))
		}
	}
	if err := w.file.Close(); err != nil {
		abortErr = errors.Join(abortErr, fmt.Errorf("close state stream: %w", err))
	}
	return abortErr
}

func newZstdEncoder(destination io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(destination,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderLevel(zstd.SpeedDefault),
	)
}

type countWriter struct {
	size int64
}

func (w *countWriter) Write(data []byte) (int, error) {
	w.size += int64(len(data))
	return len(data), nil
}

type canonicalZstdWriter struct {
	hash    hash.Hash
	count   countWriter
	encoder *zstd.Encoder
	buffer  *bufio.Writer
	closed  bool
}

func newCanonicalZstdWriter() (*canonicalZstdWriter, error) {
	canonical := &canonicalZstdWriter{hash: sha256.New()}
	encoder, err := newZstdEncoder(io.MultiWriter(canonical.hash, &canonical.count))
	if err != nil {
		return nil, fmt.Errorf("create canonical zstd writer: %w", err)
	}
	canonical.encoder = encoder
	canonical.buffer = bufio.NewWriterSize(encoder, 256*1024)
	return canonical, nil
}

func (w *canonicalZstdWriter) Write(data []byte) (int, error) {
	if w.closed {
		return 0, errors.New("canonical zstd writer is closed")
	}
	n, err := w.buffer.Write(data)
	if err != nil {
		return n, fmt.Errorf("re-encode canonical zstd stream: %w", err)
	}
	return n, nil
}

func (w *canonicalZstdWriter) WriteByte(value byte) error {
	if w.closed {
		return errors.New("canonical zstd writer is closed")
	}
	if err := w.buffer.WriteByte(value); err != nil {
		return fmt.Errorf("re-encode canonical zstd stream: %w", err)
	}
	return nil
}

func (w *canonicalZstdWriter) Close() (int64, common.Hash, error) {
	if w.closed {
		return 0, common.Hash{}, errors.New("canonical zstd writer is already closed")
	}
	w.closed = true
	var closeErr error
	if err := w.buffer.Flush(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("flush canonical zstd stream: %w", err))
	}
	if err := w.encoder.Close(); err != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("close canonical zstd stream: %w", err))
	}
	if closeErr != nil {
		return 0, common.Hash{}, closeErr
	}
	return w.count.size, common.BytesToHash(w.hash.Sum(nil)), nil
}

func (w *canonicalZstdWriter) Abort() error {
	if w == nil || w.closed {
		return nil
	}
	w.closed = true
	if err := w.encoder.Close(); err != nil {
		return fmt.Errorf("abort canonical zstd writer: %w", err)
	}
	return nil
}

func initialChainHash(head Head, headerRLP []byte) common.Hash {
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], head.BlockNumber)
	headerHash := crypto.Keccak256Hash(headerRLP)
	return crypto.Keccak256Hash(
		[]byte(recordChainDomain),
		number[:],
		head.BlockHash[:],
		head.StateRoot[:],
		headerHash[:],
	)
}

type recordChainHasher struct {
	state  crypto.KeccakState
	prefix [common.HashLength + 1 + 2*common.HashLength + 8]byte
}

func (h *recordChainHasher) Start(previous *common.Hash, header []byte) {
	if h.state == nil {
		h.state = crypto.NewKeccakState()
	}
	copy(h.prefix[:common.HashLength], previous[:])
	n := copy(h.prefix[common.HashLength:], header)
	h.state.Reset()
	_, _ = h.state.Write(h.prefix[:common.HashLength+n])
}

func (h *recordChainHasher) Write(payload []byte) {
	_, _ = h.state.Write(payload)
}

func (h *recordChainHasher) SumInto(result *common.Hash) {
	_, _ = h.state.Read(result[:])
}

func recordKeyLength(typ byte) (int, error) {
	switch typ {
	case RecordAccount, RecordCode:
		return common.HashLength, nil
	case RecordStorage:
		return 2 * common.HashLength, nil
	default:
		return 0, fmt.Errorf("unknown record type %d", typ)
	}
}

func validatePayloadLength(typ byte, length uint64) error {
	var max uint64
	switch typ {
	case RecordAccount:
		max = maxAccountPayload
	case RecordStorage:
		max = maxStoragePayload
	case RecordCode:
		max = maxCodePayload
	default:
		return fmt.Errorf("unknown record type %d", typ)
	}
	if length == 0 {
		return fmt.Errorf("record type %d has empty payload", typ)
	}
	if length > max {
		return fmt.Errorf("record type %d payload is %d bytes, maximum is %d", typ, length, max)
	}
	return nil
}
