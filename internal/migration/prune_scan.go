package migration

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

// PruneKVCount describes physical key/value records and their logical bytes.
type PruneKVCount struct {
	Keys  uint64 `json:"keys"`
	Bytes uint64 `json:"bytes"`
}

func (c *PruneKVCount) add(key, value []byte) { c.Keys++; c.Bytes += uint64(len(key) + len(value)) }

type pruneInventory struct {
	Keep       PruneKVCount
	Protected  PruneKVCount
	Candidates PruneKVCount
	Unknown    PruneKVCount
	Digest     common.Hash
}

// scanPruneDB validates the entire inventory before deletion, including keys
// after the last candidate. No output of this preflight is a deletion journal.
func scanPruneDB(ctx context.Context, db, keep ethdb.Database, execution *pruneExecution) (pruneInventory, error) {
	var result pruneInventory
	digest := crypto.NewKeccakState()
	if _, err := digest.Write([]byte("metis-l2state-prune-protected/v1")); err != nil {
		return result, err
	}
	err := walkPruneDifference(ctx, db, keep, execution, func(record *pruneScanRecord) error {
		key, value := record.key, record.value
		if record.role == pruneDelete {
			result.Candidates.add(key, value)
			return nil
		}
		if record.role == pruneRetainState {
			result.Keep.add(key, value)
		}
		if record.role == pruneUnknown {
			result.Unknown.add(key, value)
		}
		result.Protected.add(key, value)
		return hashPruneKV(digest, key, value)
	})
	if err != nil {
		return result, err
	}
	result.Digest = common.BytesToHash(digest.Sum(nil))
	return result, nil
}

const (
	pruneProtect = iota
	pruneRetainState
	pruneDelete
	pruneUnknown
)

func rejectPruneForeignKey(key, value []byte) error {
	// LES service stores these records in the full node's chaindata. Versioned
	// balance prefixes can also produce exactly 32-byte keys.
	if isPruneLESBalanceKey(key) {
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
	if isPrunePathKey(key) || bytes.Equal(key, rawdb.SnapshotRootKey) {
		return fmt.Errorf("prune does not support geth path/snapshot data: %x", key)
	}
	return nil
}

func isPruneLESBalanceKey(key []byte) bool {
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

func isPrunePathKey(key []byte) bool {
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

func hashPruneKV(h hash.Hash, key, value []byte) error {
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

func comparePruneInventory(before, after pruneInventory) error {
	if after.Candidates.Keys != 0 {
		return fmt.Errorf("prune verification found %d remaining candidates", after.Candidates.Keys)
	}
	if before.Keep != after.Keep || before.Protected != after.Protected || before.Unknown != after.Unknown || before.Digest != after.Digest {
		return fmt.Errorf("prune protected inventory changed: before %s after %s", before.Digest, after.Digest)
	}
	return nil
}
