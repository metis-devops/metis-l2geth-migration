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
			SHA256          string `json:"sha256"`
			RecordChainHash string `json:"record_chain_hash"`
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
