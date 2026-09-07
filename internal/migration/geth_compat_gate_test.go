package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestGethCompatibilityDetectsContractDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		path   string
	}{
		{"encoding", func(d map[string]any) { d["encoding"] = "0xc180" }, "encoding"},
		{"database value", func(d map[string]any) { d["database"].(map[string]any)["0x41"] = "0x02" }, "database/0x41"},
		{"database key", func(d map[string]any) { delete(d["database"].(map[string]any), "0x41") }, "database/0x41"},
		{"path metadata", func(d map[string]any) { d["database"].(map[string]any)["0x536e617073686f74526f6f74"] = "0xbb" }, "database/0x536e617073686f74526f6f74"},
		{"report", func(d map[string]any) { d["report"].(map[string]any)["state_layout"] = "legacy-l2geth" }, "report/state_layout"},
		{"continuation", func(d map[string]any) { d["continuation"].(map[string]any)["root"] = "0x02" }, "continuation/root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := map[string]any{"encoding": "0xc0", "database": map[string]any{"0x41": "0x01", "0x536e617073686f74526f6f74": "0xaa"}, "report": map[string]any{"state_layout": "geth"}, "continuation": map[string]any{"root": "0x01"}}
			data := compatJSON(t, original)
			var changed map[string]any
			if err := json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			tc.mutate(changed)
			left := &compatContract{Format: "verification", Version: 1, Cases: map[string]json.RawMessage{"canary/none/pebble/geth/path": data}}
			right := &compatContract{Format: "verification", Version: 1, Cases: map[string]json.RawMessage{"canary/none/pebble/geth/path": compatJSON(t, changed)}}
			err := compareCompatContracts(left, right)
			if err == nil || !strings.Contains(err.Error(), "verification/canary/none/pebble/geth/path/"+tc.path) || !strings.Contains(err.Error(), "expected") || !strings.Contains(err.Error(), "actual") {
				t.Fatalf("wrong drift diagnostic: %v", err)
			}
		})
	}
}

func TestGethCompatibilityNormalization(t *testing.T) {
	direct := validDirectVerificationReport(t)
	manifest := bundle.NewManifest(direct.Source, bundle.Counts{}, bundle.StateFile{Name: bundle.RecordsFileRaw, Compression: bundle.CompressionNone, Size: 1, SHA256: common.HexToHash("0x01"), RecordChainHash: common.HexToHash("0x02")})
	leftManifest := normalizeCompatJSON(t, manifest, nil)
	manifest.CreatedAt = time.Unix(3, 0)
	manifest.ToolVersion, manifest.GethVersion = "new-tool", "new-geth"
	rightManifest := normalizeCompatJSON(t, withHistoricalCommit(t, manifest), nil)
	if string(leftManifest) != string(rightManifest) {
		t.Fatal("provenance changed normalized manifest")
	}
	report := validTestVerificationReport()
	leftReport := normalizeCompatJSON(t, report, leftManifest)
	report.VerifiedAt = time.Unix(4, 0)
	report.ToolVersion, report.GethVersion = "new-tool", "new-geth"
	report.ManifestSHA256 = common.HexToHash("0x1234")
	rightReport := normalizeCompatJSON(t, withHistoricalCommit(t, report), rightManifest)
	if string(leftReport) != string(rightReport) {
		t.Fatal("provenance or derived manifest hash triggered a false difference")
	}
	report.RecordChainHash = common.HexToHash("0x9999")
	if string(leftReport) == string(normalizeCompatJSON(t, report, rightManifest)) {
		t.Fatal("normalization hid record integrity drift")
	}
}

func TestGethCompatibilityBaselineLifecycle(t *testing.T) {
	root := t.TempDir()
	if _, err := readCompatContract(root, "bundle", 1); err == nil {
		t.Fatal("missing baseline accepted")
	}
	original := &compatContract{Format: "bundle", Version: 1, Cases: map[string]json.RawMessage{"probe": json.RawMessage(`true`)}}
	path := filepath.Join(root, "bundle-v1.json")
	if err := writeCompatJSON(path, original); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompatContract(root, "bundle", 1); err != nil {
		t.Fatal(err)
	}
	original.Version = 2
	if err := writeCompatJSON(path, original); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompatContract(root, "bundle", 1); err == nil {
		t.Fatal("wrong baseline version accepted")
	}
	capture := newCompatCapture()
	output := filepath.Join(root, "candidate")
	if err := writeCompatCapture(output, capture); err != nil {
		t.Fatal(err)
	}
	before := directoryContentDigest(t, output)
	if err := writeCompatCapture(output, capture); err == nil {
		t.Fatal("candidate overwrote an existing directory")
	}
	if directoryContentDigest(t, output) != before {
		t.Fatal("existing candidate changed")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(output, alias); err != nil {
		t.Fatal(err)
	}
	if err := writeCompatCapture(alias, capture); err == nil {
		t.Fatal("candidate followed an output symlink")
	}
}

func TestGethCompatibilityDatabaseDiagnostics(t *testing.T) {
	left := &compatContract{Format: "direct", Version: 1, Cases: make(map[string]json.RawMessage), Databases: make(map[string][]compatKV)}
	right := &compatContract{Format: "direct", Version: 1, Cases: make(map[string]json.RawMessage), Databases: make(map[string][]compatKV)}
	for i, c := range []*compatContract{left, right} {
		entries := []compatKV{{Key: hexutil.Bytes{0x41}, Value: hexutil.Bytes{byte(i)}}}
		id := compatDatabaseID(entries)
		c.Databases[id] = entries
		c.add(t, "canary/pebble/geth/path", compatArtifact{Database: id, Report: json.RawMessage(`{}`)})
	}
	if err := compareCompatContracts(left, right); err == nil || !strings.Contains(err.Error(), "database/0x41") {
		t.Fatalf("no concrete key diagnostic: %v", err)
	}
	long := compatValue(strings.Repeat("x", 1000))
	if len(long) > 200 || !strings.Contains(long, "sha256=") || !strings.Contains(long, "json_bytes=") {
		t.Fatalf("unbounded or incomplete diagnostic %s", long)
	}
}

// Historical metadata is exercised only through the test normalizer, never a
// runtime manifest/report reader.
func withHistoricalCommit(t *testing.T, document any) json.RawMessage {
	t.Helper()
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(compatJSON(t, document), &wire); err != nil {
		t.Fatal(err)
	}
	wire["geth_commit"] = json.RawMessage(`"historical-commit"`)
	return compatJSON(t, wire)
}
