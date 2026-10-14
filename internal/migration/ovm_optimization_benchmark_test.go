package migration

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/golang/snappy"
)

func BenchmarkOVMVerification(b *testing.B) { benchmarkOVMVerification(b, false, false, false, false) }
func BenchmarkOVMVerificationAlloc(b *testing.B) {
	benchmarkOVMVerification(b, true, false, false, false)
}

func benchmarkOVMVerification(b *testing.B, withAlloc, withRetention, withAudit, historical bool) {
	count, mode := ovmBenchmarkSettings(b)
	for _, workers := range []int{2, 8} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			setup := time.Now()
			f, auditPath := ovmBenchmarkAuditFixture(b, count, withRetention, withAudit, historical)
			opts := MigrateOptions{SourceChaindata: f.source, Output: filepath.Join(b.TempDir(), "artifact"), Scheme: "hash", DBEngine: DBEnginePebble, CacheMB: 128, Handles: 128, Workers: workers, OVM: OVMOptions{Enabled: true, WrappedEtherCode: f.code, StateWitness: f.witness}}
			opts.OVM.AuditSummary = auditPath
			if withRetention {
				opts.OVM.ERC20RetainList = writeOVMBenchmarkRetainList(b, f)
			}
			if withAlloc {
				opts.OVM.GenesisAlloc = writeOVMBenchmarkAlloc(b, f)
			}
			if _, err := Migrate(b.Context(), opts); err != nil {
				b.Fatal(err)
			}
			setupElapsed := time.Since(setup)
			ctx, stop := sampleTemporaryBenchmark(b, b.Context(), filepath.Dir(opts.Output))
			stopProfile := profileOVMBenchmark(b)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := VerifyOVM(ctx, OVMVerifyOptions{TempDir: filepath.Dir(opts.Output), TempDB: mode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 128, Handles: 128, Workers: workers, OVM: opts.OVM}); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			stopProfile()
			stop()
			b.ReportMetric(setupElapsed.Seconds(), "setup-s")
		})
	}
}

func writeOVMBenchmarkAlloc(b *testing.B, f ovmFixture) string {
	var data strings.Builder
	data.WriteByte('{')
	for n, address := range f.holders[:1000] {
		if n > 0 {
			data.WriteByte(',')
		}
		fmt.Fprintf(&data, `"%s":{"code":"0x6001600055","balance":"0x0a","storage":{"01":"01"}}`, address)
	}
	data.WriteByte('}')
	return writeAllocFile(b, data.String())
}

// A sequential freezer read microbenchmark, separate from the two-block
// end-to-end fixture. Setup is excluded; records are synthetic 32-byte words.
func BenchmarkOVMAncientRead(b *testing.B) {
	const count = 20000
	path := writeOVMAncientReadFixture(b, count, 5000)
	b.ReportAllocs()

	for b.Loop() {
		ancient, err := openLegacyAncient(path, true)
		if err != nil {
			b.Fatal(err)
		}
		for number := range uint64(count) {
			for table := range 3 {
				if _, err := ancient.read(table, number); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := ancient.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(count*3, "records/op")
}

func writeOVMAncientReadFixture(t testing.TB, count, perFile int) string {
	t.Helper()
	path := t.TempDir()
	for table, name := range []string{"hashes", "headers", "receipts"} {
		index := make([]byte, 6*(count+1))
		indexExt, dataExt := ".ridx", ".rdat"
		if table != 0 {
			indexExt, dataExt = ".cidx", ".cdat"
		}
		var data []byte
		for n := range count {
			word := common.Hash{}
			binary.BigEndian.PutUint64(word[24:], uint64(n+1))
			blob := word[:]
			if table != 0 {
				blob = snappy.Encode(nil, blob)
			}
			data = append(data, blob...)
			binary.BigEndian.PutUint16(index[(n+1)*6:], uint16(n/perFile))
			binary.BigEndian.PutUint32(index[(n+1)*6+2:], uint32(len(data)))
			if (n+1)%perFile == 0 || n == count-1 {
				if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("%s.%04d%s", name, n/perFile, dataExt)), data, 0600); err != nil {
					t.Fatal(err)
				}
				data = data[:0]
			}
		}
		if err := os.WriteFile(filepath.Join(path, name+indexExt), index, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func BenchmarkOVMVerificationRetain(b *testing.B) {
	benchmarkOVMVerification(b, false, true, false, false)
}
func BenchmarkOVMVerificationAllocRetain(b *testing.B) {
	benchmarkOVMVerification(b, true, true, false, false)
}

func ovmBenchmarkFixture(b *testing.B, count int, withRetention bool) ovmFixture {
	var change func([]fixtureAccount)
	if withRetention || os.Getenv("L2STATE_BENCH_RETAIN_FIXTURE") == "1" {
		change = func(accounts []fixtureAccount) {
			for n := 1; n <= min(1000, len(accounts)-1); n++ {
				accounts[n].code = []byte{0x00}
			}
		}
	}
	f := newOVMFixtureSized(b, count, change)
	if os.Getenv("L2STATE_BENCH_PREIMAGES") == "1" {
		editOVMSource(b, f, func(db ethdb.Database) {
			index := newOVMIndex(db)
			defer index.batch.Close()
			for _, address := range f.holders {
				if err := index.batch.Put(append([]byte("secure-key-"), crypto.Keccak256(address[:])...), address[:]); err != nil {
					b.Fatal(err)
				}
				if index.batch.ValueSize() >= ethdb.IdealBatchSize {
					if err := index.flush(); err != nil {
						b.Fatal(err)
					}
				}
			}
			if err := index.flush(); err != nil {
				b.Fatal(err)
			}
		})
	}
	return f
}
func writeOVMBenchmarkRetainList(b *testing.B, f ovmFixture) string {
	var data strings.Builder
	for _, account := range f.accounts[1:min(1001, len(f.accounts))] {
		fmt.Fprintln(&data, account.address.Hex())
	}
	return writeRetainList(b, data.String())
}

func BenchmarkOVMVerificationAudit(b *testing.B) {
	benchmarkOVMVerification(b, false, false, true, false)
}

func BenchmarkOVMVerificationRetainAudit(b *testing.B) {
	benchmarkOVMVerification(b, false, true, true, false)
}

func BenchmarkOVMVerificationAllocAudit(b *testing.B) {
	benchmarkOVMVerification(b, true, false, true, false)
}

func BenchmarkOVMVerificationAllocRetainAudit(b *testing.B) {
	benchmarkOVMVerification(b, true, true, true, false)
}

func BenchmarkOVMVerificationAuditHistory(b *testing.B) {
	benchmarkOVMVerification(b, false, false, true, true)
}

func BenchmarkOVMVerificationRetainAuditHistory(b *testing.B) {
	benchmarkOVMVerification(b, false, true, true, true)
}

func BenchmarkOVMVerificationAllocAuditHistory(b *testing.B) {
	benchmarkOVMVerification(b, true, false, true, true)
}

func BenchmarkOVMVerificationAllocRetainAuditHistory(b *testing.B) {
	benchmarkOVMVerification(b, true, true, true, true)
}

// All three variants receive the same later source when the historical fixture
// is enabled. Only the selected summary/input presence differs.
func ovmBenchmarkAuditFixture(b *testing.B, count int, retain, audit, historical bool) (ovmFixture, string) {
	f := ovmBenchmarkFixture(b, count, retain)
	var summary ovmAuditSummary
	if historical {
		summary = fixtureAudit(b, f)
	}
	if historical || os.Getenv("L2STATE_BENCH_HISTORY_FIXTURE") == "1" {
		evolveAuditFixture(b, &f)
	}
	path := ""
	if audit {
		if !historical {
			summary = fixtureAudit(b, f)
		}
		path = writeAudit(b, summary)
	}
	return f, path
}
