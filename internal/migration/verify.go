package migration

import (
	"bytes"
	"context"
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
	state, err := verifyDatabase(ctx, filepath.Join(opts.Artifact, "db"), stored.Scheme, bundleResult.Manifest, opts.CacheMB, opts.Handles, reporter)
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

func verifyDatabase(ctx context.Context, dbPath, scheme string, manifest bundle.Manifest, cacheMB, handles int, progress *progressReporter) (result StateResult, retErr error) {
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
		if rawdb.ReadSnapshotRoot(disk) != manifest.Source.HeadBefore.StateRoot {
			return StateResult{}, fmt.Errorf("path snapshot root mismatch: have %s want %s", rawdb.ReadSnapshotRoot(disk), manifest.Source.HeadBefore.StateRoot)
		}
		if rawdb.ReadPersistentStateID(disk) != 0 {
			return StateResult{}, fmt.Errorf("path artifact persistent state ID is %d, want 0", rawdb.ReadPersistentStateID(disk))
		}
		if rawdb.ReadSnapSyncStatusFlag(disk) != rawdb.StateSyncFinished {
			return StateResult{}, errors.New("path artifact snap sync status is not finished")
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
		progressView = countProgressSnapshot(counts, &manifest.Counts)
	}
	phaseAttrs := append([]any{
		"database", dbPath,
		"scheme", scheme,
		"root", manifest.Source.HeadBefore.StateRoot,
	}, totalCountAttrs(manifest.Counts)...)
	statePhase := progress.StartPhase("verify_state", progressView, phaseAttrs...)
	state, err := TraverseState(ctx, disk, trieDB, manifest.Source.HeadBefore.StateRoot, visitor)
	if err != nil {
		verifyErr := fmt.Errorf("verify artifact state: %w", err)
		statePhase.Finish(verifyErr)
		return StateResult{}, verifyErr
	}
	if state.Counts != manifest.Counts {
		countsErr := fmt.Errorf("artifact counts mismatch: have %+v want %+v", state.Counts, manifest.Counts)
		statePhase.Finish(countsErr, "recomputed_root", state.Root)
		return StateResult{}, countsErr
	}
	statePhase.Finish(nil, "recomputed_root", state.Root)
	if err := verifyDatabaseInventory(ctx, disk, scheme, manifest.Counts, progress); err != nil {
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

func verifyDatabaseInventory(ctx context.Context, db ethdb.Database, scheme string, counts bundle.Counts, progress *progressReporter) (retErr error) {
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
	var flatAccounts, flatSlots uint64
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
				continue
			case isValidCodeEntry(key, value):
				continue
			default:
				return fmt.Errorf("hash artifact contains non-state key %x", key)
			}
		}
		switch {
		case rawdb.IsAccountTrieNode(key):
			continue
		case rawdb.IsStorageTrieNode(key):
			continue
		case isValidCodeEntry(key, value):
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
	finishAttrs = []any{"flat_accounts", flatAccounts, "flat_storage_slots", flatSlots}
	return nil
}

func isValidCodeEntry(key, code []byte) bool {
	ok, hashBytes := rawdb.IsCodeKey(key)
	if !ok || len(code) == 0 {
		return false
	}
	return crypto.Keccak256Hash(code) == common.BytesToHash(hashBytes)
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
