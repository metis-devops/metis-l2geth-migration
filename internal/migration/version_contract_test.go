package migration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

func TestFormatProvenanceContracts(t *testing.T) {
	direct := validDirectVerificationReport(t)
	manifest := bundle.NewManifest(direct.Source, bundle.Counts{}, bundle.StateFile{
		Name: bundle.RecordsFileRaw, Compression: bundle.CompressionNone, Size: 1,
		SHA256: common.HexToHash("0x01"), RecordChainHash: common.HexToHash("0x02"),
	})
	t.Run("manifest", func(t *testing.T) { assertProvenanceContract(t, manifest, bundle.FormatVersion) })
	t.Run("bundle report", func(t *testing.T) { assertProvenanceContract(t, validTestVerificationReport(), VerificationVersion) })
	t.Run("direct report", func(t *testing.T) { assertProvenanceContract(t, direct, DirectVerificationVersion) })
}

func assertProvenanceContract[T interface{ Validate() error }](t *testing.T, original T, currentVersion int) {
	t.Helper()
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	assertCurrentBuildProvenance(t, original)
	for _, field := range []string{"version", "geth_version", "geth_commit", "tool_version"} {
		t.Run(field, func(t *testing.T) {
			saved, present := wire[field]
			defer func() {
				if present {
					wire[field] = saved
				} else {
					delete(wire, field)
				}
			}()
			for _, value := range []string{"omitted", `null`, `""`, `"arbitrary-fork"`, `0`, `1`, `2`, `3`, `true`, `{}`, `[]`, strconv.Itoa(currentVersion)} {
				t.Run(value, func(t *testing.T) {
					delete(wire, field)
					if value != "omitted" {
						wire[field] = json.RawMessage(value)
					}
					encoded, err := json.Marshal(wire)
					if err != nil {
						t.Fatal(err)
					}
					decoded, err := strictio.DecodeJSON[T](encoded, "version contract")
					if err == nil {
						err = decoded.Validate()
					}
					valid := value == "omitted" || value == `null` || value == `""` || value == `"arbitrary-fork"`
					switch field {
					case "version":
						valid = value == strconv.Itoa(currentVersion)
					case "geth_commit":
						valid = value == "omitted"
					case "tool_version":
						valid = value == `"arbitrary-fork"`
					}
					if (err == nil) != valid {
						t.Fatalf("valid=%v, got %v", valid, err)
					}
				})
			}
		})
	}
}

func assertCurrentBuildProvenance(t *testing.T, document any) {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Format      string          `json:"format"`
		Version     uint64          `json:"version"`
		ToolVersion string          `json:"tool_version"`
		GethVersion string          `json:"geth_version"`
		GethCommit  json.RawMessage `json:"geth_commit"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	expectedVersion, ok := map[string]uint64{bundle.FormatName: bundle.FormatVersion, VerificationFormat: VerificationVersion, DirectVerificationFormat: DirectVerificationVersion}[wire.Format]
	if !ok {
		t.Fatalf("unknown output format %q", wire.Format)
	}
	if wire.Version != expectedVersion || wire.ToolVersion != version.ToolVersion || wire.GethVersion != version.GethVersion || wire.GethCommit != nil {
		t.Fatalf("incorrect output version/provenance: %+v", wire)
	}
}

func rewriteGethProvenance(t *testing.T, path, value string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	delete(wire, "geth_version")
	if value != "omitted" {
		wire["geth_version"] = json.RawMessage(value)
	}
	data, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalGethProvenanceWorkflows(t *testing.T) {
	source := loadGoldenLegacyKV(t)
	before := directoryContentDigest(t, source)
	for _, provenance := range []string{`"arbitrary-fork"`, "omitted", `""`, `null`} {
		t.Run(provenance, func(t *testing.T) {
			for _, compression := range []string{bundle.CompressionNone, bundle.CompressionZstd} {
				t.Run(compression, func(t *testing.T) {
					bundlePath := filepath.Join(t.TempDir(), "bundle")
					exported, err := Export(context.Background(), ExportOptions{SourceChaindata: source, Output: bundlePath, Compression: compression, CacheMB: 16, Handles: 16})
					if err != nil {
						t.Fatal(err)
					}
					assertCurrentBuildProvenance(t, exported.Manifest)
					rewriteGethProvenance(t, filepath.Join(bundlePath, bundle.ManifestFileName), provenance)
					report, err := Verify(context.Background(), VerifyOptions{Bundle: bundlePath, CacheMB: 16, Handles: 16})
					if err != nil {
						t.Fatal(err)
					}
					assertCurrentBuildProvenance(t, report)
					for _, tc := range targetTestCases() {
						t.Run(tc.name(), func(t *testing.T) {
							assertOptionalGethArtifact(t, source, bundlePath, provenance, tc)
						})
					}
				})
			}
		})
	}
	if after := directoryContentDigest(t, source); after != before {
		t.Fatal("source changed")
	}
}

func assertOptionalGethArtifact(t *testing.T, source, bundlePath, provenance string, tc targetTestCase) {
	t.Helper()
	root := t.TempDir()
	imported, err := Import(context.Background(), ImportOptions{Bundle: bundlePath, Output: filepath.Join(root, "import"), Scheme: tc.scheme, DBEngine: tc.engine, StateLayout: tc.layout, CacheMB: 16, Handles: 16})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentBuildProvenance(t, imported.Report)
	rewriteGethProvenance(t, filepath.Join(imported.ArtifactPath, VerificationFileName), provenance)
	before := directoryContentDigest(t, imported.ArtifactPath)
	verified, err := Verify(context.Background(), VerifyOptions{Bundle: bundlePath, Artifact: imported.ArtifactPath, CacheMB: 16, Handles: 16})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentBuildProvenance(t, verified)
	if after := directoryContentDigest(t, imported.ArtifactPath); after != before {
		t.Fatal("verification changed imported artifact")
	}
	direct, err := Migrate(context.Background(), MigrateOptions{SourceChaindata: source, Output: filepath.Join(root, "direct"), Scheme: tc.scheme, DBEngine: tc.engine, StateLayout: tc.layout, CacheMB: 16, Handles: 16, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentBuildProvenance(t, direct.Report)
	rewriteGethProvenance(t, filepath.Join(direct.ArtifactPath, VerificationFileName), provenance)
	before = directoryContentDigest(t, direct.ArtifactPath)
	verifiedDirect, err := VerifyDirect(context.Background(), DirectVerifyOptions{SourceChaindata: source, Artifact: direct.ArtifactPath, CacheMB: 16, Handles: 16})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentBuildProvenance(t, verifiedDirect)
	if after := directoryContentDigest(t, direct.ArtifactPath); after != before {
		t.Fatal("verification changed direct artifact")
	}
	if verified.Counts != verifiedDirect.Counts || verified.RecomputedRoot != verifiedDirect.RecomputedRoot {
		t.Fatal("portable and direct evidence disagree")
	}
}
