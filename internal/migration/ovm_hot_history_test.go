package migration

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/metis-devops/metis-l2geth-migration/internal/readonlydb"
	leveldb "github.com/syndtr/goleveldb/leveldb"
)

func TestOVMHotHistorySingleReadAndFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	db, err := gethleveldb.New(path, 16, 16, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("present"), []byte("history")); err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("empty"), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := directoryContentDigest(t, path)
	ro, err := readonlydb.Open(path, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	counted := &ovmReadCountingDB{Database: rawdb.NewDatabase(ro)}
	defer func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct {
		key     string
		value   []byte
		wantErr bool
	}{
		{"present", []byte("history"), false}, {"missing", nil, false}, {"empty", nil, true},
	} {
		got, err := optionalHistoryKV(counted, []byte(tc.key))
		if (err != nil) != tc.wantErr || !bytes.Equal(got, tc.value) {
			t.Fatalf("%s: %q %v", tc.key, got, err)
		}
	}
	injected := errors.New("history read failure")
	counted.failure = injected
	if _, err := optionalHistoryKV(counted, []byte("present")); !errors.Is(err, injected) {
		t.Fatalf("lost I/O error: %v", err)
	}
	counted.failure = fmt.Errorf("wrapped absence: %w", leveldb.ErrNotFound)
	if got, err := optionalHistoryKV(counted, []byte("missing")); err != nil || got != nil {
		t.Fatalf("wrapped absence: %x %v", got, err)
	}
	if counted.gets != 5 || counted.has != 0 {
		t.Fatalf("redundant lookups: Get=%d Has=%d", counted.gets, counted.has)
	}
	if directoryContentDigest(t, path) != before {
		t.Fatal("history reads changed source")
	}
}

// Retain the former two-lookup path as a component benchmark reference.
func optionalHistoryKVReference(db ethdb.Database, key []byte) ([]byte, error) {
	ok, err := db.Has(key)
	if err != nil || !ok {
		return nil, err
	}
	value, err := db.Get(key)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return nil, errors.New("hot canonical history record is empty")
	}
	return value, nil
}

func BenchmarkOVMHotHistoryRead(b *testing.B) {
	const records = 60000
	path := filepath.Join(b.TempDir(), "source")
	db, err := gethleveldb.New(path, 16, 16, "benchmark", false)
	if err != nil {
		b.Fatal(err)
	}
	batch := db.NewBatch()
	var key [9]byte
	key[0] = 'r'
	value := bytes.Repeat([]byte{0x42}, 512)
	for n := range records {
		binary.BigEndian.PutUint64(key[1:], uint64(n))
		if err := batch.Put(key[:], value); err != nil {
			b.Fatal(err)
		}
		if batch.ValueSize() >= ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				b.Fatal(err)
			}
			batch.Reset()
		}
	}
	if err := batch.Write(); err != nil {
		b.Fatal(err)
	}
	batch.Close()
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	for _, read := range []struct {
		name string
		read func(ethdb.Database, []byte) ([]byte, error)
	}{
		{"reference", optionalHistoryKVReference}, {"single-get", optionalHistoryKV},
	} {
		b.Run(read.name, func(b *testing.B) {
			kv, err := readonlydb.Open(path, 16, 16)
			if err != nil {
				b.Fatal(err)
			}
			ro := rawdb.NewDatabase(kv)
			defer func() {
				if err := ro.Close(); err != nil {
					b.Error(err)
				}
			}()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				for n := range records {
					binary.BigEndian.PutUint64(key[1:], uint64(n))
					if value, err := read.read(ro, key[:]); err != nil || len(value) != 512 {
						b.Fatalf("record %d: %v", n, err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(records, "records/op")
		})
	}
}
