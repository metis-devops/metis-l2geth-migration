package migration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

type targetTestCase struct{ engine, layout, scheme string }

func targetTestCases() []targetTestCase {
	return []targetTestCase{
		{"pebble", "geth", "hash"}, {"pebble", "geth", "path"},
		{"leveldb", "geth", "hash"}, {"leveldb", "geth", "path"},
	}
}

func (c targetTestCase) name() string { return c.engine + "/" + c.layout + "/" + c.scheme }

// Test helpers use disk detection so assertions can compare different engines.
func openTestTargetKV(path string, cache, handles int, namespace string, readonly bool) (ethdb.KeyValueStore, error) {
	if rawdb.PreexistingDatabase(path) == rawdb.DBLeveldb {
		if readonly {
			return readonlydb.Open(path, cache, handles)
		}
		return leveldb.New(path, cache, handles, namespace, false)
	}
	return pebble.New(path, cache, handles, namespace, readonly)
}

func TestTargetMatrixGoldenCanary(t *testing.T) {
	source := loadGoldenLegacyKV(t)
	before := directoryContentDigest(t, source)
	for _, compression := range []string{bundle.CompressionNone, bundle.CompressionZstd} {
		bundlePath := filepath.Join(t.TempDir(), "bundle")
		exported, err := Export(context.Background(), ExportOptions{SourceChaindata: source, Output: bundlePath, Compression: compression, CacheMB: 16, Handles: 16})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range targetTestCases() {
			t.Run(compression+"/"+tc.name(), func(t *testing.T) {
				out := t.TempDir()
				imported, err := Import(context.Background(), ImportOptions{Bundle: bundlePath, Output: filepath.Join(out, "import"), Scheme: tc.scheme, DBEngine: tc.engine, CacheMB: 16, Handles: 16})
				if err != nil {
					t.Fatal(err)
				}
				direct, err := Migrate(context.Background(), MigrateOptions{SourceChaindata: source, Output: filepath.Join(out, "direct"), Scheme: tc.scheme, DBEngine: tc.engine, CacheMB: 16, Handles: 16, Workers: 2})
				if err != nil {
					t.Fatal(err)
				}
				if imported.Report.Counts != direct.Report.Counts || direct.Report.RecomputedRoot != exported.Manifest.Source.HeadBefore.StateRoot || string(direct.Report.StateLayout) != tc.layout {
					t.Fatalf("reports disagree: %+v %+v", imported.Report, direct.Report)
				}
				for _, artifact := range []string{imported.ArtifactPath, direct.ArtifactPath} {
					assertArtifactHeadMetadata(t, artifact, exported.Manifest.Source)
					assertNoTemporaryTrieNodeIndexes(t, artifact)
				}
				if _, err := Verify(context.Background(), VerifyOptions{Bundle: bundlePath, Artifact: imported.ArtifactPath, CacheMB: 16, Handles: 16}); err != nil {
					t.Fatal(err)
				}
				if _, err := VerifyDirect(context.Background(), DirectVerifyOptions{SourceChaindata: source, Artifact: direct.ArtifactPath, CacheMB: 16, Handles: 16}); err != nil {
					t.Fatal(err)
				}
				assertLogicalDatabaseEqual(t, filepath.Join(imported.ArtifactPath, "chaindata"), filepath.Join(direct.ArtifactPath, "chaindata"))
				reference := filepath.Join(out, "reference")
				buildGenerateTrieReference(t, bundlePath, reference, tc.scheme, exported.Manifest.Source)
				assertLogicalDatabaseEqual(t, filepath.Join(direct.ArtifactPath, "chaindata"), reference)
			})
		}
	}
	if after := directoryContentDigest(t, source); after != before {
		t.Fatal("source changed")
	}
}

func TestLevelDBGethContinuation(t *testing.T) {
	fixture := buildLegacyFixture(t)
	for _, scheme := range []string{"hash", "path"} {
		t.Run(scheme, func(t *testing.T) {
			result, err := Migrate(context.Background(), MigrateOptions{SourceChaindata: fixture.chaindata, Output: filepath.Join(t.TempDir(), "artifact"), Scheme: scheme, DBEngine: "leveldb", CacheMB: 16, Handles: 16})
			if err != nil {
				t.Fatal(err)
			}
			assertArtifactState(t, result.ArtifactPath, scheme, fixture.root, fixture.accounts)
			root := mutateAndCommitArtifact(t, result.ArtifactPath, scheme, fixture)
			assertArtifactNonce(t, result.ArtifactPath, scheme, root, fixture.accounts[0].address, fixture.accounts[0].nonce+1)
		})
	}
}

func TestTargetOptionsFailBeforeIO(t *testing.T) {
	for _, tc := range []targetTestCase{{"bogus", "geth", "hash"}, {"pebble-v2", "geth", "hash"}, {"pebble", "geth", "invalid"}} {
		t.Run(tc.name(), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "absent-parent", "artifact")
			_, err := Migrate(context.Background(), MigrateOptions{SourceChaindata: "missing-source", Output: out, Scheme: tc.scheme, DBEngine: tc.engine})
			if err == nil || strings.Contains(err.Error(), "open legacy") {
				t.Fatalf("bad validation: %v", err)
			}
			_, err = Import(context.Background(), ImportOptions{Bundle: "missing-bundle", Output: out, Scheme: tc.scheme, DBEngine: tc.engine})
			if err == nil {
				t.Fatal("invalid import accepted")
			}
			assertPathAbsent(t, filepath.Dir(out))
		})
	}
}

func TestStateLayoutReportStrictness(t *testing.T) {
	for _, direct := range []bool{false, true} {
		var report any = validTestVerificationReport()
		if direct {
			report = validDirectVerificationReport(t)
		}
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		wire["scheme"] = json.RawMessage(`"hash"`)
		wire["db_engine"] = json.RawMessage(`"leveldb"`)
		for _, value := range []string{"omitted", `"geth"`, `"legacy-l2geth"`, `""`, `null`, `"bogus"`, `42`} {
			delete(wire, "state_layout")
			if value != "omitted" {
				wire["state_layout"] = json.RawMessage(value)
			}
			encoded, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if direct {
				var r DirectVerificationReport
				r, err = strictio.DecodeJSON[DirectVerificationReport](encoded, "report")
				if err == nil {
					err = r.Validate()
				}
			} else {
				var r VerificationReport
				r, err = strictio.DecodeJSON[VerificationReport](encoded, "report")
				if err == nil {
					err = r.Validate()
				}
			}
			valid := value == "omitted" || value == `"geth"`
			if (err == nil) != valid {
				t.Fatalf("direct=%v layout=%s: %v", direct, value, err)
			}
		}
	}
}

func TestSyncLevelDBFilesFailureAndCancellation(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"CURRENT", "000001.log", "MANIFEST-000000"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	injected := errors.New("sync failure")
	if err := syncLevelDBFiles(context.Background(), dir, func(string) error { return injected }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := syncLevelDBFiles(ctx, dir, syncFile); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var synced []string
	if err := syncLevelDBFiles(context.Background(), dir, func(path string) error { synced = append(synced, filepath.Base(path)); return syncFile(path) }); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 3 {
		t.Fatal(synced)
	}
	if err := os.Symlink(filepath.Join(dir, "CURRENT"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := syncLevelDBFiles(context.Background(), dir, syncFile); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestOldArtifactReportsDefaultToGethLayout(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	bundlePath := filepath.Join(root, "bundle")
	if _, err := Export(context.Background(), ExportOptions{SourceChaindata: fixture.chaindata, Output: bundlePath, Compression: "none", CacheMB: 16, Handles: 16}); err != nil {
		t.Fatal(err)
	}
	for _, direct := range []bool{false, true} {
		name := "import"
		if direct {
			name = "direct"
		}
		artifact := filepath.Join(root, name)
		if direct {
			if _, err := Migrate(context.Background(), MigrateOptions{SourceChaindata: fixture.chaindata, Output: artifact, Scheme: "hash", CacheMB: 16, Handles: 16}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := Import(context.Background(), ImportOptions{Bundle: bundlePath, Output: artifact, Scheme: "hash", CacheMB: 16, Handles: 16}); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(artifact, VerificationFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		delete(wire, "state_layout")
		data, err = json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		before := directoryContentDigest(t, artifact)
		if direct {
			report, err := VerifyDirect(context.Background(), DirectVerifyOptions{SourceChaindata: fixture.chaindata, Artifact: artifact, CacheMB: 16, Handles: 16})
			if err != nil || report.StateLayout != LayoutGeth {
				t.Fatalf("old direct report: %+v %v", report, err)
			}
		} else {
			report, err := Verify(context.Background(), VerifyOptions{Bundle: bundlePath, Artifact: artifact, CacheMB: 16, Handles: 16})
			if err != nil || report.StateLayout != LayoutGeth {
				t.Fatalf("old portable report: %+v %v", report, err)
			}
		}
		if after := directoryContentDigest(t, artifact); after != before {
			t.Fatal("old report rewritten during verification")
		}

		for _, layout := range []string{`"legacy-l2geth"`, `""`, `null`, `"unknown"`} {
			wire["state_layout"] = json.RawMessage(layout)
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			before := directoryContentDigest(t, artifact)
			if direct {
				_, err = VerifyDirect(context.Background(), DirectVerifyOptions{SourceChaindata: fixture.chaindata, Artifact: artifact, CacheMB: 16, Handles: 16})
			} else {
				_, err = Verify(context.Background(), VerifyOptions{Bundle: bundlePath, Artifact: artifact, CacheMB: 16, Handles: 16})
			}
			if err == nil || !strings.Contains(err.Error(), "state layout") {
				t.Fatalf("direct=%v layout=%s: %v", direct, layout, err)
			}
			if directoryContentDigest(t, artifact) != before {
				t.Fatal("rejected artifact was modified")
			}
		}
	}
}
