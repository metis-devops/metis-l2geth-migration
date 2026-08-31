package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestPortableImportFailureAndCancellationCleanPartialOutput(t *testing.T) {
	fixture := buildLegacyFixture(t)
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run("corrupt/"+scheme, func(t *testing.T) {
			parent := t.TempDir()
			bundleDir := filepath.Join(parent, "bundle")
			exported, err := Export(context.Background(), ExportOptions{
				SourceChaindata: fixture.chaindata, Output: bundleDir, Compression: bundle.CompressionNone, CacheMB: 16, Handles: 16,
			})
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(filepath.Join(bundleDir, exported.Manifest.StateFile.Name), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt([]byte{0xff}, 12); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(parent, "artifact")
			if _, err := Import(context.Background(), ImportOptions{
				Bundle: bundleDir, Output: output, Scheme: scheme, CacheMB: 16, Handles: 16,
			}); err == nil {
				t.Fatal("corrupt bundle unexpectedly imported")
			}
			assertNoPublishedOrPartialOutput(t, parent, output)
		})

		t.Run("canceled/"+scheme, func(t *testing.T) {
			parent := t.TempDir()
			bundleDir := filepath.Join(parent, "bundle")
			if _, err := Export(context.Background(), ExportOptions{
				SourceChaindata: fixture.chaindata, Output: bundleDir, Compression: bundle.CompressionZstd, CacheMB: 16, Handles: 16,
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			output := filepath.Join(parent, "artifact")
			_, err := Import(ctx, ImportOptions{Bundle: bundleDir, Output: output, Scheme: scheme, CacheMB: 16, Handles: 16})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("import returned %v, want cancellation", err)
			}
			assertNoPublishedOrPartialOutput(t, parent, output)
		})
	}
}

func assertNoPublishedOrPartialOutput(t *testing.T, parent, output string) {
	t.Helper()
	assertPathAbsent(t, output)
	partials, err := filepath.Glob(filepath.Join(parent, ".artifact.partial-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial outputs survived: %v", partials)
	}
}
