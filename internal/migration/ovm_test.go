package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/golang/snappy"
	"github.com/holiman/uint256"
)

type ovmFixture struct {
	source, code, witness string
	accounts              []fixtureAccount
	holders               []common.Address
	root                  common.Hash
	head                  *types.Header
	receipts              []byte
}

func newOVMFixture(t testing.TB, change func([]fixtureAccount)) ovmFixture {
	return newOVMFixtureSized(t, 8, change)
}

func newOVMFixtureSized(t testing.TB, holders int, change func([]fixtureAccount)) ovmFixture {
	t.Helper()
	dir := t.TempDir()
	f := ovmFixture{source: filepath.Join(dir, "source"), code: filepath.Join(dir, "wrapped.hex"), witness: filepath.Join(dir, "witness.jsonl")}
	for n := 1; n <= holders; n++ {
		f.holders = append(f.holders, common.BigToAddress(big.NewInt(int64(n+1000))))
	}
	storage := map[common.Hash]common.Hash{common.HexToHash("0x02"): common.BigToHash(big.NewInt(int64(308 + holders - 8))), common.HexToHash("0x03"): common.HexToHash("0x4d657469730000000000000000000000000000000000000000000000000000000a")}
	for n, amount := range []int64{11, 22, 33, 44, 55, 66} {
		storage[ovmBalanceSlot(f.holders[n])] = common.BigToHash(big.NewInt(amount))
	}
	storage[ovmBalanceSlot(ovmETHAddress)] = common.BigToHash(big.NewInt(77))
	inner := crypto.Keccak256Hash(common.LeftPadBytes(f.holders[1][:], 32), common.LeftPadBytes([]byte{1}, 32))
	allowance := crypto.Keccak256Hash(common.LeftPadBytes(f.holders[3][:], 32), inner[:])
	storage[allowance] = common.HexToHash("0x1234")
	for n := 8; n < holders; n++ {
		storage[ovmBalanceSlot(f.holders[n])] = common.HexToHash("0x01")
	}
	f.accounts = append(f.accounts, fixtureAccount{address: ovmETHAddress, balance: new(uint256.Int), nonce: 1, code: []byte{0x60, 0x00, 0x00}, storage: storage})
	for n, address := range f.holders {
		if n == 4 {
			continue
		} // A positive OVM balance without an account leaf.
		a := fixtureAccount{address: address, balance: new(uint256.Int), nonce: uint64(n + 1)}
		if n == 1 || n == 2 || n == 3 {
			a.code = []byte{0x60, 0x01, 0x00}
		}
		if n == 7 {
			a.code = f.accounts[0].code
		} // old OVM code is still referenced.
		f.accounts = append(f.accounts, a)
	}
	if change != nil {
		change(f.accounts)
	}
	kv, err := gethleveldb.New(f.source, 16, 16, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	f.root = writeOVMFixtureState(t, db, f.accounts)
	genesis := &types.Header{UncleHash: types.EmptyUncleHash, Root: f.root, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash, Difficulty: big.NewInt(1), Number: new(big.Int), GasLimit: 30_000_000, Time: 1}
	rawdb.WriteHeader(db, genesis)
	rawdb.WriteCanonicalHash(db, genesis.Hash(), 0)
	putOVMTest(t, db, ovmReceiptKey(genesis), []byte{0xc0})
	logs := []*types.Log{
		ovmTestEvent(ovmTransferTopic, f.holders[1], f.holders[3], 1),
		ovmTestEvent(ovmTransferTopic, f.holders[0], f.holders[3], 1),
		ovmTestEvent(ovmApprovalTopic, f.holders[1], f.holders[3], 0x1234),
	}
	r := &types.Receipt{Status: 1, CumulativeGasUsed: 21000, Logs: logs}
	r.Bloom = types.CreateBloom(r)
	f.receipts = encodeOVMTestReceipts(t, logs)
	tx := types.NewTx(&types.LegacyTx{GasPrice: big.NewInt(1), Gas: 21000, To: &ovmETHAddress, Value: new(big.Int)})
	f.head = &types.Header{ParentHash: genesis.Hash(), UncleHash: types.EmptyUncleHash, Root: f.root, TxHash: types.DeriveSha(types.Transactions{tx}, trie.NewStackTrie(nil)), ReceiptHash: types.DeriveSha(types.Receipts{r}, trie.NewStackTrie(nil)), Bloom: r.Bloom, Difficulty: big.NewInt(1), Number: big.NewInt(1), GasLimit: 30_000_000, GasUsed: 21000, Time: 2}
	rawdb.WriteHeader(db, f.head)
	rawdb.WriteCanonicalHash(db, f.head.Hash(), 1)
	rawdb.WriteHeadBlockHash(db, f.head.Hash())
	rawdb.WriteHeadHeaderHash(db, f.head.Hash())
	putOVMTest(t, db, ovmReceiptKey(f.head), f.receipts)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := os.ReadFile("testdata/ovm-conversion/wrapped.hex")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.code, runtime, 0600); err != nil {
		t.Fatal(err)
	}
	var witness bytes.Buffer
	for _, address := range f.holders {
		fmt.Fprintf(&witness, "{\"type\":\"address\",\"address\":%q}\n", address.Hex())
	}
	if err := os.WriteFile(f.witness, witness.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeOVMFixtureState(t testing.TB, db ethdb.Database, accounts []fixtureAccount) common.Hash {
	t.Helper()
	type leaf struct{ k, v []byte }
	var leaves []leaf
	write := func(_ []byte, h common.Hash, blob []byte) { putOVMTest(t, db, h[:], blob) }
	for _, a := range accounts {
		var slots []leaf
		for slot, value := range a.storage {
			if value == (common.Hash{}) {
				continue
			}
			hash := ovmStorageHash(slot)
			encoded, err := rlp.EncodeToBytes(bytes.TrimLeft(value[:], "\x00"))
			if err != nil {
				t.Fatal(err)
			}
			slots = append(slots, leaf{hash.Bytes(), encoded})
		}
		slices.SortFunc(slots, func(a, b leaf) int { return bytes.Compare(a.k, b.k) })
		storage := trie.NewStackTrie(write)
		for _, slot := range slots {
			if err := storage.Update(slot.k, slot.v); err != nil {
				t.Fatal(err)
			}
		}
		codeHash := types.EmptyCodeHash
		if len(a.code) > 0 {
			codeHash = crypto.Keccak256Hash(a.code)
			putOVMTest(t, db, codeHash[:], a.code)
		}
		account := types.StateAccount{Nonce: a.nonce, Balance: a.balance, Root: storage.Hash(), CodeHash: codeHash.Bytes()}
		blob, err := rlp.EncodeToBytes(&account)
		if err != nil {
			t.Fatal(err)
		}
		leaves = append(leaves, leaf{crypto.Keccak256(a.address[:]), blob})
	}
	slices.SortFunc(leaves, func(a, b leaf) int { return bytes.Compare(a.k, b.k) })
	stack := trie.NewStackTrie(write)
	for _, leaf := range leaves {
		if err := stack.Update(leaf.k, leaf.v); err != nil {
			t.Fatal(err)
		}
	}
	return stack.Hash()
}

func putOVMTest(t testing.TB, db ethdb.KeyValueWriter, key, value []byte) {
	t.Helper()
	if err := db.Put(key, value); err != nil {
		t.Fatal(err)
	}
}
func ovmReceiptKey(h *types.Header) []byte {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], h.Number.Uint64())
	return append(append([]byte{'r'}, n[:]...), h.Hash().Bytes()...)
}
func ovmTestEvent(topic common.Hash, from, to common.Address, amount uint64) *types.Log {
	return &types.Log{Address: ovmETHAddress, Topics: []common.Hash{topic, common.BytesToHash(from[:]), common.BytesToHash(to[:])}, Data: common.LeftPadBytes(new(big.Int).SetUint64(amount).Bytes(), 32)}
}
func encodeOVMTestReceipts(t testing.TB, logs []*types.Log) []byte {
	t.Helper()
	blob, err := rlp.EncodeToBytes([]any{[]any{[]byte{1}, uint64(21000), logs, new(big.Int), new(big.Int), new(big.Int), ""}})
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func (f ovmFixture) options(t testing.TB, engine, scheme string, workers int) MigrateOptions {
	t.Helper()
	return MigrateOptions{SourceChaindata: f.source, Output: filepath.Join(t.TempDir(), "artifact"), Scheme: scheme, DBEngine: engine, CacheMB: 64, Handles: 64, Workers: workers, OVM: OVMOptions{Enabled: true, WrappedEtherCode: f.code, StateWitness: f.witness}}
}

func TestOVMMigrateAllTargets(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMMigrateAllTargets(t, mode) })
	}
}

func testOVMMigrateAllTargets(t *testing.T, tempMode TempDBMode) {
	f := newOVMFixture(t, nil)
	before := directoryContentDigest(t, f.source)
	expected := ovmReferenceRoot(t, f)
	for _, engine := range []string{DBEnginePebble, DBEngineLevelDB} {
		for _, scheme := range []string{"hash", "path"} {
			t.Run(engine+"/"+scheme, func(t *testing.T) {
				opts := f.options(t, engine, scheme, 4)
				opts.TempDB = tempMode
				result, err := Migrate(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				r := result.OVMReport
				if r == nil || r.Target.Root != expected || r.Target.Root == f.root {
					t.Fatalf("unexpected root/report: %+v expected %s", r, expected)
				}
				if r.Balances.MigratedNative.Uint64() != 209 || r.Balances.RemainingSupply.Uint64() != 99 || r.Balances.EOAs != 3 || r.Balances.ConvertedContracts != 2 || r.Balances.RetainedContracts != 1 {
					t.Fatalf("balances: %+v", r.Balances)
				}
				if r.Checkpoint.HeadBefore.BlockNumber != 2 || r.History.Transfers != 2 {
					t.Fatalf("bad checkpoint/history: %+v", r)
				}
				got, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDB: oppositeTempMode(tempMode), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 3, OVM: opts.OVM})
				if err != nil {
					t.Fatal(err)
				}
				if got.Target.Root != expected {
					t.Fatal("verification root mismatch")
				}
				if _, err := loadDirectVerificationReport(opts.Output); err == nil {
					t.Fatal("ordinary report accepted OVM format")
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(encoded, []byte(OVMVerificationFormat)) {
					t.Fatal("JSON selected wrong report")
				}
			})
		}
	}
	if before != directoryContentDigest(t, f.source) {
		t.Fatal("source changed")
	}
}

func ovmReferenceRoot(t testing.TB, f ovmFixture) common.Hash {
	return ovmReferenceWithAlloc(t, f, nil)
}

func ovmReferenceWithAlloc(t testing.TB, f ovmFixture, apply func(*state.StateDB)) common.Hash {
	return ovmReferenceWithAllocDB(t, f, apply, nil)
}

func ovmReferenceWithAllocDB(t testing.TB, f ovmFixture, apply func(*state.StateDB), visit func(ethdb.Database, common.Hash)) common.Hash {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	root := writeOVMFixtureState(t, db, f.accounts)
	for _, a := range f.accounts {
		if len(a.code) > 0 {
			rawdb.WriteCode(db, crypto.Keccak256Hash(a.code), a.code)
		}
	}
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	defer func() {
		if err := tdb.Close(); err != nil {
			t.Error(err)
		}
	}()
	s, err := state.New(root, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 2, 3, 4, 5} {
		s.SetBalance(f.holders[n], uint256.NewInt(uint64((n+1)*11)), tracing.BalanceChangeUnspecified)
		s.SetState(ovmETHAddress, ovmBalanceSlot(f.holders[n]), common.Hash{})
	}
	for _, address := range f.holders[8:] {
		s.SetBalance(address, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
		s.SetState(ovmETHAddress, ovmBalanceSlot(address), common.Hash{})
	}
	s.SetBalance(ovmETHAddress, uint256.NewInt(99), tracing.BalanceChangeUnspecified)
	s.SetState(ovmETHAddress, common.HexToHash("0x02"), common.HexToHash("0x63"))
	runtime, err := os.ReadFile(f.code)
	if err != nil {
		t.Fatal(err)
	}
	code, err := hexutil.Decode(strings.TrimSpace(string(runtime)))
	if err != nil {
		t.Fatal(err)
	}
	s.SetCode(ovmETHAddress, code, tracing.CodeChangeUnspecified)
	if apply != nil {
		apply(s)
	}
	root, err = s.Commit(2, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if visit != nil {
		if err := tdb.Commit(root, false); err != nil {
			t.Fatal(err)
		}
		visit(db, root)
	}
	return root
}

func TestOVMRejectInvalidState(t *testing.T) {
	for _, test := range []struct {
		name, want string
		change     func([]fixtureAccount)
	}{
		{"native", "zero source native", func(a []fixtureAccount) { a[1].balance.SetUint64(1) }},
		{"supply", "totalSupply mismatch", func(a []fixtureAccount) { a[0].storage[common.HexToHash("0x02")] = common.HexToHash("0x01") }},
		{"unknown", "ownership is unknown", func(a []fixtureAccount) { a[0].storage[common.HexToHash("0x9876")] = common.HexToHash("0x01") }},
		{"missing_contract", "OVM_ETH contract", func(a []fixtureAccount) { a[0].code = nil }},
		{"overflow", "overflows uint256", func(a []fixtureAccount) {
			a[0].storage[ovmBalanceSlot(a[1].address)] = common.HexToHash("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newOVMFixture(t, test.change)
			opts := f.options(t, DBEnginePebble, "hash", 2)
			_, err := Migrate(t.Context(), opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v, want %q", err, test.want)
			}
			if _, err := os.Stat(opts.Output); !os.IsNotExist(err) {
				t.Fatal("failed migration published output")
			}
		})
	}
}

func TestOVMInputs(t *testing.T) {
	for _, data := range []string{"", "0x", "1234", "0x123", "0X00", "0xzz", "\"0x1234\""} {
		t.Run(fmt.Sprintf("%q", data), func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "code")
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readWrappedCode(t.Context(), p); err == nil {
				t.Fatal("invalid bytecode accepted")
			}
		})
	}
	for _, data := range []string{`{"type":"address","address":"0x01"}`, `{"type":"address","address":null}`, `{"type":"address","address":"0x0000000000000000000000000000000000000000","balance":1}`, `{"type":"allowance","owner":"0x0000000000000000000000000000000000000000"}`, `{"type":"retain"}`} {
		t.Run(data, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()
			defer func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			idx := newOVMIndex(db)
			defer idx.batch.Close()
			p := filepath.Join(t.TempDir(), "witness")
			if err := os.WriteFile(p, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadOVMWitness(t.Context(), p, idx); err == nil {
				t.Fatal("invalid witness accepted")
			}
		})
	}
}

func moveOVMGenesisToAncient(t *testing.T, f ovmFixture) {
	t.Helper()
	kv, err := gethleveldb.New(f.source, 16, 16, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	hash := rawdb.ReadCanonicalHash(db, 0)
	header := rawdb.ReadHeaderRLP(db, hash, 0)
	path := filepath.Join(f.source, "ancient")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for n, name := range []string{"hashes", "headers", "receipts"} {
		blob := [][]byte{hash[:], header, {0xc0}}[n]
		idxExt, dataExt := ".ridx", ".rdat"
		if n != 0 {
			idxExt, dataExt = ".cidx", ".cdat"
			blob = snappy.Encode(nil, blob)
		}
		var index [12]byte
		binary.BigEndian.PutUint32(index[8:], uint32(len(blob)))
		if err := os.WriteFile(filepath.Join(path, name+idxExt), index[:], 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, name+".0000"+dataExt), blob, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rawdb.DeleteCanonicalHash(db, 0)
	rawdb.DeleteHeader(db, hash, 0)
	var num [8]byte
	if err := db.Delete(append(append([]byte{'r'}, num[:]...), hash[:]...)); err != nil {
		t.Fatal(err)
	}
}

func TestOVMAncientAndWorkers(t *testing.T) {
	f := newOVMFixture(t, nil)
	moveOVMGenesisToAncient(t, f)
	before := directoryContentDigest(t, f.source)
	var expected common.Hash
	for workers := 2; workers <= 16; workers++ {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			r, err := Migrate(t.Context(), f.options(t, DBEnginePebble, "hash", workers))
			if err != nil {
				t.Fatal(err)
			}
			if expected == (common.Hash{}) {
				expected = r.OVMReport.Target.Root
			}
			if expected != r.OVMReport.Target.Root {
				t.Fatal("workers changed root")
			}
		})
	}
	if before != directoryContentDigest(t, f.source) {
		t.Fatal("ancient files changed")
	}
}

func TestOVMHistoryFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, ovmFixture)
	}{
		{"missing_receipts", func(t *testing.T, f ovmFixture) {
			editOVMSource(t, f, func(db ethdb.Database) {
				if err := db.Delete(ovmReceiptKey(f.head)); err != nil {
					t.Fatal(err)
				}
			})
		}},
		{"receipt_root", func(t *testing.T, f ovmFixture) {
			editOVMSource(t, f, func(db ethdb.Database) { putOVMTest(t, db, ovmReceiptKey(f.head), []byte{0xc0}) })
		}},
		{"ancient_truncated", func(t *testing.T, f ovmFixture) {
			moveOVMGenesisToAncient(t, f)
			if err := os.WriteFile(filepath.Join(f.source, "ancient", "headers.cdat"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(filepath.Join(f.source, "ancient", "headers.0000.cdat"), 1); err != nil {
				t.Fatal(err)
			}
		}},
		{"incomplete_witness", func(t *testing.T, f ovmFixture) {
			if err := os.WriteFile(f.witness, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newOVMFixture(t, nil)
			test.mutate(t, f)
			before := directoryContentDigest(t, f.source)
			_, err := Migrate(t.Context(), f.options(t, DBEnginePebble, "hash", 2))
			if err == nil {
				t.Fatal("bad history accepted")
			}
			if before != directoryContentDigest(t, f.source) {
				t.Fatal("failed scan mutated source")
			}
		})
	}
}

func editOVMSource(t *testing.T, f ovmFixture, fn func(ethdb.Database)) {
	t.Helper()
	kv, err := gethleveldb.New(f.source, 16, 16, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	fn(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOVMCancellation(t *testing.T) {
	f := newOVMFixture(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opts := f.options(t, DBEnginePebble, "path", 16)
	if _, err := Migrate(ctx, opts); err == nil {
		t.Fatal("cancelled migration succeeded")
	}
	if _, err := os.Stat(opts.Output); !os.IsNotExist(err) {
		t.Fatal("cancelled output published")
	}
}
