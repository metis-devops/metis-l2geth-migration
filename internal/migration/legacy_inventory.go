package migration

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// verifyLegacyInventory uses reachability, never RLP shape, to classify the
// shared code/node key space. One physical entry may satisfy both references.
func verifyLegacyInventory(ctx context.Context, disk ethdb.Database, source bundle.SourceEvidence, expected stateInventory, progress *progressReporter) (retErr error) {
	phase := progress.StartPhase("inspect_database", nil, "state_layout", LayoutLegacyL2Geth)
	defer func() { phase.Finish(retErr) }()
	metadata, err := expectedHeadMetadata(source)
	if err != nil {
		return err
	}
	it := disk.NewIterator(nil, nil)
	defer it.Release()
	var nodes, codes uint64
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, value := it.Key(), it.Value()
		if want, ok := metadata[string(key)]; ok {
			if !bytes.Equal(value, want) {
				return fmt.Errorf("legacy head metadata mismatch at %x", key)
			}
			delete(metadata, string(key))
			continue
		}
		node, code, err := legacyEntryRoles(key, value, expected)
		if err != nil {
			return err
		}
		if node {
			nodes++
		}
		if code {
			codes++
		}
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("inspect legacy inventory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(metadata) != 0 || nodes != expected.TrieNodes || codes != expected.CodeEntries {
		return fmt.Errorf("legacy inventory mismatch: missing metadata=%d nodes=%d/%d codes=%d/%d", len(metadata), nodes, expected.TrieNodes, codes, expected.CodeEntries)
	}
	return nil
}

func legacyEntryRoles(key, value []byte, expected stateInventory) (bool, bool, error) {
	if len(key) != common.HashLength {
		return false, false, fmt.Errorf("legacy artifact contains unexpected key %x", key)
	}
	hash := common.Hash(key)
	node, err := expected.nodeIndex.Has(hash)
	if err != nil {
		return false, false, err
	}
	code := expected.codeHashes.Has(hash)
	if !node && !code {
		return false, false, fmt.Errorf("legacy artifact contains unreferenced key %x", key)
	}
	if len(value) == 0 || crypto.Keccak256Hash(value) != hash {
		return false, false, fmt.Errorf("legacy artifact content hash mismatch at %x", key)
	}
	return node, code, nil
}
