package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

func TestVerificationReportHexTypesJSONRoundTrip(t *testing.T) {
	report := validTestVerificationReport()
	dir := t.TempDir()
	data, err := writeVerificationReport(dir, report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ManifestSHA256  string `json:"manifest_sha256"`
		StateFileSHA256 string `json:"state_file_sha256"`
		RecordChainHash string `json:"record_chain_hash"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	fields := map[string]struct {
		have string
		want common.Hash
	}{
		"manifest_sha256":   {have: wire.ManifestSHA256, want: report.ManifestSHA256},
		"state_file_sha256": {have: wire.StateFileSHA256, want: report.StateFileSHA256},
		"record_chain_hash": {have: wire.RecordChainHash, want: report.RecordChainHash},
	}
	for name, field := range fields {
		if field.have != field.want.Hex() {
			t.Fatalf("%s JSON is %q, want %q", name, field.have, field.want.Hex())
		}
		if len(field.have) != 2+2*common.HashLength || !strings.HasPrefix(field.have, "0x") || field.have != strings.ToLower(field.have) {
			t.Fatalf("%s is not canonical 0x-prefixed 32-byte hex: %q", name, field.have)
		}
	}
	loaded, err := loadVerificationReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != report {
		t.Fatalf("loaded report is %+v, want %+v", loaded, report)
	}
}

func TestLoadVerificationReportRejectsInvalidHashEvidence(t *testing.T) {
	base, err := json.Marshal(validTestVerificationReport())
	if err != nil {
		t.Fatal(err)
	}
	type testCase struct {
		name   string
		mutate func(map[string]any)
	}
	var tests []testCase
	for _, field := range []string{"manifest_sha256", "state_file_sha256", "record_chain_hash"} {
		tests = append(tests,
			testCase{name: field + " without prefix", mutate: func(document map[string]any) {
				document[field] = strings.TrimPrefix(document[field].(string), "0x")
			}},
			testCase{name: "missing " + field, mutate: func(document map[string]any) {
				delete(document, field)
			}},
			testCase{name: "zero " + field, mutate: func(document map[string]any) {
				document[field] = "0x" + strings.Repeat("0", 2*common.HashLength)
			}},
		)
	}
	tests = append(tests,
		testCase{name: "v2 report", mutate: func(document map[string]any) {
			document["version"] = float64(2)
		}},
		testCase{name: "wrong hash length", mutate: func(document map[string]any) {
			document["manifest_sha256"] = "0x01"
		}},
		testCase{name: "invalid hash hex", mutate: func(document map[string]any) {
			document["manifest_sha256"] = "0x" + strings.Repeat("z", 2*common.HashLength)
		}},
	)
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
			if _, err := loadVerificationReport(dir); err == nil {
				t.Fatal("invalid verification report unexpectedly loaded")
			}
		})
	}
}

func TestVerificationReportRejectsInvalidSemanticEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*VerificationReport)
	}{
		{name: "invalid counts", mutate: func(report *VerificationReport) {
			report.Counts = bundle.Counts{Accounts: 1, Records: 1}
		}},
		{name: "empty block hash", mutate: func(report *VerificationReport) {
			report.Head.BlockHash = common.Hash{}
		}},
		{name: "empty recomputed root", mutate: func(report *VerificationReport) {
			report.Head.StateRoot = common.Hash{}
			report.RecomputedRoot = common.Hash{}
		}},
		{name: "bundle database engine", mutate: func(report *VerificationReport) {
			report.DBEngine = "pebble-v2"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := validTestVerificationReport()
			test.mutate(&report)
			if err := report.Validate(); err == nil {
				t.Fatalf("invalid report %+v was accepted", report)
			}
		})
	}
}

func validTestVerificationReport() VerificationReport {
	root := common.HexToHash("0x04")
	return VerificationReport{
		Format:          VerificationFormat,
		Version:         VerificationVersion,
		VerifiedAt:      time.Unix(1, 0).UTC(),
		Verified:        true,
		Scheme:          "bundle",
		ToolVersion:     version.ToolVersion,
		GethVersion:     version.GethVersion,
		GethCommit:      version.GethCommit,
		ManifestSHA256:  common.HexToHash("0x01"),
		StateFileSHA256: common.HexToHash("0x02"),
		RecordChainHash: common.HexToHash("0x03"),
		Head: bundle.Head{
			BlockNumber: 1,
			BlockHash:   common.HexToHash("0x05"),
			StateRoot:   root,
		},
		Counts:         bundle.Counts{},
		RecomputedRoot: root,
	}
}
