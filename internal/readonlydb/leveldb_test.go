package readonlydb

import (
	"errors"
	"path/filepath"
	"testing"

	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestBorrowRetainsOwnershipAndRejectsMutation(t *testing.T) {
	owner, err := leveldb.OpenFile(filepath.Join(t.TempDir(), "db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	}()
	view := Borrow(owner)
	if err := view.Put([]byte("key"), []byte("value")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("borrowed view writable: %v", err)
	}
	if err := view.Delete([]byte("key")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("borrowed view deleted data: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Put([]byte("key"), []byte("value"), nil); err != nil {
		t.Fatalf("view closed owner: %v", err)
	}
	got, err := view.Get([]byte("key"))
	if err != nil || string(got) != "value" {
		t.Fatalf("borrowed read: %s %v", got, err)
	}
}

func TestStrictReadOnlyAdapter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chaindata")
	writable, err := gethleveldb.New(path, 16, 16, "readonly-fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Put([]byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close read-only database: %v", err)
		}
	}()
	value, err := db.Get([]byte("key"))
	if err != nil || string(value) != "value" {
		t.Fatalf("read value %q: %v", value, err)
	}
	if err := db.Put([]byte("other"), []byte("value")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("write returned %v", err)
	}
	if err := db.Compact(nil, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("compact returned %v", err)
	}
}
