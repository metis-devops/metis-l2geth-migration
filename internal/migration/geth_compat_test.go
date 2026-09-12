package migration

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

var gethCompatOut = flag.String("geth-compat-out", os.Getenv("L2STATE_GETH_COMPAT_OUT"), "new candidate baseline directory; does not approve compatibility")

const gethCompatRoot = "testdata/geth-compat"

func TestGethCompatibility(t *testing.T) {
	expected := newCompatCapture()
	for kind, contract := range expected.Contracts {
		loaded, err := readCompatContract(gethCompatRoot, kind, contract.Version)
		if err != nil {
			t.Fatal(err)
		}
		expected.Contracts[kind] = loaded
	}
	excludeRetiredCompatTargets(expected)
	normalizeCompatBaseline(t, expected)
	actual := captureGethCompatibility(t)
	mismatched := false
	for _, kind := range []string{"bundle", "verification", "direct"} {
		if err := compareCompatContracts(expected.Contracts[kind], actual.Contracts[kind]); err != nil {
			t.Error(err)
			mismatched = true
		}
	}
	if mismatched {
		parent, err := os.MkdirTemp("", "l2state-geth-compat-")
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(parent, "candidate")
		if err := writeCompatCapture(out, actual); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("incompatible contracts; complete candidate data retained at %s (not approved)", out)
	}
	replayGethCompatibility(t, expected)
}

func TestWriteGethCompatibilityCandidate(t *testing.T) {
	if *gethCompatOut == "" {
		t.Skip("explicit -geth-compat-out required; normal tests never update baselines")
	}
	if !filepath.IsAbs(*gethCompatOut) {
		t.Fatal("geth-compat-out must be an absolute path")
	}
	out, err := filepath.Abs(*gethCompatOut)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := filepath.Abs(gethCompatRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectOutputInsideDirectory(baseline, out, "candidate output must be outside committed baselines", "candidate aliases committed baselines"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("candidate output must be new: %s (%v)", out, err)
	}
	capture := captureGethCompatibility(t)
	if err := writeCompatCapture(out, capture); err != nil {
		t.Fatal(err)
	}
	t.Logf("candidate only, no compatibility approval: %s", out)
}

func captureGethCompatibility(t *testing.T) *compatCapture {
	t.Helper()
	capture := newCompatCapture()
	sources := compatSources(t)
	sourceDigests := make(map[string]string)
	for name, source := range sources {
		sourceDigests[name] = directoryContentDigest(t, source)
	}
	// The legacy input is never regenerated. Synthetic workloads are deterministic
	// probes, whose output is frozen too so geth cannot move both sides together.
	canary, err := os.ReadFile("testdata/legacy-l2geth-kv-v1.bin")
	if err != nil {
		t.Fatal(err)
	}
	capture.Provenance = map[string]any{
		"geth_version":         version.GethVersion,
		"legacy_canary_sha256": fmt.Sprintf("%x", sha256.Sum256(canary)),
		"synthetic_inputs":     "compatSources: fixed workload parameters, benchmarkAccountHash and writeTraversalBenchmarkHead",
	}
	captureCompatCodecs(t, capture.Contracts["bundle"])
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) { captureCompatSource(t, capture, name, sources[name]) })
	}
	for name, source := range sources {
		if directoryContentDigest(t, source) != sourceDigests[name] {
			t.Fatalf("source %s changed", name)
		}
	}
	return capture
}

func compatSources(t *testing.T) map[string]string {
	t.Helper()
	sources := map[string]string{"canary": loadGoldenLegacyKV(t)}
	workloads := map[string]traversalBenchmarkWorkload{
		"empty":               {accounts: 0, storageEvery: 1, codeSize: 16},
		"single-leaf":         {accounts: 1, storageEvery: 1, codeSize: 16},
		"single-partition":    {accounts: 3, storageEvery: 1, codeSize: 16, singleAccountPartition: true},
		"multiple-partitions": {accounts: 16, storageEvery: 1, codeSize: 16},
		"storage-1024":        {accounts: 1, storageEvery: 1, slotsPerAccount: 1024, codeSize: 16},
		"storage-1025":        {accounts: 1, storageEvery: 1, slotsPerAccount: 1025, codeSize: 16},
	}
	for name, workload := range workloads {
		source := filepath.Join(t.TempDir(), "source")
		root, _ := buildTraversalBenchmarkState(t, source, workload)
		writeTraversalBenchmarkHead(t, source, root)
		sources[name] = source
	}
	return sources
}

func captureCompatSource(t *testing.T, c *compatCapture, name, source string) {
	t.Helper()
	for _, compression := range []string{bundle.CompressionNone, bundle.CompressionZstd} {
		path := filepath.Join(t.TempDir(), "bundle")
		exported, err := Export(t.Context(), ExportOptions{SourceChaindata: source, Output: path, Compression: compression, CacheMB: 16, Handles: 16})
		if err != nil {
			t.Fatal(err)
		}
		manifest := normalizeCompatJSON(t, exported.Manifest, nil)
		records, err := os.ReadFile(filepath.Join(path, exported.Manifest.StateFile.Name))
		if err != nil {
			t.Fatal(err)
		}
		bundleName := name + "/" + compression
		c.Contracts["bundle"].add(t, bundleName, compatBundle{Manifest: manifest, Records: records})
		report, err := Verify(t.Context(), VerifyOptions{Bundle: path, CacheMB: 16, Handles: 16})
		if err != nil {
			t.Fatal(err)
		}
		c.Contracts["verification"].add(t, bundleName+"/bundle", normalizeCompatJSON(t, report, manifest))
		for _, tc := range targetTestCases() {
			imported, err := Import(t.Context(), ImportOptions{Bundle: path, Output: filepath.Join(t.TempDir(), "artifact"), Scheme: tc.scheme, DBEngine: tc.engine, CacheMB: 16, Handles: 16})
			if err != nil {
				t.Fatal(err)
			}
			verified, err := Verify(t.Context(), VerifyOptions{Bundle: path, Artifact: imported.ArtifactPath, CacheMB: 16, Handles: 16})
			if err != nil {
				t.Fatal(err)
			}
			contract := c.Contracts["verification"]
			artifact := compatArtifact{Report: normalizeCompatJSON(t, verified, manifest), Database: contract.database(t, filepath.Join(imported.ArtifactPath, "chaindata"))}
			if name == "canary" {
				artifact.Continuation = captureCompatContinuation(t, contract.Databases[artifact.Database], tc, verified.Head.StateRoot)
			}
			contract.add(t, bundleName+"/"+tc.name(), artifact)
		}
	}
	for _, tc := range targetTestCases() {
		migrated, err := Migrate(t.Context(), MigrateOptions{SourceChaindata: source, Output: filepath.Join(t.TempDir(), "artifact"), Scheme: tc.scheme, DBEngine: tc.engine, CacheMB: 16, Handles: 16, Workers: 2})
		if err != nil {
			t.Fatal(err)
		}
		report, err := VerifyDirect(t.Context(), DirectVerifyOptions{SourceChaindata: source, Artifact: migrated.ArtifactPath, CacheMB: 16, Handles: 16})
		if err != nil {
			t.Fatal(err)
		}
		contract := c.Contracts["direct"]
		artifact := compatArtifact{Report: normalizeCompatJSON(t, report, nil), Database: contract.database(t, filepath.Join(migrated.ArtifactPath, "chaindata"))}
		if name == "canary" {
			artifact.Continuation = captureCompatContinuation(t, contract.Databases[artifact.Database], tc, report.Source.HeadBefore.StateRoot)
		}
		contract.add(t, name+"/"+tc.name(), artifact)
	}
}

// The frozen consensus input is checked independently of the candidate builder.
func compatExpectedState(report json.RawMessage, direct bool) (bundle.SourceEvidence, StateResult, targetConfig, string, error) {
	if direct {
		var r DirectVerificationReport
		if err := json.Unmarshal(report, &r); err != nil {
			return bundle.SourceEvidence{}, StateResult{}, targetConfig{}, "", err
		}
		if err := r.Validate(); err != nil {
			return bundle.SourceEvidence{}, StateResult{}, targetConfig{}, "", err
		}
		target, err := reportTarget(r.DBEngine, r.StateLayout, r.Scheme)
		return r.Source, StateResult{Root: r.RecomputedRoot, Counts: r.Counts}, target, r.Scheme, err
	}
	var r VerificationReport
	if err := json.Unmarshal(report, &r); err != nil {
		return bundle.SourceEvidence{}, StateResult{}, targetConfig{}, "", err
	}
	if err := r.Validate(); err != nil {
		return bundle.SourceEvidence{}, StateResult{}, targetConfig{}, "", err
	}
	target, err := reportTarget(r.DBEngine, r.StateLayout, r.Scheme)
	return bundle.SourceEvidence{HeadBefore: r.Head, HeadAfter: r.Head}, StateResult{Root: r.RecomputedRoot, Counts: r.Counts}, target, r.Scheme, err
}
