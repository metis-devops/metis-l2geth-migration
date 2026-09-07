package migration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/metis-devops/metis-l2geth-migration/internal/formatversion"
)

// These files freeze logical contracts, never physical SST/WAL representations.
// Database blobs are deduplicated within each independently versioned contract.
type compatContract struct {
	BundleVersion int                        `json:"bundle_version,omitempty"`
	Format        string                     `json:"format"`
	Version       int                        `json:"version"`
	Cases         map[string]json.RawMessage `json:"cases"`
	Databases     map[string][]compatKV      `json:"databases,omitempty"`
}
type compatKV struct {
	Key   hexutil.Bytes `json:"key"`
	Value hexutil.Bytes `json:"value"`
}
type compatBundle struct {
	Manifest json.RawMessage `json:"manifest"`
	Records  hexutil.Bytes   `json:"records"`
}
type compatArtifact struct {
	Report       json.RawMessage `json:"report"`
	Database     string          `json:"database"`
	Continuation json.RawMessage `json:"continuation,omitempty"`
}
type compatCapture struct {
	Contracts  map[string]*compatContract
	Provenance map[string]any
}

func newCompatCapture() *compatCapture {
	c := &compatCapture{Contracts: make(map[string]*compatContract), Provenance: make(map[string]any)}
	for kind, version := range map[string]int{"bundle": formatversion.Bundle, "verification": formatversion.Verification, "direct": formatversion.DirectVerification} {
		c.Contracts[kind] = &compatContract{Format: kind, Version: version, Cases: make(map[string]json.RawMessage), Databases: make(map[string][]compatKV)}
	}
	c.Contracts["verification"].BundleVersion = formatversion.Bundle
	return c
}
func compatJSON(t testing.TB, value any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}
func normalizeCompatJSON(t testing.TB, document any, manifest []byte) json.RawMessage {
	t.Helper()
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(compatJSON(t, document), &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"created_at", "verified_at"} {
		if _, ok := wire[key]; ok {
			wire[key] = json.RawMessage(`"2000-01-01T00:00:00Z"`)
		}
	}
	for _, key := range []string{"tool_version", "geth_version"} {
		if _, ok := wire[key]; ok {
			wire[key] = json.RawMessage(`"compatibility-baseline"`)
		}
	}
	// This former provenance field exists only in historical test baselines.
	// Remove it in memory; runtime input decoding has no compatibility exception.
	delete(wire, "geth_commit")
	if _, ok := wire["manifest_sha256"]; ok {
		if len(manifest) == 0 {
			t.Fatal("report normalization requires its normalized manifest")
		}
		digest := sha256.Sum256(manifest)
		wire["manifest_sha256"] = compatJSON(t, hexutil.Bytes(digest[:]))
	}
	return compatJSON(t, wire)
}
func (c *compatContract) add(t testing.TB, name string, value any) {
	t.Helper()
	if _, exists := c.Cases[name]; exists {
		t.Fatalf("duplicate compatibility case %s/%s", c.Format, name)
	}
	c.Cases[name] = compatJSON(t, value)
}
func (c *compatContract) database(t *testing.T, path string) string {
	t.Helper()
	entries := readLogicalDatabase(t, path, "compat-capture")
	kv := make([]compatKV, 0, len(entries))
	for _, entry := range entries {
		kv = append(kv, compatKV{entry.key, entry.value})
	}
	id := compatDatabaseID(kv)
	c.Databases[id] = kv
	return id
}
func compatDatabaseID(entries []compatKV) string {
	// The representation is local to the baseline corpus, not an artifact schema.
	data, err := json.Marshal(entries)
	if err != nil {
		panic(err)
	} // Only byte slices are encoded.
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
func compatFilename(kind string, version int) string {
	if kind == "verification" {
		return fmt.Sprintf("verification-v%d-bundle-v%d.json", version, formatversion.Bundle)
	}
	return fmt.Sprintf("%s-v%d.json", kind, version)
}

func writeCompatCapture(out string, capture *compatCapture) error {
	if out == "" {
		return errors.New("candidate output directory is required")
	}
	// Mkdir, not MkdirAll: reject any pre-existing directory, including symlinks.
	if err := os.Mkdir(out, 0700); err != nil {
		return fmt.Errorf("create new candidate directory: %w", err)
	}
	for kind, contract := range capture.Contracts {
		if err := writeCompatJSON(filepath.Join(out, compatFilename(kind, contract.Version)), contract); err != nil {
			return err
		}
	}
	return writeCompatJSON(filepath.Join(out, "provenance.json"), capture.Provenance)
}
func writeCompatJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}
func readCompatContract(root, kind string, version int) (*compatContract, error) {
	path := filepath.Join(root, compatFilename(kind, version))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load %s v%d baseline (never regenerated automatically): %w", kind, version, err)
	}
	var c compatContract
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return nil, err
	}
	if c.Format != kind || c.Version != version || (kind == "verification" && c.BundleVersion != formatversion.Bundle) {
		return nil, fmt.Errorf("%s: baseline format/version mismatch: %s v%d", path, c.Format, c.Version)
	}
	if err := decoder.Decode(new(any)); err == nil {
		return nil, fmt.Errorf("%s: trailing baseline JSON value", path)
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: trailing baseline JSON: %w", path, err)
	}
	if len(c.Cases) == 0 {
		return nil, fmt.Errorf("%s: empty baseline cases", path)
	}
	for id, entries := range c.Databases {
		if compatDatabaseID(entries) != id {
			return nil, fmt.Errorf("%s: database blob %s digest mismatch", path, id)
		}
		for i, entry := range entries {
			if len(entry.Key) == 0 || (i > 0 && bytes.Compare(entries[i-1].Key, entry.Key) >= 0) {
				return nil, fmt.Errorf("%s: database %s keys not strictly ordered", path, id)
			}
		}
	}
	return &c, nil
}

func compareCompatContracts(want, got *compatContract) error {
	if want.Format != got.Format || want.Version != got.Version || want.BundleVersion != got.BundleVersion {
		return fmt.Errorf("format/version differs: expected %s v%d, actual %s v%d", want.Format, want.Version, got.Format, got.Version)
	}
	keys := make(map[string]struct{})
	for k := range want.Cases {
		keys[k] = struct{}{}
	}
	for k := range got.Cases {
		keys[k] = struct{}{}
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	var differences []error
	for _, name := range names {
		expected, err := expandCompatCase(want, name)
		if err != nil {
			differences = append(differences, err)
			continue
		}
		actual, err := expandCompatCase(got, name)
		if err != nil {
			differences = append(differences, err)
			continue
		}
		if err := diffCompatJSON(expected, actual, want.Format+"/"+name); err != nil {
			differences = append(differences, err)
		}
	}
	return errors.Join(differences...)
}
func expandCompatCase(c *compatContract, name string) (any, error) {
	b, ok := c.Cases[name]
	if !ok {
		return nil, fmt.Errorf("%s/%s: missing case", c.Format, name)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if doc, ok := value.(map[string]any); ok {
		if id, ok := doc["database"].(string); ok {
			entries, exists := c.Databases[id]
			if !exists {
				return nil, fmt.Errorf("%s/%s: missing database %s", c.Format, name, id)
			}
			kv := make(map[string]any, len(entries))
			for _, entry := range entries {
				kv[entry.Key.String()] = entry.Value.String()
			}
			doc["database"] = kv
		}
	}
	return value, nil
}
func diffCompatJSON(want, got any, path string) error {
	if reflect.DeepEqual(want, got) {
		return nil
	}
	if left, ok := want.(map[string]any); ok {
		if right, ok := got.(map[string]any); ok {
			keys := make(map[string]bool)
			for k := range left {
				keys[k] = true
			}
			for k := range right {
				keys[k] = true
			}
			ordered := make([]string, 0, len(keys))
			for k := range keys {
				ordered = append(ordered, k)
			}
			slices.Sort(ordered)
			for _, k := range ordered {
				lv, lok := left[k]
				rv, rok := right[k]
				if !lok || !rok {
					return fmt.Errorf("%s/%s: expected present=%v %s; actual present=%v %s", path, k, lok, compatValue(lv), rok, compatValue(rv))
				}
				if err := diffCompatJSON(lv, rv, path+"/"+k); err != nil {
					return err
				}
			}
		}
	}
	if left, ok := want.([]any); ok {
		if right, ok := got.([]any); ok && len(left) == len(right) {
			for i := range left {
				if err := diffCompatJSON(left[i], right[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return fmt.Errorf("%s: expected %s; actual %s", path, compatValue(want), compatValue(got))
}
func compatValue(value any) string {
	b, _ := json.Marshal(value)
	if len(b) <= 160 {
		return string(b)
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("json_bytes=%d sha256=%x prefix=%s...", len(b), sum, strings.TrimSuffix(string(b[:64]), `"`))
}

// normalizeCompatBaseline transforms only in-memory historical provenance.
// Dependent report digests use the same normalized manifest bytes as new output;
// records, consensus evidence and database entries remain untouched.
func normalizeCompatBaseline(t *testing.T, capture *compatCapture) {
	t.Helper()
	manifests := make(map[string]json.RawMessage)
	bundles := capture.Contracts["bundle"]
	for name, data := range bundles.Cases {
		if name == "constants" || strings.HasPrefix(name, "codec/") {
			continue
		}
		var frozen compatBundle
		if err := json.Unmarshal(data, &frozen); err != nil {
			t.Fatal(err)
		}
		frozen.Manifest = normalizeCompatJSON(t, frozen.Manifest, nil)
		manifests[name] = frozen.Manifest
		bundles.Cases[name] = compatJSON(t, frozen)
	}
	for _, kind := range []string{"verification", "direct"} {
		contract := capture.Contracts[kind]
		for name, data := range contract.Cases {
			var manifest json.RawMessage
			if kind == "verification" {
				parts := strings.Split(name, "/")
				if len(parts) < 3 {
					t.Fatalf("invalid baseline case %s/%s", kind, name)
				}
				manifest = manifests[strings.Join(parts[:2], "/")]
				if len(manifest) == 0 {
					t.Fatalf("missing input manifest for baseline %s/%s", kind, name)
				}
			}
			if kind == "verification" && strings.HasSuffix(name, "/bundle") {
				contract.Cases[name] = normalizeCompatJSON(t, data, manifest)
				continue
			}
			var frozen compatArtifact
			if err := json.Unmarshal(data, &frozen); err != nil {
				t.Fatal(err)
			}
			frozen.Report = normalizeCompatJSON(t, frozen.Report, manifest)
			contract.Cases[name] = compatJSON(t, frozen)
		}
	}
}
