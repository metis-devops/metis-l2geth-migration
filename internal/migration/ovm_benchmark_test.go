package migration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
)

func BenchmarkOVMMigration(b *testing.B) {
	benchmarkOVMMigration(b, false)
}

func BenchmarkOVMMigrationAlloc(b *testing.B) {
	benchmarkOVMMigration(b, true)
}

func benchmarkOVMMigration(b *testing.B, withAlloc bool) {
	count, mode := ovmBenchmarkSettings(b)
	for _, workers := range []int{2, 8} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			setup := time.Now()
			f := newOVMFixtureSized(b, count, nil)
			var alloc string
			if withAlloc {
				alloc = writeOVMBenchmarkAlloc(b, f)
			}
			root := b.TempDir()
			setupElapsed := time.Since(setup)
			ctx, stop := sampleTemporaryBenchmark(b, b.Context(), root)
			b.ReportAllocs()
			b.ResetTimer()
			for n := range b.N {
				opts := MigrateOptions{TempDB: mode, SourceChaindata: f.source, Output: filepath.Join(root, fmt.Sprint(n)), Scheme: "hash", DBEngine: "pebble", CacheMB: 128, Handles: 128, Workers: workers, OVM: OVMOptions{Enabled: true, WrappedEtherCode: f.code, StateWitness: f.witness, GenesisAlloc: alloc}}
				if _, err := Migrate(ctx, opts); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			stop()
			b.ReportMetric(setupElapsed.Seconds(), "setup-s")
		})
	}
}

// Only explicit maintenance invocations write a candidate outside the corpus.
func TestWriteOVMFixtureCandidate(t *testing.T) {
	out := os.Getenv("L2STATE_OVM_FIXTURE_OUT")
	if out == "" {
		t.Skip("set L2STATE_OVM_FIXTURE_OUT to a new absolute candidate directory")
	}
	if !filepath.IsAbs(out) {
		t.Fatal("candidate path must be absolute")
	}
	corpus, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectOutputInsideDirectory(corpus, out, "candidate must be outside testdata", "candidate aliases testdata"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	f := newOVMFixture(t, nil)
	kv, err := readonlydb.Open(f.source, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	entries := make(map[string]string)
	it := db.NewIterator(nil, nil)
	for it.Next() {
		entries[hexutil.Encode(it.Key())] = hexutil.Encode(it.Value())
	}
	err = it.Error()
	it.Release()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "source.json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	witness, err := os.ReadFile(f.witness)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "witness.jsonl"), witness, 0600); err != nil {
		t.Fatal(err)
	}
}
