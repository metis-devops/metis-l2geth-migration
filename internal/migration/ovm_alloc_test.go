package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm/runtime"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func writeAllocFile(t testing.TB, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alloc.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testAllocIndex(t testing.TB) *ovmIndex {
	t.Helper()
	db, err := openOVMDatabase(targetConfig{engine: "pebble-v2"}, filepath.Join(t.TempDir(), "index"), 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	index := newOVMIndex(db)
	t.Cleanup(func() {
		index.batch.Close()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return index
}

func TestOVMGenesisAllocGethEncoding(t *testing.T) {
	address := common.HexToAddress("0x1234567890123456789012345678901234567890")
	a := types.Account{Balance: big.NewInt(123), Nonce: 9, Code: []byte{0x60, 0x01}, Storage: map[common.Hash]common.Hash{common.HexToHash("0x01"): common.HexToHash("0xff")}}
	data, err := json.Marshal(types.GenesisAlloc{address: a})
	if err != nil {
		t.Fatal(err)
	}
	idx := testAllocIndex(t)
	digest, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, string(data)), idx)
	if err != nil || digest != common.Hash(sha256.Sum256(data)) {
		t.Fatalf("geth encoding: %s %v", digest, err)
	}
	hash := crypto.Keccak256Hash(address[:])
	blob, ok, err := idx.get(ovmAllocAccountPrefix, hash[:])
	if err != nil || !ok {
		t.Fatalf("missing account: %v", err)
	}
	var patch ovmAllocAccount
	if err := rlp.DecodeBytes(blob, &patch); err != nil {
		t.Fatal(err)
	}
	if patch.Fields != 15 || patch.Nonce != 9 || !bytes.Equal(patch.Code, a.Code) || new(big.Int).SetBytes(patch.Balance).Cmp(a.Balance) != 0 {
		t.Fatalf("geth fields lost: %+v", patch)
	}
}

func TestOVMGenesisAllocRejectsMalformedInput(t *testing.T) {
	address := "1234567890123456789012345678901234567890"
	account := func(fields string) string { return fmt.Sprintf(`{"%s":{%s}}`, address, fields) }
	for name, data := range map[string]string{
		"null": `null`, "array": `[]`, "full_genesis": `{"alloc":{}}`,
		"null_account": fmt.Sprintf(`{"%s":null}`, address),
		"unknown":      account(`"state":{}`), "private_key": account(`"secretKey":"0x01"`),
		"null_code": account(`"code":null`), "null_balance": account(`"balance":null`),
		"null_nonce": account(`"nonce":null`), "null_storage": account(`"storage":null`),
		"duplicate_field":    account(`"nonce":0,"nonce":1`),
		"duplicate_account":  fmt.Sprintf(`{"%s":{},"%s":{}}`, address, address),
		"alias_account":      fmt.Sprintf(`{"%s":{},"0x%s":{}}`, address, address),
		"case_alias_account": `{"abcdefabcdefabcdefabcdefabcdefabcdefabcd":{},"ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD":{}}`,
		"duplicate_slot":     account(`"storage":{"0x01":"01","0x01":"02"}`),
		"alias_slot":         account(`"storage":{"01":"01","0x0001":"02"}`),
		"case_alias_slot":    account(`"storage":{"aa":"01","AA":"02"}`),
		"bad_address":        `{"0x123":{}}`, "protected": fmt.Sprintf(`{"%s":{}}`, ovmETHAddress),
		"negative": account(`"balance":-1`), "fraction": account(`"balance":1.5`),
		"overflow_balance": account(`"balance":"0x1` + strings.Repeat("0", 64) + `"`),
		"overflow_nonce":   account(`"nonce":"0x10000000000000000"`),
		"bad_code":         account(`"code":"0x1"`), "unprefixed_code": account(`"code":"00"`),
		"large_code":   account(`"code":"0x` + strings.Repeat("00", ovmAllocMaxCode+1) + `"`),
		"huge_token":   account(`"code":"` + strings.Repeat("a", 6*(2*ovmAllocMaxCode+2)+3) + `"`),
		"odd_slot":     account(`"storage":{"0x1":"01"}`),
		"large_slot":   account(`"storage":{"` + strings.Repeat("00", 33) + `":"01"}`),
		"null_value":   account(`"storage":{"01":null}`),
		"number_value": account(`"storage":{"01":1}`),
		"large_value":  account(`"storage":{"01":"` + strings.Repeat("00", 33) + `"}`),
		"tail":         account(`"nonce":0`) + `{}`, "junk": account(`"nonce":0`) + `!`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, data), testAllocIndex(t)); err == nil {
				t.Fatal("invalid alloc accepted")
			}
		})
	}
}

func TestOVMGenesisAllocAllTargets(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMGenesisAllocAllTargets(t, mode) })
	}
}

func testOVMGenesisAllocAllTargets(t *testing.T, tempMode TempDBMode) {
	slot1, slot2 := common.HexToHash("0x01"), common.HexToHash("0x02")
	f := newOVMFixture(t, func(accounts []fixtureAccount) {
		accounts[3].storage = map[common.Hash]common.Hash{slot1: common.HexToHash("0x11"), slot2: common.HexToHash("0x22")}
	})
	before := directoryContentDigest(t, f.source)
	newAddress := common.Address{0x99}
	emptyAddress := common.Address{0x98}
	code := common.FromHex("0x602a60005260206000f3") // return uint256(42)
	data := fmt.Sprintf(`{
		"%s":{"code":%q,"storage":{"01":"07"}},
		"%s":{"code":"0x","balance":123,"nonce":0},
		"%s":{"storage":{"01":"00","03":"33"}},
		"%s":{"balance":0},
		"%s":{"storage":{}},
		"%s":{"code":%q,"balance":"0x09","nonce":3,"storage":{"ff":"aa"}},
		"%s":{}
	}`, f.holders[0], hexutil.Encode(code), f.holders[1], f.holders[2], f.holders[3], f.holders[4], newAddress, hexutil.Encode(code), emptyAddress)
	allocPath := writeAllocFile(t, data)
	apply := func(s *state.StateDB) {
		s.SetCode(f.holders[0], code, tracing.CodeChangeUnspecified)
		s.SetState(f.holders[0], slot1, common.HexToHash("0x07"))
		s.SetCode(f.holders[1], nil, tracing.CodeChangeUnspecified)
		s.SetBalance(f.holders[1], uint256.NewInt(123), tracing.BalanceChangeUnspecified)
		s.SetNonce(f.holders[1], 0, tracing.NonceChangeUnspecified)
		s.SetState(f.holders[2], slot1, common.Hash{})
		s.SetState(f.holders[2], common.HexToHash("0x03"), common.HexToHash("0x33"))
		s.SetBalance(f.holders[3], new(uint256.Int), tracing.BalanceChangeUnspecified)
		s.SetCode(newAddress, code, tracing.CodeChangeUnspecified)
		s.SetBalance(newAddress, uint256.NewInt(9), tracing.BalanceChangeUnspecified)
		s.SetNonce(newAddress, 3, tracing.NonceChangeUnspecified)
		s.SetState(newAddress, common.HexToHash("0xff"), common.HexToHash("0xaa"))
	}
	converted := ovmReferenceRoot(t, f)
	for _, scheme := range []string{"hash", "path"} {
		var reference []logicalEntry
		for _, engine := range []string{"pebble", "leveldb"} {
			t.Run(engine+"/"+scheme, func(t *testing.T) {
				opts := f.options(t, engine, scheme, 4)
				opts.TempDB = tempMode
				opts.OVM.GenesisAlloc = allocPath
				result, err := Migrate(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				r := result.OVMReport
				expected := ovmReferenceWithAllocDB(t, f, apply, func(db ethdb.Database, root common.Hash) {
					if reference == nil {
						reference = allocReferenceInventory(t, db, root, scheme, r.Checkpoint)
					}
				})
				if r.Target.Root != expected || r.GenesisAlloc == nil || r.GenesisAlloc.Converted.Root != converted {
					t.Fatalf("incorrect roots: target=%s expected=%s alloc=%+v", r.Target.Root, expected, r.GenesisAlloc)
				}
				if r.Balances.MigratedNative.Uint64() != 209 || r.Balances.RemainingSupply.Uint64() != 99 {
					t.Fatal("alloc affected original classification or balance evidence")
				}
				actual := readLogicalDatabase(t, filepath.Join(opts.Output, "chaindata"), "alloc")
				if !reflect.DeepEqual(actual, reference) {
					t.Fatal("final inventory differs from independent StateDB/GenerateTrie reference")
				}
				if _, err := VerifyOVM(t.Context(), OVMVerifyOptions{TempDB: tempMode, SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 2, OVM: opts.OVM}); err != nil {
					t.Fatal(err)
				}
				withArtifactState(t, opts.Output, scheme, expected, true, func(s *state.StateDB) {
					if s.GetBalance(f.holders[0]).Uint64() != 11 || s.GetState(f.holders[2], slot2) != common.HexToHash("0x22") || s.Exist(emptyAddress) {
						t.Fatal("omitted values or empty account semantics changed")
					}
					output, _, err := runtime.Call(newAddress, nil, &runtime.Config{State: s, GasLimit: 100000})
					if err != nil || new(big.Int).SetBytes(output).Uint64() != 42 {
						t.Fatalf("installed code execution: %x %v", output, err)
					}
				})
				next := commitOVMContinuation(t, opts, expected, newAddress)
				withArtifactState(t, opts.Output, scheme, next, true, func(s *state.StateDB) {
					if s.GetBalance(newAddress).Uint64() != 12 || !bytes.Equal(s.GetCode(newAddress), code) || s.GetState(newAddress, common.HexToHash("0xff")) != common.HexToHash("0xaa") {
						t.Fatal("alloc state did not survive continuation")
					}
				})
			})
		}
	}
	if before != directoryContentDigest(t, f.source) {
		t.Fatal("source changed")
	}
}

func allocReferenceInventory(t *testing.T, source ethdb.Database, root common.Hash, scheme string, checkpoint bundle.SourceEvidence) []logicalEntry {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	tdb := triedb.NewDatabase(source, triedb.HashDefaults)
	defer func() {
		if err := tdb.Close(); err != nil {
			t.Error(err)
		}
	}()
	writer := newReferenceFlatWriter(db)
	defer writer.Abort()
	accounts, err := ovmIterator(tdb, trie.StateTrieID(root))
	if err != nil {
		t.Fatal(err)
	}
	for accounts.Next() {
		owner := common.BytesToHash(accounts.Key)
		a, err := decodeFullAccount(owner, accounts.Value)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Account(owner, a, accounts.Value); err != nil {
			t.Fatal(err)
		}
		storage, err := ovmIterator(tdb, trie.StorageTrieID(root, owner, a.Root))
		if err != nil {
			t.Fatal(err)
		}
		for storage.Next() {
			if err := writer.Storage(owner, common.BytesToHash(storage.Key), storage.Value); err != nil {
				t.Fatal(err)
			}
		}
		if storage.Err != nil {
			t.Fatal(storage.Err)
		}
		hash := common.BytesToHash(a.CodeHash)
		if hash != types.EmptyCodeHash {
			if err := writer.Code(owner, hash, rawdb.ReadCode(source, hash)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if accounts.Err != nil {
		t.Fatal(accounts.Err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := triedb.GenerateTrie(db, scheme, root, nil); err != nil {
		t.Fatal(err)
	}
	if scheme == "hash" {
		if err := removeFlatStateForTest(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	} else if err := adoptPathStateDatabase(t.Context(), db, root); err != nil {
		t.Fatal(err)
	}
	if err := writeHeadMetadata(db, checkpoint); err != nil {
		t.Fatal(err)
	}
	for key, value := range ovmBodyMetadata(checkpoint) {
		putOVMTest(t, db, []byte(key), value)
	}
	return collectMemoryEntries(t, db)
}

func TestOVMGenesisAllocLargeStorageAndDuplicates(t *testing.T) {
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		t.Run(string(mode), func(t *testing.T) { testOVMGenesisAllocLargeStorageAndDuplicates(t, mode) })
	}
}

func testOVMGenesisAllocLargeStorageAndDuplicates(t *testing.T, tempMode TempDBMode) {
	f := newOVMFixture(t, nil)
	var fields strings.Builder
	for n := range 10000 {
		if n > 0 {
			fields.WriteByte(',')
		}
		fmt.Fprintf(&fields, `"%064x":"%064x"`, n, n+1)
	}
	data := fmt.Sprintf(`{"%s":{"storage":{%s}}}`, f.holders[0], fields.String())
	for _, suffix := range []string{fmt.Sprintf(`,"%s":{}`, f.holders[0]), ""} {
		duplicate := strings.TrimSuffix(data, "}") + suffix + "}"
		if suffix == "" {
			duplicate = strings.Replace(data, `}}}`, `,"00":"01"}}}`, 1)
		}
		if _, err := loadOVMGenesisAlloc(t.Context(), writeAllocFile(t, duplicate), testAllocIndex(t)); err == nil {
			t.Fatal("duplicate after batch flush was accepted")
		}
	}
	opts := f.options(t, "pebble", "path", 2)
	opts.TempDB = tempMode
	opts.OVM.GenesisAlloc = writeAllocFile(t, data)
	result, err := Migrate(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	expected := ovmReferenceWithAlloc(t, f, func(s *state.StateDB) {
		for n := range int64(10000) {
			s.SetState(f.holders[0], common.BigToHash(big.NewInt(n)), common.BigToHash(big.NewInt(n+1)))
		}
	})
	if expected != result.OVMReport.Target.Root {
		t.Fatal("large storage root differs")
	}
}

func TestOVMGenesisAllocNoOpAndInputStability(t *testing.T) {
	f := newOVMFixture(t, nil)
	for _, data := range []string{`{}`, fmt.Sprintf(`{"%s":{},"%s":{"storage":{}}}`, f.holders[0], common.Address{0xab})} {
		opts := f.options(t, "pebble", "hash", 2)
		opts.OVM.GenesisAlloc = writeAllocFile(t, data)
		result, err := Migrate(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if result.OVMReport.GenesisAlloc.Converted != result.OVMReport.Target {
			t.Fatal("no-op changed state")
		}
	}
	path := writeAllocFile(t, `{}`)
	idx := testAllocIndex(t)
	digest, err := loadOVMGenesisAlloc(t.Context(), path, idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{} `), 0600); err != nil {
		t.Fatal(err)
	}
	w := ovmWork{ctx: t.Context(), opts: MigrateOptions{OVM: OVMOptions{GenesisAlloc: path}}, inputs: ovmInputs{allocDigest: digest}}
	if err := w.confirmInputs(); err == nil || !strings.Contains(err.Error(), "GenesisAlloc input changed") {
		t.Fatalf("changed input accepted: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOVMGenesisAlloc(t.Context(), link, idx); err == nil {
		t.Fatal("symlink accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := loadOVMGenesisAlloc(ctx, path, idx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	reader := &ovmAllocReader{ctx: ctx, r: strings.NewReader(strings.Repeat(" ", 1<<20))}
	if _, err := io.Copy(io.Discard, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("reader cancellation: %v", err)
	}
}
