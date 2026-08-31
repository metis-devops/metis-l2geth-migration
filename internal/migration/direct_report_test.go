package migration

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

func TestDirectVerificationReportJSONRoundTrip(t *testing.T) {
	report := validDirectVerificationReport(t)
	dir := t.TempDir()
	data, err := writeDirectVerificationReport(dir, report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Format string `json:"format"`
		Source struct {
			HeadBefore struct {
				BlockHash string `json:"block_hash"`
				StateRoot string `json:"state_root"`
			} `json:"head_before"`
			HeaderRLP string `json:"header_rlp"`
		} `json:"source"`
		RecomputedRoot string `json:"recomputed_state_root"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Format != DirectVerificationFormat {
		t.Fatalf("format is %q, want %q", wire.Format, DirectVerificationFormat)
	}
	for name, value := range map[string]string{
		"block_hash":            wire.Source.HeadBefore.BlockHash,
		"state_root":            wire.Source.HeadBefore.StateRoot,
		"header_rlp":            wire.Source.HeaderRLP,
		"recomputed_state_root": wire.RecomputedRoot,
	} {
		if !strings.HasPrefix(value, "0x") || value != strings.ToLower(value) {
			t.Fatalf("%s is not canonical 0x-prefixed hex: %q", name, value)
		}
	}
	loaded, err := loadDirectVerificationReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, report) {
		t.Fatalf("loaded report is %+v, want %+v", loaded, report)
	}
	if _, err := loadVerificationReport(dir); err == nil {
		t.Fatal("direct report unexpectedly loaded as a bundle verification report")
	}
}

func TestLoadDirectVerificationReportRejectsInvalidEvidence(t *testing.T) {
	base, err := json.Marshal(validDirectVerificationReport(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown field", mutate: func(document map[string]any) { document["extra"] = true }},
		{name: "wrong version", mutate: func(document map[string]any) { document["version"] = float64(2) }},
		{name: "missing header RLP", mutate: func(document map[string]any) {
			delete(document["source"].(map[string]any), "header_rlp")
		}},
		{name: "unprefixed block hash", mutate: func(document map[string]any) {
			source := document["source"].(map[string]any)
			head := source["head_before"].(map[string]any)
			head["block_hash"] = strings.TrimPrefix(head["block_hash"].(string), "0x")
		}},
		{name: "root mismatch", mutate: func(document map[string]any) {
			document["recomputed_state_root"] = common.HexToHash("0x01").Hex()
		}},
		{name: "invalid counts", mutate: func(document map[string]any) {
			document["counts"].(map[string]any)["records"] = float64(1)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(base, &document); err != nil {
				t.Fatal(err)
			}
			tt.mutate(document)
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "db"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, VerificationFileName), data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadDirectVerificationReport(dir); err == nil {
				t.Fatal("invalid direct verification report unexpectedly loaded")
			}
		})
	}
}

func TestDirectAndBundleReportFormatsAreDistinct(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeVerificationReport(dir, validTestVerificationReport()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDirectVerificationReport(dir); err == nil {
		t.Fatal("bundle verification report unexpectedly loaded as a direct report")
	}
}

func validDirectVerificationReport(t *testing.T) DirectVerificationReport {
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
	head := bundle.Head{BlockNumber: 1, BlockHash: header.Hash(), StateRoot: header.Root}
	return DirectVerificationReport{
		Format:      DirectVerificationFormat,
		Version:     DirectVerificationVersion,
		VerifiedAt:  time.Unix(1, 0).UTC(),
		Verified:    true,
		Scheme:      rawdb.HashScheme,
		DBEngine:    "pebble-v2",
		ToolVersion: version.ToolVersion,
		GethVersion: version.GethVersion,
		GethCommit:  version.GethCommit,
		Source: bundle.SourceEvidence{
			HeadBefore: head,
			HeadAfter:  head,
			HeaderRLP:  hexutil.Bytes(headerRLP),
		},
		Counts:         bundle.Counts{},
		RecomputedRoot: header.Root,
	}
}
