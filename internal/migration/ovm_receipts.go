package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// Decode precisely the storage encodings supported by the pinned legacy
// l2geth reader, then rebuild the consensus receipt (including its bloom).
func decodeOVMReceiptsContext(ctx context.Context, blob []byte) (types.Receipts, error) {
	var records []rlp.RawValue
	if err := rlp.DecodeBytes(blob, &records); err != nil {
		return nil, fmt.Errorf("decode legacy receipts: %w", err)
	}
	receipts := make(types.Receipts, len(records))
	for n, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, err := decodeOVMReceiptContext(ctx, record)
		if err != nil {
			return nil, fmt.Errorf("decode legacy receipt %d: %w", n, err)
		}
		if n > 0 && r.CumulativeGasUsed < receipts[n-1].CumulativeGasUsed {
			return nil, errors.New("legacy cumulative gas decreases")
		}
		receipts[n] = r
	}
	return receipts, nil
}

func decodeOVMReceipt(blob []byte) (*types.Receipt, error) {
	return decodeOVMReceiptContext(context.Background(), blob)
}
func decodeOVMReceiptContext(ctx context.Context, blob []byte) (*types.Receipt, error) {
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(blob, &fields); err != nil {
		return nil, err
	}
	if len(fields) != 6 && len(fields) != 7 {
		return nil, errors.New("unsupported legacy receipt storage encoding")
	}
	r := new(types.Receipt)
	var status []byte
	if err := rlp.DecodeBytes(fields[0], &status); err != nil {
		return nil, err
	}
	switch {
	case len(status) == 0:
		r.Status = 0
	case bytes.Equal(status, []byte{1}):
		r.Status = 1
	case len(status) == 32:
		r.PostState = status
	default:
		return nil, errors.New("invalid legacy receipt status")
	}
	if err := rlp.DecodeBytes(fields[1], &r.CumulativeGasUsed); err != nil {
		return nil, err
	}
	logIndex, err := validateOVMReceiptMetadata(fields)
	if err != nil {
		return nil, err
	}
	var logs []rlp.RawValue
	if err := rlp.DecodeBytes(fields[logIndex], &logs); err != nil {
		return nil, err
	}
	for _, raw := range logs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		log, err := decodeOVMLog(raw)
		if err != nil {
			return nil, err
		}
		r.Logs = append(r.Logs, log)
	}
	r.Bloom = types.CreateBloom(r)
	if logIndex == 5 {
		var bloom types.Bloom
		if err := rlp.DecodeBytes(fields[2], &bloom); err != nil {
			return nil, err
		}
		if bloom != r.Bloom {
			return nil, errors.New("legacy stored bloom disagrees with logs")
		}
	}
	if len(r.PostState) == 0 && r.Status == 0 && len(r.Logs) > 0 {
		return nil, errors.New("failed legacy receipt contains logs")
	}
	return r, nil
}

func validateOVMReceiptMetadata(f []rlp.RawValue) (int, error) {
	kind, _, _, err := rlp.Split(f[2])
	if err != nil {
		return 0, err
	}
	if len(f) == 7 && kind == rlp.List {
		for _, field := range f[3:6] {
			var n big.Int
			if err := rlp.DecodeBytes(field, &n); err != nil {
				return 0, err
			}
		}
		var scalar string
		if err := rlp.DecodeBytes(f[6], &scalar); err != nil {
			return 0, err
		}
		if scalar != "" {
			if _, _, err := big.ParseFloat(scalar, 10, 256, big.ToNearestEven); err != nil {
				return 0, fmt.Errorf("invalid legacy fee scalar: %w", err)
			}
		}
		return 2, nil
	}
	// The two older legacy forms store tx hash, contract address and gas used.
	offset := 2
	if len(f) == 7 {
		offset = 3
	}
	var hash [32]byte
	var address [20]byte
	var gas uint64
	if err := rlp.DecodeBytes(f[offset], &hash); err != nil {
		return 0, err
	}
	if err := rlp.DecodeBytes(f[offset+1], &address); err != nil {
		return 0, err
	}
	if err := rlp.DecodeBytes(f[offset+3], &gas); err != nil {
		return 0, err
	}
	return offset + 2, nil
}

func decodeOVMLog(blob []byte) (*types.Log, error) {
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(blob, &fields); err != nil {
		return nil, err
	}
	if len(fields) != 3 && len(fields) != 8 {
		return nil, errors.New("unsupported legacy log storage encoding")
	}
	l := new(types.Log)
	if err := rlp.DecodeBytes(fields[0], &l.Address); err != nil {
		return nil, err
	}
	if err := rlp.DecodeBytes(fields[1], &l.Topics); err != nil {
		return nil, err
	}
	if err := rlp.DecodeBytes(fields[2], &l.Data); err != nil {
		return nil, err
	}
	if len(fields) == 8 {
		for n, value := range []any{&l.BlockNumber, &l.TxHash, &l.TxIndex, &l.BlockHash, &l.Index} {
			if err := rlp.DecodeBytes(fields[n+3], value); err != nil {
				return nil, err
			}
		}
	}
	return l, nil
}
