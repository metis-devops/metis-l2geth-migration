package bundle

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

func TestRecordPayloadLengthBounds(t *testing.T) {
	for _, test := range []struct {
		name    string
		typ     byte
		length  uint64
		wantErr bool
	}{
		{name: "account maximum", typ: RecordAccount, length: maxAccountPayload},
		{name: "account oversized", typ: RecordAccount, length: maxAccountPayload + 1, wantErr: true},
		{name: "storage maximum", typ: RecordStorage, length: maxStoragePayload},
		{name: "storage oversized", typ: RecordStorage, length: maxStoragePayload + 1, wantErr: true},
		{name: "empty code", typ: RecordCode, length: 0, wantErr: true},
		{name: "code maximum", typ: RecordCode, length: maxCodePayload},
		{name: "code oversized", typ: RecordCode, length: maxCodePayload + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validatePayloadLength(test.typ, test.length)
			if (err != nil) != test.wantErr {
				t.Fatalf("validation returned %v", err)
			}
		})
	}
}

func TestScanRecordsBorrowedReusesPayloadAndOwnedDoesNot(t *testing.T) {
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(context.Background(), dir, CompressionNone, head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	for nonce := uint64(1); nonce <= 2; nonce++ {
		account := types.NewEmptyStateAccount()
		account.Nonce = nonce
		fullRLP, err := rlp.EncodeToBytes(account)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := EncodeAccount(account)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteAccount(common.BigToHash(new(big.Int).SetUint64(nonce)), payload, uint64(len(fullRLP))); err != nil {
			t.Fatal(err)
		}
	}
	result, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest := NewManifest(SourceEvidence{HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP)}, result.Counts, StateFile{
		Name: result.FileName, Compression: result.Compression, Size: result.Size,
		RecordPayloadBytes: result.RecordPayloadBytes, SHA256: result.SHA256, RecordChainHash: result.RecordChainHash,
	})
	if _, err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	var borrowedPointers []*byte
	if _, err := ScanRecordsBorrowed(context.Background(), dir, manifest, func(record Record) error {
		borrowedPointers = append(borrowedPointers, &record.Payload[0])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(borrowedPointers) != 2 || borrowedPointers[0] != borrowedPointers[1] {
		t.Fatalf("borrowed scanner did not reuse payload storage: %p %p", borrowedPointers[0], borrowedPointers[1])
	}
	var ownedPayloads [][]byte
	if _, err := ScanRecords(context.Background(), dir, manifest, func(record Record) error {
		ownedPayloads = append(ownedPayloads, record.Payload)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if &ownedPayloads[0][0] == &ownedPayloads[1][0] {
		t.Fatal("owned scanner reused payload storage")
	}
}

func TestWriterHonorsCanceledContextBeforeRecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(ctx, dir, CompressionNone, head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := writer.WriteCode(common.HexToHash("0x01"), []byte{0x60}); !errors.Is(err, context.Canceled) {
		t.Fatalf("write returned %v, want cancellation", err)
	}
	if err := writer.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterHonorsCancellationDuringRecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	destination := &cancelOnWrite{cancel: cancel}
	writer := &Writer{ctx: ctx, buffer: bufio.NewWriterSize(destination, 1)}
	err := writer.WriteCode(common.HexToHash("0x01"), make([]byte, ioChunkSize+1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("write returned %v, want cancellation", err)
	}
}

func TestScannerPayloadReadHonorsMidRecordCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelAfterReader{remaining: ioChunkSize + 1, cancel: cancel}
	scanner := &recordScanner{
		ctx:    ctx,
		reader: bufio.NewReaderSize(source, ioChunkSize),
	}
	scanner.chainHasher.Start(new(common.Hash), []byte{bundleRecordAccountHeaderByteForTest})
	err := scanner.readPayload(make([]byte, ioChunkSize+1))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("payload read returned %v, want cancellation", err)
	}
}

const bundleRecordAccountHeaderByteForTest = RecordAccount

type cancelAfterReader struct {
	remaining int
	cancel    context.CancelFunc
	canceled  bool
}

type cancelOnWrite struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelOnWrite) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		w.cancel()
	}
	return len(data), nil
}

func (r *cancelAfterReader) Read(destination []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	read := min(len(destination), r.remaining)
	for index := range read {
		destination[index] = 1
	}
	r.remaining -= read
	if !r.canceled {
		r.canceled = true
		r.cancel()
	}
	return read, nil
}

func TestRecordRoundTrip(t *testing.T) {
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		t.Run(compression, func(t *testing.T) {
			dir := t.TempDir()
			head, headerRLP := testHead(t)
			writer, err := NewWriter(context.Background(), dir, compression, head, headerRLP)
			if err != nil {
				t.Fatal(err)
			}
			account := types.NewEmptyStateAccount()
			account.Nonce = 3
			accountRLP, err := rlp.EncodeToBytes(account)
			if err != nil {
				t.Fatal(err)
			}
			accountPayload, err := EncodeAccount(account)
			if err != nil {
				t.Fatal(err)
			}
			accountHash := common.HexToHash("0x01")
			if err := writer.WriteAccount(accountHash, accountPayload, uint64(len(accountRLP))); err != nil {
				t.Fatal(err)
			}
			result, err := writer.Close()
			if err != nil {
				t.Fatal(err)
			}
			if result.Counts.PayloadBytes != uint64(len(accountRLP)) || result.RecordPayloadBytes != uint64(len(accountPayload)) {
				t.Fatalf("payload counts are semantic=%d record=%d, want %d and %d",
					result.Counts.PayloadBytes, result.RecordPayloadBytes, len(accountRLP), len(accountPayload))
			}
			manifest := NewManifest(SourceEvidence{
				HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP),
			}, result.Counts, StateFile{
				Name: result.FileName, Compression: result.Compression, Size: result.Size,
				RecordPayloadBytes: result.RecordPayloadBytes,
				SHA256:             result.SHA256, RecordChainHash: result.RecordChainHash,
			})
			if _, err := WriteManifest(dir, manifest); err != nil {
				t.Fatal(err)
			}
			var records []Record
			scan, err := ScanRecords(context.Background(), dir, manifest, func(record Record) error {
				records = append(records, record)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].Type != RecordAccount || records[0].AccountHash != accountHash {
				t.Fatalf("unexpected records: %+v", records)
			}
			wantWireCounts := WireCounts{Accounts: 1, Records: 1, PayloadBytes: result.RecordPayloadBytes}
			if scan.Counts != wantWireCounts ||
				scan.FileSHA256 != result.SHA256 || scan.RecordChainHash != result.RecordChainHash {
				t.Fatalf("scan result mismatch: %+v writer %+v", scan, result)
			}
			wrongPayloadBytes := manifest
			wrongPayloadBytes.StateFile.RecordPayloadBytes++
			if _, err := ScanRecords(context.Background(), dir, wrongPayloadBytes, nil); err == nil {
				t.Fatal("record payload byte mismatch unexpectedly verified")
			}
		})
	}
}

func TestScanRecordsRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(context.Background(), dir, CompressionNone, head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	result, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, result.FileName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0xff}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	manifest := NewManifest(SourceEvidence{
		HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP),
	}, result.Counts, StateFile{
		Name: result.FileName, Compression: result.Compression, Size: int64(len(data)),
		RecordPayloadBytes: result.RecordPayloadBytes,
		SHA256:             common.Hash(sum), RecordChainHash: result.RecordChainHash,
	})
	if _, err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanRecords(context.Background(), dir, manifest, nil); err == nil {
		t.Fatal("record stream with trailing data unexpectedly verified")
	}
}

func TestScanRecordsRejectsZstdSkippableTrailingFrame(t *testing.T) {
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(context.Background(), dir, CompressionZstd, head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	result, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, result.FileName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hidden trailing payload")
	frame := []byte{0x50, 0x2a, 0x4d, 0x18}
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(payload)))
	frame = append(frame, size[:]...)
	frame = append(frame, payload...)
	if _, err := file.Write(frame); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	manifest := NewManifest(SourceEvidence{
		HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP),
	}, result.Counts, StateFile{
		Name: result.FileName, Compression: result.Compression, Size: int64(len(data)),
		RecordPayloadBytes: result.RecordPayloadBytes,
		SHA256:             common.Hash(sum), RecordChainHash: result.RecordChainHash,
	})
	if _, err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanRecords(context.Background(), dir, manifest, nil); err == nil {
		t.Fatal("zstd stream with a trailing skippable frame unexpectedly verified")
	}
}

func TestLoadManifestRejectsTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(context.Background(), dir, CompressionNone, head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	result, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest := NewManifest(SourceEvidence{
		HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP),
	}, result.Counts, StateFile{
		Name: result.FileName, Compression: result.Compression, Size: result.Size,
		RecordPayloadBytes: result.RecordPayloadBytes,
		SHA256:             result.SHA256, RecordChainHash: result.RecordChainHash,
	})
	if _, err := WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ManifestFileName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadManifest(dir); err == nil {
		t.Fatal("manifest with trailing JSON unexpectedly loaded")
	}
}

func testHead(t *testing.T) (Head, []byte) {
	t.Helper()
	header := &types.Header{
		UncleHash:   types.EmptyUncleHash,
		Root:        types.EmptyRootHash,
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Difficulty:  big.NewInt(1),
		Number:      big.NewInt(1),
		GasLimit:    1,
		Time:        1,
	}
	headerRLP, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	return Head{BlockNumber: 1, BlockHash: header.Hash(), StateRoot: header.Root}, headerRLP
}
