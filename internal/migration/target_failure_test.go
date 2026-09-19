package migration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestTargetTamperingRejected(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testTargetTamperingRejected(t, mode) })
	}
}
func testTargetTamperingRejected(t *testing.T, mode TempDBMode) {
	fixture := buildLegacyFixture(t)
	for _, tc := range targetTestCases() {
		for _, damage := range []string{"missing-code", "extra-code", "extra-node", "mixed-code", "engine", "layout", "corrupt-current"} {
			if damage == "corrupt-current" && tc.engine != DBEngineLevelDB {
				continue
			}
			t.Run(tc.name()+"/"+damage, func(t *testing.T) {
				artifact := filepath.Join(t.TempDir(), "artifact")
				result, err := Migrate(context.Background(), MigrateOptions{TempDB: mode, SourceChaindata: fixture.chaindata, Output: artifact, Scheme: tc.scheme, DBEngine: tc.engine, CacheMB: 16, Handles: 16})
				if err != nil {
					t.Fatal(err)
				}
				report := requireDirectReport(t, result)
				dbPath := filepath.Join(artifact, "chaindata")
				switch damage {
				case "engine":
					if tc.engine == DBEngineLevelDB {
						report.DBEngine = DBEnginePebble
					} else {
						report.DBEngine = DBEngineLevelDB
					}
					writeUncheckedDirectReport(t, artifact, report)
				case "layout":
					report.StateLayout = "legacy-l2geth"
					writeUncheckedDirectReport(t, artifact, report)
				case "corrupt-current":
					if err := os.WriteFile(filepath.Join(dbPath, "CURRENT"), []byte("bad manifest\n"), 0600); err != nil {
						t.Fatal(err)
					}
				default:
					kv, err := openTestTargetKV(dbPath, 16, 16, "damage", false)
					if err != nil {
						t.Fatal(err)
					}
					damageTargetKey(t, kv, tc, damage, fixture.accounts[1].code)
					if err := kv.Close(); err != nil {
						t.Fatal(err)
					}
				}
				before := directoryContentDigest(t, artifact)
				if _, err := VerifyDirect(context.Background(), DirectVerifyOptions{TempDB: mode, SourceChaindata: fixture.chaindata, Artifact: artifact, CacheMB: 16, Handles: 16}); err == nil {
					t.Fatal("tampered artifact accepted")
				}
				if after := directoryContentDigest(t, artifact); after != before {
					t.Fatal("verification modified damaged artifact")
				}
			})
		}
	}
}

func writeUncheckedDirectReport(t *testing.T, artifact string, report DirectVerificationReport) {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifact, VerificationFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func damageTargetKey(t *testing.T, db ethdb.KeyValueStore, tc targetTestCase, damage string, originalCode []byte) {
	t.Helper()
	code := originalCode
	if damage == "extra-code" {
		code = []byte{0xc2, 0x20, 0x01}
	}
	hash := crypto.Keccak256Hash(code)
	key := prefixedKey(rawdb.CodePrefix, hash[:])
	var err error
	switch damage {
	case "missing-code":
		err = db.Delete(key)
	case "extra-code":
		err = db.Put(key, code)
	case "extra-node":
		node := []byte{0xc2, 0x20, 0x02}
		h := crypto.Keccak256Hash(node)
		if tc.scheme == "path" {
			err = db.Put([]byte{'A', 15, 15, 15}, node)
		} else {
			err = db.Put(h[:], node)
		}
	case "mixed-code":
		key = hash[:]
		err = db.Put(key, code)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestSourceCodeMatchingTrieNode(t *testing.T) {
	fixture := buildLegacyFixture(t)
	kv, err := openTestTargetKV(fixture.chaindata, 16, 16, "alias", true)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	tdb := triedb.NewDatabase(disk, triedb.HashDefaults)
	sdb, err := state.New(fixture.root, state.NewDatabase(tdb, state.NewCodeDB(disk)))
	if err != nil {
		t.Fatal(err)
	}
	storageRoot := sdb.GetStorageRoot(fixture.accounts[1].address)
	code, err := disk.Get(storageRoot[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := tdb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	fixture = buildLegacyFixtureWithCode(t, code)
	for _, compression := range []string{bundle.CompressionNone, bundle.CompressionZstd} {
		t.Run(compression, func(t *testing.T) {
			root := t.TempDir()
			bundlePath := filepath.Join(root, "bundle")
			_, err := Export(context.Background(), ExportOptions{SourceChaindata: fixture.chaindata, Output: bundlePath, Compression: compression, CacheMB: 16, Handles: 16})
			if err != nil {
				t.Fatal(err)
			}
			direct, err := Migrate(context.Background(), MigrateOptions{SourceChaindata: fixture.chaindata, Output: filepath.Join(root, "direct"), Scheme: "hash", DBEngine: DBEngineLevelDB, CacheMB: 16, Handles: 16})
			if err != nil {
				t.Fatal(err)
			}
			imported, err := Import(context.Background(), ImportOptions{Bundle: bundlePath, Output: filepath.Join(root, "import"), Scheme: "hash", DBEngine: DBEngineLevelDB, CacheMB: 16, Handles: 16})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyDirect(context.Background(), DirectVerifyOptions{SourceChaindata: fixture.chaindata, Artifact: direct.ArtifactPath, CacheMB: 16, Handles: 16}); err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(context.Background(), VerifyOptions{Bundle: bundlePath, Artifact: imported.ArtifactPath, CacheMB: 16, Handles: 16}); err != nil {
				t.Fatal(err)
			}
			assertArtifactState(t, direct.ArtifactPath, "hash", fixture.root, fixture.accounts)
			assertLogicalDatabaseEqual(t, filepath.Join(direct.ArtifactPath, "chaindata"), filepath.Join(imported.ArtifactPath, "chaindata"))
		})
	}
}
