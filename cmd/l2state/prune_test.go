package main

import (
	"bytes"
	"context"
	"math/big"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
)

func TestCLIPrune(t *testing.T) {
	source, err := filepath.EvalSymlinks(loadGoldenSource(t))
	if err != nil {
		t.Fatal(err)
	}
	kv, err := gethleveldb.New(source, 16, 16, "", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	hash := rawdb.ReadHeadBlockHash(db)
	number, ok := rawdb.ReadHeaderNumber(db, hash)
	if !ok {
		t.Fatal("missing head")
	}
	h := rawdb.ReadHeader(db, hash, number)
	genesis := &types.Header{Root: h.Root, Number: big.NewInt(0), Difficulty: big.NewInt(1)}
	rawdb.WriteHeader(db, genesis)
	rawdb.WriteCanonicalHash(db, genesis.Hash(), 0)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, quiet := range []bool{false, true} {
		args := []string{"prune", "--chaindata", source, "--cache-mb", "32", "--handles", "32", "--workers", "4", "--dry-run"}
		if quiet {
			args = append(args, "--quiet")
		}
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, &stdout, &stderr); err != nil {
			t.Fatalf("prune: %v stderr=%s", err, stderr.String())
		}
		assertJSON(t, stdout.Bytes())
		if !strings.Contains(stdout.String(), `"format": "metis-l2state-prune"`) {
			t.Fatalf("unexpected output %s", stdout.String())
		}
		if quiet {
			if stderr.Len() != 0 {
				t.Fatalf("quiet progress %s", stderr.String())
			}
		} else {
			assertProgressLog(t, stderr.String(), "prune", "phase=collect", "phase=verify_keep", "phase=preflight")
		}
	}
}

func TestCLIPruneValidation(t *testing.T) {
	for _, args := range [][]string{
		{"prune"}, {"prune", "positional"}, {"prune", "--state-layout", "legacy-l2geth"},
		{"prune", "--dry-run", "--compact"}, {"prune", "--db-engine", "leveldb"}, {"prune", "--scheme", "hash"},
		{"prune", "--workers", "17"},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, &stdout, &stderr); err == nil {
			t.Fatalf("accepted %v", args)
		}
		if stdout.Len() != 0 {
			t.Fatalf("error emitted JSON %s", stdout.String())
		}
	}
}
