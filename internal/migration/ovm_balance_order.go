package migration

import (
	"bytes"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

// q + account hash => address (20), hashed storage slot (32), balance (32).
// This namespace is private scratch, not an artifact or retention policy.
func (t *ovmTransformer) queueBalance(address, key []byte, value *uint256.Int) error {
	// Balance-slot hashes scramble account order. Stage only fixed-size jobs
	// in the existing temporary database so bounded readers visit adjacent
	// account paths without retaining a state-sized decoded trie.
	var record [common.AddressLength + 2*common.HashLength]byte
	copy(record[:], address)
	copy(record[common.AddressLength:], key)
	word := value.Bytes32()
	copy(record[common.AddressLength+common.HashLength:], word[:])
	hash := crypto.Keccak256Hash(address)
	return t.index.put('q', hash[:], record[:])
}

func (t *ovmTransformer) inspectOrderedBalances(batch *ovmBalanceBatch) error {
	if err := t.index.flush(); err != nil {
		return err
	}
	it := t.index.db.NewIterator([]byte{'q'}, nil)
	defer it.Release()
	lease := newMigrateWorkLease(t.limiter)
	defer lease.release()
	for {
		if err := lease.acquire(batch.ctx); err != nil {
			return err
		}
		if !it.Next() {
			lease.release()
			break
		}
		if err := batch.ctx.Err(); err != nil {
			return err
		}
		record := it.Value()
		if len(it.Key()) != 1+common.HashLength || len(record) != common.AddressLength+2*common.HashLength {
			return errors.New("invalid indexed OVM balance job")
		}
		address := common.Address(record[:common.AddressLength])
		hash := crypto.Keccak256Hash(address[:])
		if !bytes.Equal(it.Key()[1:], hash[:]) {
			return errors.New("indexed OVM balance job account hash mismatch")
		}
		slot := common.Hash(record[common.AddressLength : common.AddressLength+common.HashLength])
		value := new(uint256.Int).SetBytes(record[common.AddressLength+common.HashLength:])
		lease.release()
		if err := batch.submit(address, slot, value); err != nil {
			return err
		}
	}
	if err := it.Error(); err != nil {
		return err
	}
	return batch.drain()
}
