package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIOVMRetention(t *testing.T) {
	source := loadCLIOVMSource(t)
	fixture := filepath.Join("..", "..", "internal", "migration", "testdata", "ovm-conversion")
	list := filepath.Join(t.TempDir(), "retain.txt")
	if err := os.WriteFile(list, []byte("0x00000000000000000000000000000000000003eb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, quiet := range []bool{false, true} {
		t.Run(map[bool]string{false: "progress", true: "quiet"}[quiet], func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "artifact")
			args := []string{"migrate", "--source-chaindata", source, "--out", out, "--scheme", "path", "--migrate-ovm-eth", "--wrapped-ether-code", filepath.Join(fixture, "wrapped.hex"), "--ovm-state-witness", filepath.Join(fixture, "witness.jsonl"), "--ovm-erc20-retain-list", list, "--cache-mb", "64", "--handles", "64", "--workers", "2"}
			if quiet {
				args = append(args, "--quiet", "--temp-db", "memory")
			}
			var stdout, stderr bytes.Buffer
			if err := run(t.Context(), args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			assertJSON(t, stdout.Bytes())
			if !bytes.Contains(stdout.Bytes(), []byte(`"erc20_retention"`)) {
				t.Fatal("retention evidence missing")
			}
			if quiet {
				if stderr.Len() != 0 {
					t.Fatal("quiet progress was not suppressed")
				}
			} else {
				assertProgressLog(t, stderr.String(), "migrate", "phase=convert_ovm_balances", "phase=publish_artifact")
			}
			stdout.Reset()
			stderr.Reset()
			verify := []string{"verify", "--source-chaindata", source, "--artifact", out, "--wrapped-ether-code", filepath.Join(fixture, "wrapped.hex"), "--ovm-state-witness", filepath.Join(fixture, "witness.jsonl"), "--cache-mb", "64", "--handles", "64", "--quiet"}
			if err := run(t.Context(), verify, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "--ovm-erc20-retain-list") || stdout.Len() != 0 {
				t.Fatalf("missing list accepted: %v", err)
			}
			verify = append(verify, "--ovm-erc20-retain-list", list)
			if err := run(t.Context(), verify, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			assertJSON(t, stdout.Bytes())
			if stderr.Len() != 0 {
				t.Fatal("quiet verification emitted progress")
			}
		})
	}
	for _, args := range [][]string{
		{"migrate", "--ovm-erc20-retain-list", list},
		{"verify", "--bundle", "bundle", "--ovm-erc20-retain-list", list},
		{"import", "--ovm-erc20-retain-list", list},
		{"export", "--ovm-erc20-retain-list", list},
		{"prune", "--ovm-erc20-retain-list", list},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(t.Context(), args, &stdout, &stderr); err == nil || stdout.Len() != 0 {
			t.Fatalf("invalid retention scope accepted: %v", args)
		}
	}
	out := filepath.Join(t.TempDir(), "plain")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"migrate", "--source-chaindata", source, "--out", out, "--scheme", "hash", "--quiet"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run(t.Context(), []string{"verify", "--source-chaindata", source, "--artifact", out, "--ovm-erc20-retain-list", list}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "require an OVM artifact") {
		t.Fatalf("ordinary verification accepted retention: %v", err)
	}
}
