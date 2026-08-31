package strictio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootExactLayoutAndBoundedRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.json"), []byte("{\"value\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			t.Errorf("close strict root: %v", err)
		}
	}()
	if err := root.RequireExactLayout(map[string]EntryKind{"input.json": RegularFile, "db": Directory}); err != nil {
		t.Fatal(err)
	}
	data, err := root.ReadRegular("input.json", 32)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeJSON[struct {
		Value int `json:"value"`
	}](data, "test input")
	if err != nil || decoded.Value != 1 {
		t.Fatalf("decoded %+v: %v", decoded, err)
	}
	if _, err := root.ReadRegular("input.json", 4); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversize read returned %v", err)
	}
}

func TestRootRejectsAliasesAndUnexpectedEntries(t *testing.T) {
	t.Run("root symlink", func(t *testing.T) {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "root-link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := OpenRoot(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("root symlink returned %v", err)
		}
	})

	t.Run("entry symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("value"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "value")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		root, err := OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := root.Close(); err != nil {
				t.Errorf("close root: %v", err)
			}
		}()
		if err := root.RequireExactLayout(map[string]EntryKind{"value": RegularFile}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("entry symlink returned %v", err)
		}
	})

	t.Run("extra entry", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"expected", "extra"} {
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		root, err := OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := root.Close(); err != nil {
				t.Errorf("close root: %v", err)
			}
		}()
		if err := root.RequireExactLayout(map[string]EntryKind{"expected": RegularFile}); err == nil {
			t.Fatal("extra entry was accepted")
		}
	})
}

func TestDecodeJSONRejectsUnknownAndTrailingValues(t *testing.T) {
	type document struct {
		Value int `json:"value"`
	}
	for _, data := range [][]byte{
		[]byte(`{"value":1,"extra":2}`),
		[]byte(`{"value":1} {"value":2}`),
	} {
		if _, err := DecodeJSON[document](data, "document"); err == nil {
			t.Fatalf("invalid JSON %q was accepted", data)
		}
	}
	if _, err := DecodeJSON[document]([]byte(`{"value":1}`), "document"); err != nil {
		t.Fatal(err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	root, err := OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Fatalf("second close returned %v", err)
	}
}
