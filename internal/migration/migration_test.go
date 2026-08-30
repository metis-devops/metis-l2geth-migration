package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/ethereum/go-ethereum/ethdb"
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
		Compression:     bundle.CompressionNone,
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
	if counts := exported.Manifest.Counts; counts.Accounts != 5 || counts.StorageSlots != 9 || counts.CodeReferences != 3 || counts.CodeRecords != 2 {
		t.Fatalf("golden OVM state shape mismatch: %+v", counts)
	}
	assertNoTemporaryCodeHashIndexes(t, bundleDir)
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
		assertArtifactHeadMetadata(t, artifact, exported.Manifest.Source)
		assertGoldenOVMState(t, artifact, scheme, head.StateRoot, expected.OVMETHCodeHash)
		assertNoTemporaryCodeHashIndexes(t, artifact)
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
		Compression:     bundle.CompressionZstd,
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
	if counts := exported.Manifest.Counts; counts.CodeReferences != 2 || counts.CodeRecords != 1 {
		t.Fatalf("duplicate code was not deduplicated: %+v", counts)
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
			assertArtifactHeadMetadata(t, artifact, exported.Manifest.Source)
			assertArtifactState(t, artifact, scheme, fixture.root, fixture.accounts)
			newRoot := mutateAndCommitArtifact(t, artifact, scheme, fixture)
			if newRoot == fixture.root {
				t.Fatal("state mutation did not change the root")
			}
			assertArtifactNonce(t, artifact, scheme, newRoot, fixture.accounts[0].address, fixture.accounts[0].nonce+1)
		})
	}
}

func TestExportSharedCodeBundleCompatibility(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	expected := map[string]struct {
		size   int64
		sha256 common.Hash
	}{
		bundle.CompressionNone: {
			size:   621,
			sha256: common.HexToHash("0x0fd92aaf8536e48be3e64e6af4beb4d54ba42d6b8e8ef4b71693191b2edfc72a"),
		},
		bundle.CompressionZstd: {
			size:   430,
			sha256: common.HexToHash("0x476c95293b215b0724ee77c1cf61fb3f829b1180849033057abf211c404b7ae4"),
		},
	}
	wantChain := common.HexToHash("0x0f4be584bbf3af55cb97815b23cd7a44028af566104a655719ba305906d0ceeb")
	wantCounts := bundle.Counts{
		Accounts: 3, StorageSlots: 3, CodeReferences: 2,
		CodeRecords: 1, Records: 7, PayloadBytes: 229,
	}
	wantOrder := strings.Join([]string{
		"1:0x25baa1f53460dfe937af66419cef1b8dd5251c7daa1faf4061b53f21a5cd51e0:0x0000000000000000000000000000000000000000000000000000000000000000",
		"2:0x25baa1f53460dfe937af66419cef1b8dd5251c7daa1faf4061b53f21a5cd51e0:0x1b6847dc741a1b0cd08d278845f9d819d87b734759afb55fe2de5cb82a9ae672",
		"2:0x25baa1f53460dfe937af66419cef1b8dd5251c7daa1faf4061b53f21a5cd51e0:0x405787fa12a823e0f2b7631cc41b3ba8828b3321ca811111fa75cd3aa3bb5ace",
		"2:0x25baa1f53460dfe937af66419cef1b8dd5251c7daa1faf4061b53f21a5cd51e0:0xb10e2d527612073b26eecdfd717e6a320cf44b4afac2b0732d9fcbe2b7fa0cf6",
		"3:0x0000000000000000000000000000000000000000000000000000000000000000:0x8c4432ac99bfd0009e1a8e9a45d598159325c5d14ab1587f48c3021a467597e7",
		"1:0x3e164124bd9d00a221af88f9a0890dbf7cced84de0a4434b7b24ced06e63434d:0x0000000000000000000000000000000000000000000000000000000000000000",
		"1:0x3ed02be1e351ddbcc2bf3ffafc25fb42a533df024b33c85f9805e17b60f7230c:0x0000000000000000000000000000000000000000000000000000000000000000",
	}, ",")
	for _, compression := range []string{bundle.CompressionNone, bundle.CompressionZstd} {
		t.Run(compression, func(t *testing.T) {
			exported, err := Export(context.Background(), ExportOptions{
				SourceChaindata: fixture.chaindata,
				Output:          filepath.Join(root, "bundle-"+compression),
				Compression:     compression,
				CacheMB:         16,
				Handles:         16,
			})
			if err != nil {
				t.Fatal(err)
			}
			want := expected[compression]
			if stateFile := exported.Manifest.StateFile; stateFile.Size != want.size || stateFile.SHA256 != want.sha256 || stateFile.RecordChainHash != wantChain {
				t.Fatalf("bundle bytes changed: have size=%d sha256=%s chain=%s want size=%d sha256=%s chain=%s",
					stateFile.Size, stateFile.SHA256, stateFile.RecordChainHash,
					want.size, want.sha256, wantChain,
				)
			}
			if exported.Manifest.Counts != wantCounts {
				t.Fatalf("bundle counts changed: have %+v want %+v", exported.Manifest.Counts, wantCounts)
			}
			var recordOrder []string
			if _, err := bundle.ScanRecords(context.Background(), exported.BundlePath, exported.Manifest, func(record bundle.Record) error {
				recordOrder = append(recordOrder, fmt.Sprintf("%d:%s:%s", record.Type, record.AccountHash, record.SubHash))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if gotOrder := strings.Join(recordOrder, ","); gotOrder != wantOrder {
				t.Fatalf("bundle record order changed:\nhave %s\nwant %s", gotOrder, wantOrder)
			}
			assertNoTemporaryCodeHashIndexes(t, exported.BundlePath)
		})
	}
}

func TestTraverseStateReadsSharedCodeOnce(t *testing.T) {
	fixture := buildLegacyFixture(t)
	kv, err := gethleveldb.New(fixture.chaindata, 16, 16, "shared-code-read", true)
	if err != nil {
		t.Fatal(err)
	}
	disk := rawdb.NewDatabase(kv)
	defer func() {
		if err := disk.Close(); err != nil {
			t.Errorf("close shared-code database: %v", err)
		}
	}()
	trieDB := triedb.NewDatabase(disk, triedb.HashDefaults)
	defer func() {
		if err := trieDB.Close(); err != nil {
			t.Errorf("close shared-code trie database: %v", err)
		}
	}()
	var reads uint64
	result, _, err := traverseState(context.Background(), disk, trieDB, fixture.root, nil, false, stateTraversalOptions{
		CodeIndex: codeHashIndexOptions{Parent: t.TempDir(), CacheMB: 16, Handles: 16},
		ReadCode: func(db ethdb.KeyValueReader, hash common.Hash) []byte {
			reads++
			code, _ := db.Get(hash[:])
			return code
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != result.Counts.CodeRecords {
		t.Fatalf("read code %d times for %d unique records and %d references", reads, result.Counts.CodeRecords, result.Counts.CodeReferences)
	}
	if result.Counts.CodeReferences <= result.Counts.CodeRecords {
		t.Fatalf("fixture does not exercise shared code: %+v", result.Counts)
	}
}

func TestScanBundleCodeHashIndexSemanticsAndCleanup(t *testing.T) {
	for _, test := range []struct {
		name        string
		codeRecords []bool
		wantError   string
	}{
		{name: "later shared reference", codeRecords: []bool{true, false}},
		{name: "repeated code record", codeRecords: []bool{true, true}, wantError: "repeats code record"},
		{name: "code record provided too late", codeRecords: []bool{false, true}, wantError: "has not been provided"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := writeSharedCodeSemanticBundle(t, test.codeRecords)
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			result, err := ScanBundle(context.Background(), dir, nil)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if result.State.Counts.CodeReferences != 2 || result.State.Counts.CodeRecords != 1 {
					t.Fatalf("unexpected shared-code counts: %+v", result.State.Counts)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("expected %q error, got %v", test.wantError, err)
			}
			assertNoTemporaryCodeHashIndexes(t, scratch)
		})
	}
}

func TestScanBundleCancellationRemovesCodeHashIndex(t *testing.T) {
	dir := writeSharedCodeSemanticBundle(t, []bool{true, false})
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanBundle(ctx, dir, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	assertNoTemporaryCodeHashIndexes(t, scratch)
}

func writeSharedCodeSemanticBundle(t *testing.T, codeRecords []bool) string {
	t.Helper()
	if len(codeRecords) != 2 {
		t.Fatalf("code record selection has length %d, want 2", len(codeRecords))
	}
	code := []byte{0x60, 0x00}
	codeHash := crypto.Keccak256Hash(code)
	account := types.NewEmptyStateAccount()
	account.CodeHash = codeHash.Bytes()
	accountRLP, err := rlp.EncodeToBytes(account)
	if err != nil {
		t.Fatal(err)
	}
	accountHashes := []common.Hash{common.HexToHash("0x01"), common.HexToHash("0x02")}
	stack := trie.NewStackTrie(nil)
	for _, hash := range accountHashes {
		if err := stack.Update(hash[:], accountRLP); err != nil {
			t.Fatal(err)
		}
	}
	root := stack.Hash()
	header := &types.Header{
		UncleHash: types.EmptyUncleHash, Root: root,
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
		Difficulty: big.NewInt(1), Number: big.NewInt(1), GasLimit: 1, Time: 1,
	}
	headerRLP, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	head := bundle.Head{BlockNumber: 1, BlockHash: header.Hash(), StateRoot: root}
	dir := t.TempDir()
	writer, err := bundle.NewWriter(dir, bundle.CompressionNone, head, headerRLP)
	if err != nil {
		t.Fatal(err)
	}
	for index, hash := range accountHashes {
		if err := writer.WriteAccount(hash, accountRLP); err != nil {
			t.Fatal(err)
		}
		if err := writer.CountCodeReference(); err != nil {
			t.Fatal(err)
		}
		if codeRecords[index] {
			if err := writer.WriteCode(codeHash, code); err != nil {
				t.Fatal(err)
			}
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
	return dir
}

func assertNoTemporaryCodeHashIndexes(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), codeHashIndexTempPrefix) {
			return fmt.Errorf("temporary code-hash index survived at %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExportRejectsMissingLegacyCode(t *testing.T) {
	fixture := buildLegacyFixture(t)
	parent := t.TempDir()
	output := filepath.Join(parent, "bundle")
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
		Output:          output,
		Compression:     bundle.CompressionNone,
		CacheMB:         16,
		Handles:         16,
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("is missing")) {
		t.Fatalf("expected missing code error, got %v", err)
	}
	assertPathAbsent(t, output)
	partials, err := filepath.Glob(filepath.Join(parent, ".bundle.partial-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partial bundles survived missing-code failure: %v", partials)
	}
}

func TestExportDoesNotMutateLegacyLevelDB(t *testing.T) {
	fixture := buildLegacyFixture(t)
	before := directoryContentDigest(t, fixture.chaindata)
	if _, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          filepath.Join(t.TempDir(), "bundle"),
		Compression:     bundle.CompressionZstd,
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
		Compression:     bundle.CompressionNone,
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
		Compression:     bundle.CompressionNone,
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
	writer, err := bundle.NewWriter(dir, bundle.CompressionNone, head, headerRLP)
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
	writer, err := bundle.NewWriter(dir, bundle.CompressionZstd, head, headerRLP)
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

func TestAtomicDirCommitDoesNotReplaceAppearedOutput(t *testing.T) {
	final := filepath.Join(t.TempDir(), "artifact")
	output, err := newAtomicDir(final)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := output.Abort(); err != nil {
			t.Errorf("abort temporary output: %v", err)
		}
	}()
	if err := os.Mkdir(final, 0o701); err != nil {
		t.Fatal(err)
	}
	if err := output.Commit(); err == nil {
		t.Fatal("commit replaced an output directory that appeared during the operation")
	}
	info, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o701 {
		t.Fatalf("appeared output directory was replaced: mode=%o", info.Mode().Perm())
	}
}

func TestExportRejectsSymlinkedOutputInsideSource(t *testing.T) {
	fixture := buildLegacyFixture(t)
	alias := filepath.Join(t.TempDir(), "source-alias")
	if err := os.Symlink(fixture.chaindata, alias); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	output := filepath.Join(alias, "bundle")
	_, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          output,
		Compression:     bundle.CompressionNone,
		CacheMB:         16,
		Handles:         16,
	})
	if err == nil {
		t.Fatal("output routed through a symlink into the source was accepted")
	}
	if _, statErr := os.Lstat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected output unexpectedly exists: %v", statErr)
	}
}

func TestHashFlatCleanupPreservesOverlappingTrieHashes(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close memory database: %v", err)
		}
	}()
	accountHash := common.HexToHash("0x01")
	slotHash := common.HexToHash("0x02")
	rawdb.WriteAccountSnapshot(db, accountHash, []byte{0x01})
	rawdb.WriteStorageSnapshot(db, accountHash, slotHash, []byte{0x02})
	var trieHashes []common.Hash
	for _, prefix := range []byte{rawdb.SnapshotAccountPrefix[0], rawdb.SnapshotStoragePrefix[0]} {
		hash, blob := findBlobWithHashPrefix(t, prefix)
		if err := db.Put(hash[:], blob); err != nil {
			t.Fatal(err)
		}
		trieHashes = append(trieHashes, hash)
	}
	if err := removeFlatState(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for _, key := range [][]byte{
		prefixedKey(rawdb.SnapshotAccountPrefix, accountHash[:]),
		prefixedKey(rawdb.SnapshotStoragePrefix, append(accountHash[:], slotHash[:]...)),
	} {
		has, err := db.Has(key)
		if err != nil {
			t.Fatal(err)
		}
		if has {
			t.Fatalf("flat key %x survived cleanup", key)
		}
	}
	for _, hash := range trieHashes {
		has, err := db.Has(hash[:])
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Fatalf("overlapping trie hash %s was deleted", hash)
		}
	}
}

func TestFlatCleanupAndCompactionHonorCanceledContext(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close memory database: %v", err)
		}
	}()
	accountHash := common.HexToHash("0x01")
	rawdb.WriteAccountSnapshot(db, accountHash, []byte{0x01})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := removeFlatState(ctx, db); !errors.Is(err, context.Canceled) {
		t.Fatalf("flat cleanup returned %v, want context cancellation", err)
	}
	has, err := db.Has(prefixedKey(rawdb.SnapshotAccountPrefix, accountHash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("canceled flat cleanup deleted state before observing cancellation")
	}
	if err := compactPebbleRanges(ctx, filepath.Join(t.TempDir(), "missing"), 16, 16, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("compaction returned %v, want context cancellation", err)
	}
}

func TestVerifyRejectsExtraArtifactState(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	exported, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          bundleDir,
		Compression:     bundle.CompressionNone,
		CacheMB:         16,
		Handles:         16,
	})
	if err != nil {
		t.Fatal(err)
	}
	head := exported.Manifest.Source.HeadBefore
	tests := []struct {
		name      string
		scheme    string
		wantError string
		mutate    func(ethdb.Database) error
	}{
		{
			name: "hash orphan trie node", scheme: rawdb.HashScheme, wantError: "trie-node inventory mismatch",
			mutate: func(db ethdb.Database) error {
				blob := []byte("unreachable hash trie payload")
				hash := crypto.Keccak256Hash(blob)
				return db.Put(hash[:], blob)
			},
		},
		{
			name: "path orphan trie node", scheme: rawdb.PathScheme, wantError: "trie-node inventory mismatch",
			mutate: func(db ethdb.Database) error {
				return db.Put(prefixedKey(rawdb.TrieNodeAccountPrefix, make([]byte, 2*common.HashLength)), []byte{0x80})
			},
		},
		{
			name: "invalid path trie key", scheme: rawdb.PathScheme, wantError: "non-state key",
			mutate: func(db ethdb.Database) error {
				return db.Put(prefixedKey(rawdb.TrieNodeAccountPrefix, []byte{0xff}), []byte{0x80})
			},
		},
		{
			name: "unreferenced code", scheme: rawdb.HashScheme, wantError: "code inventory mismatch",
			mutate: func(db ethdb.Database) error {
				code := []byte{0x60, 0xaa}
				hash := crypto.Keccak256Hash(code)
				return db.Put(prefixedKey(rawdb.CodePrefix, hash[:]), code)
			},
		},
		{
			name: "corrupted referenced code", scheme: rawdb.HashScheme, wantError: "code hash mismatch",
			mutate: func(db ethdb.Database) error {
				it := db.NewIterator(rawdb.CodePrefix, nil)
				defer it.Release()
				if !it.Next() {
					if err := it.Error(); err != nil {
						return err
					}
					return errors.New("artifact contains no code entry to corrupt")
				}
				return db.Put(append([]byte(nil), it.Key()...), []byte{0x60, 0xbb})
			},
		},
		{
			name: "mismatched path flat account", scheme: rawdb.PathScheme, wantError: "value does not match its trie leaf",
			mutate: func(db ethdb.Database) error {
				it := db.NewIterator(rawdb.SnapshotAccountPrefix, nil)
				defer it.Release()
				if !it.Next() {
					if err := it.Error(); err != nil {
						return err
					}
					return errors.New("artifact contains no flat account to corrupt")
				}
				return db.Put(append([]byte(nil), it.Key()...), []byte{0x80})
			},
		},
		{
			name: "missing path flat storage", scheme: rawdb.PathScheme, wantError: "key mismatch",
			mutate: func(db ethdb.Database) error {
				it := db.NewIterator(rawdb.SnapshotStoragePrefix, nil)
				defer it.Release()
				if !it.Next() {
					if err := it.Error(); err != nil {
						return err
					}
					return errors.New("artifact contains no flat storage to remove")
				}
				return db.Delete(append([]byte(nil), it.Key()...))
			},
		},
		{
			name: "extra path flat account", scheme: rawdb.PathScheme, wantError: "extra flat account",
			mutate: func(db ethdb.Database) error {
				account := types.NewEmptyStateAccount()
				return db.Put(prefixedKey(rawdb.SnapshotAccountPrefix, common.MaxHash[:]), types.SlimAccountRLP(*account))
			},
		},
		{
			name: "non-canonical path metadata", scheme: rawdb.PathScheme, wantError: "metadata is non-canonical",
			mutate: func(db ethdb.Database) error {
				return db.Put([]byte("LastStateID"), []byte{0})
			},
		},
		{
			name: "extra header", scheme: rawdb.HashScheme, wantError: "non-state key",
			mutate: func(db ethdb.Database) error {
				rawdb.WriteHeader(db, &types.Header{
					Number: new(big.Int).SetUint64(head.BlockNumber + 1),
					Root:   head.StateRoot,
				})
				return nil
			},
		},
		{
			name: "extra canonical hash", scheme: rawdb.PathScheme, wantError: "non-state key",
			mutate: func(db ethdb.Database) error {
				rawdb.WriteCanonicalHash(db, common.HexToHash("0xfeed"), head.BlockNumber+1)
				return nil
			},
		},
		{
			name: "extra LastFast", scheme: rawdb.HashScheme, wantError: "non-state key",
			mutate: func(db ethdb.Database) error {
				rawdb.WriteHeadFastBlockHash(db, head.BlockHash)
				return nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifact := filepath.Join(root, strings.ReplaceAll(tt.name, " ", "-"))
			if _, err := Import(context.Background(), ImportOptions{
				Bundle: bundleDir, Output: artifact, Scheme: tt.scheme, CacheMB: 16, Handles: 16,
			}); err != nil {
				t.Fatal(err)
			}
			kv, err := pebble.New(filepath.Join(artifact, "db"), 16, 16, "inventory-mutate", false)
			if err != nil {
				t.Fatal(err)
			}
			disk := rawdb.NewDatabase(kv)
			if err := tt.mutate(disk); err != nil {
				t.Fatal(err)
			}
			if err := disk.SyncKeyValue(); err != nil {
				t.Fatal(err)
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = Verify(context.Background(), VerifyOptions{
				Bundle: bundleDir, Artifact: artifact, CacheMB: 16, Handles: 16,
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected %q error, got %v", tt.wantError, err)
			}
		})
	}
}

func TestVerifyRejectsTamperedArtifactHeadMetadata(t *testing.T) {
	fixture := buildLegacyFixture(t)
	root := t.TempDir()
	bundleDir := filepath.Join(root, "bundle")
	exported, err := Export(context.Background(), ExportOptions{
		SourceChaindata: fixture.chaindata,
		Output:          bundleDir,
		Compression:     bundle.CompressionNone,
		CacheMB:         16,
		Handles:         16,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := exported.Manifest.Source
	head := source.HeadBefore
	tests := []struct {
		name      string
		wantError string
		mutate    func(ethdb.Database) error
	}{
		{
			name: "missing header", wantError: "head header is missing",
			mutate: func(db ethdb.Database) error {
				rawdb.DeleteHeader(db, head.BlockHash, head.BlockNumber)
				return nil
			},
		},
		{
			name: "corrupt header RLP", wantError: "header RLP does not match",
			mutate: func(db ethdb.Database) error {
				entries, err := expectedHeadMetadata(source)
				if err != nil {
					return err
				}
				for key, value := range entries {
					if bytes.Equal(value, source.HeaderRLP) {
						return db.Put([]byte(key), []byte{0x80})
					}
				}
				return errors.New("expected header metadata entry is missing")
			},
		},
		{
			name: "missing hash-to-number mapping", wantError: "hash-to-number mapping is missing",
			mutate: func(db ethdb.Database) error {
				rawdb.DeleteHeaderNumber(db, head.BlockHash)
				return nil
			},
		},
		{
			name: "wrong canonical hash", wantError: "canonical hash",
			mutate: func(db ethdb.Database) error {
				rawdb.WriteCanonicalHash(db, common.HexToHash("0xdead"), head.BlockNumber)
				return nil
			},
		},
		{
			name: "wrong LastBlock", wantError: "LastBlock",
			mutate: func(db ethdb.Database) error {
				rawdb.WriteHeadBlockHash(db, common.HexToHash("0xbeef"))
				return nil
			},
		},
		{
			name: "wrong LastHeader", wantError: "LastHeader",
			mutate: func(db ethdb.Database) error {
				rawdb.WriteHeadHeaderHash(db, common.HexToHash("0xcafe"))
				return nil
			},
		},
		{
			name: "legacy artifact without head metadata", wantError: "head header is missing",
			mutate: func(db ethdb.Database) error {
				entries, err := expectedHeadMetadata(source)
				if err != nil {
					return err
				}
				for key := range entries {
					if err := db.Delete([]byte(key)); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}
	for _, scheme := range []string{rawdb.HashScheme, rawdb.PathScheme} {
		for _, tt := range tests {
			t.Run(scheme+"/"+tt.name, func(t *testing.T) {
				artifact := filepath.Join(root, scheme+"-head-"+strings.ReplaceAll(tt.name, " ", "-"))
				if _, err := Import(context.Background(), ImportOptions{
					Bundle: bundleDir, Output: artifact, Scheme: scheme, CacheMB: 16, Handles: 16,
				}); err != nil {
					t.Fatal(err)
				}
				kv, err := pebble.New(filepath.Join(artifact, "db"), 16, 16, "head-metadata-mutate", false)
				if err != nil {
					t.Fatal(err)
				}
				db := rawdb.NewDatabase(kv)
				if err := tt.mutate(db); err != nil {
					t.Fatal(err)
				}
				if err := db.SyncKeyValue(); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				_, err = Verify(context.Background(), VerifyOptions{
					Bundle: bundleDir, Artifact: artifact, CacheMB: 16, Handles: 16,
				})
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected %q error, got %v", tt.wantError, err)
				}
			})
		}
	}
}

func findBlobWithHashPrefix(t *testing.T, prefix byte) (common.Hash, []byte) {
	t.Helper()
	for i := uint64(0); ; i++ {
		blob := make([]byte, 8)
		binary.BigEndian.PutUint64(blob, i)
		hash := crypto.Keccak256Hash(blob)
		if hash[0] == prefix {
			return hash, blob
		}
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

func assertArtifactHeadMetadata(t *testing.T, artifact string, source bundle.SourceEvidence) {
	t.Helper()
	kv, err := pebble.New(filepath.Join(artifact, "db"), 16, 16, "head-metadata-read", true)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close artifact database: %v", err)
		}
	}()
	head := source.HeadBefore
	if headerRLP := rawdb.ReadHeaderRLP(db, head.BlockHash, head.BlockNumber); !bytes.Equal(headerRLP, source.HeaderRLP) {
		t.Fatal("artifact header RLP does not match source evidence")
	}
	if number, ok := rawdb.ReadHeaderNumber(db, head.BlockHash); !ok || number != head.BlockNumber {
		t.Fatalf("artifact header number mapping is %d/%t, want %d/true", number, ok, head.BlockNumber)
	}
	if hash := rawdb.ReadCanonicalHash(db, head.BlockNumber); hash != head.BlockHash {
		t.Fatalf("artifact canonical hash is %s, want %s", hash, head.BlockHash)
	}
	if hash := rawdb.ReadHeadBlockHash(db); hash != head.BlockHash {
		t.Fatalf("artifact LastBlock is %s, want %s", hash, head.BlockHash)
	}
	if hash := rawdb.ReadHeadHeaderHash(db); hash != head.BlockHash {
		t.Fatalf("artifact LastHeader is %s, want %s", hash, head.BlockHash)
	}
	if hash := rawdb.ReadHeadFastBlockHash(db); hash != (common.Hash{}) {
		t.Fatalf("artifact unexpectedly has LastFast %s", hash)
	}
	if body := rawdb.ReadBodyRLP(db, head.BlockHash, head.BlockNumber); len(body) != 0 {
		t.Fatal("artifact unexpectedly contains a block body")
	}
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
