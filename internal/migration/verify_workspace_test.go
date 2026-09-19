package migration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/log"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestVerificationWorkspacePathsAndLifetime(t *testing.T) {
	input, parent := t.TempDir(), t.TempDir()
	inside := filepath.Join(input, "nested")
	if err := os.Mkdir(inside, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(input, alias); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{input, inside, alias, filepath.Join(alias, "nested"), file, filepath.Join(parent, "missing")} {
		if _, err := prepareVerificationWorkspace(bad, input); err == nil {
			t.Fatalf("accepted temp-dir %s", bad)
		}
	}
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, scheme := range []string{"hash", "path", "bundle"} {
			w, err := prepareVerificationWorkspace(parent, input)
			if err != nil {
				t.Fatal(err)
			}
			index, err := w.nodeIndex(t.Context(), mode, scheme, 16, 16)
			if err != nil {
				t.Fatal(err)
			}
			wantDisk := mode == TempDBDisk && scheme == "hash"
			if (index.Parent != "") != wantDisk || (w.path != "") != wantDisk {
				t.Fatalf("unexpected workspace for %s/%s", mode, scheme)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			assertVerificationParentEmpty(t, parent)
		}
	}
	w, err := prepareVerificationWorkspace(parent, input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := w.create(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create: %v", err)
	}
	assertVerificationParentEmpty(t, parent)
	t.Setenv("TMPDIR", input)
	w, err = prepareVerificationWorkspace("", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.create(t.Context()); err == nil {
		t.Fatal("TMPDIR inside input was accepted")
	}
	t.Setenv("TMPDIR", parent)
	w, err = prepareVerificationWorkspace("", input)
	if err != nil {
		t.Fatal(err)
	}
	path, err := w.create(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != resolved {
		t.Fatalf("default scratch %s is not under TMPDIR %s", path, resolved)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	assertVerificationParentEmpty(t, parent)
}

func assertVerificationParentEmpty(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch survived: entries=%v err=%v", entries, err)
	}
}

func TestVerifyArtifactPreflightBeforeBundleScan(t *testing.T) {
	f := buildLegacyFixture(t)
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), ExportOptions{SourceChaindata: f.chaindata, Output: dir, Compression: bundle.CompressionNone, CacheMB: 16, Handles: 16}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"missing", "malformed"} {
		t.Run(invalid, func(t *testing.T) {
			artifact := filepath.Join(t.TempDir(), "artifact")
			if invalid == "malformed" {
				if err := os.MkdirAll(filepath.Join(artifact, artifactDatabaseDirName), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(artifact, VerificationFileName), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var progress bytes.Buffer
			_, err := Verify(t.Context(), VerifyOptions{Bundle: dir, Artifact: artifact, Progress: ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&progress, false))}})
			if err == nil || strings.Contains(progress.String(), "scan_bundle") {
				t.Fatalf("preflight did not stop scan: err=%v progress=%s", err, progress.String())
			}
		})
	}
}

func TestVerifyWorkspaceEndToEnd(t *testing.T) {
	f := buildLegacyFixture(t)
	before := directoryContentDigest(t, f.chaindata)
	dir := filepath.Join(t.TempDir(), "bundle")
	if _, err := Export(t.Context(), ExportOptions{SourceChaindata: f.chaindata, Output: dir, Compression: bundle.CompressionNone, CacheMB: 16, Handles: 16}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range targetTestCases() {
		t.Run(tc.name(), func(t *testing.T) {
			parent := t.TempDir()
			artifact := filepath.Join(t.TempDir(), "direct")
			if _, err := Migrate(t.Context(), MigrateOptions{SourceChaindata: f.chaindata, Output: artifact, Scheme: tc.scheme, DBEngine: tc.engine, CacheMB: 16, Handles: 16}); err != nil {
				t.Fatal(err)
			}
			imported := filepath.Join(t.TempDir(), "imported")
			if _, err := Import(t.Context(), ImportOptions{Bundle: dir, Output: imported, Scheme: tc.scheme, DBEngine: tc.engine, CacheMB: 16, Handles: 16}); err != nil {
				t.Fatal(err)
			}
			artifactBefore, importBefore := directoryContentDigest(t, artifact), directoryContentDigest(t, imported)
			for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
				if _, err := VerifyDirect(t.Context(), DirectVerifyOptions{TempDir: parent, TempDB: mode, SourceChaindata: f.chaindata, Artifact: artifact, CacheMB: 16, Handles: 16}); err != nil {
					t.Fatal(err)
				}
				if _, err := Verify(t.Context(), VerifyOptions{TempDir: parent, TempDB: mode, Bundle: dir, Artifact: imported, CacheMB: 16, Handles: 16}); err != nil {
					t.Fatal(err)
				}
				assertVerificationParentEmpty(t, parent)
			}
			if directoryContentDigest(t, artifact) != artifactBefore || directoryContentDigest(t, imported) != importBefore {
				t.Fatal("verification changed an artifact")
			}
		})
	}
	if directoryContentDigest(t, f.chaindata) != before {
		t.Fatal("verification changed source")
	}
}

func TestOVMVerifyReadOnlyArtifactParent(t *testing.T) {
	f := newOVMFixture(t, nil)
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) {
			opts := f.options(t, DBEnginePebble, "hash", 2)
			opts.TempDB = mode
			if _, err := Migrate(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Dir(opts.Output)
			if err := os.Chmod(parent, 0555); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Chmod(parent, 0700); err != nil {
					t.Error(err)
				}
			}()
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			before := directoryContentDigest(t, opts.Output)
			for _, configured := range []string{"", scratch} {
				_, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDir: configured, TempDB: mode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM})
				if err != nil {
					t.Fatal(err)
				}
				assertVerificationParentEmpty(t, scratch)
			}
			if directoryContentDigest(t, opts.Output) != before {
				t.Fatal("verification changed artifact")
			}
		})
	}
}

func TestVerificationWorkspaceCleanupOnFailureAndCancellation(t *testing.T) {
	f := newOVMFixture(t, nil)
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) {
			opts := f.options(t, DBEnginePebble, "hash", 2)
			if _, err := Migrate(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			scratch := t.TempDir()
			v := OVMVerifyOptions{TempDir: scratch, TempDB: mode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}
			before := directoryContentDigest(t, opts.Output)
			bad := v
			bad.OVM.WrappedEtherCode = filepath.Join(t.TempDir(), "bad.hex")
			if err := os.WriteFile(bad.OVM.WrappedEtherCode, []byte("0x"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyOVM(t.Context(), bad); err == nil {
				t.Fatal("accepted bad runtime")
			}
			assertVerificationParentEmpty(t, scratch)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hook := ovmPhaseWriter{phase: "convert_ovm_balances", act: cancel}
			v.Progress = ProgressOptions{Logger: log.NewLogger(log.NewTerminalHandler(&hook, false))}
			if _, err := VerifyOVM(ctx, v); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			assertVerificationParentEmpty(t, scratch)
			if directoryContentDigest(t, opts.Output) != before {
				t.Fatal("failed verification changed artifact")
			}
		})
	}
}
