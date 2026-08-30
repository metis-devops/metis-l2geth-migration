package bundle

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func TestManifestHexTypesJSONRoundTrip(t *testing.T) {
	manifest, headerRLP := validTestManifest(t)
	dir := t.TempDir()
	data, err := WriteManifest(dir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Source struct {
			HeaderRLP string `json:"header_rlp"`
		} `json:"source"`
		StateFile struct {
			RecordPayloadBytes uint64 `json:"record_payload_bytes"`
			SHA256             string `json:"sha256"`
			RecordChainHash    string `json:"record_chain_hash"`
		} `json:"state_file"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Source.HeaderRLP != hexutil.Bytes(headerRLP).String() {
		t.Fatalf("header RLP JSON is %q, want %q", wire.Source.HeaderRLP, hexutil.Bytes(headerRLP).String())
	}
	if wire.StateFile.SHA256 != manifest.StateFile.SHA256.Hex() {
		t.Fatalf("SHA-256 JSON is %q, want %q", wire.StateFile.SHA256, manifest.StateFile.SHA256.Hex())
	}
	if wire.StateFile.RecordChainHash != manifest.StateFile.RecordChainHash.Hex() {
		t.Fatalf("record chain hash JSON is %q, want %q", wire.StateFile.RecordChainHash, manifest.StateFile.RecordChainHash.Hex())
	}
	if wire.StateFile.RecordPayloadBytes != manifest.StateFile.RecordPayloadBytes ||
		!strings.Contains(string(data), `"record_payload_bytes": 0`) {
		t.Fatalf("record payload bytes are not explicitly encoded: %s", data)
	}
	for name, value := range map[string]string{
		"sha256":            wire.StateFile.SHA256,
		"record_chain_hash": wire.StateFile.RecordChainHash,
	} {
		if len(value) != 2+2*common.HashLength || !strings.HasPrefix(value, "0x") || value != strings.ToLower(value) {
			t.Fatalf("%s is not canonical 0x-prefixed 32-byte hex: %q", name, value)
		}
	}
	loaded, _, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Source.HeaderRLP, headerRLP) {
		t.Fatal("loaded header RLP does not match")
	}
	if loaded.StateFile != manifest.StateFile {
		t.Fatalf("loaded state file evidence is %+v, want %+v", loaded.StateFile, manifest.StateFile)
	}
}

func TestSourceEvidenceValidatedHeader(t *testing.T) {
	head, headerRLP := testHead(t)
	evidence := SourceEvidence{
		HeadBefore: head,
		HeadAfter:  head,
		HeaderRLP:  headerRLP,
	}
	header, err := evidence.ValidatedHeader()
	if err != nil {
		t.Fatal(err)
	}
	if header.Number == nil || header.Number.Uint64() != head.BlockNumber || header.Root != head.StateRoot {
		t.Fatalf("validated header is %+v, want block %d root %s", header, head.BlockNumber, head.StateRoot)
	}

	evidence.HeadBefore.BlockHash = common.HexToHash("0xdeadbeef")
	evidence.HeadAfter = evidence.HeadBefore
	if _, err := evidence.ValidatedHeader(); err == nil || !strings.Contains(err.Error(), "header block hash mismatch") {
		t.Fatalf("validated header error is %v, want block hash mismatch", err)
	}
}

func TestManifestRejectsV2AndInvalidRecordPayloadBytes(t *testing.T) {
	t.Parallel()

	valid, _ := validTestManifest(t)
	valid.Counts = Counts{Accounts: 1, Records: 1, PayloadBytes: 70}
	valid.StateFile.RecordPayloadBytes = 5
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		wantErr string
	}{
		{name: "v2", mutate: func(m *Manifest) { m.Version = 2 }, wantErr: "unsupported bundle format"},
		{name: "missing record payload bytes", mutate: func(m *Manifest) { m.StateFile.RecordPayloadBytes = 0 }, wantErr: "zero record payload bytes"},
		{name: "record payload exceeds consensus payload", mutate: func(m *Manifest) { m.StateFile.RecordPayloadBytes = 71 }, wantErr: "exceed expanded consensus"},
		{name: "empty stream with record payload bytes", mutate: func(m *Manifest) {
			m.Counts = Counts{}
			m.StateFile.RecordPayloadBytes = 1
		}, wantErr: "empty record stream"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manifest := valid
			test.mutate(&manifest)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validation error is %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadManifestRejectsInvalidHexEvidence(t *testing.T) {
	manifest, _ := validTestManifest(t)
	base, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(source, stateFile map[string]any)
	}{
		{name: "header without prefix", mutate: func(source, _ map[string]any) {
			source["header_rlp"] = strings.TrimPrefix(source["header_rlp"].(string), "0x")
		}},
		{name: "header with odd length", mutate: func(source, _ map[string]any) {
			source["header_rlp"] = "0x1"
		}},
		{name: "header with invalid hex", mutate: func(source, _ map[string]any) {
			source["header_rlp"] = "0xzz"
		}},
		{name: "empty header", mutate: func(source, _ map[string]any) {
			source["header_rlp"] = "0x"
		}},
		{name: "missing header", mutate: func(source, _ map[string]any) {
			delete(source, "header_rlp")
		}},
		{name: "sha256 without prefix", mutate: func(_ map[string]any, stateFile map[string]any) {
			stateFile["sha256"] = strings.TrimPrefix(stateFile["sha256"].(string), "0x")
		}},
		{name: "sha256 with wrong length", mutate: func(_ map[string]any, stateFile map[string]any) {
			stateFile["sha256"] = "0x01"
		}},
		{name: "sha256 with invalid hex", mutate: func(_ map[string]any, stateFile map[string]any) {
			stateFile["sha256"] = "0x" + strings.Repeat("z", 2*common.HashLength)
		}},
		{name: "missing sha256", mutate: func(_ map[string]any, stateFile map[string]any) {
			delete(stateFile, "sha256")
		}},
		{name: "zero sha256", mutate: func(_ map[string]any, stateFile map[string]any) {
			stateFile["sha256"] = "0x" + strings.Repeat("0", 2*common.HashLength)
		}},
		{name: "record chain hash without prefix", mutate: func(_ map[string]any, stateFile map[string]any) {
			stateFile["record_chain_hash"] = strings.TrimPrefix(stateFile["record_chain_hash"].(string), "0x")
		}},
		{name: "missing record chain hash", mutate: func(_ map[string]any, stateFile map[string]any) {
			delete(stateFile, "record_chain_hash")
		}},
		{name: "zero record chain hash", mutate: func(_ map[string]any, stateFile map[string]any) {
			stateFile["record_chain_hash"] = "0x" + strings.Repeat("0", 2*common.HashLength)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(base, &document); err != nil {
				t.Fatal(err)
			}
			source := document["source"].(map[string]any)
			stateFile := document["state_file"].(map[string]any)
			tt.mutate(source, stateFile)
			data, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ManifestFileName), data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadManifest(dir); err == nil {
				t.Fatal("invalid manifest unexpectedly loaded")
			}
		})
	}
}

func validTestManifest(t *testing.T) (Manifest, []byte) {
	t.Helper()
	head, headerRLP := testHead(t)
	return NewManifest(SourceEvidence{
		HeadBefore: head,
		HeadAfter:  head,
		HeaderRLP:  hexutil.Bytes(headerRLP),
	}, Counts{}, StateFile{
		Name:            RecordsFileRaw,
		Compression:     CompressionNone,
		Size:            1,
		SHA256:          common.HexToHash("0x01"),
		RecordChainHash: common.HexToHash("0x02"),
	}), headerRLP
}
