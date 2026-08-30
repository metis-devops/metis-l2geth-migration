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
	state, err := verifyDatabase(ctx, filepath.Join(opts.Artifact, "db"), stored.Scheme, bundleResult.Manifest.Source, bundleResult.State, opts.CacheMB, opts.Handles, reporter, "")
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

func verifyDatabase(ctx context.Context, dbPath, scheme string, source bundle.SourceEvidence, expected StateResult, cacheMB, handles int, progress *progressReporter, scratchParent string) (result StateResult, retErr error) {
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
	headPhase := progress.StartPhase("verify_head_metadata", nil,
		"block", source.HeadBefore.BlockNumber,
		"hash", source.HeadBefore.BlockHash,
	)
	if err := verifyHeadMetadata(disk, source); err != nil {
		headPhase.Finish(err)
		return StateResult{}, err
	}
	headPhase.Finish(nil)

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
	var (
		visitor      StateVisitor
		flatVerifier *flatStateVerifier
	)
	if scheme == rawdb.PathScheme {
		flatVerifier = newFlatStateVerifier(ctx, disk)
		defer flatVerifier.Release()
		visitor = flatVerifier
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
	state, inventory, err := traverseState(ctx, disk, trieDB, expected.Root, visitor, true, stateTraversalOptions{
		NodeIndex: trieNodeIndexOptions{Parent: scratchParent, CacheMB: cacheMB, Handles: handles},
		ReadCode:  func(db ethdb.KeyValueReader, hash common.Hash) []byte { return rawdb.ReadCodeWithPrefix(db, hash) },
	})
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
	if flatVerifier != nil {
		if err := flatVerifier.Finish(); err != nil {
			verifyErr := fmt.Errorf("verify path flat-state inventory: %w", err)
			statePhase.Finish(verifyErr, "recomputed_root", state.Root)
			return StateResult{}, verifyErr
		}
	}
	statePhase.Finish(nil, "recomputed_root", state.Root)
	if err := verifyDatabaseInventory(ctx, disk, scheme, source, expected.Counts, inventory, progress); err != nil {
		return StateResult{}, err
	}
	return state, nil
}

type flatStateVerifier struct {
	ctx      context.Context
	accounts ethdb.Iterator
	storage  ethdb.Iterator
}

func newFlatStateVerifier(ctx context.Context, db ethdb.Database) *flatStateVerifier {
	return &flatStateVerifier{
		ctx:      ctx,
		accounts: db.NewIterator(rawdb.SnapshotAccountPrefix, nil),
		storage:  db.NewIterator(rawdb.SnapshotStoragePrefix, nil),
	}
}

func (v *flatStateVerifier) Account(hash common.Hash, account *types.StateAccount, _ []byte) error {
	key := prefixedKey(rawdb.SnapshotAccountPrefix, hash[:])
	if err := v.compareNext(v.accounts, key, types.SlimAccountRLP(*account)); err != nil {
		return fmt.Errorf("flat account %s: %w", hash, err)
	}
	return nil
}

func (v *flatStateVerifier) Storage(accountHash, slotHash common.Hash, valueRLP []byte) error {
	key := make([]byte, 0, len(rawdb.SnapshotStoragePrefix)+2*common.HashLength)
	key = append(key, rawdb.SnapshotStoragePrefix...)
	key = append(key, accountHash[:]...)
	key = append(key, slotHash[:]...)
	if err := v.compareNext(v.storage, key, valueRLP); err != nil {
		return fmt.Errorf("flat account %s slot %s: %w", accountHash, slotHash, err)
	}
	return nil
}

func (v *flatStateVerifier) Code(common.Hash, common.Hash, []byte) error {
	return nil
}

func (v *flatStateVerifier) compareNext(it ethdb.Iterator, key, value []byte) error {
	if err := v.ctx.Err(); err != nil {
		return err
	}
	if !it.Next() {
		if err := it.Error(); err != nil {
			return fmt.Errorf("iterate flat state: %w", err)
		}
		return errors.New("entry is missing")
	}
	if !bytes.Equal(it.Key(), key) {
		return fmt.Errorf("key mismatch: have %x want %x", it.Key(), key)
	}
	if !bytes.Equal(it.Value(), value) {
		return errors.New("value does not match its trie leaf")
	}
	return nil
}

func (v *flatStateVerifier) Finish() error {
	for _, target := range []struct {
		name string
		it   ethdb.Iterator
	}{
		{name: "account", it: v.accounts},
		{name: "storage", it: v.storage},
	} {
		if err := v.ctx.Err(); err != nil {
			return err
		}
		if target.it.Next() {
			return fmt.Errorf("path artifact contains extra flat %s key %x", target.name, target.it.Key())
		}
		if err := target.it.Error(); err != nil {
			return fmt.Errorf("finish flat %s iteration: %w", target.name, err)
		}
	}
	return nil
}

func (v *flatStateVerifier) Release() {
	v.accounts.Release()
	v.storage.Release()
}

func verifyDatabaseInventory(ctx context.Context, db ethdb.Database, scheme string, source bundle.SourceEvidence, counts bundle.Counts, expected stateInventory, progress *progressReporter) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	headMetadata, err := expectedHeadMetadata(source)
	if err != nil {
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
	var flatAccounts, flatSlots, trieNodes, codeEntries, headEntries uint64
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if trackProgress {
			keys.Add(1)
		}
		key := it.Key()
		if expectedValue, ok := headMetadata[string(key)]; ok {
			if !bytes.Equal(it.Value(), expectedValue) {
				return fmt.Errorf("artifact head metadata key %x has an unexpected value", key)
			}
			delete(headMetadata, string(key))
			headEntries++
			continue
		}
		if scheme == rawdb.HashScheme {
			value := it.Value()
			switch {
			case rawdb.IsLegacyTrieNode(key, value):
				trieNodes++
				continue
			case isNonEmptyCodeEntry(key, value):
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
		case isNonEmptyCodeEntry(key, it.Value()):
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
	if len(headMetadata) != 0 {
		return fmt.Errorf("artifact head metadata inventory is missing %d entries", len(headMetadata))
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
		"head_metadata_entries", headEntries,
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

func isNonEmptyCodeEntry(key, code []byte) bool {
	ok, _ := rawdb.IsCodeKey(key)
	return ok && len(code) != 0
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
