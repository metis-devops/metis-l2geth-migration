package bundle

import (
	"context"
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

func BenchmarkRecordStreamScan(b *testing.B) {
	const recordCount = 20_000
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		b.Run(compression, func(b *testing.B) {
			dir := b.TempDir()
			head, headerRLP := benchmarkHead(b)
			account := types.NewEmptyStateAccount()
			fullRLP, err := rlp.EncodeToBytes(account)
			if err != nil {
				b.Fatal(err)
			}
			payload, err := EncodeAccount(account)
			if err != nil {
				b.Fatal(err)
			}
			writer, err := NewWriter(context.Background(), dir, compression, head, headerRLP)
			if err != nil {
				b.Fatal(err)
			}
			for index := uint64(1); index <= recordCount; index++ {
				var hash common.Hash
				binary.BigEndian.PutUint64(hash[common.HashLength-8:], index)
				if err := writer.WriteAccount(hash, payload, uint64(len(fullRLP))); err != nil {
					b.Fatal(err)
				}
			}
			result, err := writer.Close()
			if err != nil {
				b.Fatal(err)
			}
			manifest := NewManifest(SourceEvidence{
				HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP),
			}, result.Counts, StateFile{
				Name: result.FileName, Compression: result.Compression, Size: result.Size,
				RecordPayloadBytes: result.RecordPayloadBytes, SHA256: result.SHA256, RecordChainHash: result.RecordChainHash,
			})
			if _, err := WriteManifest(dir, manifest); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(result.Size)
			b.ResetTimer()
			for range b.N {
				if _, err := ScanRecordsBorrowed(context.Background(), dir, manifest, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkHead(tb testing.TB) (Head, []byte) {
	tb.Helper()
	header := &types.Header{
		UncleHash: types.EmptyUncleHash, Root: types.EmptyRootHash,
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
		Difficulty: big.NewInt(1), Number: big.NewInt(1), GasLimit: 1, Time: 1,
	}
	headerRLP, err := rlp.EncodeToBytes(header)
	if err != nil {
		tb.Fatal(err)
	}
	return Head{BlockNumber: 1, BlockHash: header.Hash(), StateRoot: header.Root}, headerRLP
}
