package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/MetisProtocol/mvm/l2geth/common"
	"github.com/MetisProtocol/mvm/l2geth/common/hexutil"
	l2core "github.com/MetisProtocol/mvm/l2geth/core"
	"github.com/MetisProtocol/mvm/l2geth/core/rawdb"
	"github.com/MetisProtocol/mvm/l2geth/core/state"
	l2types "github.com/MetisProtocol/mvm/l2geth/core/types"
	"github.com/MetisProtocol/mvm/l2geth/crypto"
	"github.com/MetisProtocol/mvm/l2geth/ethdb"
	"github.com/MetisProtocol/mvm/l2geth/rollup/dump"
	"github.com/MetisProtocol/mvm/l2geth/rollup/rcfg"
)

var fixtureMagic = [8]byte{'L', '2', 'G', 'K', 'V', '0', '0', '1'}

const ovmETHRepositoryCommit = "696b5613df9cf23ecdb597b588b30f47c4f81c6c"

//go:embed ovm-eth-andromeda.json
var ovmETHAllocationJSON []byte

type expected struct {
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

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run . OUTPUT_DIRECTORY")
		os.Exit(2)
	}
	if err := generate(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(output string) error {
	// Match the production legacy client default. In OVM mode, balances live in
	// OVM_ETH contract storage instead of the ordinary account balance field.
	rcfg.UsingOVM = true
	disk := rawdb.NewMemoryDatabase()
	stateDatabase := state.NewDatabase(disk)
	stateDB, err := state.New(common.Hash{}, stateDatabase)
	if err != nil {
		return err
	}
	ovmETH, err := loadOVMETHAllocation()
	if err != nil {
		return err
	}
	stateDB.SetCode(ovmETH.address, ovmETH.account.Code)
	for key, value := range ovmETH.account.Storage {
		if value != (common.Hash{}) {
			stateDB.SetState(ovmETH.address, key, value)
		}
	}
	code := common.FromHex("0x60016000556002600155")
	accounts := []struct {
		address    common.Address
		nonce      uint64
		balance    int64
		ovmBalance int64
		code       []byte
		storage    map[common.Hash]common.Hash
	}{
		{address: common.HexToAddress("0x1000000000000000000000000000000000000001"), nonce: 1, balance: 0, ovmBalance: 100},
		{
			address: common.HexToAddress("0x2000000000000000000000000000000000000002"), nonce: 7, balance: 0, ovmBalance: 999, code: code,
			storage: map[common.Hash]common.Hash{
				common.HexToHash("0x01"): common.HexToHash("0x1234"),
				common.HexToHash("0x02"): common.HexToHash("0xffff"),
				common.HexToHash("0x10"): common.HexToHash("0x42"),
			},
		},
		{address: common.HexToAddress("0x3000000000000000000000000000000000000003"), nonce: 2, balance: 0, ovmBalance: 55, code: code},
		{address: common.HexToAddress("0x4000000000000000000000000000000000000004"), nonce: 4, balance: 0, ovmBalance: 0},
	}
	for _, account := range accounts {
		if account.balance != 0 {
			return fmt.Errorf("ordinary account balance for %s must be zero", account.address)
		}
		stateDB.SetNonce(account.address, account.nonce)
		// Leave the consensus account balance at its zero default and place the
		// user-visible balance explicitly in OVM_ETH mapping slot zero.
		if account.ovmBalance < 0 {
			return fmt.Errorf("OVM balance for %s must not be negative", account.address)
		}
		if account.ovmBalance > 0 {
			stateDB.SetState(
				dump.OvmEthAddress,
				state.GetOVMBalanceKey(account.address),
				common.BigToHash(big.NewInt(account.ovmBalance)),
			)
		}
		if len(account.code) > 0 {
			stateDB.SetCode(account.address, account.code)
		}
		for key, value := range account.storage {
			stateDB.SetState(account.address, key, value)
		}
	}
	root, err := stateDB.Commit(false)
	if err != nil {
		return err
	}
	if err := stateDatabase.TrieDB().Commit(root, true); err != nil {
		return err
	}
	header := &l2types.Header{
		ParentHash:  common.HexToHash("0xabc1"),
		UncleHash:   l2types.EmptyUncleHash,
		Coinbase:    common.HexToAddress("0x4000000000000000000000000000000000000004"),
		Root:        root,
		TxHash:      l2types.EmptyRootHash,
		ReceiptHash: l2types.EmptyRootHash,
		Difficulty:  big.NewInt(1),
		Number:      big.NewInt(12345),
		GasLimit:    30_000_000,
		GasUsed:     21_000,
		Time:        1_700_000_000,
		Extra:       []byte("legacy-l2geth-golden"),
	}
	rawdb.WriteHeader(disk, header)
	rawdb.WriteCanonicalHash(disk, header.Hash(), header.Number.Uint64())
	rawdb.WriteHeadBlockHash(disk, header.Hash())
	rawdb.WriteHeadHeaderHash(disk, header.Hash())
	headerRLP := rawdb.ReadHeaderRLP(disk, header.Hash(), header.Number.Uint64())
	if len(headerRLP) == 0 {
		return fmt.Errorf("generated header RLP is empty")
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return err
	}
	if err := writeKV(filepath.Join(output, "legacy-l2geth-kv-v1.bin"), disk); err != nil {
		return err
	}
	metadata := expected{
		GeneratorModule:    "github.com/MetisProtocol/mvm/l2geth",
		UsingOVM:           rcfg.UsingOVM,
		AccountBalanceZero: true,
		OVMETHSource:       ovmETH.source,
		OVMETHCodeHash:     crypto.Keccak256Hash(ovmETH.account.Code),
		BlockNumber:        header.Number.Uint64(),
		BlockHash:          header.Hash(),
		StateRoot:          root,
		HeaderRLP:          hexutil.Bytes(headerRLP),
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(output, "legacy-l2geth-expected-v1.json"), data, 0o644)
}

type ovmETHAllocationFile struct {
	Source           string                `json:"source"`
	RepositoryCommit string                `json:"repository_commit"`
	Address          string                `json:"address"`
	Account          l2core.GenesisAccount `json:"account"`
}

type ovmETHAllocation struct {
	source  string
	address common.Address
	account l2core.GenesisAccount
}

func loadOVMETHAllocation() (ovmETHAllocation, error) {
	var file ovmETHAllocationFile
	if err := json.Unmarshal(ovmETHAllocationJSON, &file); err != nil {
		return ovmETHAllocation{}, fmt.Errorf("decode embedded OVM_ETH allocation: %w", err)
	}
	if file.RepositoryCommit != ovmETHRepositoryCommit {
		return ovmETHAllocation{}, fmt.Errorf("unexpected metis-networks commit %q", file.RepositoryCommit)
	}
	address := common.HexToAddress(file.Address)
	if address != dump.OvmEthAddress {
		return ovmETHAllocation{}, fmt.Errorf("unexpected OVM_ETH address %s", address)
	}
	if file.Account.Balance == nil || file.Account.Balance.Sign() != 0 {
		return ovmETHAllocation{}, errors.New("OVM_ETH ordinary account balance must be zero")
	}
	if len(file.Account.Code) == 0 {
		return ovmETHAllocation{}, errors.New("OVM_ETH code is empty")
	}
	if len(file.Account.Storage) != 4 {
		return ovmETHAllocation{}, fmt.Errorf("OVM_ETH allocation has %d storage entries, want 4", len(file.Account.Storage))
	}
	return ovmETHAllocation{source: file.Source, address: address, account: file.Account}, nil
}

func writeKV(path string, db ethdb.Database) error {
	type entry struct {
		key   []byte
		value []byte
	}
	var entries []entry
	it := db.NewIterator()
	defer it.Release()
	for it.Next() {
		if bytes.HasPrefix(it.Key(), []byte("secure-key-")) {
			continue
		}
		entries = append(entries, entry{
			key:   append([]byte(nil), it.Key()...),
			value: append([]byte(nil), it.Value()...),
		})
	}
	if err := it.Error(); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	if _, err := writer.Write(fixtureMagic[:]); err != nil {
		_ = file.Close()
		return err
	}
	if err := writeUint64(writer, uint64(len(entries))); err != nil {
		_ = file.Close()
		return err
	}
	for _, entry := range entries {
		if err := writeUint32(writer, uint32(len(entry.key))); err != nil {
			_ = file.Close()
			return err
		}
		if err := writeUint64(writer, uint64(len(entry.value))); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := writer.Write(entry.key); err != nil {
			_ = file.Close()
			return err
		}
		if _, err := writer.Write(entry.value); err != nil {
			_ = file.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeUint32(w *bufio.Writer, value uint32) error {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	_, err := w.Write(data[:])
	return err
}

func writeUint64(w *bufio.Writer, value uint64) error {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	_, err := w.Write(data[:])
	return err
}
