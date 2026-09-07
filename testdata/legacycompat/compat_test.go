package legacycompat

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MetisProtocol/mvm/l2geth/common"
	"github.com/MetisProtocol/mvm/l2geth/core/rawdb"
	"github.com/MetisProtocol/mvm/l2geth/core/state"
	"github.com/MetisProtocol/mvm/l2geth/ethdb"
	"github.com/MetisProtocol/mvm/l2geth/ethdb/leveldb"
	"github.com/MetisProtocol/mvm/l2geth/rollup/rcfg"
)

// This module deliberately uses the actual old state API, isolated from the
// root module's newer geth. The committed fixture is consumed, never regenerated.
func TestLegacyStateCompatibility(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "l2state")
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/l2state")
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build l2state: %v\n%s", err, out)
	}
	source := filepath.Join(root, "source")
	loadCanary(t, source)
	for _, mode := range []string{"direct", "none", "zstd"} {
		t.Run(mode, func(t *testing.T) {
			artifact := filepath.Join(root, mode)
			args := []string{"migrate", "--source-chaindata", source}
			if mode != "direct" {
				bundlePath := filepath.Join(root, "bundle-"+mode)
				runCLI(t, binaryPath, "export", "--source-chaindata", source, "--out", bundlePath, "--compression", mode)
				args = []string{"import", "--bundle", bundlePath}
			}
			args = append(args, "--out", artifact, "--db-engine", "leveldb", "--scheme", "hash", "--state-layout", "legacy-l2geth")
			runCLI(t, binaryPath, args...)
			// Work on a separate database copy; acceptance never mutates the artifact.
			copyPath := filepath.Join(t.TempDir(), "db")
			copyDB(t, filepath.Join(artifact, "chaindata"), copyPath)
			checkAndContinue(t, copyPath)
			if mode == "direct" {
				runCLI(t, binaryPath, "verify", "--source-chaindata", source, "--artifact", artifact)
			} else {
				runCLI(t, binaryPath, "verify", "--bundle", filepath.Join(root, "bundle-"+mode), "--artifact", artifact)
			}
		})
	}
}

func runCLI(t *testing.T, binaryPath string, args ...string) {
	t.Helper()
	args = append(args, "--cache-mb", "16", "--handles", "16", "--quiet")
	cmd := exec.Command(binaryPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("invalid CLI JSON: %s", stdout.String())
	}
}

func openDB(t *testing.T, path string) ethdb.Database {
	t.Helper()
	kv, err := leveldb.New(path, 16, 16, "legacycompat")
	if err != nil {
		t.Fatal(err)
	}
	return rawdb.NewDatabase(kv)
}

func checkAndContinue(t *testing.T, path string) map[string]any {
	t.Helper()
	disk := openDB(t, path)
	headHash := rawdb.ReadHeadBlockHash(disk)
	number := rawdb.ReadHeaderNumber(disk, headHash)
	if number == nil {
		t.Fatal("missing header number")
	}
	header := rawdb.ReadHeader(disk, headHash, *number)
	if header == nil {
		t.Fatal("old API cannot decode selected header")
	}
	if rawdb.ReadCanonicalHash(disk, *number) != headHash || rawdb.ReadHeadHeaderHash(disk) != headHash {
		t.Fatal("head metadata mismatch")
	}
	database := state.NewDatabase(disk)
	sdb, err := state.New(header.Root, database)
	if err != nil {
		t.Fatal(err)
	}
	contract := common.HexToAddress("0x2000000000000000000000000000000000000002")
	shared := common.HexToAddress("0x3000000000000000000000000000000000000003")
	code := common.FromHex("0x60016000556002600155")
	if sdb.GetNonce(contract) != 7 || !bytes.Equal(sdb.GetCode(contract), code) || !bytes.Equal(sdb.GetCode(shared), code) {
		t.Fatal("old API account/code read mismatch")
	}
	if got := sdb.GetState(contract, common.HexToHash("0x01")); got != common.HexToHash("0x1234") {
		t.Fatalf("storage read mismatch: %s", got)
	}
	rcfg.UsingOVM = false
	if sdb.GetBalance(contract).Sign() != 0 {
		t.Fatal("consensus balance changed")
	}
	rcfg.UsingOVM = true
	if sdb.GetBalance(contract).Cmp(big.NewInt(999)) != 0 {
		t.Fatal("OVM balance changed")
	}
	iterator := state.NewNodeIterator(sdb)
	for iterator.Next() {
	}
	if iterator.Error != nil {
		t.Fatal(iterator.Error)
	}
	if err := sdb.Error(); err != nil {
		t.Fatal(err)
	}
	sdb.SetNonce(contract, 8)
	sdb.SetState(contract, common.HexToHash("0x01"), common.HexToHash("0xbeef"))
	newCode := common.FromHex("0x6003600055")
	sdb.SetCode(contract, newCode)
	sdb.SetBalance(contract, big.NewInt(1001))
	newRoot, err := sdb.Commit(false)
	if err != nil {
		t.Fatal(err)
	}
	if newRoot == header.Root {
		t.Fatal("commit did not change root")
	}
	if err := database.TrieDB().Commit(newRoot, true); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	disk = openDB(t, path)
	defer func() {
		if err := disk.Close(); err != nil {
			t.Error(err)
		}
	}()
	sdb, err = state.New(newRoot, state.NewDatabase(disk))
	if err != nil {
		t.Fatal(err)
	}
	if sdb.GetNonce(contract) != 8 || sdb.GetState(contract, common.HexToHash("0x01")) != common.HexToHash("0xbeef") || !bytes.Equal(sdb.GetCode(contract), newCode) || !bytes.Equal(sdb.GetCode(shared), code) || sdb.GetBalance(contract).Cmp(big.NewInt(1001)) != 0 {
		t.Fatal("old API commit/reopen mismatch")
	}
	if err := sdb.Error(); err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"root": newRoot.Hex(), "nonce": sdb.GetNonce(contract),
		"balance": sdb.GetBalance(contract).String(), "storage": sdb.GetState(contract, common.HexToHash("0x01")).Hex(),
		"code": fmt.Sprintf("0x%x", sdb.GetCode(contract)), "shared_code": fmt.Sprintf("0x%x", sdb.GetCode(shared)),
	}
}

var compatDB = flag.String("compat-db", "", "new database copy for frozen compatibility continuation")
var compatResult = flag.String("compat-result", "", "new result JSON path")

func TestFrozenGethArtifactContinuation(t *testing.T) {
	if *compatDB == "" {
		t.Skip("invoked by root compatibility gate with a disposable database copy")
	}
	if *compatResult == "" {
		t.Fatal("compat-result is required")
	}
	result := checkAndContinue(t, *compatDB)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(*compatResult, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func loadCanary(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "migration", "testdata", "legacy-l2geth-kv-v1.bin"))
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(data)
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		t.Fatal(err)
	}
	if string(magic[:]) != "L2GKV001" {
		t.Fatal("bad fixture magic")
	}
	var count uint64
	if err := binary.Read(r, binary.BigEndian, &count); err != nil {
		t.Fatal(err)
	}
	db := openDB(t, path)
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	for i := uint64(0); i < count; i++ {
		var keyLen uint32
		var valueLen uint64
		if err := binary.Read(r, binary.BigEndian, &keyLen); err != nil {
			t.Fatal(err)
		}
		if err := binary.Read(r, binary.BigEndian, &valueLen); err != nil {
			t.Fatal(err)
		}
		if keyLen == 0 || keyLen > 1<<20 || valueLen > 128<<20 {
			t.Fatal("bad fixture length")
		}
		key, value := make([]byte, keyLen), make([]byte, valueLen)
		if _, err := io.ReadFull(r, key); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(r, value); err != nil {
			t.Fatal(err)
		}
		if err := db.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if r.Len() != 0 {
		t.Fatal("trailing fixture data")
	}
}

func copyDB(t *testing.T, source, dest string) {
	t.Helper()
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			t.Fatalf("non-regular database file %s", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, entry.Name()), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
