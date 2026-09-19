package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/holiman/uint256"
)

func cursorTestKey(n uint64) []byte {
	var key common.Hash
	binary.BigEndian.PutUint64(key[24:], n)
	return key[:]
}

type cursorCountingDB struct {
	ethdb.Database
	opens, steps, releases int
	failure                error
	failAt                 int
}

func (d *cursorCountingDB) NewIterator(prefix, start []byte) ethdb.Iterator {
	d.opens++
	return &cursorCountingIterator{Iterator: d.Database.NewIterator(prefix, start), db: d}
}

type cursorCountingIterator struct {
	ethdb.Iterator
	db     *cursorCountingDB
	failed bool
}

func (i *cursorCountingIterator) Next() bool {
	i.db.steps++
	if i.db.failAt == i.db.steps {
		i.failed = true
		return false
	}
	return i.Iterator.Next()
}
func (i *cursorCountingIterator) Error() error {
	if i.failed {
		return i.db.failure
	}
	return i.Iterator.Error()
}
func (i *cursorCountingIterator) Release() { i.db.releases++; i.Iterator.Release() }

func TestOVMStorageCursorDenseSparseAndErrors(t *testing.T) {
	index := testAllocIndex(t)
	for n := range uint64(1000) {
		if err := index.put('b', cursorTestKey(n*2), cursorTestKey(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := index.flush(); err != nil {
		t.Fatal(err)
	}
	for _, sparse := range []bool{false, true} {
		t.Run(fmt.Sprint("sparse=", sparse), func(t *testing.T) {
			db := &cursorCountingDB{Database: index.db}
			c := ovmStorageCursor{db: db, prefix: 'b'}
			defer c.close()
			queries := uint64(2001)
			stride := uint64(1)
			if sparse {
				stride = 200
			}
			calls := 0
			for n := uint64(0); n < queries; n += stride {
				calls++
				want, found, err := index.get('b', cursorTestKey(n))
				if err != nil {
					t.Fatal(err)
				}
				got, ok, err := c.get(t.Context(), cursorTestKey(n))
				if err != nil || ok != found || !bytes.Equal(got, want) {
					t.Fatalf("key=%d: %x %v %v; want %x %v", n, got, ok, err, want, found)
				}
			}
			if !sparse && db.opens != 1 {
				t.Fatalf("dense scan reopened %d times", db.opens)
			}
			if sparse && (db.opens <= 1 || db.steps > calls*(ovmEvidenceCursorSteps+2)) {
				t.Fatalf("unbounded sparse scan: opens=%d steps=%d", db.opens, db.steps)
			}
			c.close()
			c.close()
			if db.opens != db.releases {
				t.Fatalf("leaked iterators: %+v", db)
			}
		})
	}
	for _, failAt := range []int{1, 2, ovmEvidenceCursorSteps + 2} {
		t.Run(fmt.Sprint("failure=", failAt), func(t *testing.T) {
			injected := errors.New("injected cursor failure")
			db := &cursorCountingDB{Database: index.db, failAt: failAt, failure: injected}
			c := ovmStorageCursor{db: db, prefix: 'b'}
			defer c.close()
			_, _, err := c.get(t.Context(), cursorTestKey(0))
			if err == nil {
				_, _, err = c.get(t.Context(), cursorTestKey(1900))
			}
			if !errors.Is(err, injected) {
				t.Fatalf("lost iterator failure: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	db := &cursorCountingDB{Database: index.db}
	c := ovmStorageCursor{db: db, prefix: 'b'}
	if _, _, err := c.get(ctx, cursorTestKey(0)); !errors.Is(err, context.Canceled) || db.opens != 0 {
		t.Fatalf("canceled lookup: %v opens=%d", err, db.opens)
	}
}

func TestOVMStorageClassificationCursorRejectsAmbiguity(t *testing.T) {
	for _, prefixes := range []string{"", "ba", "bk", "ak", "bak", "b"} {
		t.Run("prefixes="+prefixes, func(t *testing.T) {
			index := testAllocIndex(t)
			key := cursorTestKey(1)
			for _, prefix := range []byte(prefixes) {
				// Deliberately malformed address for the b-only case.
				if err := index.put(prefix, key, []byte{1}); err != nil {
					t.Fatal(err)
				}
			}
			if err := index.flush(); err != nil {
				t.Fatal(err)
			}
			cursors := [3]ovmStorageCursor{{db: index.db, prefix: 'b'}, {db: index.db, prefix: 'a'}, {db: index.db, prefix: 'k'}}
			defer func() {
				for n := range cursors {
					cursors[n].close()
				}
			}()
			tr := ovmTransformer{index: index}
			if err := tr.classifySlot(t.Context(), key, new(uint256.Int), &cursors); err == nil {
				t.Fatal("invalid ownership accepted")
			}
		})
	}
}

func BenchmarkOVMStorageEvidence(b *testing.B) {
	for _, sparse := range []bool{false, true} {
		for _, variant := range []string{"point", "cursor"} {
			b.Run(fmt.Sprintf("sparse=%t/%s", sparse, variant), func(b *testing.B) {
				storage := newTemporaryStorage(TempDBDisk)
				db, err := storage.create(b.TempDir()+"/index", 16, 16)
				if err != nil {
					b.Fatal(err)
				}
				defer func() {
					if err := db.Close(); err != nil {
						b.Error(err)
					}
				}()
				index := newOVMIndex(db)
				defer index.batch.Close()
				const count = 100000
				for n := range uint64(count) {
					if err := index.put('b', cursorTestKey(n), cursorTestKey(n)); err != nil {
						b.Fatal(err)
					}
				}
				if err := index.flush(); err != nil {
					b.Fatal(err)
				}
				stride := uint64(1)
				if sparse {
					stride = 1000
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					c := ovmStorageCursor{db: db, prefix: 'b'}
					for n := uint64(0); n < count; n += stride {
						var value []byte
						var ok bool
						var err error
						if variant == "point" {
							value, ok, err = index.get('b', cursorTestKey(n))
						} else {
							value, ok, err = c.get(b.Context(), cursorTestKey(n))
						}
						if err != nil || !ok || !bytes.Equal(value, cursorTestKey(n)) {
							b.Fatalf("lookup: %v %v", ok, err)
						}
					}
					c.close()
				}
				b.StopTimer()
			})
		}
	}
}
