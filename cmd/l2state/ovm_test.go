package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
)

func TestCLIOVMMigrationAndVerification(t *testing.T) {
	source := loadCLIOVMSource(t)
	fixture := filepath.Join("..", "..", "internal", "migration", "testdata", "ovm-conversion")
	for _, quiet := range []bool{false, true} {
		for _, withAlloc := range []bool{false, true} {
			t.Run(map[bool]string{false: "progress", true: "quiet"}[quiet]+"/"+map[bool]string{false: "plain", true: "alloc"}[withAlloc], func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "artifact")
				args := []string{"migrate", "--source-chaindata", source, "--out", out, "--scheme", "path", "--migrate-ovm-eth", "--wrapped-ether-code", filepath.Join(fixture, "wrapped.hex"), "--ovm-state-witness", filepath.Join(fixture, "witness.jsonl"), "--cache-mb", "64", "--handles", "64", "--workers", "2"}
				var alloc string
				if withAlloc {
					alloc = filepath.Join(t.TempDir(), "alloc.json")
					if err := os.WriteFile(alloc, []byte(`{"0000000000000000000000000000000000000001":{"code":"0x00","storage":{"01":"ff"}}}`), 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "--ovm-genesis-alloc", alloc)
				}
				if quiet {
					args = append(args, "--quiet")
				}
				var stdout, stderr bytes.Buffer
				if err := run(t.Context(), args, &stdout, &stderr); err != nil {
					t.Fatalf("%v\n%s", err, stderr.String())
				}
				assertJSON(t, stdout.Bytes())
				if !bytes.Contains(stdout.Bytes(), []byte("metis-l2state-ovm-verification")) {
					t.Fatal("wrong result format")
				}
				if bytes.Contains(stdout.Bytes(), []byte(`"genesis_alloc"`)) != withAlloc {
					t.Fatal("wrong alloc evidence presence")
				}
				if withAlloc && !quiet {
					assertProgressLog(t, stderr.String(), "migrate", "phase=apply_genesis_alloc")
				}
				if quiet {
					if stderr.Len() != 0 {
						t.Fatalf("quiet stderr: %s", stderr.String())
					}
				} else {
					assertProgressLog(t, stderr.String(), "migrate", "phase=scan_ovm_history", "phase=convert_ovm_balances", "phase=publish_artifact")
				}
				stdout.Reset()
				stderr.Reset()
				args = []string{"verify", "--source-chaindata", source, "--artifact", out, "--wrapped-ether-code", filepath.Join(fixture, "wrapped.hex"), "--ovm-state-witness", filepath.Join(fixture, "witness.jsonl"), "--cache-mb", "64", "--handles", "64", "--workers", "2", "--quiet"}
				if withAlloc {
					args = append(args, "--ovm-genesis-alloc", alloc)
				}
				if err := run(t.Context(), args, &stdout, &stderr); err != nil {
					t.Fatal(err)
				}
				assertJSON(t, stdout.Bytes())
				if stderr.Len() != 0 {
					t.Fatalf("quiet verify stderr: %s", stderr.String())
				}
			})
		}
	}
}

func loadCLIOVMSource(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "migration", "testdata", "ovm-conversion", "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]string
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source")
	db, err := gethleveldb.New(source, 16, 16, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range entries {
		key, err := hexutil.Decode(k)
		if err != nil {
			t.Fatal(err)
		}
		value, err := hexutil.Decode(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestCLIOVMFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"migrate", "--ovm-genesis-alloc", "alloc.json"}, "require --migrate-ovm-eth"},
		{[]string{"verify", "--bundle", "bundle", "--ovm-genesis-alloc", "alloc.json"}, "cannot be used with --bundle"},
		{[]string{"import", "--ovm-genesis-alloc", "alloc.json"}, "flag provided but not defined"},
		{[]string{"export", "--ovm-genesis-alloc", "alloc.json"}, "flag provided but not defined"},
		{[]string{"prune", "--ovm-genesis-alloc", "alloc.json"}, "flag provided but not defined"},
		{[]string{"migrate", "--migrate-ovm-eth"}, "requires --wrapped-ether-code"},
		{[]string{"migrate", "--wrapped-ether-code", "code.hex"}, "require --migrate-ovm-eth"},
		{[]string{"migrate", "--migrate-ovm-eth=false", "--ovm-state-witness", "w.jsonl"}, "require --migrate-ovm-eth"},
		{[]string{"verify", "--bundle", "bundle", "--wrapped-ether-code", "code.hex"}, "cannot be used with --bundle"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(t.Context(), tc.args, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if stdout.Len() != 0 {
			t.Fatal("failure wrote stdout JSON")
		}
	}
}

func TestCLIVerifyRejectsAllocForOrdinaryArtifact(t *testing.T) {
	source := loadCLIOVMSource(t)
	out := filepath.Join(t.TempDir(), "artifact")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"migrate", "--source-chaindata", source, "--out", out, "--scheme", "hash", "--quiet"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	err := run(t.Context(), []string{"verify", "--source-chaindata", source, "--artifact", out, "--ovm-genesis-alloc", "alloc.json"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "require an OVM artifact") || stdout.Len() != 0 {
		t.Fatalf("ordinary artifact accepted alloc: %v", err)
	}
}
