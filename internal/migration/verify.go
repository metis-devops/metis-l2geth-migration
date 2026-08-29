package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

// VerifyOptions configures independent bundle and optional artifact verification.
type VerifyOptions struct {
	Bundle   string
	Artifact string
	CacheMB  int
	Handles  int
	Progress ProgressOptions
}

// Verify recomputes bundle evidence and optionally validates an imported database.
func Verify(ctx context.Context, opts VerifyOptions) (result VerificationReport, retErr error) {
	reporter := newProgressReporter("verify", opts.Progress,
		"bundle", opts.Bundle,
		"artifact", opts.Artifact,
	)
	defer func() {
		attrs := []any{"scheme", result.Scheme}
		if result.RecomputedRoot != (common.Hash{}) {
			attrs = append(attrs,
				"block", result.Head.BlockNumber,
				"root", result.RecomputedRoot,
			)
		}
		reporter.Finish(retErr, attrs...)
	}()
	if opts.Bundle == "" {
		return VerificationReport{}, errors.New("bundle path is required")
	}
	bundleResult, err := scanBundle(ctx, opts.Bundle, nil, reporter)
	if err != nil {
		return VerificationReport{}, err
	}
	if opts.Artifact == "" {
		return newVerificationReport(bundleResult, "bundle"), nil
	}
	stored, err := loadVerificationReport(opts.Artifact)
	if err != nil {
		return VerificationReport{}, err
	}
	if stored.Scheme != rawdb.HashScheme && stored.Scheme != rawdb.PathScheme {
		return VerificationReport{}, fmt.Errorf("artifact report has invalid database scheme %q", stored.Scheme)
	}
	if err := compareStoredReport(stored, bundleResult); err != nil {
		return VerificationReport{}, err
	}
	state, err := verifyDatabase(ctx, filepath.Join(opts.Artifact, "db"), stored.Scheme, bundleResult.State, opts.CacheMB, opts.Handles, reporter)
	if err != nil {
		return VerificationReport{}, err
	}
	if state != bundleResult.State {
		return VerificationReport{}, fmt.Errorf("artifact state result mismatch: database %+v bundle %+v", state, bundleResult.State)
	}
	return newVerificationReport(bundleResult, stored.Scheme), nil
}

func compareStoredReport(stored VerificationReport, current BundleResult) error {
	if stored.ManifestSHA256 != current.ManifestSHA256 {
		return errors.New("artifact report refers to a different manifest")
	}
	if stored.StateFileSHA256 != current.Records.FileSHA256 || stored.RecordChainHash != current.Records.RecordChainHash {
		return errors.New("artifact report refers to different state records")
	}
	if stored.Head != current.Manifest.Source.HeadBefore || stored.Counts != current.State.Counts || stored.RecomputedRoot != current.State.Root {
		return errors.New("artifact report evidence does not match the bundle")
	}
	return nil
}

func verifyDatabase(ctx context.Context, dbPath, scheme string, expected StateResult, cacheMB, handles int, progress *progressReporter) (result StateResult, retErr error) {
	diskKV, err := pebble.New(dbPath, cacheMB, handles, "l2state/verify", true)
	if err != nil {
		return StateResult{}, fmt.Errorf("open artifact Pebble database read-only: %w", err)
	}
	disk := rawdb.NewDatabase(diskKV)
	defer func() {
		if err := disk.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close artifact database: %w", err))
		}
	}()

	var config *triedb.Config
	switch scheme {
	case rawdb.HashScheme:
		copyConfig := *triedb.HashDefaults
		config = &copyConfig
	case rawdb.PathScheme:
		pathConfig := *pathdb.ReadOnly
		pathConfig.SnapshotNoBuild = true
		config = &triedb.Config{PathDB: &pathConfig}
	default:
		return StateResult{}, fmt.Errorf("unsupported artifact scheme %q", scheme)
	}
	trieDB := triedb.NewDatabase(disk, config)
	defer func() {
		if err := trieDB.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close artifact trie database: %w", err))
		}
	}()
	if scheme == rawdb.PathScheme {
		if !trieDB.SnapshotCompleted() {
			return StateResult{}, errors.New("path artifact snapshot is not complete")
		}
		if rawdb.ReadSnapshotRoot(disk) != expected.Root {
			return StateResult{}, fmt.Errorf("path snapshot root mismatch: have %s want %s", rawdb.ReadSnapshotRoot(disk), expected.Root)
		}
		if rawdb.ReadPersistentStateID(disk) != 0 {
			return StateResult{}, fmt.Errorf("path artifact persistent state ID is %d, want 0", rawdb.ReadPersistentStateID(disk))
		}
		if rawdb.ReadSnapSyncStatusFlag(disk) != rawdb.StateSyncFinished {
			return StateResult{}, errors.New("path artifact snap sync status is not finished")
		}
		if err := verifyPathMetadata(disk, expected.Root); err != nil {
			return StateResult{}, err
		}
	}
	var visitor StateVisitor
	if scheme == rawdb.PathScheme {
		visitor = &flatStateVerifier{db: disk}
	}
	var progressView progressSnapshot
	if progress.Enabled() {
		counts := new(progressCounts)
		visitor = newCountingStateVisitor(visitor, counts)
		progressView = countProgressSnapshot(counts, &expected.Counts)
	}
	phaseAttrs := append([]any{
		"database", dbPath,
		"scheme", scheme,
		"root", expected.Root,
	}, totalCountAttrs(expected.Counts)...)
	statePhase := progress.StartPhase("verify_state", progressView, phaseAttrs...)
	state, inventory, err := traverseState(ctx, disk, trieDB, expected.Root, visitor, true)
	if err != nil {
		verifyErr := fmt.Errorf("verify artifact state: %w", err)
		statePhase.Finish(verifyErr)
		return StateResult{}, verifyErr
	}
	if state.Counts != expected.Counts {
		countsErr := fmt.Errorf("artifact counts mismatch: have %+v want %+v", state.Counts, expected.Counts)
		statePhase.Finish(countsErr, "recomputed_root", state.Root)
		return StateResult{}, countsErr
	}
	statePhase.Finish(nil, "recomputed_root", state.Root)
	if err := verifyDatabaseInventory(ctx, disk, scheme, expected.Counts, inventory, progress); err != nil {
		return StateResult{}, err
	}
	return state, nil
}

type flatStateVerifier struct {
	db ethdb.Database
}

func (v *flatStateVerifier) Account(hash common.Hash, account *types.StateAccount, _ []byte) error {
	have := rawdb.ReadAccountSnapshot(v.db, hash)
	want := types.SlimAccountRLP(*account)
	if !bytes.Equal(have, want) {
		return fmt.Errorf("flat account %s does not match its trie leaf", hash)
	}
	return nil
}

func (v *flatStateVerifier) Storage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	have := rawdb.ReadStorageSnapshot(v.db, accountHash, slotHash)
	if !bytes.Equal(have, valueRLP) {
		return fmt.Errorf("flat account %s slot %s does not match its trie leaf", accountHash, slotHash)
	}
	return nil
}

func (v *flatStateVerifier) Code(common.Hash, common.Hash, []byte) error {
	return nil
}

func verifyDatabaseInventory(ctx context.Context, db ethdb.Database, scheme string, counts bundle.Counts, expected stateInventory, progress *progressReporter) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	it := db.NewIterator(nil, nil)
	defer it.Release()
	var (
		keys         atomic.Uint64
		progressView progressSnapshot
	)
	trackProgress := progress.Enabled()
	if trackProgress {
		progressView = counterProgressSnapshot(&keys, "keys", "keys_per_second")
	}
	phase := progress.StartPhase("inspect_database", progressView, "scheme", scheme)
	var finishAttrs []any
	defer func() {
		phase.Finish(retErr, finishAttrs...)
	}()
	var flatAccounts, flatSlots, trieNodes, codeEntries uint64
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if trackProgress {
			keys.Add(1)
		}
		key, value := it.Key(), it.Value()
		if scheme == rawdb.HashScheme {
			switch {
			case rawdb.IsLegacyTrieNode(key, value):
				trieNodes++
				continue
			case isValidCodeEntry(key, value):
				codeEntries++
				continue
			default:
				return fmt.Errorf("hash artifact contains non-state key %x", key)
			}
		}
		switch {
		case isCanonicalPathAccountTrieNodeKey(key):
			trieNodes++
			continue
		case isCanonicalPathStorageTrieNodeKey(key):
			trieNodes++
			continue
		case isValidCodeEntry(key, value):
			codeEntries++
			continue
		case bytes.HasPrefix(key, rawdb.SnapshotAccountPrefix) && len(key) == len(rawdb.SnapshotAccountPrefix)+common.HashLength:
			flatAccounts++
			continue
		case bytes.HasPrefix(key, rawdb.SnapshotStoragePrefix) && len(key) == len(rawdb.SnapshotStoragePrefix)+2*common.HashLength:
			flatSlots++
			continue
		case allowedPathMetadataKey(key):
			continue
		default:
			return fmt.Errorf("path artifact contains non-state key %x", key)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("inspect artifact database: %w", err)
	}
	if scheme == rawdb.PathScheme {
		if flatAccounts != counts.Accounts || flatSlots != counts.StorageSlots {
			return fmt.Errorf("path flat-state inventory mismatch: accounts=%d/%d slots=%d/%d", flatAccounts, counts.Accounts, flatSlots, counts.StorageSlots)
		}
	}
	if trieNodes != expected.TrieNodes {
		return fmt.Errorf("artifact trie-node inventory mismatch: have %d want %d", trieNodes, expected.TrieNodes)
	}
	if codeEntries != expected.CodeEntries {
		return fmt.Errorf("artifact code inventory mismatch: have %d want %d", codeEntries, expected.CodeEntries)
	}
	finishAttrs = []any{
		"flat_accounts", flatAccounts,
		"flat_storage_slots", flatSlots,
		"trie_nodes", trieNodes,
		"code_entries", codeEntries,
	}
	return nil
}

type snapshotGeneratorMarker struct {
	Wiping   bool
	Done     bool
	Marker   []byte
	Accounts uint64
	Slots    uint64
	Storage  uint64
}

func verifyPathMetadata(db ethdb.Database, root common.Hash) error {
	var stateID [8]byte
	binary.BigEndian.PutUint64(stateID[:], 0)
	generator, err := rlp.EncodeToBytes(snapshotGeneratorMarker{Done: true})
	if err != nil {
		return fmt.Errorf("encode expected path snapshot generator marker: %w", err)
	}
	for _, expected := range []struct {
		name  string
		key   []byte
		value []byte
	}{
		{name: "snapshot root", key: rawdb.SnapshotRootKey, value: root[:]},
		{name: "snapshot generator", key: []byte("SnapshotGenerator"), value: generator},
		{name: "persistent state ID", key: []byte("LastStateID"), value: stateID[:]},
		{name: "snap sync status", key: []byte("SnapSyncStatus"), value: []byte{rawdb.StateSyncFinished}},
	} {
		value, err := db.Get(expected.key)
		if err != nil {
			return fmt.Errorf("read path %s metadata: %w", expected.name, err)
		}
		if !bytes.Equal(value, expected.value) {
			return fmt.Errorf("path %s metadata is non-canonical", expected.name)
		}
	}
	return nil
}

func isValidCodeEntry(key, code []byte) bool {
	ok, hashBytes := rawdb.IsCodeKey(key)
	if !ok || len(code) == 0 {
		return false
	}
	return crypto.Keccak256Hash(code) == common.BytesToHash(hashBytes)
}

func isCanonicalPathAccountTrieNodeKey(key []byte) bool {
	if !bytes.HasPrefix(key, rawdb.TrieNodeAccountPrefix) {
		return false
	}
	return isCanonicalHexPath(key[len(rawdb.TrieNodeAccountPrefix):])
}

func isCanonicalPathStorageTrieNodeKey(key []byte) bool {
	if !bytes.HasPrefix(key, rawdb.TrieNodeStoragePrefix) {
		return false
	}
	pathOffset := len(rawdb.TrieNodeStoragePrefix) + common.HashLength
	if len(key) < pathOffset {
		return false
	}
	return isCanonicalHexPath(key[pathOffset:])
}

func isCanonicalHexPath(path []byte) bool {
	if len(path) > 2*common.HashLength {
		return false
	}
	for _, nibble := range path {
		if nibble > 0x0f {
			return false
		}
	}
	return true
}

func allowedPathMetadataKey(key []byte) bool {
	for _, allowed := range [][]byte{
		rawdb.SnapshotRootKey,
		[]byte("SnapshotGenerator"),
		[]byte("LastStateID"),
		[]byte("SnapSyncStatus"),
	} {
		if bytes.Equal(key, allowed) {
			return true
		}
	}
	return false
}
