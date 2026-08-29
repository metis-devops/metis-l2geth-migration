package migration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestDirectMigrateGoldenLegacyL2GethFixtureBothSchemes(t *testing.T) {
	chaindata := loadGoldenLegacyKV(t)
	before := directoryContentDigest(t, chaindata)
	expectedData, err := os.ReadFile(filepath.Join("testdata", "legacy-l2geth-expected-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		StateRoot      common.Hash `json:"state_root"`
		OVMETHCodeHash common.Hash `json:"ovm_eth_code_hash"`
	}
	if err := json.Unmarshal(expectedData, &expected); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			artifact := filepath.Join(root, "direct-"+scheme)
			migrated, err := Migrate(context.Background(), MigrateOptions{
				SourceChaindata: chaindata,
				Output:          artifact,
				Scheme:          scheme,
				CacheMB:         16,
				Handles:         16,
			})
			if err != nil {
				t.Fatalf("direct migrate golden fixture as %s: %v", scheme, err)
			}
			if migrated.Report.Source.HeadBefore.StateRoot != expected.StateRoot || migrated.Report.RecomputedRoot != expected.StateRoot {
				t.Fatalf("unexpected direct root evidence: %+v", migrated.Report)
			}
			if counts := migrated.Report.Counts; counts.Accounts != 5 || counts.StorageSlots != 9 || counts.CodeReferences != 3 || counts.CodeRecords != 2 {
				t.Fatalf("golden direct state shape mismatch: %+v", counts)
			}
			verified, err := VerifyDirect(context.Background(), DirectVerifyOptions{
				SourceChaindata: chaindata,
				Artifact:        artifact,
				CacheMB:         16,
				Handles:         16,
			})
			if err != nil {
				t.Fatalf("verify direct golden %s artifact: %v", scheme, err)
			}
			if verified.Scheme != scheme || verified.Counts != migrated.Report.Counts ||
				verified.RecomputedRoot != migrated.Report.RecomputedRoot ||
				!sameSourceEvidence(verified.Source, migrated.Report.Source) {
				t.Fatalf("direct verification result mismatch: have %+v want %+v", verified, migrated.Report)
			}
			for _, name := range []string{bundle.ManifestFileName, bundle.RecordsFileRaw, bundle.RecordsFileZstd} {
				if _, err := os.Lstat(filepath.Join(artifact, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("direct artifact unexpectedly contains %s: %v", name, err)
				}
			}
			assertGoldenOVMState(t, artifact, scheme, expected.StateRoot, expected.OVMETHCodeHash)
		})
	}
	after := directoryContentDigest(t, chaindata)
	if before != after {
		t.Fatalf("legacy source content changed during direct migration: before %s after %s", before, after)
	}
}

func TestDirectMigrateMatchesBundleImportBothSchemes(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	exported, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          bundleDir,
		Compression:     "none",
		CacheMB:         16,
		Handles:         16,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			directArtifact := filepath.Join(root, "direct-"+scheme)
			direct, err := Migrate(context.Background(), MigrateOptions{
				SourceChaindata: fixture.chaindata,
				Output:          directArtifact,
				Scheme:          scheme,
				CacheMB:         16,
				Handles:         16,
			})
			if err != nil {
				t.Fatal(err)
			}
			importArtifact := filepath.Join(root, "import-"+scheme)
			imported, err := Import(context.Background(), ImportOptions{
				Bundle:  bundleDir,
				Output:  importArtifact,
				Scheme:  scheme,
				CacheMB: 16,
				Handles: 16,
			})
			if err != nil {
				t.Fatal(err)
			}
			if direct.Report.Source.HeadBefore != exported.Manifest.Source.HeadBefore ||
				direct.Report.Counts != imported.Report.Counts ||
				direct.Report.RecomputedRoot != imported.Report.RecomputedRoot {
				t.Fatalf("direct and bundle paths disagree: direct=%+v imported=%+v", direct.Report, imported.Report)
			}
			assertArtifactState(t, directArtifact, scheme, fixture.root, fixture.accounts)
			if _, err := Verify(context.Background(), VerifyOptions{
				Bundle: bundleDir, Artifact: directArtifact, CacheMB: 16, Handles: 16,
			}); err == nil {
				t.Fatal("direct artifact unexpectedly verified as a bundle import")
			}
			if _, err := VerifyDirect(context.Background(), DirectVerifyOptions{
				SourceChaindata: fixture.chaindata, Artifact: importArtifact, CacheMB: 16, Handles: 16,
			}); err == nil {
				t.Fatal("bundle import unexpectedly verified as a direct artifact")
			}
			newRoot := mutateAndCommitArtifact(t, directArtifact, scheme, fixture)
			if newRoot == fixture.root {
				t.Fatal("direct artifact state mutation did not change the root")
			}
			assertArtifactNonce(t, directArtifact, scheme, newRoot, fixture.accounts[0].address, fixture.accounts[0].nonce+1)
		})
	}
}

func TestDirectMigrateRejectsInvalidSourceAndCleansOutput(t *testing.T) {
	t.Run("missing code", func(t *testing.T) {
		fixture := buildLegacyFixture(t)
		codeHash := crypto.Keccak256Hash(fixture.accounts[1].code)
		db, err := gethleveldb.New(fixture.chaindata, 16, 16, "direct-missing-code", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Delete(codeHash[:]); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "artifact")
		_, err = Migrate(context.Background(), MigrateOptions{
			SourceChaindata: fixture.chaindata, Output: output, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		})
		if err == nil || !strings.Contains(err.Error(), "is missing") {
			t.Fatalf("expected missing-code error, got %v", err)
		}
		assertPathAbsent(t, output)
	})

	t.Run("non-canonical head", func(t *testing.T) {
		fixture := buildLegacyFixture(t)
		kv, err := gethleveldb.New(fixture.chaindata, 16, 16, "direct-noncanonical", false)
		if err != nil {
			t.Fatal(err)
		}
		db := rawdb.NewDatabase(kv)
		rawdb.WriteCanonicalHash(db, common.HexToHash("0xdeadbeef"), fixture.head.BlockNumber)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(t.TempDir(), "artifact")
		_, err = Migrate(context.Background(), MigrateOptions{
			SourceChaindata: fixture.chaindata, Output: output, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		})
		if err == nil || !strings.Contains(err.Error(), "not canonical") {
			t.Fatalf("expected non-canonical-head error, got %v", err)
		}
		assertPathAbsent(t, output)
	})

	t.Run("canceled", func(t *testing.T) {
		fixture := buildLegacyFixture(t)
		parent := t.TempDir()
		output := filepath.Join(parent, "artifact")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Migrate(ctx, MigrateOptions{
			SourceChaindata: fixture.chaindata, Output: output, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
		assertPathAbsent(t, output)
		partials, err := filepath.Glob(filepath.Join(parent, ".artifact.partial-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(partials) != 0 {
			t.Fatalf("partial outputs survived cancellation: %v", partials)
		}
	})

	t.Run("output inside source", func(t *testing.T) {
		fixture := buildLegacyFixture(t)
		output := filepath.Join(fixture.chaindata, "artifact")
		_, err := Migrate(context.Background(), MigrateOptions{
			SourceChaindata: fixture.chaindata, Output: output, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		})
		if err == nil || !strings.Contains(err.Error(), "inside the source") {
			t.Fatalf("expected source/output containment error, got %v", err)
		}
		assertPathAbsent(t, output)
	})

	t.Run("existing output", func(t *testing.T) {
		fixture := buildLegacyFixture(t)
		output := filepath.Join(t.TempDir(), "artifact")
		if err := os.Mkdir(output, 0o701); err != nil {
			t.Fatal(err)
		}
		_, err := Migrate(context.Background(), MigrateOptions{
			SourceChaindata: fixture.chaindata, Output: output, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		})
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected existing-output error, got %v", err)
		}
		info, err := os.Stat(output)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o701 {
			t.Fatalf("existing output mode changed: %o", info.Mode().Perm())
		}
	})
}

func TestLegacySourceStableHeadFenceRejectsChange(t *testing.T) {
	fixture := buildLegacyFixture(t)
	kv, err := gethleveldb.New(fixture.chaindata, 16, 16, "stable-head", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	head, headerRLP, err := readLegacyHead(db)
	if err != nil {
		t.Fatal(err)
	}
	captured := head
	captured.BlockHash = common.HexToHash("0x01")
	source := &legacySource{db: db, head: captured, headerRLP: headerRLP}
	if _, err := source.ConfirmStableAndClose("test"); err == nil || !strings.Contains(err.Error(), "source head changed") {
		t.Fatalf("expected stable-head fence error, got %v", err)
	}
}

func TestVerifyDirectRejectsTamperedReportAndArtifact(t *testing.T) {
	t.Run("report evidence", func(t *testing.T) {
		fixture := buildLegacyFixture(t)
		artifact := filepath.Join(t.TempDir(), "artifact")
		if _, err := Migrate(context.Background(), MigrateOptions{
			SourceChaindata: fixture.chaindata, Output: artifact, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(artifact, VerificationFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		counts := document["counts"].(map[string]any)
		counts["payload_bytes"] = counts["payload_bytes"].(float64) + 1
		data, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = VerifyDirect(context.Background(), DirectVerifyOptions{
			SourceChaindata: fixture.chaindata, Artifact: artifact, CacheMB: 16, Handles: 16,
		})
		if err == nil || !strings.Contains(err.Error(), "does not match the legacy source") {
			t.Fatalf("expected report/source mismatch, got %v", err)
		}
	})

	t.Run("extra target code", func(t *testing.T) {
		fixture := buildLegacyFixture(t)
		artifact := filepath.Join(t.TempDir(), "artifact")
		if _, err := Migrate(context.Background(), MigrateOptions{
			SourceChaindata: fixture.chaindata, Output: artifact, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		}); err != nil {
			t.Fatal(err)
		}
		kv, err := pebble.New(filepath.Join(artifact, "db"), 16, 16, "direct-tamper", false)
		if err != nil {
			t.Fatal(err)
		}
		db := rawdb.NewDatabase(kv)
		code := []byte{0x60, 0xaa}
		hash := crypto.Keccak256Hash(code)
		if err := db.Put(prefixedKey(rawdb.CodePrefix, hash[:]), code); err != nil {
			t.Fatal(err)
		}
		if err := db.SyncKeyValue(); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = VerifyDirect(context.Background(), DirectVerifyOptions{
			SourceChaindata: fixture.chaindata, Artifact: artifact, CacheMB: 16, Handles: 16,
		})
		if err == nil || !strings.Contains(err.Error(), "code inventory mismatch") {
			t.Fatalf("expected target inventory error, got %v", err)
		}
	})
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %s unexpectedly exists: %v", path, err)
	}
}
