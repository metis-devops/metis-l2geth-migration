package migration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

func TestPruneRejectsManifestFallbackWithoutRepair(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		for _, scenario := range []string{"corrupt-current", "missing-current", "missing-manifest", "wrong-file-type", "oversized-current", "pending-current"} {
			t.Run(fmt.Sprintf("%s/dry=%t", scenario, dryRun), func(t *testing.T) {
				opts, _ := pruneTestOptions(t)
				opts.DryRun = dryRun
				currentPath := filepath.Join(opts.Chaindata, "CURRENT")
				current, err := os.ReadFile(currentPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(opts.Chaindata, "CURRENT.bak"), current, 0600); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "corrupt-current":
					err = os.WriteFile(currentPath, []byte("corrupt CURRENT\n"), 0600)
				case "missing-current":
					err = os.Remove(currentPath)
				case "missing-manifest":
					err = os.WriteFile(currentPath, []byte("MANIFEST-9223372036854775807\n"), 0600)
				case "wrong-file-type":
					err = os.WriteFile(currentPath, []byte("000001.log\n"), 0600)
				case "oversized-current":
					err = os.WriteFile(currentPath, bytes.Repeat([]byte{'x'}, 4096), 0600)
				case "pending-current":
					err = os.WriteFile(filepath.Join(opts.Chaindata, "CURRENT.9223372036854775807"), current, 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				before := pruneMetadataFiles(t, opts.Chaindata)
				result, err := Prune(context.Background(), opts)
				if err == nil || result.Verified {
					t.Fatalf("accepted damaged/pending metadata: %+v %v", result, err)
				}
				if strings.Contains(err.Error(), "deletion_started=true") {
					t.Fatalf("started deletion with invalid metadata: %v", err)
				}
				after := pruneMetadataFiles(t, opts.Chaindata)
				if len(before) != len(after) {
					t.Fatal("prune created or removed metadata files")
				}
				for name, data := range before {
					if !bytes.Equal(after[name], data) {
						t.Fatalf("prune repaired/changed %s", name)
					}
				}
				assertNoPruneTemps(t, filepath.Dir(opts.Chaindata))
			})
		}
	}
}

// Logs are diagnostic rather than logical database state. No manifest,
// CURRENT, table or journal may change on these rejected source opens.
func pruneMetadataFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string][]byte)
	for _, entry := range entries {
		if entry.Name() == "LOG" || entry.Name() == "LOG.old" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = data
	}
	return result
}

func TestPruneDoesNotMistakeHashIndexesForLES(t *testing.T) {
	opts, _ := pruneTestOptions(t)
	mutatePruneFixture(t, opts.Chaindata, func(db ethdb.Database) {
		for _, marker := range []string{"nb:", "pb:"} {
			var hash common.Hash
			copy(hash[1:4], marker)
			rawdb.WriteHeaderNumber(db, hash, 99)
			if err := db.Put(append([]byte{'l'}, hash[:]...), []byte{99}); err != nil {
				t.Fatal(err)
			}
		}
	})
	before := pruneLogicalKV(t, opts.Chaindata)
	for _, dryRun := range []bool{true, false} {
		opts.DryRun = dryRun
		result, err := Prune(context.Background(), opts)
		if err != nil || !result.Verified {
			t.Fatalf("ordinary index keys rejected: %+v %v", result, err)
		}
	}
	after := pruneLogicalKV(t, opts.Chaindata)
	for key, value := range before {
		if len(key) == 33 && (key[0] == 'H' || key[0] == 'l') && !bytes.Equal(after[key], value) {
			t.Fatalf("ordinary index key changed: %x", key)
		}
	}
}

func TestPruneLESBalanceKeyShapes(t *testing.T) {
	for _, version := range []byte{0, 1} {
		for _, tc := range []struct {
			marker string
			id     []byte
		}{
			{"pb:", make([]byte, 32)},
			{"nb:", []byte("192.0.2.1")},
			{"nb:", []byte("2001:db8:12:5678:abcd:1:2:3")},
			{"nb:", []byte(strings.Repeat("ab", 32))},
		} {
			key := append(append([]byte{0, version}, tc.marker...), tc.id...)
			if err := rejectPruneForeignKey(key, []byte{0xc0}); err == nil {
				t.Fatalf("LES key accepted: %x", key)
			}
		}
	}
	for _, key := range [][]byte{
		append([]byte{0, 1, 'p', 'b', ':'}, make([]byte, 31)...),
		append([]byte{0, 1, 'n', 'b', ':'}, []byte("not-an-ip-or-node-id")...),
		append([]byte{'H', 0, 'p', 'b', ':'}, make([]byte, 28)...),
		append([]byte{'l', 0, 'n', 'b', ':'}, make([]byte, 28)...),
	} {
		if isPruneLESBalanceKey(key) {
			t.Fatalf("unrelated/malformed key classified as LES: %x", key)
		}
	}
}

func TestPruneAcceptsValidCurrentWithUnusedBackup(t *testing.T) {
	opts, _ := pruneTestOptions(t)
	backup := filepath.Join(opts.Chaindata, "CURRENT.bak")
	if err := os.WriteFile(backup, []byte("unused stale backup\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.DryRun = true
	before := directoryContentDigest(t, opts.Chaindata)
	result, err := Prune(context.Background(), opts)
	if err != nil || !result.Verified {
		t.Fatalf("valid CURRENT rejected: %+v %v", result, err)
	}
	if directoryContentDigest(t, opts.Chaindata) != before {
		t.Fatal("dry run touched valid CURRENT or unused backup")
	}
}
