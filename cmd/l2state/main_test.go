package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
)

func TestCLIEndToEnd(t *testing.T) {
	source := loadGoldenSource(t)
	root := t.TempDir()
	bundlePath := filepath.Join(root, "bundle")
	artifactPath := filepath.Join(root, "artifact")
	directArtifactPath := filepath.Join(root, "direct-artifact")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{
		"export", "--source-chaindata", source, "--out", bundlePath,
		"--compression", "none", "--cache-mb", "16", "--handles", "16",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("export command: %v stderr=%s", err, stderr.String())
	}
	assertJSON(t, stdout.Bytes())
	assertHexJSONField(t, stdout.Bytes(), "manifest.source.header_rlp", -1)
	assertHexJSONField(t, stdout.Bytes(), "manifest.state_file.sha256", 32)
	assertHexJSONField(t, stdout.Bytes(), "manifest.state_file.record_chain_hash", 32)
	assertProgressLog(t, stderr.String(), "export", "phase=export_state", "phase=publish_bundle")
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"import", "--bundle", bundlePath, "--out", artifactPath, "--scheme", "hash",
		"--cache-mb", "16", "--handles", "16",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("import command: %v stderr=%s", err, stderr.String())
	}
	assertJSON(t, stdout.Bytes())
	assertHexJSONField(t, stdout.Bytes(), "verification.manifest_sha256", 32)
	assertHexJSONField(t, stdout.Bytes(), "verification.state_file_sha256", 32)
	assertHexJSONField(t, stdout.Bytes(), "verification.record_chain_hash", 32)
	assertProgressLog(t, stderr.String(), "import",
		"phase=scan_bundle",
		"phase=generate_trie",
		"estimated=true",
		"phase=verify_state",
		"phase=inspect_database",
	)
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"verify", "--bundle", bundlePath, "--artifact", artifactPath,
		"--cache-mb", "16", "--handles", "16",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("verify command: %v stderr=%s", err, stderr.String())
	}
	assertJSON(t, stdout.Bytes())
	assertHexJSONField(t, stdout.Bytes(), "manifest_sha256", 32)
	assertHexJSONField(t, stdout.Bytes(), "state_file_sha256", 32)
	assertHexJSONField(t, stdout.Bytes(), "record_chain_hash", 32)
	assertProgressLog(t, stderr.String(), "verify",
		"phase=scan_bundle",
		"phase=verify_state",
		"phase=inspect_database",
	)
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"migrate", "--source-chaindata", source, "--out", directArtifactPath, "--scheme", "hash",
		"--cache-mb", "16", "--handles", "16",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("migrate command: %v stderr=%s", err, stderr.String())
	}
	assertJSON(t, stdout.Bytes())
	assertHexJSONField(t, stdout.Bytes(), "verification.source.header_rlp", -1)
	assertHexJSONField(t, stdout.Bytes(), "verification.source.head_before.block_hash", 32)
	assertHexJSONField(t, stdout.Bytes(), "verification.source.head_before.state_root", 32)
	assertHexJSONField(t, stdout.Bytes(), "verification.recomputed_state_root", 32)
	if bytes.Contains(stdout.Bytes(), []byte("manifest_sha256")) || bytes.Contains(stdout.Bytes(), []byte("record_chain_hash")) {
		t.Fatalf("direct migration output contains bundle-only evidence: %s", stdout.String())
	}
	assertProgressLog(t, stderr.String(), "migrate",
		"phase=migrate_state",
		"phase=generate_trie",
		"estimated=true",
		"phase=verify_state",
		"phase=inspect_database",
	)
	stdout.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{
		"verify", "--source-chaindata", source, "--artifact", directArtifactPath,
		"--cache-mb", "16", "--handles", "16",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("direct verify command: %v stderr=%s", err, stderr.String())
	}
	assertJSON(t, stdout.Bytes())
	assertHexJSONField(t, stdout.Bytes(), "source.header_rlp", -1)
	assertHexJSONField(t, stdout.Bytes(), "recomputed_state_root", 32)
	assertProgressLog(t, stderr.String(), "verify",
		"phase=verify_source_state",
		"phase=verify_state",
		"phase=inspect_database",
	)
}

func TestCLIQuiet(t *testing.T) {
	source := loadGoldenSource(t)
	root := t.TempDir()
	bundlePath := filepath.Join(root, "bundle")
	artifactPath := filepath.Join(root, "artifact")
	directArtifactPath := filepath.Join(root, "direct-artifact")
	var stdout, stderr bytes.Buffer
	commands := [][]string{
		{
			"export", "--source-chaindata", source, "--out", bundlePath,
			"--compression", "none", "--cache-mb", "16", "--handles", "16", "--quiet",
		},
		{
			"import", "--bundle", bundlePath, "--out", artifactPath, "--scheme", "hash",
			"--cache-mb", "16", "--handles", "16", "--quiet",
		},
		{
			"verify", "--bundle", bundlePath, "--artifact", artifactPath,
			"--cache-mb", "16", "--handles", "16", "--quiet",
		},
		{
			"migrate", "--source-chaindata", source, "--out", directArtifactPath, "--scheme", "hash",
			"--cache-mb", "16", "--handles", "16", "--quiet",
		},
		{
			"verify", "--source-chaindata", source, "--artifact", directArtifactPath,
			"--cache-mb", "16", "--handles", "16", "--quiet",
		},
	}
	for _, args := range commands {
		stdout.Reset()
		stderr.Reset()
		if err := run(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("%s command: %v stderr=%s", args[0], err, stderr.String())
		}
		assertJSON(t, stdout.Bytes())
		if stderr.Len() != 0 {
			t.Fatalf("%s --quiet wrote stderr %q", args[0], stderr.String())
		}
	}
}

func TestCLIHelp(t *testing.T) {
	commands := []string{"export", "import", "migrate", "verify"}
	helpFlags := []struct {
		name string
		arg  string
	}{
		{name: "short", arg: "-h"},
		{name: "long", arg: "--help"},
	}
	for _, command := range commands {
		for _, helpFlag := range helpFlags {
			t.Run(command+"/"+helpFlag.name, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				if err := run(context.Background(), []string{command, helpFlag.arg}, &stdout, &stderr); err != nil {
					t.Fatalf("run(%s %s): %v", command, helpFlag.arg, err)
				}
				if stdout.Len() != 0 {
					t.Fatalf("run(%s %s) wrote stdout %q", command, helpFlag.arg, stdout.String())
				}
				if want := "Usage of " + command + ":"; !strings.Contains(stderr.String(), want) {
					t.Fatalf("run(%s %s) stderr does not contain %q: %q", command, helpFlag.arg, want, stderr.String())
				}
				if strings.Contains(stderr.String(), flag.ErrHelp.Error()) {
					t.Fatalf("run(%s %s) reported help as an error: %q", command, helpFlag.arg, stderr.String())
				}
			})
		}
	}
}

func TestCLIVerifyInputModes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing source", args: []string{"verify"}, want: "exactly one"},
		{name: "both sources", args: []string{"verify", "--bundle", "bundle", "--source-chaindata", "source"}, want: "exactly one"},
		{name: "direct artifact required", args: []string{"verify", "--source-chaindata", "source"}, want: "--artifact is required"},
		{name: "migrate positional", args: []string{"migrate", "unexpected"}, want: "does not accept positional"},
		{name: "migrate has no compression", args: []string{"migrate", "--compression", "none"}, want: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(context.Background(), tt.args, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run(%v) error is %v, want %q", tt.args, err, tt.want)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("go-ethereum v1.17.5")) {
		t.Fatalf("unexpected version output %q", stdout.String())
	}
}

func assertJSON(t *testing.T, data []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("invalid JSON output %q: %v", data, err)
	}
}

func assertHexJSONField(t *testing.T, data []byte, path string, byteLength int) {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode JSON for %s: %v", path, err)
	}
	for component := range strings.SplitSeq(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("JSON path %s does not contain an object at %s", path, component)
		}
		value, ok = object[component]
		if !ok {
			t.Fatalf("JSON path %s is missing %s", path, component)
		}
	}
	hexValue, ok := value.(string)
	if !ok {
		t.Fatalf("JSON path %s is %T, want string", path, value)
	}
	if !strings.HasPrefix(hexValue, "0x") || hexValue != strings.ToLower(hexValue) {
		t.Fatalf("JSON path %s is not canonical 0x-prefixed hex: %q", path, hexValue)
	}
	decoded, err := hex.DecodeString(hexValue[2:])
	if err != nil {
		t.Fatalf("JSON path %s is not hexadecimal: %v", path, err)
	}
	if byteLength >= 0 && len(decoded) != byteLength {
		t.Fatalf("JSON path %s has %d bytes, want %d", path, len(decoded), byteLength)
	}
}

func assertProgressLog(t *testing.T, data, operation string, extra ...string) {
	t.Helper()
	wants := []string{
		"Migration operation started",
		"operation=" + operation,
		"status=completed",
	}
	wants = append(wants, extra...)
	for _, want := range wants {
		if !strings.Contains(data, want) {
			t.Fatalf("progress log for %s does not contain %q: %s", operation, want, data)
		}
	}
	if strings.Contains(data, "\x1b[") {
		t.Fatalf("progress log for %s contains ANSI color escapes: %q", operation, data)
	}
}

func loadGoldenSource(t *testing.T) string {
	t.Helper()
	fixturePath := filepath.Join("..", "..", "internal", "migration", "testdata", "legacy-l2geth-kv-v1.bin")
	file, err := os.Open(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close golden fixture: %v", err)
		}
	}()
	magic := make([]byte, 8)
	if _, err := io.ReadFull(file, magic); err != nil {
		t.Fatal(err)
	}
	if string(magic) != "L2GKV001" {
		t.Fatalf("invalid fixture magic %q", magic)
	}
	var count uint64
	if err := binary.Read(file, binary.BigEndian, &count); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chaindata")
	db, err := gethleveldb.New(path, 16, 16, "cli-fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	dbClosed := false
	defer func() {
		if !dbClosed {
			if err := db.Close(); err != nil {
				t.Errorf("close fixture database: %v", err)
			}
		}
	}()
	for i := uint64(0); i < count; i++ {
		var keyLen uint32
		var valueLen uint64
		if err := binary.Read(file, binary.BigEndian, &keyLen); err != nil {
			t.Fatal(err)
		}
		if err := binary.Read(file, binary.BigEndian, &valueLen); err != nil {
			t.Fatal(err)
		}
		if keyLen == 0 || keyLen > 1<<20 || valueLen > 128<<20 {
			t.Fatal("invalid fixture record length")
		}
		key := make([]byte, int(keyLen))
		value := make([]byte, int(valueLen))
		if _, err := io.ReadFull(file, key); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(file, value); err != nil {
			t.Fatal(err)
		}
		if err := db.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var probe [1]byte
	if n, err := file.Read(probe[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatal("fixture has trailing data")
	}
	closeErr := db.Close()
	dbClosed = true
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return path
}
