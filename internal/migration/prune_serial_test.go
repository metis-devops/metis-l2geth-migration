package migration

// Frozen test-only serial prune reference captured before the performance refactor.
// Keep this independent of the optimized traversal, writer and scan pipeline.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/formatversion"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

func serialDefaultPruneHooks() pruneHooks {
	return pruneHooks{
		writeBatch: func(db *leveldb.DB, b *leveldb.Batch) error { return db.Write(b, &opt.WriteOptions{Sync: true}) },
		compact:    func(db *leveldb.DB) error { return db.CompactRange(util.Range{}) },
		syncFiles:  syncPruneFiles,
	}
}

// serialPrune deletes only unreferenced, content-addressed legacy state entries.
// The node must remain stopped; interruption is handled by a fresh invocation.
func serialPrune(ctx context.Context, options PruneOptions) (PruneResult, error) {
	return serialPruneWithHooks(ctx, options, serialDefaultPruneHooks())
}

func serialPruneWithHooks(ctx context.Context, options PruneOptions, hooks pruneHooks) (result PruneResult, retErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	path, parent, err := serialValidatePruneOptions(options)
	if err != nil {
		return result, err
	}
	reporter := newProgressReporter("prune", options.Progress, "chaindata", path, "dry_run", options.DryRun)
	stage := "open"
	deletionStarted := false
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("prune phase %s (deletion_started=%t); rerun to rebuild the keep set: %w", stage, deletionStarted, retErr)
		}
	}()
	source, err := openPruneDatabase(path, options.CacheMB-16, options.Handles-16, options.DryRun)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, source.close()) }()
	head, header, err := readLegacyHead(source.view())
	if err != nil {
		return result, err
	}
	genesis, node, err := readPruneGenesis(source.view())
	if err != nil {
		return result, err
	}
	result = PruneResult{Format: "metis-l2state-prune", Version: formatversion.Prune, ToolVersion: version.ToolVersion, Head: head, GenesisRoot: genesis, DryRun: options.DryRun}
	temp, err := os.MkdirTemp(parent, ".l2state-prune-")
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(temp)) }()
	stage = "collect"
	var state StateResult
	err = serialRunPrunePhase(reporter, stage, func() error {
		var err error
		state, err = serialBuildPruneKeep(ctx, source.view(), temp, head, genesis, node)
		if err != nil {
			return err
		}
		if err := syncPruneFiles(ctx, filepath.Join(temp, "keep")); err != nil {
			return err
		}
		return syncDirectory(temp)
	})
	if err != nil {
		return result, err
	}
	result.StateCounts = state.Counts
	keep, err := serialOpenPruneKeep(temp, true)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, keep.Close()) }()
	stage = "verify_keep"
	err = serialRunPrunePhase(reporter, stage, func() error { return serialVerifyPruneState(ctx, keep, head, state) })
	if err != nil {
		return result, err
	}
	stage = "preflight"
	var before pruneInventory
	err = serialRunPrunePhase(reporter, stage, func() error { var err error; before, err = serialScanPruneDB(ctx, source.view(), keep); return err })
	if err != nil {
		return result, err
	}
	result.RetainedState = before.Keep
	result.Protected = before.Protected
	result.Candidates = before.Candidates
	result.Unknown = before.Unknown
	result.ProtectedDigest = before.Digest
	if err := serialConfirmPruneHead(source.view(), head, header); err != nil {
		return result, err
	}
	if options.DryRun {
		result.Verified = true
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	stage = "delete"
	if err := source.reopen(false); err != nil {
		return result, err
	}
	err = serialRunPrunePhase(reporter, stage, func() error {
		var err error
		result.Deleted, err = serialDeletePruneDifference(ctx, source, keep, hooks, &deletionStarted)
		return err
	})
	if err != nil {
		return result, err
	}
	stage = "verify"
	err = serialRunPrunePhase(reporter, stage, func() error { return serialVerifyPruneDatabase(ctx, source, keep, head, header, state, before, hooks) })
	if err != nil {
		return result, err
	}
	if options.Compact {
		stage = "compact"
		err = serialRunPrunePhase(reporter, stage, func() error { return serialCompactPruneDatabase(ctx, source, hooks) })
		if err != nil {
			return result, err
		}
		result.Compacted = true
		stage = "verify_compacted"
		err = serialRunPrunePhase(reporter, stage, func() error { return serialVerifyPruneDatabase(ctx, source, keep, head, header, state, before, hooks) })
		if err != nil {
			return result, err
		}
	}
	result.Verified = true
	return result, nil
}

func serialValidatePruneOptions(o PruneOptions) (string, string, error) {
	if o.CacheMB < 32 || o.Handles < 32 {
		return "", "", errors.New("prune cache-mb and handles must each be at least 32 (16 reserved for the keep database)")
	}
	if o.DryRun && o.Compact {
		return "", "", errors.New("prune dry-run and compact are mutually exclusive")
	}
	path, err := validatePruneDirectory(o.Chaindata)
	if err != nil {
		return "", "", err
	}
	if err := validatePruneEntries(path); err != nil {
		return "", "", err
	}
	parent := o.TempDir
	if parent == "" {
		parent = filepath.Dir(path)
	}
	parent, err = validatePruneDirectory(parent)
	if err != nil {
		return "", "", err
	}
	if err := rejectOutputInsideDirectory(path, parent, "prune temp-dir must be outside chaindata", "prune temp-dir aliases chaindata"); err != nil {
		return "", "", err
	}
	return path, parent, nil
}

func serialRunPrunePhase(reporter *progressReporter, name string, fn func() error) error {
	phase := reporter.StartPhase(name, nil)
	err := fn()
	phase.Finish(err)
	return err
}

func serialOpenPruneKeep(path string, readOnly bool) (ethdb.Database, error) {
	kv, err := pebble.New(filepath.Join(path, "keep"), 16, 16, "", readOnly)
	if err != nil {
		return nil, fmt.Errorf("open prune keep database: %w", err)
	}
	return rawdb.NewDatabase(kv), nil
}

func serialBuildPruneKeep(ctx context.Context, db ethdb.Database, path string, head bundle.Head, genesis common.Hash, node []byte) (state StateResult, retErr error) {
	keep, err := serialOpenPruneKeep(path, false)
	if err != nil {
		return state, err
	}
	defer func() { retErr = errors.Join(retErr, keep.Close()) }()
	writer := serialNewPruneKeepWriter(keep)
	state, err = serialTraversePruneState(ctx, &serialRecordingDB{Database: db, keep: writer}, head)
	if err == nil && len(node) > 0 {
		err = writer.put(genesis[:], node)
	}
	if err = errors.Join(err, writer.close()); err != nil {
		return state, err
	}
	if err := keep.SyncKeyValue(); err != nil {
		return state, err
	}
	return state, nil
}

func serialVerifyPruneState(ctx context.Context, db ethdb.Database, head bundle.Head, want StateResult) error {
	state, err := serialTraversePruneState(ctx, db, head)
	if err != nil {
		return err
	}
	if state != want {
		return errors.New("prune state root or counts changed")
	}
	return nil
}

func serialConfirmPruneHead(db ethdb.Database, head bundle.Head, header []byte) error {
	after, blob, err := readLegacyHead(db)
	if err != nil {
		return err
	}
	if head != after || !bytes.Equal(header, blob) {
		return errors.New("prune canonical head changed")
	}
	return nil
}

func serialDeletePruneDifference(ctx context.Context, p *pruneDatabase, keep ethdb.Database, hooks pruneHooks, started *bool) (deleted PruneKVCount, retErr error) {
	it := p.db.NewIterator(nil, nil)
	defer it.Release()
	batch := new(leveldb.Batch)
	var pending PruneKVCount
	flush := func() error {
		if batch.Len() == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		*started = true
		if err := hooks.writeBatch(p.db, batch); err != nil {
			return fmt.Errorf("delete prune batch: %w", err)
		}
		deleted.Keys += pending.Keys
		deleted.Bytes += pending.Bytes
		pending = PruneKVCount{}
		batch.Reset()
		return nil
	}
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		role, err := serialPruneKeyRole(keep, it.Key(), it.Value())
		if err != nil {
			return deleted, err
		}
		if role != pruneDelete {
			continue
		}
		batch.Delete(it.Key())
		pending.add(it.Key(), it.Value())
		if batch.Len() >= 1024 {
			if err := flush(); err != nil {
				return deleted, err
			}
		}
	}
	if err := it.Error(); err != nil {
		return deleted, err
	}
	if err := flush(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func serialVerifyPruneDatabase(ctx context.Context, p *pruneDatabase, keep ethdb.Database, head bundle.Head, header []byte, state StateResult, before pruneInventory, hooks pruneHooks) error {
	if err := p.closeDB(); err != nil {
		return err
	}
	if err := hooks.syncFiles(ctx, p.path); err != nil {
		return fmt.Errorf("sync pruned database: %w", err)
	}
	if err := p.reopen(true); err != nil {
		return err
	}
	if err := serialConfirmPruneHead(p.view(), head, header); err != nil {
		return err
	}
	if err := serialVerifyPruneState(ctx, p.view(), head, state); err != nil {
		return err
	}
	if _, _, err := readPruneGenesis(p.view()); err != nil {
		return err
	}
	after, err := serialScanPruneDB(ctx, p.view(), keep)
	if err != nil {
		return err
	}
	return serialComparePruneInventory(before, after)
}

func serialCompactPruneDatabase(ctx context.Context, p *pruneDatabase, hooks pruneHooks) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.reopen(false); err != nil {
		return err
	}
	if err := hooks.compact(p.db); err != nil {
		return fmt.Errorf("compact pruned database: %w", err)
	}
	return ctx.Err()
}
