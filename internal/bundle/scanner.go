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

	"github.com/ethereum/go-ethereum/common"
	"github.com/klauspost/compress/zstd"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

// WireCounts contains only values independently recomputed from record
// framing. Expanded consensus payload bytes and code references are semantic
// counts and deliberately do not appear here.
type WireCounts struct {
	Accounts     uint64
	StorageSlots uint64
	CodeRecords  uint64
	Records      uint64
	PayloadBytes uint64
}

// ScanResult summarizes a verified record-stream scan.
type ScanResult struct {
	Counts          WireCounts
	FileSHA256      common.Hash
	RecordChainHash common.Hash
}

// ScanRecords verifies and decodes the manifest's record stream in order. Each
// delivered payload is independently owned by its Record.
func ScanRecords(ctx context.Context, dir string, manifest Manifest, consume func(Record) error) (ScanResult, error) {
	return scanRecords(ctx, dir, manifest, consume, false)
}

// ScanRecordsBorrowed is the allocation-conscious form used by migration hot
// paths. Record.Payload is valid only until consume returns and must be cloned
// by consumers that retain it.
func ScanRecordsBorrowed(ctx context.Context, dir string, manifest Manifest, consume func(Record) error) (ScanResult, error) {
	return scanRecords(ctx, dir, manifest, consume, true)
}

func scanRecords(ctx context.Context, dir string, manifest Manifest, consume func(Record) error, borrow bool) (result ScanResult, retErr error) {
	scanner, err := openRecordScanner(ctx, dir, manifest, borrow)
	if err != nil {
		return ScanResult{}, err
	}
	defer func() {
		if err := scanner.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	for {
		record, done, err := scanner.Next()
		if err != nil {
			return ScanResult{}, err
		}
		if done {
			break
		}
		if consume != nil {
			if err := consume(record); err != nil {
				return ScanResult{}, fmt.Errorf("consume record %d: %w", scanner.counts.Records, err)
			}
		}
	}
	return scanner.Finish()
}

type recordScanner struct {
	ctx         context.Context
	manifest    Manifest
	root        *strictio.Root
	file        *os.File
	fileSize    int64
	fileHash    hash.Hash
	raw         io.Reader
	decoder     *zstd.Decoder
	reader      *bufio.Reader
	canonical   *canonicalZstdWriter
	chain       common.Hash
	chainHasher recordChainHasher
	counts      WireCounts
	header      [1 + 2*common.HashLength + 8]byte
	payload     []byte
	borrow      bool
	terminated  bool
}

func openRecordScanner(ctx context.Context, dir string, manifest Manifest, borrow bool) (_ *recordScanner, retErr error) {
	if ctx == nil {
		return nil, errors.New("record scanner context is nil")
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("validate record manifest: %w", err)
	}
	root, err := strictio.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	scanner := &recordScanner{ctx: ctx, manifest: manifest, root: root, borrow: borrow}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, scanner.Close())
		}
	}()
	if err := root.RequireExactLayout(map[string]strictio.EntryKind{
		ManifestFileName:        strictio.RegularFile,
		manifest.StateFile.Name: strictio.RegularFile,
	}); err != nil {
		return nil, fmt.Errorf("validate bundle layout: %w", err)
	}
	file, info, err := root.OpenRegular(manifest.StateFile.Name)
	if err != nil {
		return nil, fmt.Errorf("open state records: %w", err)
	}
	scanner.file = file
	scanner.fileSize = info.Size()
	if info.Size() != manifest.StateFile.Size {
		return nil, fmt.Errorf("state records size mismatch: have %d want %d", info.Size(), manifest.StateFile.Size)
	}
	scanner.fileHash = sha256.New()
	scanner.raw = io.TeeReader(file, scanner.fileHash)
	stream := scanner.raw
	if manifest.StateFile.Compression == CompressionZstd {
		decoder, err := zstd.NewReader(scanner.raw, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, fmt.Errorf("open zstd stream: %w", err)
		}
		scanner.decoder = decoder
		stream = decoder
		canonical, err := newCanonicalZstdWriter()
		if err != nil {
			return nil, err
		}
		scanner.canonical = canonical
	}
	scanner.reader = bufio.NewReaderSize(stream, ioChunkSize)
	var magic [len(streamMagic)]byte
	if err := scanner.readFull(magic[:]); err != nil {
		return nil, fmt.Errorf("read state stream header: %w", err)
	}
	if !bytes.Equal(magic[:], streamMagic[:]) {
		return nil, errors.New("invalid state stream magic")
	}
	if scanner.canonical != nil {
		if _, err := scanner.canonical.Write(magic[:]); err != nil {
			return nil, err
		}
	}
	scanner.chain = initialChainHash(manifest.Source.HeadBefore, manifest.Source.HeaderRLP)
	return scanner, nil
}

func (s *recordScanner) Next() (Record, bool, error) {
	if s.terminated {
		return Record{}, true, nil
	}
	if err := s.ctx.Err(); err != nil {
		return Record{}, false, err
	}
	typ, err := s.reader.ReadByte()
	if err != nil {
		return Record{}, false, fmt.Errorf("read record type: %w", err)
	}
	if typ == recordEnd {
		return Record{}, true, s.finishTerminator()
	}
	header, err := s.readHeader(typ)
	if err != nil {
		return Record{}, false, err
	}
	length := binary.BigEndian.Uint64(header[len(header)-8:])
	if err := validatePayloadLength(typ, length); err != nil {
		return Record{}, false, err
	}
	payload := s.allocatePayload(int(length))
	if s.canonical != nil {
		if _, err := s.canonical.Write(header); err != nil {
			return Record{}, false, err
		}
	}
	s.chainHasher.Start(&s.chain, header)
	if err := s.readPayload(payload); err != nil {
		return Record{}, false, err
	}
	s.chainHasher.SumInto(&s.chain)
	s.addCount(typ, length)
	return recordFromHeader(typ, header, payload), false, nil
}

func (s *recordScanner) readHeader(typ byte) ([]byte, error) {
	keyLen, err := recordKeyLength(typ)
	if err != nil {
		return nil, err
	}
	header := s.header[:1+keyLen+8]
	header[0] = typ
	if err := s.readFull(header[1:]); err != nil {
		return nil, fmt.Errorf("read record header: %w", err)
	}
	return header, nil
}

func (s *recordScanner) allocatePayload(length int) []byte {
	if !s.borrow {
		return make([]byte, length)
	}
	if cap(s.payload) < length {
		s.payload = make([]byte, length)
	} else {
		s.payload = s.payload[:length]
	}
	return s.payload
}

func (s *recordScanner) readPayload(payload []byte) error {
	for offset := 0; offset < len(payload); {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		end := min(offset+ioChunkSize, len(payload))
		chunk := payload[offset:end]
		if _, err := io.ReadFull(s.reader, chunk); err != nil {
			return fmt.Errorf("read record payload: %w", err)
		}
		if s.canonical != nil {
			if _, err := s.canonical.Write(chunk); err != nil {
				return err
			}
		}
		s.chainHasher.Write(chunk)
		offset = end
	}
	return nil
}

func (s *recordScanner) readFull(destination []byte) error {
	for offset := 0; offset < len(destination); {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		end := min(offset+ioChunkSize, len(destination))
		if _, err := io.ReadFull(s.reader, destination[offset:end]); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func (s *recordScanner) finishTerminator() error {
	if s.canonical != nil {
		if err := s.canonical.WriteByte(recordEnd); err != nil {
			return err
		}
	}
	_, err := s.reader.ReadByte()
	if err == nil {
		return errors.New("trailing data after state stream terminator")
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("finish state stream: %w", err)
	}
	s.terminated = true
	return nil
}

func (s *recordScanner) addCount(typ byte, length uint64) {
	s.counts.Records++
	s.counts.PayloadBytes += length
	switch typ {
	case RecordAccount:
		s.counts.Accounts++
	case RecordStorage:
		s.counts.StorageSlots++
	case RecordCode:
		s.counts.CodeRecords++
	}
}

func recordFromHeader(typ byte, header, payload []byte) Record {
	key := header[1 : len(header)-8]
	record := Record{Type: typ, AccountHash: common.BytesToHash(key[:common.HashLength]), Payload: payload}
	switch typ {
	case RecordStorage:
		record.SubHash = common.BytesToHash(key[common.HashLength:])
	case RecordCode:
		record.SubHash = record.AccountHash
		record.AccountHash = common.Hash{}
	}
	return record
}

func (s *recordScanner) Finish() (ScanResult, error) {
	if !s.terminated {
		return ScanResult{}, errors.New("state stream terminator was not consumed")
	}
	if err := copyContext(s.ctx, io.Discard, s.raw); err != nil {
		return ScanResult{}, fmt.Errorf("finish hashing state records: %w", err)
	}
	fileHash := common.BytesToHash(s.fileHash.Sum(nil))
	if err := s.finishCanonical(fileHash); err != nil {
		return ScanResult{}, err
	}
	if err := s.verifyCounts(); err != nil {
		return ScanResult{}, err
	}
	if err := s.root.RequireExactLayout(map[string]strictio.EntryKind{
		ManifestFileName:          strictio.RegularFile,
		s.manifest.StateFile.Name: strictio.RegularFile,
	}); err != nil {
		return ScanResult{}, fmt.Errorf("re-check bundle layout: %w", err)
	}
	result := ScanResult{Counts: s.counts, FileSHA256: fileHash, RecordChainHash: s.chain}
	if result.FileSHA256 != s.manifest.StateFile.SHA256 {
		return ScanResult{}, fmt.Errorf("state records SHA-256 mismatch: have %s want %s", result.FileSHA256, s.manifest.StateFile.SHA256)
	}
	if result.RecordChainHash != s.manifest.StateFile.RecordChainHash {
		return ScanResult{}, fmt.Errorf("record chain hash mismatch: have %s want %s", result.RecordChainHash, s.manifest.StateFile.RecordChainHash)
	}
	return result, nil
}

func (s *recordScanner) finishCanonical(fileHash common.Hash) error {
	if s.canonical == nil {
		return nil
	}
	canonicalSize, canonicalHash, err := s.canonical.Close()
	if err != nil {
		return err
	}
	if canonicalSize != s.fileSize || canonicalHash != fileHash {
		return errors.New("zstd state records do not use the canonical single-frame encoding")
	}
	return nil
}

func (s *recordScanner) verifyCounts() error {
	want := s.manifest.Counts
	if s.counts.Accounts != want.Accounts || s.counts.StorageSlots != want.StorageSlots ||
		s.counts.CodeRecords != want.CodeRecords || s.counts.Records != want.Records {
		return fmt.Errorf("record counts mismatch: have %+v want %+v", s.counts, want)
	}
	if s.counts.PayloadBytes != s.manifest.StateFile.RecordPayloadBytes {
		return fmt.Errorf("record payload bytes mismatch: have %d want %d", s.counts.PayloadBytes, s.manifest.StateFile.RecordPayloadBytes)
	}
	return nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var probe [1]byte
	read, readErr := source.Read(probe[:])
	if read > 0 {
		if _, err := destination.Write(probe[:read]); err != nil {
			return err
		}
	}
	if errors.Is(readErr, io.EOF) {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	buffer := make([]byte, ioChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			if _, err := destination.Write(buffer[:read]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if read == 0 {
			return io.ErrNoProgress
		}
	}
}

func (s *recordScanner) Close() (retErr error) {
	if s == nil {
		return nil
	}
	if s.canonical != nil {
		if err := s.canonical.Abort(); err != nil {
			retErr = errors.Join(retErr, err)
		}
		s.canonical = nil
	}
	if s.decoder != nil {
		s.decoder.Close()
		s.decoder = nil
	}
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close state records: %w", err))
		}
		s.file = nil
	}
	if s.root != nil {
		if err := s.root.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
		s.root = nil
	}
	return retErr
}
