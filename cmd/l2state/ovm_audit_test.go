package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIOVMAudit(t *testing.T) {
	source := loadCLIOVMSource(t)
	fixture := filepath.Join("..", "..", "internal", "migration", "testdata", "ovm-conversion")
	summary, err := os.ReadFile(filepath.Join(fixture, "..", "ovm-audit", "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(t.TempDir(), "summary.json")
	if err := os.WriteFile(list, summary, 0600); err != nil {
		t.Fatal(err)
	}
	for _, quiet := range []bool{false, true} {
		t.Run(map[bool]string{false: "progress", true: "quiet"}[quiet], func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "artifact")
			args := []string{"migrate", "--source-chaindata", source, "--out", out, "--scheme", "path", "--migrate-ovm-eth", "--wrapped-ether-code", filepath.Join(fixture, "wrapped.hex"), "--ovm-state-witness", filepath.Join(fixture, "witness.jsonl"), "--ovm-audit-summary", list, "--cache-mb", "64", "--handles", "64", "--workers", "2"}
			if quiet {
				args = append(args, "--quiet", "--temp-db", "memory")
			}
			var stdout, stderr bytes.Buffer
			if err := run(t.Context(), args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			assertJSON(t, stdout.Bytes())
			if !bytes.Contains(stdout.Bytes(), []byte(`"audit_summary"`)) {
				t.Fatal("audit evidence missing")
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
			if err := run(t.Context(), append(verify, "--ovm-audit-summary="), &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "--ovm-audit-summary") || stdout.Len() != 0 {
				t.Fatalf("missing summary accepted: %v", err)
			}
			verify = append(verify, "--ovm-audit-summary", list)
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
		{"migrate", "--ovm-audit-summary", list},
		{"verify", "--bundle", "bundle", "--ovm-audit-summary", list},
		{"import", "--ovm-audit-summary", list},
		{"export", "--ovm-audit-summary", list},
		{"prune", "--ovm-audit-summary", list},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(t.Context(), args, &stdout, &stderr); err == nil || stdout.Len() != 0 {
			t.Fatalf("invalid audit scope accepted: %v", args)
		}
	}
	out := filepath.Join(t.TempDir(), "plain")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"migrate", "--source-chaindata", source, "--out", out, "--scheme", "hash", "--quiet"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run(t.Context(), []string{"verify", "--source-chaindata", source, "--artifact", out, "--ovm-audit-summary", list}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "require an OVM artifact") {
		t.Fatalf("ordinary verification accepted audit: %v", err)
	}
}

func TestCLIOVMAuditEmpty(t *testing.T) {
	source := loadCLIOVMSource(t)
	fixture := filepath.Join("..", "..", "internal", "migration", "testdata", "ovm-conversion")
	for _, ovm := range []bool{false, true} {
		for _, empty := range [][]string{{"--ovm-audit-summary="}, {"--ovm-audit-summary", ""}} {
			t.Run(fmt.Sprintf("ovm=%t/%d-args", ovm, len(empty)), func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "artifact")
				args := []string{"migrate", "--source-chaindata", source, "--out", out, "--scheme", "hash", "--cache-mb", "64", "--handles", "64", "--workers", "2", "--quiet"}
				verify := []string{"verify", "--source-chaindata", source, "--artifact", out, "--cache-mb", "64", "--handles", "64", "--workers", "2", "--quiet"}
				if ovm {
					inputs := []string{"--wrapped-ether-code", filepath.Join(fixture, "wrapped.hex"), "--ovm-state-witness", filepath.Join(fixture, "witness.jsonl")}
					args = append(args, "--migrate-ovm-eth")
					args = append(args, inputs...)
					verify = append(verify, inputs...)
				}
				var stdout, stderr bytes.Buffer
				if err := run(t.Context(), append(args, empty...), &stdout, &stderr); err != nil {
					t.Fatal(err)
				}
				assertJSON(t, stdout.Bytes())
				if bytes.Contains(stdout.Bytes(), []byte(`"audit_summary"`)) {
					t.Fatal("empty path recorded evidence")
				}
				stdout.Reset()
				if err := run(t.Context(), append(verify, empty...), &stdout, &stderr); err != nil {
					t.Fatal(err)
				}
				assertJSON(t, stdout.Bytes())
				if stderr.Len() != 0 {
					t.Fatal("quiet commands emitted progress")
				}
			})
		}
	}
}

func TestCLIOVMHistoricalAudit(t *testing.T) {
	source := loadCLIOVMSource(t)
	fixture := filepath.Join("..", "..", "internal", "migration", "testdata", "ovm-conversion")
	audit := filepath.Join(fixture, "..", "ovm-audit", "summary.json")
	kv, err := gethleveldb.New(source, 16, 16, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	hash := rawdb.ReadHeadBlockHash(db)
	number, ok := rawdb.ReadHeaderNumber(db, hash)
	if !ok {
		t.Fatal("missing head")
	}
	head := rawdb.ReadHeader(db, hash, number)
	if head == nil {
		t.Fatal("missing header")
	}
	next := &types.Header{ParentHash: hash, UncleHash: types.EmptyUncleHash, Root: head.Root, Number: new(big.Int).Add(head.Number, big.NewInt(1)), Difficulty: big.NewInt(1), Time: head.Time + 1, GasLimit: head.GasLimit, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash}
	rawdb.WriteHeader(db, next)
	rawdb.WriteCanonicalHash(db, next.Hash(), number+1)
	rawdb.WriteHeadBlockHash(db, next.Hash())
	rawdb.WriteHeadHeaderHash(db, next.Hash())
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], number+1)
	if err := db.Put(append(append([]byte{'r'}, encoded[:]...), next.Hash().Bytes()...), []byte{0xc0}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "artifact")
	inputs := []string{"--source-chaindata", source, "--wrapped-ether-code", filepath.Join(fixture, "wrapped.hex"), "--ovm-state-witness", filepath.Join(fixture, "witness.jsonl"), "--ovm-audit-summary", audit, "--cache-mb", "64", "--handles", "64", "--workers", "2", "--quiet"}
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), append([]string{"migrate", "--migrate-ovm-eth", "--scheme", "path", "--out", out}, inputs...), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	type auditReport struct {
		Audit struct {
			Head struct {
				Number uint64 `json:"block_number"`
			} `json:"audit_head"`
		} `json:"audit_summary"`
	}
	var result struct {
		Verification auditReport `json:"verification"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Verification.Audit.Head.Number != 1 {
		t.Fatal("CLI omitted historical head")
	}

	stdout.Reset()
	if err := run(t.Context(), append([]string{"verify", "--artifact", out}, inputs...), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	assertJSON(t, stdout.Bytes())
	var verified auditReport
	if err := json.Unmarshal(stdout.Bytes(), &verified); err != nil {
		t.Fatal(err)
	}
	if verified.Audit.Head.Number != 1 {
		t.Fatal("verify omitted historical head")
	}
	if stderr.Len() != 0 {
		t.Fatal("quiet command emitted progress")
	}
}
