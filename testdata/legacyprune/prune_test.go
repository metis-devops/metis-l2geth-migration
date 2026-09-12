package legacyprune

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/MetisProtocol/mvm/l2geth/common"
	"github.com/MetisProtocol/mvm/l2geth/consensus/ethash"
	"github.com/MetisProtocol/mvm/l2geth/core"
	"github.com/MetisProtocol/mvm/l2geth/core/rawdb"
	"github.com/MetisProtocol/mvm/l2geth/core/state"
	"github.com/MetisProtocol/mvm/l2geth/core/types"
	"github.com/MetisProtocol/mvm/l2geth/core/vm"
	"github.com/MetisProtocol/mvm/l2geth/crypto"
	"github.com/MetisProtocol/mvm/l2geth/ethdb"
	"github.com/MetisProtocol/mvm/l2geth/params"
	"github.com/MetisProtocol/mvm/l2geth/rollup/rcfg"
)

var tool string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "legacy-prune-cli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tool = filepath.Join(dir, "l2state")
	args := []string{"build", "-o", tool}
	if os.Getenv("L2STATE_TEST_RACE") == "1" {
		args = append(args, "-race")
	}
	args = append(args, "./cmd/l2state")
	cmd := exec.Command("go", args...)
	cmd.Dir = "../.."
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	code := 0
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	} else {
		code = m.Run()
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func collectState(t *testing.T, db ethdb.Database, root common.Hash) map[string][]byte {
	t.Helper()
	s, err := state.New(root, state.NewDatabase(db))
	must(t, err)
	result := map[string][]byte{}
	it := state.NewNodeIterator(s)
	for it.Next() {
		if it.Hash != (common.Hash{}) {
			blob, err := db.Get(it.Hash[:])
			must(t, err)
			result[string(it.Hash[:])] = common.CopyBytes(blob)
		}
	}
	must(t, it.Error)
	return result
}

func allKV(t *testing.T, db ethdb.Database) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	it := db.NewIterator()
	defer it.Release()
	for it.Next() {
		result[string(it.Key())] = common.CopyBytes(it.Value())
	}
	must(t, it.Error())
	return result
}

func openDB(t *testing.T, path string) ethdb.Database {
	t.Helper()
	db, err := rawdb.NewLevelDBDatabase(path, 16, 16, "")
	must(t, err)
	return db
}

func TestCLIPruneLegacyStartupAndNextBlock(t *testing.T) {
	oldOVM := rcfg.UsingOVM
	rcfg.UsingOVM = false
	defer func() { rcfg.UsingOVM = oldOVM }()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	path := filepath.Join(parent, "chaindata")
	db := openDB(t, path)
	key, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	must(t, err)
	sender := crypto.PubkeyToAddress(key.PublicKey)
	contract := common.HexToAddress("0x1234")
	genesisSpec := &core.Genesis{Config: params.AllEthashProtocolChanges, GasLimit: 8000000, Difficulty: big.NewInt(131072), Alloc: core.GenesisAlloc{
		sender:   {Balance: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)},
		contract: {Balance: big.NewInt(1), Code: common.FromHex("0x60003560005500"), Storage: map[common.Hash]common.Hash{{}: common.HexToHash("0x99")}},
	}}
	genesis, err := genesisSpec.Commit(db)
	must(t, err)
	engine := ethash.NewFaker()
	defer func() { must(t, engine.Close()) }()
	blocks, receipts := core.GenerateChain(genesisSpec.Config, genesis, engine, db, 4, func(i int, b *core.BlockGen) {
		input := common.BigToHash(big.NewInt(int64(i + 1)))
		tx := types.NewTransaction(uint64(i), contract, big.NewInt(0), 100000, big.NewInt(1), input[:])
		tx, err := types.SignTx(tx, types.NewEIP155Signer(genesisSpec.Config.ChainID), key)
		must(t, err)
		b.AddTx(tx)
	})
	td := new(big.Int).Set(genesis.Difficulty())
	for i, b := range blocks {
		rawdb.WriteBlock(db, b)
		rawdb.WriteReceipts(db, b.Hash(), b.NumberU64(), receipts[i])
		td.Add(td, b.Difficulty())
		rawdb.WriteTd(db, b.Hash(), b.NumberU64(), new(big.Int).Set(td))
		rawdb.WriteCanonicalHash(db, b.Hash(), b.NumberU64())
		rawdb.WriteTxLookupEntries(db, b)
	}
	head := blocks[len(blocks)-1]
	rawdb.WriteHeadBlockHash(db, head.Hash())
	rawdb.WriteHeadHeaderHash(db, head.Hash())
	rawdb.WriteHeadFastBlockHash(db, head.Hash())
	rawdb.WriteHeadIndex(db, head.NumberU64()-1)
	rawdb.WriteHeadQueueIndex(db, 3)
	rawdb.WriteHeadBatchIndex(db, 2)
	rawdb.WriteHeadVerifiedIndex(db, head.NumberU64()-1)
	before := allKV(t, db)
	keep := collectState(t, db, head.Root())
	genesisNode, err := db.Get(genesis.Root().Bytes())
	must(t, err)
	keep[string(genesis.Root().Bytes())] = genesisNode
	must(t, db.Close())
	cmd := exec.Command(tool, "prune", "--chaindata", path, "--cache-mb", "32", "--handles", "32", "--quiet")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("prune CLI: %v\n%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("quiet stderr: %s", stderr.String())
	}
	var result struct {
		Verified bool `json:"verified"`
		Deleted  struct {
			Keys uint64 `json:"keys"`
		} `json:"deleted"`
	}
	must(t, json.Unmarshal(stdout.Bytes(), &result))
	if !result.Verified || result.Deleted.Keys == 0 {
		t.Fatalf("unexpected report %s", stdout.String())
	}
	db = openDB(t, path)
	after := allKV(t, db)
	var removed uint64
	for k, v := range before {
		_, retained := keep[k]
		deleted := len(k) == 32 && !retained && bytes.Equal(crypto.Keccak256(v), []byte(k))
		if deleted {
			removed++
			if _, ok := after[k]; ok {
				t.Fatalf("old key retained %x", k)
			}
		} else if !bytes.Equal(after[k], v) {
			t.Fatalf("protected key changed %x", k)
		}
	}
	if removed != result.Deleted.Keys || len(after) != len(before)-int(removed) {
		t.Fatal("unexpected physical key inventory")
	}
	got := collectState(t, db, head.Root())
	for k, v := range got {
		if !bytes.Equal(keep[k], v) {
			t.Fatalf("latest state changed %x", k)
		}
	}
	genesisState, err := state.New(genesis.Root(), state.NewDatabase(db))
	must(t, err)
	iter := state.NewNodeIterator(genesisState)
	for iter.Next() {
	}
	if iter.Error == nil {
		t.Fatal("expected pruned genesis descendants")
	}
	_, _, err = core.SetupGenesisBlock(db, genesisSpec)
	must(t, err)
	if rawdb.ReadHeadBlockHash(db) != head.Hash() {
		t.Fatal("genesis initialization reset head")
	}
	cache := &core.CacheConfig{TrieCleanLimit: 16, TrieDirtyDisabled: true}
	chain, err := core.NewBlockChain(db, cache, genesisSpec.Config, engine, vm.Config{}, nil)
	must(t, err)
	if chain.CurrentBlock().Hash() != head.Hash() {
		t.Fatal("startup lost head")
	}
	// Generate on a disposable in-memory copy so the next state is not already
	// present in the pruned database when InsertChain executes it.
	reference := rawdb.NewMemoryDatabase()
	for k, v := range after {
		must(t, reference.Put([]byte(k), v))
	}
	next, _ := core.GenerateChain(genesisSpec.Config, head, engine, reference, 1, func(_ int, b *core.BlockGen) {
		input := common.BigToHash(big.NewInt(5))
		tx := types.NewTransaction(4, contract, big.NewInt(0), 100000, big.NewInt(1), input[:])
		signed, err := types.SignTx(tx, types.NewEIP155Signer(genesisSpec.Config.ChainID), key)
		must(t, err)
		b.AddTx(signed)
	})
	must(t, reference.Close())
	n, err := chain.InsertChain(next)
	must(t, err)
	if chain.CurrentBlock().Hash() != next[0].Hash() {
		t.Fatalf("next block was not imported: returned=%d current=%s want=%s", n, chain.CurrentBlock().Hash(), next[0].Hash())
	}
	chain.Stop()
	must(t, db.Close())
	db = openDB(t, path)
	chain, err = core.NewBlockChain(db, cache, genesisSpec.Config, engine, vm.Config{}, nil)
	must(t, err)
	if chain.CurrentBlock().Hash() != next[0].Hash() {
		t.Fatal("restart lost next block")
	}
	st, err := chain.State()
	must(t, err)
	if st.GetState(contract, common.Hash{}) != common.BigToHash(big.NewInt(5)) {
		t.Fatal("contract storage changed")
	}
	must(t, st.Error())
	chain.Stop()
	must(t, db.Close())
}
