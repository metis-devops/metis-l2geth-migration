package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	gethleveldb "github.com/ethereum/go-ethereum/ethdb/leveldb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

type fixtureAccount struct {
	address common.Address
	nonce   uint64
	balance *uint256.Int
	code    []byte
	storage map[common.Hash]common.Hash
}

type legacyFixture struct {
	root      common.Hash
	head      bundle.Head
	accounts  []fixtureAccount
	headerRLP []byte
	chaindata string
}

func TestGoldenLegacyL2GethFixtureBothSchemes(t *testing.T) {
	chaindata := loadGoldenLegacyKV(t)
	expectedData, err := os.ReadFile(filepath.Join("testdata", "legacy-l2geth-expected-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		GeneratorModule    string        `json:"generator_module"`
		UsingOVM           bool          `json:"using_ovm"`
		AccountBalanceZero bool          `json:"account_balance_zero"`
		OVMETHSource       string        `json:"ovm_eth_source"`
		OVMETHCodeHash     common.Hash   `json:"ovm_eth_code_hash"`
		BlockNumber        uint64        `json:"block_number"`
		BlockHash          common.Hash   `json:"block_hash"`
		StateRoot          common.Hash   `json:"state_root"`
		HeaderRLP          hexutil.Bytes `json:"header_rlp"`
	}
	if err := json.Unmarshal(expectedData, &expected); err != nil {
		t.Fatal(err)
	}
	if expected.GeneratorModule != "github.com/MetisProtocol/mvm/l2geth" {
		t.Fatalf("unexpected fixture generator %q", expected.GeneratorModule)
	}
	if !expected.UsingOVM {
		t.Fatal("golden legacy fixture must enable OVM mode")
	}
	if !expected.AccountBalanceZero {
		t.Fatal("golden legacy fixture must keep ordinary account balances at zero")
	}
	if expected.OVMETHSource != "https://github.com/MetisProtocol/metis-networks/blob/696b5613df9cf23ecdb597b588b30f47c4f81c6c/andromeda-mainnet/state-dump.latest.json#L91-L99" {
		t.Fatalf("unexpected OVM_ETH allocation source %q", expected.OVMETHSource)
	}
	if expected.OVMETHCodeHash != common.HexToHash("0xcaf944c2c05ce2fa9f5fb1a832759082612937b4543c038b3c0648d693f57f44") {
		t.Fatalf("unexpected OVM_ETH code hash %s", expected.OVMETHCodeHash)
	}
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	exported, err := Export(context.Background(), ExportOptions{
		SourceChaindata: chaindata,
		Output:          bundleDir,
		Compression:     "none",
		CacheMB:         16,
		Handles:         16,
	})
	if err != nil {
		t.Fatalf("export golden legacy fixture: %v", err)
	}
	head := exported.Manifest.Source.HeadBefore
	if head.BlockNumber != expected.BlockNumber || head.BlockHash != expected.BlockHash || head.StateRoot != expected.StateRoot {
		t.Fatalf("golden evidence mismatch: have %+v expected %+v", head, expected)
	}
	if !bytes.Equal(exported.Manifest.Source.HeaderRLP, expected.HeaderRLP) {
		t.Fatal("golden header RLP mismatch")
	}
	if counts := exported.Manifest.Counts; counts.Accounts != 5 || counts.StorageSlots != 9 || counts.CodeReferences != 3 {
		t.Fatalf("golden OVM state shape mismatch: %+v", counts)
	}
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		artifact := filepath.Join(root, "artifact-"+scheme)
		if _, err := Import(context.Background(), ImportOptions{
			Bundle: bundleDir, Output: artifact, Scheme: scheme, CacheMB: 16, Handles: 16,
		}); err != nil {
			t.Fatalf("import golden fixture as %s: %v", scheme, err)
		}
		if _, err := Verify(context.Background(), VerifyOptions{
			Bundle: bundleDir, Artifact: artifact, CacheMB: 16, Handles: 16,
		}); err != nil {
			t.Fatalf("verify golden %s artifact: %v", scheme, err)
		}
		assertGoldenOVMState(t, artifact, scheme, head.StateRoot, expected.OVMETHCodeHash)
	}
}

func assertGoldenOVMState(t *testing.T, artifact, scheme string, root, expectedCodeHash common.Hash) {
	t.Helper()
	balances := map[common.Address]int64{
		common.HexToAddress("0x1000000000000000000000000000000000000001"): 100,
		common.HexToAddress("0x2000000000000000000000000000000000000002"): 999,
		common.HexToAddress("0x3000000000000000000000000000000000000003"): 55,
		common.HexToAddress("0x4000000000000000000000000000000000000004"): 0,
	}
	ovmETH := common.HexToAddress("0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000")
	withArtifactState(t, artifact, scheme, root, true, func(stateDB *state.StateDB) {
		if !stateDB.GetBalance(ovmETH).IsZero() {
			t.Fatalf("OVM_ETH ordinary account balance is non-zero: %s", stateDB.GetBalance(ovmETH))
		}
		code := stateDB.GetCode(ovmETH)
		if len(code) == 0 || crypto.Keccak256Hash(code) != expectedCodeHash {
			t.Fatalf("OVM_ETH code mismatch: length=%d hash=%s want=%s", len(code), crypto.Keccak256Hash(code), expectedCodeHash)
		}
		baseStorage := map[common.Hash]common.Hash{
			common.HexToHash("0x03"): common.HexToHash("0x4d6574697320546f6b656e000000000000000000000000000000000000000016"),
			common.HexToHash("0x04"): common.HexToHash("0x4d6574697300000000000000000000000000000000000000000000000000000a"),
			common.HexToHash("0x06"): common.HexToHash("0x0000000000000000000000004200000000000000000000000000000000000010"),
		}
		for key, want := range baseStorage {
			if have := stateDB.GetState(ovmETH, key); have != want {
				t.Fatalf("OVM_ETH base slot %s mismatch: have %s want %s", key, have, want)
			}
		}
		if have := stateDB.GetState(ovmETH, common.HexToHash("0x05")); have != (common.Hash{}) {
			t.Fatalf("OVM_ETH zero base slot is unexpectedly non-zero: %s", have)
		}
		for address, balance := range balances {
			if !stateDB.GetBalance(address).IsZero() {
				t.Fatalf("ordinary account balance for %s is non-zero: %s", address, stateDB.GetBalance(address))
			}
			key := ovmBalanceKey(address)
			want := common.BigToHash(big.NewInt(balance))
			if have := stateDB.GetState(ovmETH, key); have != want {
				t.Fatalf("OVM_ETH balance for %s mismatch: have %s want %s", address, have, want)
			}
		}
	})
}

func ovmBalanceKey(address common.Address) common.Hash {
	return crypto.Keccak256Hash(
		common.LeftPadBytes(address.Bytes(), common.HashLength),
		make([]byte, common.HashLength),
	)
}

func TestExportImportAndVerifyBothSchemes(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	exported, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          bundleDir,
		Compression:     "zstd",
		CacheMB:         32,
		Handles:         32,
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if exported.Manifest.Source.HeadBefore != fixture.head {
		t.Fatalf("head mismatch: have %+v want %+v", exported.Manifest.Source.HeadBefore, fixture.head)
	}
	if !bytes.Equal(exported.Manifest.Source.HeaderRLP, fixture.headerRLP) {
		t.Fatal("header RLP mismatch")
	}
	bundleReport, err := Verify(context.Background(), VerifyOptions{Bundle: bundleDir, CacheMB: 32, Handles: 32})
	if err != nil {
		t.Fatalf("verify bundle: %v", err)
	}
	if bundleReport.Scheme != "bundle" || bundleReport.RecomputedRoot != fixture.root {
		t.Fatalf("unexpected bundle report: %+v", bundleReport)
	}

	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		t.Run(scheme, func(t *testing.T) {
			artifact := filepath.Join(root, "artifact-"+scheme)
			imported, err := Import(context.Background(), ImportOptions{
				Bundle:  bundleDir,
				Output:  artifact,
				Scheme:  scheme,
				CacheMB: 32,
				Handles: 32,
			})
			if err != nil {
				t.Fatalf("import %s: %v", scheme, err)
			}
			if imported.Report.RecomputedRoot != fixture.root {
				t.Fatalf("imported root %s, want %s", imported.Report.RecomputedRoot, fixture.root)
			}
			if _, err := Verify(context.Background(), VerifyOptions{
				Bundle: bundleDir, Artifact: artifact, CacheMB: 32, Handles: 32,
			}); err != nil {
				t.Fatalf("verify artifact: %v", err)
			}
			assertArtifactState(t, artifact, scheme, fixture.root, fixture.accounts)
			newRoot := mutateAndCommitArtifact(t, artifact, scheme, fixture)
			if newRoot == fixture.root {
				t.Fatal("state mutation did not change the root")
			}
			assertArtifactNonce(t, artifact, scheme, newRoot, fixture.accounts[0].address, fixture.accounts[0].nonce+1)
		})
	}
}

func TestExportRejectsMissingLegacyCode(t *testing.T) {
	fixture := buildLegacyFixture(t)
	codeHash := crypto.Keccak256Hash(fixture.accounts[1].code)
	db, err := gethleveldb.New(fixture.chaindata, 16, 16, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(codeHash[:]); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          filepath.Join(t.TempDir(), "bundle"),
		Compression:     "none",
		CacheMB:         16,
		Handles:         16,
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("is missing")) {
		t.Fatalf("expected missing code error, got %v", err)
	}
}

func TestExportDoesNotMutateLegacyLevelDB(t *testing.T) {
	fixture := buildLegacyFixture(t)
	before := directoryContentDigest(t, fixture.chaindata)
	if _, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          filepath.Join(t.TempDir(), "bundle"),
		Compression:     "zstd",
		CacheMB:         16,
		Handles:         16,
	}); err != nil {
		t.Fatal(err)
	}
	after := directoryContentDigest(t, fixture.chaindata)
	if before != after {
		t.Fatalf("legacy source content changed: before %s after %s", before, after)
	}
}

func TestExportRejectsNonCanonicalHead(t *testing.T) {
	fixture := buildLegacyFixture(t)
	db, err := gethleveldb.New(fixture.chaindata, 16, 16, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	rawdb.WriteCanonicalHash(rawdb.NewDatabase(db), common.HexToHash("0xdeadbeef"), fixture.head.BlockNumber)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          filepath.Join(t.TempDir(), "bundle"),
		Compression:     "none",
		CacheMB:         16,
		Handles:         16,
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("not canonical")) {
		t.Fatalf("expected non-canonical head error, got %v", err)
	}
}

func TestVerifyRejectsCorruptedRecordFile(t *testing.T) {
	fixture := buildLegacyFixture(t)
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	result, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          bundleDir,
		Compression:     "none",
		CacheMB:         16,
		Handles:         16,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bundleDir, result.Manifest.StateFile.Name)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, 12); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), VerifyOptions{Bundle: bundleDir, CacheMB: 16, Handles: 16}); err == nil {
		t.Fatal("corrupted record file unexpectedly verified")
	}
}

func TestScanBundleRejectsStorageBeforeAccount(t *testing.T) {
	dir := t.TempDir()
	header := &types.Header{
		UncleHash: types.EmptyUncleHash, Root: types.EmptyRootHash,
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
		Difficulty: big.NewInt(1), Number: big.NewInt(1), GasLimit: 1, Time: 1,
	}
	headerRLP, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	head := bundle.Head{BlockNumber: 1, BlockHash: header.Hash(), StateRoot: header.Root}
	writer, err := bundle.NewWriter(dir, "none", head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	value := encodeStorageValue(t, common.HexToHash("0x01"))
	if err := writer.WriteStorage(common.HexToHash("0xaa"), common.HexToHash("0xbb"), value); err != nil {
		t.Fatal(err)
	}
	result, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest := bundle.NewManifest(bundle.SourceEvidence{
		HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP),
	}, result.Counts, bundle.StateFile{
		Name: result.FileName, Compression: result.Compression, Size: result.Size,
		SHA256: result.SHA256, RecordChainHash: result.RecordChainHash,
	})
	if _, err := bundle.WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanBundle(context.Background(), dir, nil); err == nil || !strings.Contains(err.Error(), "before an account") {
		t.Fatalf("expected semantic ordering error, got %v", err)
	}
}

func TestBundleSemanticScanLargeStream(t *testing.T) {
	const accountCount = 10_000
	account := types.NewEmptyStateAccount()
	accountRLP, err := rlp.EncodeToBytes(account)
	if err != nil {
		t.Fatal(err)
	}
	stack := trie.NewStackTrie(nil)
	for i := uint64(1); i <= accountCount; i++ {
		var key common.Hash
		binary.BigEndian.PutUint64(key[common.HashLength-8:], i)
		if err := stack.Update(key[:], accountRLP); err != nil {
			t.Fatal(err)
		}
	}
	root := stack.Hash()
	header := &types.Header{
		UncleHash: types.EmptyUncleHash, Root: root,
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
		Difficulty: big.NewInt(1), Number: big.NewInt(9), GasLimit: 1, Time: 1,
	}
	headerRLP, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	head := bundle.Head{BlockNumber: 9, BlockHash: header.Hash(), StateRoot: root}
	dir := t.TempDir()
	writer, err := bundle.NewWriter(dir, "zstd", head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= accountCount; i++ {
		var key common.Hash
		binary.BigEndian.PutUint64(key[common.HashLength-8:], i)
		if err := writer.WriteAccount(key, accountRLP); err != nil {
			t.Fatal(err)
		}
	}
	result, err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest := bundle.NewManifest(bundle.SourceEvidence{
		HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(headerRLP),
	}, result.Counts, bundle.StateFile{
		Name: result.FileName, Compression: result.Compression, Size: result.Size,
		SHA256: result.SHA256, RecordChainHash: result.RecordChainHash,
	})
	if _, err := bundle.WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	scanned, err := ScanBundle(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scanned.State.Root != root || scanned.State.Counts.Accounts != accountCount {
		t.Fatalf("unexpected scan result %+v", scanned.State)
	}
}

func TestOutputsMustNotExist(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newAtomicDir(existing); err == nil {
		t.Fatal("existing output path was accepted")
	}
}

func loadGoldenLegacyKV(t *testing.T) string {
	t.Helper()
	data, err := os.Open(filepath.Join("testdata", "legacy-l2geth-kv-v1.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := data.Close(); err != nil {
			t.Errorf("close golden fixture: %v", err)
		}
	}()
	magic := make([]byte, 8)
	if _, err := io.ReadFull(data, magic); err != nil {
		t.Fatal(err)
	}
	if string(magic) != "L2GKV001" {
		t.Fatalf("invalid golden fixture magic %q", magic)
	}
	var count uint64
	if err := binary.Read(data, binary.BigEndian, &count); err != nil {
		t.Fatal(err)
	}
	chaindata := filepath.Join(t.TempDir(), "chaindata")
	db, err := gethleveldb.New(chaindata, 16, 16, "golden-load", false)
	if err != nil {
		t.Fatal(err)
	}
	dbClosed := false
	defer func() {
		if !dbClosed {
			if err := db.Close(); err != nil {
				t.Errorf("close golden fixture database: %v", err)
			}
		}
	}()
	for i := uint64(0); i < count; i++ {
		var keyLen uint32
		var valueLen uint64
		if err := binary.Read(data, binary.BigEndian, &keyLen); err != nil {
			t.Fatal(err)
		}
		if err := binary.Read(data, binary.BigEndian, &valueLen); err != nil {
			t.Fatal(err)
		}
		if keyLen == 0 || keyLen > 1<<20 || valueLen > 128<<20 {
			t.Fatalf("invalid golden fixture record lengths key=%d value=%d", keyLen, valueLen)
		}
		key := make([]byte, int(keyLen))
		value := make([]byte, int(valueLen))
		if _, err := io.ReadFull(data, key); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(data, value); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(string(key), "secure-key-") {
			t.Fatal("golden fixture unexpectedly contains a preimage")
		}
		if err := db.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	var probe [1]byte
	if n, err := data.Read(probe[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatal("golden fixture has trailing data")
	}
	closeErr := db.Close()
	dbClosed = true
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return chaindata
}

func directoryContentDigest(t *testing.T, root string) string {
	t.Helper()
	hasher := sha256.New()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hasher.Write([]byte(rel))
		hasher.Write([]byte{0})
		hasher.Write(data)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func buildLegacyFixture(t *testing.T) legacyFixture {
	t.Helper()
	rootDir := t.TempDir()
	chaindata := filepath.Join(rootDir, "chaindata")
	kv, err := gethleveldb.New(chaindata, 32, 32, "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	accounts := []fixtureAccount{
		{
			address: common.HexToAddress("0x1000000000000000000000000000000000000001"),
			nonce:   1,
			balance: uint256.NewInt(100),
		},
		{
			address: common.HexToAddress("0x2000000000000000000000000000000000000002"),
			nonce:   7,
			balance: uint256.NewInt(999),
			code:    common.FromHex("0x60016000556002600155"),
			storage: map[common.Hash]common.Hash{
				common.HexToHash("0x01"): common.HexToHash("0x1234"),
				common.HexToHash("0x02"): common.HexToHash("0xffff"),
				common.HexToHash("0x10"): common.HexToHash("0x42"),
			},
		},
		{
			address: common.HexToAddress("0x3000000000000000000000000000000000000003"),
			nonce:   2,
			balance: uint256.NewInt(55),
			code:    common.FromHex("0x60016000556002600155"),
		},
	}
	type builtAccount struct {
		hash    common.Hash
		account types.StateAccount
	}
	built := make([]builtAccount, 0, len(accounts))
	for _, fixture := range accounts {
		accountHash := crypto.Keccak256Hash(fixture.address[:])
		storageRoot := types.EmptyRootHash
		if len(fixture.storage) > 0 {
			type slot struct {
				hash  common.Hash
				value common.Hash
			}
			slots := make([]slot, 0, len(fixture.storage))
			for key, value := range fixture.storage {
				slots = append(slots, slot{hash: crypto.Keccak256Hash(key[:]), value: value})
			}
			sort.Slice(slots, func(i, j int) bool { return bytes.Compare(slots[i].hash[:], slots[j].hash[:]) < 0 })
			stack := trie.NewStackTrie(nil)
			for _, slot := range slots {
				encoded := encodeStorageValue(t, slot.value)
				if err := stack.Update(slot.hash[:], encoded); err != nil {
					t.Fatal(err)
				}
				rawdb.WriteStorageSnapshot(disk, accountHash, slot.hash, encoded)
			}
			storageRoot = stack.Hash()
		}
		codeHash := types.EmptyCodeHash
		if len(fixture.code) > 0 {
			codeHash = crypto.Keccak256Hash(fixture.code)
			if err := disk.Put(codeHash[:], fixture.code); err != nil { // legacy unprefixed code key
				t.Fatal(err)
			}
		}
		account := types.StateAccount{
			Nonce:    fixture.nonce,
			Balance:  new(uint256.Int).Set(fixture.balance),
			Root:     storageRoot,
			CodeHash: codeHash.Bytes(),
		}
		rawdb.WriteAccountSnapshot(disk, accountHash, types.SlimAccountRLP(account))
		built = append(built, builtAccount{hash: accountHash, account: account})
	}
	sort.Slice(built, func(i, j int) bool { return bytes.Compare(built[i].hash[:], built[j].hash[:]) < 0 })
	accountStack := trie.NewStackTrie(nil)
	for _, account := range built {
		encoded, err := rlp.EncodeToBytes(&account.account)
		if err != nil {
			t.Fatal(err)
		}
		if err := accountStack.Update(account.hash[:], encoded); err != nil {
			t.Fatal(err)
		}
	}
	stateRoot := accountStack.Hash()
	stats, err := triedb.GenerateTrie(disk, rawdb.HashScheme, stateRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Updated != 0 || stats.Deleted != 0 {
		t.Fatalf("fixture state reconciled unexpectedly: %+v", stats)
	}
	if err := disk.DeleteRange(rawdb.SnapshotAccountPrefix, prefixLimit(rawdb.SnapshotAccountPrefix)); err != nil {
		t.Fatal(err)
	}
	if err := disk.DeleteRange(rawdb.SnapshotStoragePrefix, prefixLimit(rawdb.SnapshotStoragePrefix)); err != nil {
		t.Fatal(err)
	}
	header := &types.Header{
		ParentHash:  common.HexToHash("0xabc1"),
		UncleHash:   types.EmptyUncleHash,
		Coinbase:    common.HexToAddress("0x4000000000000000000000000000000000000004"),
		Root:        stateRoot,
		TxHash:      types.EmptyTxsHash,
		ReceiptHash: types.EmptyReceiptsHash,
		Difficulty:  big.NewInt(1),
		Number:      big.NewInt(12345),
		GasLimit:    30_000_000,
		GasUsed:     21_000,
		Time:        1_700_000_000,
		Extra:       []byte("legacy-l2geth-fixture"),
	}
	rawdb.WriteHeader(disk, header)
	rawdb.WriteCanonicalHash(disk, header.Hash(), header.Number.Uint64())
	rawdb.WriteHeadBlockHash(disk, header.Hash())
	rawdb.WriteHeadHeaderHash(disk, header.Hash())
	if err := disk.SyncKeyValue(); err != nil {
		t.Fatal(err)
	}
	headerRLP := rawdb.ReadHeaderRLP(disk, header.Hash(), header.Number.Uint64())
	if len(headerRLP) == 0 {
		t.Fatal("fixture header RLP missing")
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	return legacyFixture{
		root:      stateRoot,
		head:      bundle.Head{BlockNumber: header.Number.Uint64(), BlockHash: header.Hash(), StateRoot: stateRoot},
		accounts:  accounts,
		headerRLP: append([]byte(nil), headerRLP...),
		chaindata: chaindata,
	}
}

func encodeStorageValue(t *testing.T, value common.Hash) []byte {
	t.Helper()
	encoded, err := rlp.EncodeToBytes(common.TrimLeftZeroes(value[:]))
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func assertArtifactState(t *testing.T, artifact, scheme string, root common.Hash, accounts []fixtureAccount) {
	t.Helper()
	withArtifactState(t, artifact, scheme, root, true, func(sdb *state.StateDB) {
		for _, account := range accounts {
			if sdb.GetNonce(account.address) != account.nonce {
				t.Fatalf("account %s nonce mismatch", account.address)
			}
			if sdb.GetBalance(account.address).Cmp(account.balance) != 0 {
				t.Fatalf("account %s balance mismatch", account.address)
			}
			if !bytes.Equal(sdb.GetCode(account.address), account.code) {
				t.Fatalf("account %s code mismatch", account.address)
			}
			for key, value := range account.storage {
				if have := sdb.GetState(account.address, key); have != value {
					t.Fatalf("account %s slot %s mismatch: have %s want %s", account.address, key, have, value)
				}
			}
		}
	})
}

func assertArtifactNonce(t *testing.T, artifact, scheme string, root common.Hash, address common.Address, nonce uint64) {
	t.Helper()
	withArtifactState(t, artifact, scheme, root, true, func(sdb *state.StateDB) {
		if have := sdb.GetNonce(address); have != nonce {
			t.Fatalf("nonce after continuation mismatch: have %d want %d", have, nonce)
		}
	})
}

func withArtifactState(t *testing.T, artifact, scheme string, root common.Hash, readonly bool, fn func(*state.StateDB)) {
	t.Helper()
	kv, err := pebble.New(filepath.Join(artifact, "db"), 32, 32, "fixture-read", readonly)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	config := trieConfig(scheme, readonly)
	tdb := triedb.NewDatabase(disk, config)
	stateDB := state.NewDatabase(tdb, state.NewCodeDB(disk))
	sdb, err := state.New(root, stateDB)
	if err != nil {
		t.Fatal(err)
	}
	fn(sdb)
	if err := tdb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
}

func mutateAndCommitArtifact(t *testing.T, artifact, scheme string, fixture legacyFixture) common.Hash {
	t.Helper()
	kv, err := pebble.New(filepath.Join(artifact, "db"), 32, 32, "fixture-write", false)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	tdb := triedb.NewDatabase(disk, trieConfig(scheme, false))
	stateDB := state.NewDatabase(tdb, state.NewCodeDB(disk))
	sdb, err := state.New(fixture.root, stateDB)
	if err != nil {
		t.Fatal(err)
	}
	account := fixture.accounts[0]
	sdb.SetNonce(account.address, account.nonce+1, tracing.NonceChangeUnspecified)
	newRoot, err := sdb.Commit(fixture.head.BlockNumber+1, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tdb.Commit(newRoot, false); err != nil {
		t.Fatal(err)
	}
	if err := tdb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := disk.SyncKeyValue(); err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	return newRoot
}

func trieConfig(scheme string, readonly bool) *triedb.Config {
	if scheme == rawdb.HashScheme {
		config := *triedb.HashDefaults
		return &config
	}
	var config pathdb.Config
	if readonly {
		config = *pathdb.ReadOnly
	} else {
		config = *pathdb.Defaults
		config.EnableStateIndexing = false
		config.TrienodeHistory = -1
	}
	config.SnapshotNoBuild = true
	return &triedb.Config{PathDB: &config}
}
