package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func writeCompatDatabase(t *testing.T, path string, entries []compatKV, target targetConfig) {
	t.Helper()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	kv, err := target.open(path, 16, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	for _, entry := range entries {
		if err := db.Put(entry.Key, entry.Value); err != nil {
			closeErr := db.Close()
			t.Fatalf("restore baseline key %x: %v (close: %v)", entry.Key, err, closeErr)
		}
	}
	syncErr := db.SyncKeyValue()
	closeErr := db.Close()
	if syncErr != nil || closeErr != nil {
		t.Fatalf("close restored baseline: %v %v", syncErr, closeErr)
	}
	if target.engine == DBEngineLevelDB {
		if err := syncLevelDBFiles(t.Context(), path, syncFile); err != nil {
			t.Fatal(err)
		}
	}
}

func captureCompatContinuation(t *testing.T, entries []compatKV, tc targetTestCase, root common.Hash) json.RawMessage {
	t.Helper()
	target, err := targetOptions(tc.engine, tc.scheme)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy")
	writeCompatDatabase(t, path, entries, target)
	db := openCompatStateDatabase(t, path)
	tdb := triedb.NewDatabase(db, trieConfig(tc.scheme, false))
	sdb, err := state.New(root, state.NewDatabase(tdb, state.NewCodeDB(db)))
	if err != nil {
		closeCompatState(t, tdb, db)
		t.Fatal(err)
	}
	contract := common.HexToAddress("0x2000000000000000000000000000000000000002")
	shared := common.HexToAddress("0x3000000000000000000000000000000000000003")
	slot := common.HexToHash("0x01")
	before := compatStateValues(sdb, contract, shared, slot)
	if err := sdb.Error(); err != nil {
		closeCompatState(t, tdb, db)
		t.Fatal(err)
	}
	sdb.SetNonce(contract, 8, tracing.NonceChangeUnspecified)
	sdb.SetBalance(contract, uint256.NewInt(1001), tracing.BalanceChangeUnspecified)
	sdb.SetState(contract, slot, common.HexToHash("0xbeef"))
	sdb.SetCode(contract, common.FromHex("0x6003600055"), tracing.CodeChangeUnspecified)
	newRoot, err := sdb.Commit(12346, false, true)
	if err == nil {
		err = tdb.Commit(newRoot, false)
	}
	closeCompatState(t, tdb, db)
	if err != nil {
		t.Fatal(err)
	}
	db = openCompatStateDatabase(t, path)
	tdb = triedb.NewDatabase(db, trieConfig(tc.scheme, true))
	defer closeCompatState(t, tdb, db)
	sdb, err = state.New(newRoot, state.NewDatabase(tdb, state.NewCodeDB(db)))
	if err != nil {
		t.Fatal(err)
	}
	after := compatStateValues(sdb, contract, shared, slot)
	if err := sdb.Error(); err != nil {
		t.Fatal(err)
	}
	if sdb.GetNonce(contract) != 8 || sdb.GetBalance(contract).Cmp(uint256.NewInt(1001)) != 0 || sdb.GetState(contract, slot) != common.HexToHash("0xbeef") || !bytes.Equal(sdb.GetCode(contract), common.FromHex("0x6003600055")) {
		t.Fatal("geth continuation lost committed state")
	}
	return compatJSON(t, map[string]any{"root": newRoot, "before": before, "after": after})
}
func compatStateValues(sdb *state.StateDB, contract, shared common.Address, slot common.Hash) map[string]any {
	return map[string]any{"nonce": sdb.GetNonce(contract), "balance": sdb.GetBalance(contract).String(), "storage": sdb.GetState(contract, slot), "code": hexutil.Bytes(sdb.GetCode(contract)), "shared_code": hexutil.Bytes(sdb.GetCode(shared))}
}
func openCompatStateDatabase(t *testing.T, path string) ethdb.Database {
	t.Helper()
	kv, err := openTestTargetKV(path, 16, 16, "compat-continuation", false)
	if err != nil {
		t.Fatal(err)
	}
	return rawdb.NewDatabase(kv)
}
func closeCompatState(t *testing.T, tdb *triedb.Database, db ethdb.Database) {
	t.Helper()
	trieErr := tdb.Close()
	syncErr := db.SyncKeyValue()
	closeErr := db.Close()
	if trieErr != nil || syncErr != nil || closeErr != nil {
		t.Errorf("close continuation: %v %v %v", trieErr, syncErr, closeErr)
	}
}

func replayGethCompatibility(t *testing.T, expected *compatCapture) {
	t.Helper()
	bundles := make(map[string]string)
	manifests := make(map[string]bundle.Manifest)
	for name, data := range expected.Contracts["bundle"].Cases {
		if strings.HasPrefix(name, "codec/") || name == "constants" {
			continue
		}
		var frozen compatBundle
		if err := json.Unmarshal(data, &frozen); err != nil {
			t.Fatal(err)
		}
		var manifest bundle.Manifest
		if err := json.Unmarshal(frozen.Manifest, &manifest); err != nil {
			t.Fatal(err)
		}
		if err := manifest.Validate(); err != nil {
			t.Fatalf("frozen bundle %s: %v", name, err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, bundle.ManifestFileName), frozen.Manifest, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, manifest.StateFile.Name), frozen.Records, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(t.Context(), VerifyOptions{Bundle: dir, CacheMB: 16, Handles: 16}); err != nil {
			t.Fatalf("read frozen bundle %s: %v", name, err)
		}
		bundles[name], manifests[name] = dir, manifest
	}
	for _, kind := range []string{"verification", "direct"} {
		contract := expected.Contracts[kind]
		for name, data := range contract.Cases {
			if strings.HasSuffix(name, "/bundle") {
				continue
			}
			t.Run("replay/"+kind+"/"+name, func(t *testing.T) {
				var frozen compatArtifact
				if err := json.Unmarshal(data, &frozen); err != nil {
					t.Fatal(err)
				}
				source, expectedState, target, scheme, err := compatExpectedState(frozen.Report, kind == "direct")
				if err != nil {
					t.Fatal(err)
				}
				parts := strings.Split(name, "/")
				if kind == "verification" {
					bundleName := strings.Join(parts[:2], "/")
					source = manifests[bundleName].Source
					replayed, err := Import(t.Context(), ImportOptions{Bundle: bundles[bundleName], Output: filepath.Join(t.TempDir(), "import"), Scheme: scheme, DBEngine: target.engine, CacheMB: 16, Handles: 16})
					if err != nil {
						t.Fatal(err)
					}
					actual := &compatContract{Databases: make(map[string][]compatKV)}
					id := actual.database(t, filepath.Join(replayed.ArtifactPath, "chaindata"))
					if id != frozen.Database {
						t.Fatalf("frozen bundle import %s inventory differs: expected %s actual %s", name, frozen.Database, id)
					}
				}
				dbPath := filepath.Join(t.TempDir(), "chaindata")
				entries := contract.Databases[frozen.Database]
				writeCompatDatabase(t, dbPath, entries, target)
				if _, err := verifyTargetDatabase(t.Context(), dbPath, scheme, target, source, expectedState, 16, 16, nil, trieNodeIndexOptions{}); err != nil {
					t.Fatalf("read frozen logical database %s: %v", name, err)
				}
				if len(frozen.Continuation) != 0 {
					got := captureCompatContinuation(t, entries, targetTestCase{target.engine, string(target.layout), scheme}, expectedState.Root)
					var left, right any
					if err := json.Unmarshal(frozen.Continuation, &left); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(got, &right); err != nil {
						t.Fatal(err)
					}
					if err := diffCompatJSON(left, right, fmt.Sprintf("%s/%s/continuation", kind, name)); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
