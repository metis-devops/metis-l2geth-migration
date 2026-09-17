package migration

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestOVMAncientDataHandleReuseAndRotation(t *testing.T) {
	path := writeOVMAncientReadFixture(t, 4, 2)
	before := directoryContentDigest(t, path)
	ancient, err := openLegacyAncient(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ancient.Close(); err != nil {
			t.Error(err)
		}
	}()
	var previous [3]*os.File
	for n := range uint64(4) {
		for table := range 3 {
			blob, err := ancient.read(table, n)
			if err != nil {
				t.Fatal(err)
			}
			expected := common.Hash{}
			binary.BigEndian.PutUint64(expected[24:], n+1)
			if !bytes.Equal(blob, expected[:]) {
				t.Fatalf("read %d/%d: %x", table, n, blob)
			}
			current := ancient.tables[table].data
			if n%2 == 1 && current != previous[table] {
				t.Fatal("same file was reopened")
			}
			if n == 2 {
				if current == previous[table] {
					t.Fatal("data handle did not rotate")
				}
				if _, err := previous[table].Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("old handle leaked: %v", err)
				}
			}
			previous[table] = current
		}
	}
	if err := ancient.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range previous {
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("final data handle leaked: %v", err)
		}
	}
	if before != directoryContentDigest(t, path) {
		t.Fatal("ancient reads changed files")
	}
}

func TestOVMAncientCachedFileMutation(t *testing.T) {
	for _, mode := range []string{"truncate", "replace", "symlink", "modify", "index-replace"} {
		for _, at := range []string{"rotate", "close"} {
			t.Run(mode+"/"+at, func(t *testing.T) {
				path := writeOVMAncientReadFixture(t, 4, 2)
				ancient, err := openLegacyAncient(path, true)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ancient.read(0, 0); err != nil {
					t.Fatal(err)
				}
				held := ancient.tables[0].data
				name := held.Name()
				if mode == "index-replace" {
					name = ancient.tables[0].index.Name()
				}
				data, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "truncate":
					err = os.Truncate(name, 1)
				case "modify":
					data[0] ^= 1
					err = os.WriteFile(name, data, 0600)
					if err == nil {
						now := time.Now().Add(time.Second)
						err = os.Chtimes(name, now, now)
					}
				default:
					alternate := filepath.Join(path, "replacement")
					if err = os.WriteFile(alternate, data, 0600); err == nil {
						if mode == "symlink" {
							err = os.Remove(name)
							if err == nil {
								err = os.Symlink(alternate, name)
							}
						} else {
							err = os.Rename(alternate, name)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if at == "rotate" && mode != "index-replace" {
					if _, err := ancient.read(0, 2); err == nil {
						t.Fatal("rotation missed modified data")
					}
				} else {
					if err := ancient.Close(); err == nil {
						t.Fatal("close missed modified ancient file")
					}
				}
				if err := ancient.Close(); err != nil {
					t.Error(err)
				}
				if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("failed close leaked handle: %v", err)
				}
			})
		}
	}
}
