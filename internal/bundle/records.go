package bundle

import (
	"bufio"
	"bytes"
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

	maxAccountPayload = 1 << 20
	maxStoragePayload = 64
	maxCodePayload    = 128 << 20
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
	file        *os.File
	encoder     *zstd.Encoder
	buffer      *bufio.Writer
	fileHash    hash.Hash
	chain       common.Hash
	counts      Counts
	recordBytes uint64
	fileName    string
	compression string
	closed      bool
	header      [1 + 2*common.HashLength + 8]byte
}

// NewWriter creates a record stream in dir using the requested compression.
func NewWriter(dir, compression string, head Head, headerRLP []byte) (*Writer, error) {
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
	if _, err := w.buffer.Write(payload); err != nil {
		return fmt.Errorf("write record payload: %w", err)
	}
	w.chain = nextChainHash(w.chain, header, payload)
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

// ScanResult summarizes a verified record-stream scan.
type ScanResult struct {
	Counts             Counts
	RecordPayloadBytes uint64
	FileSHA256         common.Hash
	RecordChainHash    common.Hash
}

// ScanRecords verifies and decodes the manifest's record stream in order.
func ScanRecords(ctx context.Context, dir string, manifest Manifest, consume func(Record) error) (scanResult ScanResult, retErr error) {
	path := filepath.Join(dir, manifest.StateFile.Name)
	info, err := os.Stat(path)
	if err != nil {
		return ScanResult{}, fmt.Errorf("stat state records: %w", err)
	}
	if !info.Mode().IsRegular() {
		return ScanResult{}, errors.New("state records is not a regular file")
	}
	if info.Size() != manifest.StateFile.Size {
		return ScanResult{}, fmt.Errorf("state records size mismatch: have %d want %d", info.Size(), manifest.StateFile.Size)
	}
	file, err := os.Open(path)
	if err != nil {
		return ScanResult{}, fmt.Errorf("open state records: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close state records: %w", err))
		}
	}()
	fileHash := sha256.New()
	tee := io.TeeReader(file, fileHash)
	var (
		stream  = tee
		decoder *zstd.Decoder
	)
	if manifest.StateFile.Compression == CompressionZstd {
		decoder, err = zstd.NewReader(tee, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return ScanResult{}, fmt.Errorf("open zstd stream: %w", err)
		}
		defer decoder.Close()
		stream = decoder
	}
	reader := bufio.NewReaderSize(stream, 256*1024)
	var canonical *canonicalZstdWriter
	if manifest.StateFile.Compression == CompressionZstd {
		canonical, err = newCanonicalZstdWriter()
		if err != nil {
			return ScanResult{}, err
		}
		defer canonical.Abort()
	}
	magic := make([]byte, len(streamMagic))
	if _, err := io.ReadFull(reader, magic); err != nil {
		return ScanResult{}, fmt.Errorf("read state stream header: %w", err)
	}
	if !bytes.Equal(magic, streamMagic[:]) {
		return ScanResult{}, errors.New("invalid state stream magic")
	}
	if canonical != nil {
		if _, err := canonical.Write(magic); err != nil {
			return ScanResult{}, err
		}
	}
	chain := initialChainHash(manifest.Source.HeadBefore, manifest.Source.HeaderRLP)
	var (
		counts      Counts
		recordBytes uint64
		headerSpace [1 + 2*common.HashLength + 8]byte
	)
	for {
		if err := ctx.Err(); err != nil {
			return ScanResult{}, err
		}
		typ, err := reader.ReadByte()
		if err != nil {
			return ScanResult{}, fmt.Errorf("read record type: %w", err)
		}
		if typ == recordEnd {
			if canonical != nil {
				if err := canonical.WriteByte(recordEnd); err != nil {
					return ScanResult{}, err
				}
			}
			_, err := reader.ReadByte()
			if err == nil {
				return ScanResult{}, errors.New("trailing data after state stream terminator")
			}
			if !errors.Is(err, io.EOF) {
				return ScanResult{}, fmt.Errorf("finish state stream: %w", err)
			}
			break
		}
		keyLen, err := recordKeyLength(typ)
		if err != nil {
			return ScanResult{}, err
		}
		header := headerSpace[:1+keyLen+8]
		header[0] = typ
		if _, err := io.ReadFull(reader, header[1:]); err != nil {
			return ScanResult{}, fmt.Errorf("read record header: %w", err)
		}
		length := binary.BigEndian.Uint64(header[len(header)-8:])
		if err := validatePayloadLength(typ, length); err != nil {
			return ScanResult{}, err
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(reader, payload); err != nil {
			return ScanResult{}, fmt.Errorf("read record payload: %w", err)
		}
		if canonical != nil {
			if _, err := canonical.Write(header); err != nil {
				return ScanResult{}, err
			}
			if _, err := canonical.Write(payload); err != nil {
				return ScanResult{}, err
			}
		}
		key := header[1 : len(header)-8]
		record := Record{Type: typ, AccountHash: common.BytesToHash(key[:common.HashLength]), Payload: payload}
		switch typ {
		case RecordStorage:
			record.SubHash = common.BytesToHash(key[common.HashLength:])
		case RecordCode:
			record.SubHash = record.AccountHash
			record.AccountHash = common.Hash{}
		}
		chain = nextChainHash(chain, header, payload)
		counts.Records++
		recordBytes += length
		switch typ {
		case RecordAccount:
			counts.Accounts++
		case RecordStorage:
			counts.StorageSlots++
		case RecordCode:
			counts.CodeRecords++
		}
		if consume != nil {
			if err := consume(record); err != nil {
				return ScanResult{}, err
			}
		}
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return ScanResult{}, fmt.Errorf("finish hashing state records: %w", err)
	}
	if canonical != nil {
		canonicalSize, canonicalHash, err := canonical.Close()
		if err != nil {
			return ScanResult{}, err
		}
		if canonicalSize != info.Size() || canonicalHash != common.BytesToHash(fileHash.Sum(nil)) {
			return ScanResult{}, errors.New("zstd state records do not use the canonical single-frame encoding")
		}
	}
	if counts.Accounts != manifest.Counts.Accounts ||
		counts.StorageSlots != manifest.Counts.StorageSlots ||
		counts.CodeRecords != manifest.Counts.CodeRecords ||
		counts.Records != manifest.Counts.Records {
		return ScanResult{}, fmt.Errorf("record counts mismatch: have %+v want %+v", counts, manifest.Counts)
	}
	if recordBytes != manifest.StateFile.RecordPayloadBytes {
		return ScanResult{}, fmt.Errorf("record payload bytes mismatch: have %d want %d", recordBytes, manifest.StateFile.RecordPayloadBytes)
	}
	counts.CodeReferences = manifest.Counts.CodeReferences
	counts.PayloadBytes = manifest.Counts.PayloadBytes
	result := ScanResult{
		Counts:             counts,
		RecordPayloadBytes: recordBytes,
		FileSHA256:         common.BytesToHash(fileHash.Sum(nil)),
		RecordChainHash:    chain,
	}
	if result.FileSHA256 != manifest.StateFile.SHA256 {
		return ScanResult{}, fmt.Errorf("state records SHA-256 mismatch: have %s want %s", result.FileSHA256, manifest.StateFile.SHA256)
	}
	if result.RecordChainHash != manifest.StateFile.RecordChainHash {
		return ScanResult{}, fmt.Errorf("record chain hash mismatch: have %s want %s", result.RecordChainHash, manifest.StateFile.RecordChainHash)
	}
	return result, nil
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

func (w *canonicalZstdWriter) Abort() {
	if w == nil || w.closed {
		return
	}
	w.closed = true
	_ = w.encoder.Close()
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

func nextChainHash(previous common.Hash, header, payload []byte) common.Hash {
	return crypto.Keccak256Hash(previous[:], header, payload)
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
	if length == 0 && typ != RecordCode {
		return fmt.Errorf("record type %d has empty payload", typ)
	}
	if length > max {
		return fmt.Errorf("record type %d payload is %d bytes, maximum is %d", typ, length, max)
	}
	return nil
}
