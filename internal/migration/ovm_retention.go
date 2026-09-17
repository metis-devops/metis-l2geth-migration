package migration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
)

// Manual eligibility is separate from authenticated Transfer-from membership.
const ovmERC20RetainPrefix byte = 'R'

func loadOVMERC20RetainList(ctx context.Context, path string, index *ovmIndex) (digest common.Hash, retErr error) {
	f, err := openOVMInput(path)
	if err != nil {
		return digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	return readOVMERC20RetainList(ctx, f, index)
}

func readOVMERC20RetainList(ctx context.Context, r io.Reader, index *ovmIndex) (digest common.Hash, retErr error) {
	h := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(r, h))
	scanner.Buffer(make([]byte, 4096), 4096)
	// This set covers only the bounded unflushed batch. Older duplicates are
	// looked up in Pebble, including when it uses the private memory filesystem.
	pending := make(map[common.Address]struct{})
	for line := uint64(1); scanner.Scan(); line++ {
		if err := ctx.Err(); err != nil {
			return digest, err
		}
		address, err := parseOVMRetainAddress(strings.TrimSpace(scanner.Text()))
		if err == nil {
			err = addOVMRetainAddress(index, pending, address)
		}
		if err != nil {
			return digest, fmt.Errorf("OVM ERC20 retain list line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return digest, fmt.Errorf("read OVM ERC20 retain list: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return digest, err
	}
	if err := index.flush(); err != nil {
		return digest, err
	}
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func parseOVMRetainAddress(text string) (common.Address, error) {
	var address common.Address
	if len(text) != 42 || !strings.HasPrefix(text, "0x") {
		return address, errors.New("expected one 0x-prefixed 20-byte address")
	}
	if err := address.UnmarshalText([]byte(text)); err != nil {
		return address, err
	}
	if address == ovmETHAddress {
		return address, errors.New("OVM_ETH must not appear in the ERC20 retain list; its self balance is already retained")
	}
	return address, nil
}

func addOVMRetainAddress(index *ovmIndex, pending map[common.Address]struct{}, address common.Address) error {
	if _, exists := pending[address]; exists {
		return errors.New("duplicate ERC20 retain address")
	}
	if _, exists, err := index.get(ovmERC20RetainPrefix, address[:]); err != nil {
		return err
	} else if exists {
		return errors.New("duplicate ERC20 retain address")
	}
	if err := index.batch.Put(prefixedKey([]byte{ovmERC20RetainPrefix}, address[:]), []byte{1}); err != nil {
		return err
	}
	// The operator-supplied address also identifies its balance slot. Neither
	// this write nor ordinary witness discovery creates a historical event.
	slot := ovmStorageHash(ovmBalanceSlot(address))
	if err := index.batch.Put(prefixedKey([]byte{'b'}, slot[:]), address[:]); err != nil {
		return err
	}
	pending[address] = struct{}{}
	if index.batch.ValueSize() >= ethdb.IdealBatchSize {
		if err := index.flush(); err != nil {
			return err
		}
		clear(pending)
	}
	return nil
}

// Validate every listed address, even if its balance is zero and it will never
// appear in the balance-worker queue. This runs on the verified original state.
func validateOVMERC20RetainList(ctx context.Context, index *ovmIndex, db *triedb.Database, root common.Hash) error {
	reader := ovmAccountReader{db: db, root: root}
	it := index.db.NewIterator([]byte{ovmERC20RetainPrefix}, nil)
	defer it.Release()
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := it.Key()
		if len(key) != 1+common.AddressLength || !bytes.Equal(it.Value(), []byte{1}) {
			return errors.New("invalid ERC20 retention index record")
		}
		address := common.Address(key[1:])
		account, err := reader.read(crypto.Keccak256Hash(address[:]))
		if err != nil {
			return fmt.Errorf("read retained contract %s: %w", address, err)
		}
		if address == ovmETHAddress || bytes.Equal(account.CodeHash, types.EmptyCodeHash[:]) {
			return fmt.Errorf("ERC20 retain address %s must be a non-OVM_ETH contract with code at the source head", address)
		}
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("iterate ERC20 retain list: %w", err)
	}
	return ctx.Err()
}

func confirmOVMRetentionEvidence(ctx context.Context, path string, evidence *OVMERC20RetentionEvidence) error {
	if (path != "") != (evidence != nil) {
		return errors.New("OVM verification requires the original --ovm-erc20-retain-list input exactly when recorded")
	}
	if evidence == nil {
		return ctx.Err()
	}
	digest, err := hashOVMInput(ctx, path)
	if err != nil {
		return fmt.Errorf("confirm ERC20 retain list: %w", err)
	}
	if digest != evidence.FileSHA256 {
		return errors.New("ERC20 retain list changed or differs from recorded digest")
	}
	return nil
}
