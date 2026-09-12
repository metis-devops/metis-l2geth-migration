package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
	"github.com/syndtr/goleveldb/leveldb"
)

func pruneTestOptions(t *testing.T) (PruneOptions, legacyFixture) {
	t.Helper()
	f := buildLegacyFixture(t)
	path, err := filepath.EvalSymlinks(f.chaindata)
	if err != nil {
		t.Fatal(err)
	}
	f.chaindata = path
	mutatePruneFixture(t, path, func(db ethdb.Database) {
		genesis := &types.Header{Number: big.NewInt(0), Root: f.root, Difficulty: big.NewInt(1)}
		rawdb.WriteHeader(db, genesis)
		rawdb.WriteCanonicalHash(db, genesis.Hash(), 0)
		for i := range 2100 {
			value := fmt.Appendf(nil, "old-state-%d", i)
			key := crypto.Keccak256(value)
			if err := db.Put(key, value); err != nil {
				t.Fatal(err)
			}
		}
		for _, key := range []string{"LastIndex", "LastBatch", "LastQueueIndex", "LastVerifiedIndex", "LastFast", "DatabaseVersion", "secure-key-unchanged"} {
			if err := db.Put([]byte(key), []byte("keep")); err != nil {
				t.Fatal(err)
			}
		}
		unknown := bytes.Repeat([]byte{0xee}, 32)
		if err := db.Put(unknown, []byte("unknown non-state")); err != nil {
			t.Fatal(err)
		}
	})
	return PruneOptions{Chaindata: path, CacheMB: 32, Handles: 32}, f
}

func mutatePruneFixture(t *testing.T, path string, fn func(ethdb.Database)) {
	t.Helper()
	kv, err := gethleveldb.New(path, 16, 16, "", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	fn(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func pruneLogicalKV(t *testing.T, path string) map[string][]byte {
	t.Helper()
	db, err := readonlydb.Open(path, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	out := map[string][]byte{}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		out[string(it.Key())] = bytes.Clone(it.Value())
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPruneDifferenceAndIdempotence(t *testing.T) {
	o, f := pruneTestOptions(t)
	ancient := filepath.Join(o.Chaindata, "ancient")
	if err := os.Mkdir(ancient, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancient, "hashes.rdat"), []byte("untouched ancient"), 0600); err != nil {
		t.Fatal(err)
	}
	before := pruneLogicalKV(t, o.Chaindata)
	for _, compact := range []bool{false, true} {
		o.Compact = compact
		r, err := Prune(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		wantDeleted := uint64(2100)
		if compact {
			wantDeleted = 0
		}
		if !r.Verified || r.Head != f.head || r.Deleted.Keys != wantDeleted || r.Compacted != compact || r.Unknown.Keys != 1 {
			t.Fatalf("unexpected result %+v", r)
		}
	}
	after := pruneLogicalKV(t, o.Chaindata)
	for key, value := range before {
		deleted := strings.HasPrefix(string(value), "old-state-")
		got, exists := after[key]
		if deleted && exists {
			t.Fatalf("old key retained %x", key)
		}
		if !deleted && (!exists || !bytes.Equal(got, value)) {
			t.Fatalf("protected key changed %x", key)
		}
	}
	data, err := os.ReadFile(filepath.Join(ancient, "hashes.rdat"))
	if err != nil || string(data) != "untouched ancient" {
		t.Fatalf("ancient changed %s %v", data, err)
	}
	assertNoPruneTemps(t, filepath.Dir(o.Chaindata))
}

func TestPruneDryRunIsPhysicallyReadOnly(t *testing.T) {
	o, _ := pruneTestOptions(t)
	before := directoryContentDigest(t, o.Chaindata)
	o.DryRun = true
	r, err := Prune(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Candidates.Keys != 2100 || r.Deleted.Keys != 0 || !r.Verified {
		t.Fatalf("bad dry run %+v", r)
	}
	if before != directoryContentDigest(t, o.Chaindata) {
		t.Fatal("dry run modified database files")
	}
	assertNoPruneTemps(t, filepath.Dir(o.Chaindata))
}

func TestPruneRejectsBeforeDeleting(t *testing.T) {
	for _, scenario := range []string{"les", "geth-code", "path", "path32", "missing-code", "bad-node", "missing-genesis", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			o, f := pruneTestOptions(t)
			mutatePruneFixture(t, o.Chaindata, func(db ethdb.Database) {
				var err error
				switch scenario {
				case "les":
					err = db.Put(append([]byte{0, 1, 'n', 'b', ':'}, []byte("2001:db8:12:5678:abcd:1:2:3")...), []byte{0xc1, 1})
				case "geth-code":
					value := []byte("foreign code")
					err = db.Put(append([]byte{'c'}, crypto.Keccak256(value)...), value)
				case "path":
					err = db.Put([]byte{'A'}, []byte{0xc0})
				case "path32":
					err = db.Put(append([]byte{'A'}, make([]byte, 31)...), []byte{0xc0})
				case "missing-code":
					err = db.Delete(crypto.Keccak256(f.accounts[1].code))
				case "bad-node":
					err = db.Put(f.root[:], []byte{0xc0})
				case "missing-genesis":
					rawdb.DeleteCanonicalHash(db, 0)
				}
				if err != nil {
					t.Fatal(err)
				}
			})
			if scenario == "symlink" {
				if err := os.Symlink("CURRENT", filepath.Join(o.Chaindata, "alias")); err != nil {
					t.Fatal(err)
				}
			}
			before := pruneLogicalKV(t, o.Chaindata)
			if _, err := Prune(context.Background(), o); err == nil {
				t.Fatal("invalid source accepted")
			}
			after := pruneLogicalKV(t, o.Chaindata)
			if len(after) != len(before) {
				t.Fatal("preflight deleted data")
			}
			for k, v := range before {
				if !bytes.Equal(after[k], v) {
					t.Fatalf("preflight changed key %x", k)
				}
			}
		})
	}
}

func TestPruneInterruptedDeletionCanRestart(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRun), func(t *testing.T) {
			o, _ := pruneTestOptions(t)
			mutatePruneFixture(t, o.Chaindata, func(db ethdb.Database) {
				batch := db.NewBatch()
				defer batch.Close()
				for i := range 2 * pruneDeleteBatchKeys {
					value := fmt.Appendf(nil, "additional-old-state-%d", i)
					if err := batch.Put(crypto.Keccak256(value), value); err != nil {
						t.Fatal(err)
					}
				}
				if err := batch.Write(); err != nil {
					t.Fatal(err)
				}
			})
			hooks := defaultPruneHooks()
			normal := hooks.writeBatch
			calls := 0
			committed := uint64(0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := errors.New("injected batch failure")
			hooks.writeBatch = func(db *leveldb.DB, b *leveldb.Batch) error {
				calls++
				if calls == 2 && !cancelRun {
					return injected
				}
				err := normal(db, b)
				if err == nil {
					committed += uint64(b.Len())
				}
				if cancelRun {
					cancel()
				}
				return err
			}
			_, err := pruneWithHooks(ctx, o, hooks)
			expected := injected
			if cancelRun {
				expected = context.Canceled
			}
			if !errors.Is(err, expected) || !strings.Contains(err.Error(), "deletion_started=true") {
				t.Fatalf("bad failure %v", err)
			}
			r, err := Prune(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if r.Deleted.Keys != 2100+2*pruneDeleteBatchKeys-committed {
				t.Fatalf("unexpected remainder %+v", r)
			}
		})
	}
}

func TestPruneSyncAndCompactionFailures(t *testing.T) {
	for _, scenario := range []string{"sync", "compact", "protected-change"} {
		t.Run(scenario, func(t *testing.T) {
			o, _ := pruneTestOptions(t)
			hooks := defaultPruneHooks()
			injected := errors.New("injected failure")
			switch scenario {
			case "sync":
				hooks.syncFiles = func(context.Context, string) error { return injected }
			case "compact":
				o.Compact = true
				hooks.compact = func(*leveldb.DB) error { return injected }
			case "protected-change":
				normal := hooks.writeBatch
				hooks.writeBatch = func(db *leveldb.DB, b *leveldb.Batch) error {
					b.Put([]byte("LastIndex"), []byte("changed"))
					return normal(db, b)
				}
			}
			r, err := pruneWithHooks(context.Background(), o, hooks)
			if err == nil || r.Verified {
				t.Fatalf("reported success %+v %v", r, err)
			}
			if scenario != "protected-change" && !errors.Is(err, injected) {
				t.Fatalf("lost error %v", err)
			}
		})
	}
}

func TestPruneLockSurvivesReopen(t *testing.T) {
	o, _ := pruneTestOptions(t)
	p, err := openPruneDatabase(o.Chaindata, 16, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.close(); err != nil {
			t.Error(err)
		}
	}()
	for _, readOnly := range []bool{false, true} {
		if err := p.reopen(readOnly); err != nil {
			t.Fatal(err)
		}
		db, err := leveldb.OpenFile(o.Chaindata, nil)
		if err == nil {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
			t.Fatal("prune released file lock")
		}
	}
}

func TestPruneValidationAndCancellation(t *testing.T) {
	o, _ := pruneTestOptions(t)
	for _, change := range []func(*PruneOptions){func(o *PruneOptions) { o.Compact = true; o.DryRun = true }, func(o *PruneOptions) { o.TempDir = o.Chaindata }, func(o *PruneOptions) { o.CacheMB = 1 }, func(o *PruneOptions) { o.Handles = 1 }, func(o *PruneOptions) { o.Chaindata = "" }} {
		invalid := o
		change(&invalid)
		if _, err := Prune(context.Background(), invalid); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prune(ctx, o); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func assertNoPruneTemps(t *testing.T, parent string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(parent, ".l2state-prune-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("prune temporary databases left behind %v", matches)
	}
}

type pruneStorageRootVisitor struct{ root common.Hash }

func (v *pruneStorageRootVisitor) Account(_ common.Hash, a *types.StateAccount, _ []byte) error {
	if a.Root != types.EmptyRootHash {
		v.root = a.Root
	}
	return nil
}
func (*pruneStorageRootVisitor) Storage(common.Hash, common.Hash, []byte) error { return nil }
func (*pruneStorageRootVisitor) Code(common.Hash, common.Hash, []byte) error    { return nil }

func TestPruneSharedPhysicalCodeAndTrieNode(t *testing.T) {
	original := buildLegacyFixture(t)
	source, err := openLegacySource(original.chaindata, 16, 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	visitor := new(pruneStorageRootVisitor)
	if _, err := source.Traverse(context.Background(), visitor); err != nil {
		t.Fatal(err)
	}
	blob, err := source.db.Get(visitor.root[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	// Both fixtures use identical storage. This code is also a reachable storage
	// trie node, so deleting by code-vs-RLP heuristics would destroy live state.
	f := buildLegacyFixtureWithCode(t, blob)
	path, err := filepath.EvalSymlinks(f.chaindata)
	if err != nil {
		t.Fatal(err)
	}
	mutatePruneFixture(t, path, func(db ethdb.Database) {
		h := &types.Header{Number: big.NewInt(0), Root: types.EmptyRootHash, Difficulty: big.NewInt(1)}
		rawdb.WriteHeader(db, h)
		rawdb.WriteCanonicalHash(db, h.Hash(), 0)
	})
	r, err := Prune(context.Background(), PruneOptions{Chaindata: path, CacheMB: 32, Handles: 32})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Verified || r.StateCounts.CodeReferences != 2 || r.StateCounts.CodeRecords != 1 {
		t.Fatalf("bad shared-code counts %+v", r)
	}
	if !bytes.Equal(pruneLogicalKV(t, path)[string(visitor.root[:])], blob) {
		t.Fatal("shared physical code/node entry removed")
	}
}

func TestPruneEmptyStateAndGenesis(t *testing.T) {
	for _, emptyHead := range []bool{false, true} {
		for _, emptyGenesis := range []bool{false, true} {
			t.Run(fmt.Sprintf("head=%t/genesis=%t", emptyHead, emptyGenesis), func(t *testing.T) {
				o, f := pruneTestOptions(t)
				mutatePruneFixture(t, o.Chaindata, func(db ethdb.Database) {
					if emptyGenesis {
						h := &types.Header{Number: big.NewInt(0), Root: types.EmptyRootHash, Difficulty: big.NewInt(1)}
						rawdb.WriteHeader(db, h)
						rawdb.WriteCanonicalHash(db, h.Hash(), 0)
					}
					if emptyHead {
						h := &types.Header{Number: big.NewInt(12346), Root: types.EmptyRootHash, Difficulty: big.NewInt(1), ParentHash: f.head.BlockHash}
						rawdb.WriteHeader(db, h)
						rawdb.WriteCanonicalHash(db, h.Hash(), 12346)
						rawdb.WriteHeadBlockHash(db, h.Hash())
					}
				})
				r, err := Prune(context.Background(), o)
				if err != nil {
					t.Fatal(err)
				}
				if emptyHead && (r.StateCounts.Accounts != 0 || r.Head.StateRoot != types.EmptyRootHash) {
					t.Fatalf("bad empty-state result %+v", r)
				}
				if emptyGenesis && r.GenesisRoot != types.EmptyRootHash {
					t.Fatal("genesis root changed")
				}
			})
		}
	}
}

func TestPruneCompactionCancellation(t *testing.T) {
	o, _ := pruneTestOptions(t)
	o.Compact = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks := defaultPruneHooks()
	called := false
	hooks.compact = func(db *leveldb.DB) error {
		called = true
		cancel()
		_, err := db.Get([]byte("LastBlock"), nil)
		return err
	}
	r, err := pruneWithHooks(ctx, o, hooks)
	if !called || !errors.Is(err, context.Canceled) || r.Verified {
		t.Fatalf("bad compaction cancellation %+v %v", r, err)
	}
	o.Compact = false
	if _, err := Prune(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}
