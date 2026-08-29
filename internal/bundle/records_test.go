package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

func TestRecordRoundTrip(t *testing.T) {
	for _, compression := range []string{"none", "zstd"} {
		t.Run(compression, func(t *testing.T) {
			dir := t.TempDir()
			head, headerRLP := testHead(t)
			writer, err := NewWriter(dir, compression, head, headerRLP)
			if err != nil {
				t.Fatal(err)
			}
			account := types.NewEmptyStateAccount()
			account.Nonce = 3
			accountRLP, err := rlp.EncodeToBytes(account)
			if err != nil {
				t.Fatal(err)
			}
			accountHash := common.HexToHash("0x01")
			if err := writer.WriteAccount(accountHash, accountRLP); err != nil {
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
				SHA256: result.SHA256, RecordChainHash: result.RecordChainHash,
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
			if scan.Counts != result.Counts || scan.FileSHA256 != result.SHA256 || scan.RecordChainHash != result.RecordChainHash {
				t.Fatalf("scan result mismatch: %+v writer %+v", scan, result)
			}
		})
	}
}

func TestScanRecordsRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(dir, "none", head, headerRLP)
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
		SHA256: common.Hash(sum), RecordChainHash: result.RecordChainHash,
	})
	if _, err := ScanRecords(context.Background(), dir, manifest, nil); err == nil {
		t.Fatal("record stream with trailing data unexpectedly verified")
	}
}

func TestScanRecordsRejectsZstdSkippableTrailingFrame(t *testing.T) {
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(dir, "zstd", head, headerRLP)
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
		SHA256: common.Hash(sum), RecordChainHash: result.RecordChainHash,
	})
	if _, err := ScanRecords(context.Background(), dir, manifest, nil); err == nil {
		t.Fatal("zstd stream with a trailing skippable frame unexpectedly verified")
	}
}

func TestLoadManifestRejectsTrailingJSON(t *testing.T) {
	dir := t.TempDir()
	head, headerRLP := testHead(t)
	writer, err := NewWriter(dir, "none", head, headerRLP)
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
		SHA256: result.SHA256, RecordChainHash: result.RecordChainHash,
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
