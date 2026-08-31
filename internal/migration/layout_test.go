package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

func TestBundleLayoutIsStrict(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	newBundle := func(t *testing.T, name string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if _, err := Export(context.Background(), ExportOptions{
			SourceChaindata: fixture.chaindata, Output: path, Compression: bundle.CompressionNone, CacheMB: 16, Handles: 16,
		}); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("extra entry", func(t *testing.T) {
		path := newBundle(t, "extra")
		if err := os.WriteFile(filepath.Join(path, ".DS_Store"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		assertBundleVerifyFails(t, path, "layout")
	})

	t.Run("records symlink", func(t *testing.T) {
		path := newBundle(t, "records-link")
		manifest, _, err := bundle.LoadManifest(path)
		if err != nil {
			t.Fatal(err)
		}
		records := filepath.Join(path, manifest.StateFile.Name)
		target := filepath.Join(root, "records-target")
		if err := os.Rename(records, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, records); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		assertBundleVerifyFails(t, path, "symbolic link")
	})

	t.Run("root symlink", func(t *testing.T) {
		path := newBundle(t, "root-target")
		link := filepath.Join(root, "bundle-link")
		if err := os.Symlink(path, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		assertBundleVerifyFails(t, link, "symbolic link")
	})

	t.Run("oversize manifest", func(t *testing.T) {
		path := newBundle(t, "oversize")
		file, err := os.OpenFile(filepath.Join(path, bundle.ManifestFileName), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(make([]byte, strictio.MaxMetadataSize)); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		assertBundleVerifyFails(t, path, "maximum")
	})

	t.Run("output inside bundle", func(t *testing.T) {
		path := newBundle(t, "nested-output")
		output := filepath.Join(path, "artifact")
		_, err := Import(context.Background(), ImportOptions{
			Bundle: path, Output: output, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		})
		if err == nil || !strings.Contains(err.Error(), "inside the input bundle") {
			t.Fatalf("nested output returned %v", err)
		}
		if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("nested output exists: %v", statErr)
		}
	})
}

func TestArtifactLayoutIsStrict(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	if _, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata, Output: bundleDir, Compression: bundle.CompressionNone, CacheMB: 16, Handles: 16,
	}); err != nil {
		t.Fatal(err)
	}
	newArtifact := func(t *testing.T, name string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if _, err := Import(context.Background(), ImportOptions{
			Bundle: bundleDir, Output: path, Scheme: rawdb.HashScheme, CacheMB: 16, Handles: 16,
		}); err != nil {
			t.Fatal(err)
		}
		return path
	}

	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string) string
		want   string
	}{
		{name: "extra entry", want: "layout", mutate: func(t *testing.T, path string) string {
			if err := os.WriteFile(filepath.Join(path, "README"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "verification symlink", want: "symbolic link", mutate: func(t *testing.T, path string) string {
			report := filepath.Join(path, VerificationFileName)
			target := filepath.Join(root, "report-target")
			if err := os.Rename(report, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, report); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return path
		}},
		{name: "database symlink", want: "symbolic link", mutate: func(t *testing.T, path string) string {
			db := filepath.Join(path, "db")
			target := filepath.Join(root, "db-target")
			if err := os.Rename(db, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, db); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return path
		}},
		{name: "root symlink", want: "symbolic link", mutate: func(t *testing.T, path string) string {
			link := filepath.Join(root, "artifact-link")
			if err := os.Symlink(path, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return link
		}},
		{name: "oversize report", want: "maximum", mutate: func(t *testing.T, path string) string {
			file, err := os.OpenFile(filepath.Join(path, VerificationFileName), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(make([]byte, strictio.MaxMetadataSize)); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := newArtifact(t, strings.ReplaceAll(test.name, " ", "-"))
			verifyPath := test.mutate(t, path)
			_, err := Verify(context.Background(), VerifyOptions{Bundle: bundleDir, Artifact: verifyPath, CacheMB: 16, Handles: 16})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verification returned %v, want %q", err, test.want)
			}
		})
	}
}

func assertBundleVerifyFails(t *testing.T, path, want string) {
	t.Helper()
	_, err := Verify(context.Background(), VerifyOptions{Bundle: path, CacheMB: 16, Handles: 16})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("bundle verification returned %v, want %q", err, want)
	}
}
