package migration

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAtomicDirPublicationDurabilityErrorRetainsFinalOutput(t *testing.T) {
	parent := t.TempDir()
	final := filepath.Join(parent, "artifact")
	injected := errors.New("injected parent sync failure")
	var syncCalls, renameCalls int
	ops := atomicDirOps{
		syncDir: func(path string) error {
			syncCalls++
			if syncCalls == 3 {
				return injected
			}
			return syncDirectory(path)
		},
		rename: func(oldPath, newPath string) error {
			renameCalls++
			if renameCalls == 1 {
				return os.ErrExist
			}
			return renameNoReplace(oldPath, newPath)
		},
	}
	output, err := newAtomicDirWithOps(final, ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output.Path(), "complete"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = output.Commit()
	var durability *PublicationDurabilityError
	if !errors.As(err, &durability) || !errors.Is(err, injected) {
		t.Fatalf("commit error is %v, want publication durability error", err)
	}
	resolvedFinal, resolveErr := filepath.EvalSymlinks(final)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if durability.Path != resolvedFinal || !strings.Contains(err.Error(), "do not rerun") {
		t.Fatalf("durability error is %+v", durability)
	}
	if data, err := os.ReadFile(filepath.Join(final, "complete")); err != nil || string(data) != "yes" {
		t.Fatalf("published output was not retained: data=%q err=%v", data, err)
	}
	if err := output.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("abort removed published output: %v", err)
	}
}

func TestAtomicDirPrePublishFailuresRemainRecoverable(t *testing.T) {
	injected := errors.New("injected failure")
	t.Run("preflight parent sync", func(t *testing.T) {
		final := filepath.Join(t.TempDir(), "artifact")
		_, err := newAtomicDirWithOps(final, atomicDirOps{
			syncDir: func(string) error { return injected },
			rename:  renameNoReplace,
		})
		if !errors.Is(err, injected) {
			t.Fatalf("preflight returned %v", err)
		}
		assertPathAbsent(t, final)
	})

	for _, test := range []struct {
		name       string
		failSync   bool
		failRename bool
	}{
		{name: "temporary directory sync", failSync: true},
		{name: "publication rename", failRename: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			final := filepath.Join(parent, "artifact")
			var syncCalls, renameCalls int
			output, err := newAtomicDirWithOps(final, atomicDirOps{
				syncDir: func(path string) error {
					syncCalls++
					if test.failSync && syncCalls == 2 {
						return injected
					}
					return syncDirectory(path)
				},
				rename: func(oldPath, newPath string) error {
					renameCalls++
					if renameCalls == 1 {
						return os.ErrExist
					}
					if test.failRename {
						return injected
					}
					return renameNoReplace(oldPath, newPath)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := output.Commit(); !errors.Is(err, injected) {
				t.Fatalf("commit returned %v", err)
			}
			assertPathAbsent(t, final)
			if err := output.Abort(); err != nil {
				t.Fatal(err)
			}
			partials, err := filepath.Glob(filepath.Join(parent, ".artifact.partial-*"))
			if err != nil || len(partials) != 0 {
				t.Fatalf("partial outputs survived: %v err=%v", partials, err)
			}
		})
	}
}
