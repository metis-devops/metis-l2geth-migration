package migration

// Frozen test-only serial prune reference captured before the performance refactor.
// Keep this independent of the optimized traversal, writer and scan pipeline.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"net/netip"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

// serialScanPruneDB validates the entire inventory before deletion, including keys
// after the last candidate. No output of this preflight is a deletion journal.
func serialScanPruneDB(ctx context.Context, db, keep ethdb.Database) (pruneInventory, error) {
	var result pruneInventory
	digest := crypto.NewKeccakState()
	if _, err := digest.Write([]byte("metis-l2state-prune-protected/v1")); err != nil {
		return result, err
	}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		key, value := it.Key(), it.Value()
		role, err := serialPruneKeyRole(keep, key, value)
		if err != nil {
			return result, err
		}
		// Content-addressed state keys can contain any prefix. Only interpret
		// namespaces after excluding validated state and keep-set entries.
		if role != pruneDelete && role != pruneRetainState {
			if err := serialRejectPruneForeignKey(key, value); err != nil {
				return result, err
			}
		}
		if role == pruneDelete {
			result.Candidates.add(key, value)
			continue
		}
		if role == pruneRetainState {
			result.Keep.add(key, value)
		}
		if role == pruneUnknown {
			result.Unknown.add(key, value)
		}
		result.Protected.add(key, value)
		if err := serialHashPruneKV(digest, key, value); err != nil {
			return result, err
		}
	}
	if err := it.Error(); err != nil {
		return result, fmt.Errorf("scan prune database: %w", err)
	}
	result.Digest = common.BytesToHash(digest.Sum(nil))
	return result, nil
}

func serialPruneKeyRole(keep ethdb.Database, key, value []byte) (int, error) {
	if len(key) != common.HashLength {
		return pruneProtect, nil
	}
	exists, err := keep.Has(key)
	if err != nil {
		return 0, fmt.Errorf("lookup prune keep key: %w", err)
	}
	if exists {
		expected, err := keep.Get(key)
		if err != nil {
			return 0, err
		}
		return pruneRetainState, serialComparePruneValue(key, expected, value)
	}
	if crypto.Keccak256Hash(value) == common.BytesToHash(key) {
		return pruneDelete, nil
	}
	return pruneUnknown, nil
}

func serialRejectPruneForeignKey(key, value []byte) error {
	// LES service stores these records in the full node's chaindata. Versioned
	// balance prefixes can also produce exactly 32-byte keys.
	if serialIsPruneLESBalanceKey(key) {
		return fmt.Errorf("prune does not support LES service metadata: %x", key)
	}
	for _, prefix := range []string{"cumulativeTime:", "_globalCostFactor", "chtIndexV2-", "bltIndex-", "cht-", "blt-", "chtRootV2-", "bltRoot-"} {
		if bytes.HasPrefix(key, []byte(prefix)) {
			return fmt.Errorf("prune does not support LES service metadata: %x", key)
		}
	}
	if len(key) == 33 && key[0] == 'c' && bytes.Equal(crypto.Keccak256(value), key[1:]) {
		return fmt.Errorf("prune does not support geth prefixed code: %x", key)
	}
	if serialIsPrunePathKey(key) || bytes.Equal(key, rawdb.SnapshotRootKey) {
		return fmt.Errorf("prune does not support geth path/snapshot data: %x", key)
	}
	return nil
}

func serialIsPruneLESBalanceKey(key []byte) bool {
	// The pinned client writes version 1. Version 0 records may remain after
	// its balance-format upgrade. H+hash and l+hash are unrelated namespaces.
	if len(key) < 5 || key[0] != 0 || key[1] > 1 {
		return false
	}
	switch string(key[2:5]) {
	case "pb:":
		return len(key) == 5+common.HashLength // binary enode.ID
	case "nb:":
		id := string(key[5:])
		if _, err := netip.ParseAddr(id); err == nil {
			return true
		}
		// Loopback/non-TCP clients use the hex-encoded enode.ID instead of IP.
		if len(id) == 2*common.HashLength {
			_, err := hex.DecodeString(id)
			return err == nil
		}
	}
	return false
}

func serialIsPrunePathKey(key []byte) bool {
	account, path := rawdb.ResolveAccountTrieNodeKey(key)
	if !account {
		storage, _, storagePath := rawdb.ResolveStorageTrieNode(key)
		if !storage {
			return false
		}
		path = storagePath
	}
	for _, nibble := range path {
		if nibble > 15 {
			return false
		}
	}
	return true
}

func serialHashPruneKV(h hash.Hash, key, value []byte) error {
	var length [8]byte
	for _, part := range [][]byte{key, value} {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		if _, err := h.Write(length[:]); err != nil {
			return err
		}
		if _, err := h.Write(part); err != nil {
			return err
		}
	}
	return nil
}

func serialComparePruneInventory(before, after pruneInventory) error {
	if after.Candidates.Keys != 0 {
		return fmt.Errorf("prune verification found %d remaining candidates", after.Candidates.Keys)
	}
	if before.Keep != after.Keep || before.Protected != after.Protected || before.Unknown != after.Unknown || before.Digest != after.Digest {
		return fmt.Errorf("prune protected inventory changed: before %s after %s", before.Digest, after.Digest)
	}
	return nil
}
