package migration

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
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/formatversion"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// PruneOptions configures offline, in-place legacy state pruning.
type PruneOptions struct {
	Chaindata string
	TempDir   string
	CacheMB   int
	Handles   int
	Workers   int
	DryRun    bool
	Compact   bool
	Progress  ProgressOptions
}

// PruneResult is separate from all migration artifact and bundle reports.
type PruneResult struct {
	Format          string        `json:"format"`
	Version         uint64        `json:"version"`
	ToolVersion     string        `json:"tool_version"`
	Head            bundle.Head   `json:"head"`
	GenesisRoot     common.Hash   `json:"genesis_root"`
	DryRun          bool          `json:"dry_run"`
	Compacted       bool          `json:"compacted"`
	Verified        bool          `json:"verified"`
	StateCounts     bundle.Counts `json:"state_counts"`
	RetainedState   PruneKVCount  `json:"retained_state"`
	Protected       PruneKVCount  `json:"protected"`
	Candidates      PruneKVCount  `json:"candidates"`
	Deleted         PruneKVCount  `json:"deleted"`
	Unknown         PruneKVCount  `json:"unknown"`
	ProtectedDigest common.Hash   `json:"protected_digest"`
}

type pruneHooks struct {
	writeBatch func(*leveldb.DB, *leveldb.Batch) error
	compact    func(*leveldb.DB) error
	syncFiles  func(context.Context, string) error
}

func defaultPruneHooks() pruneHooks {
	return pruneHooks{
		writeBatch: func(db *leveldb.DB, b *leveldb.Batch) error { return db.Write(b, &opt.WriteOptions{Sync: true}) },
		compact:    func(db *leveldb.DB) error { return db.CompactRange(util.Range{}) },
		syncFiles:  syncPruneFiles,
	}
}

// Prune deletes only unreferenced, content-addressed legacy state entries.
// The node must remain stopped; interruption is handled by a fresh invocation.
func Prune(ctx context.Context, options PruneOptions) (PruneResult, error) {
	return pruneWithHooks(ctx, options, defaultPruneHooks())
}

func pruneWithHooks(ctx context.Context, options PruneOptions, hooks pruneHooks) (result PruneResult, retErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	path, parent, err := validatePruneOptions(options)
	if err != nil {
		return result, err
	}
	execution := newPruneExecution(options)
	reporter := newProgressReporter("prune", options.Progress, "chaindata", path, "dry_run", options.DryRun, "workers", execution.workers)
	stage := "open"
	deletionStarted := false
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("prune phase %s (deletion_started=%t); rerun to rebuild the keep set: %w", stage, deletionStarted, retErr)
		}
	}()
	source, err := openPruneDatabase(path, execution.sourceCache, execution.sourceHandles, options.DryRun)
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
	err = runPrunePhase(reporter, stage, execution, func() error {
		var err error
		state, err = buildPruneKeep(ctx, source.view(), temp, head, genesis, node, execution)
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
	keep, err := openPruneKeep(temp, true, execution)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, keep.Close()) }()
	stage = "verify_keep"
	err = runPrunePhase(reporter, stage, execution, func() error { return verifyPruneState(ctx, keep, head, state, execution) })
	if err != nil {
		return result, err
	}
	stage = "preflight"
	var before pruneInventory
	err = runPrunePhase(reporter, stage, execution, func() error {
		var err error
		before, err = scanPruneDB(ctx, source.view(), keep, execution)
		return err
	})
	if err != nil {
		return result, err
	}
	result.RetainedState = before.Keep
	result.Protected = before.Protected
	result.Candidates = before.Candidates
	result.Unknown = before.Unknown
	result.ProtectedDigest = before.Digest
	if err := confirmPruneHead(source.view(), head, header); err != nil {
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
	err = runPrunePhase(reporter, stage, execution, func() error {
		var err error
		result.Deleted, err = deletePruneDifference(ctx, source, keep, hooks, &deletionStarted, execution)
		return err
	})
	if err != nil {
		return result, err
	}
	stage = "verify"
	err = runPrunePhase(reporter, stage, execution, func() error {
		return verifyPruneDatabase(ctx, source, keep, head, header, state, before, hooks, execution)
	})
	if err != nil {
		return result, err
	}
	if options.Compact {
		stage = "compact"
		err = runPrunePhase(reporter, stage, execution, func() error { return compactPruneDatabase(ctx, source, hooks) })
		if err != nil {
			return result, err
		}
		result.Compacted = true
		stage = "verify_compacted"
		err = runPrunePhase(reporter, stage, execution, func() error {
			return verifyPruneDatabase(ctx, source, keep, head, header, state, before, hooks, execution)
		})
		if err != nil {
			return result, err
		}
	}
	result.Verified = true
	return result, nil
}

func validatePruneOptions(o PruneOptions) (string, string, error) {
	if o.Workers > maxMigrateWorkers {
		return "", "", fmt.Errorf("workers must not exceed %d", maxMigrateWorkers)
	}
	if o.CacheMB < 32 || o.Handles < 32 {
		return "", "", errors.New("prune cache-mb and handles must each be at least 32 (each database needs at least 16)")
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

func runPrunePhase(reporter *progressReporter, name string, execution *pruneExecution, fn func() error) error {
	var snapshot progressSnapshot
	if reporter.Enabled() {
		execution.progress = new(progressCounts)
		execution.scan = new(pruneScanProgress)
		snapshot = execution.phaseSnapshot()
	}
	phase := reporter.StartPhase(name, snapshot)
	err := fn()
	phase.Finish(err)
	return err
}

func openPruneKeep(path string, readOnly bool, execution *pruneExecution) (ethdb.Database, error) {
	kv, err := pebble.New(filepath.Join(path, "keep"), execution.keepCache, execution.keepHandles, "", readOnly)
	if err != nil {
		return nil, fmt.Errorf("open prune keep database: %w", err)
	}
	return rawdb.NewDatabase(kv), nil
}

func buildPruneKeep(ctx context.Context, db ethdb.Database, path string, head bundle.Head, genesis common.Hash, node []byte, execution *pruneExecution) (state StateResult, retErr error) {
	keep, err := openPruneKeep(path, false, execution)
	if err != nil {
		return state, err
	}
	defer func() { retErr = errors.Join(retErr, keep.Close()) }()
	state, err = collectPruneState(ctx, db, keep, head, genesis, node, execution)
	if err != nil {
		return state, err
	}
	return state, keep.SyncKeyValue()
}

func collectPruneState(ctx context.Context, db, keep ethdb.Database, head bundle.Head, genesis common.Hash, node []byte, execution *pruneExecution) (StateResult, error) {
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	writer := newPruneKeepWriter(keep, execution.workers, cancel)
	writer.progress = execution.scan
	state, err := validatePartitionedState(workCtx, &pruneRecordingDB{Database: db, keep: writer}, head, execution.workers, execution.progress)
	if err == nil && len(node) > 0 {
		err = writer.put(genesis[:], node)
	}
	if latched := writer.failure.load(); latched != nil {
		err = latched
	}
	if err != nil {
		writer.abort()
		return state, err
	}
	if err = writer.close(ctx); err != nil {
		return state, err
	}
	return state, nil
}

func verifyPruneState(ctx context.Context, db ethdb.Database, head bundle.Head, want StateResult, execution *pruneExecution) error {
	state, err := validatePartitionedState(ctx, db, head, execution.workers, execution.progress)
	if err != nil {
		return err
	}
	if state != want {
		return errors.New("prune state root or counts changed")
	}
	return nil
}

func confirmPruneHead(db ethdb.Database, head bundle.Head, header []byte) error {
	after, blob, err := readLegacyHead(db)
	if err != nil {
		return err
	}
	if head != after || !bytes.Equal(header, blob) {
		return errors.New("prune canonical head changed")
	}
	return nil
}

func validatePruneGenesisRoot(db ethdb.Database, root common.Hash) (retErr error) {
	nodes := triedb.NewDatabase(db, triedb.HashDefaults)
	defer func() { retErr = errors.Join(retErr, nodes.Close()) }()
	tree, err := trie.NewStateTrie(trie.StateTrieID(root), nodes)
	if err != nil {
		return err
	}
	it, err := tree.NodeIterator(nil)
	if err != nil {
		return err
	}
	if !it.Next(true) {
		if err := it.Error(); err != nil {
			return err
		}
		return errors.New("genesis root node is missing")
	}
	return it.Error()
}

func deletePruneDifference(ctx context.Context, p *pruneDatabase, keep ethdb.Database, hooks pruneHooks, started *bool, execution *pruneExecution) (deleted PruneKVCount, retErr error) {
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
	err := walkPruneDifference(ctx, p.view(), keep, execution, func(record *pruneScanRecord) error {
		if record.role != pruneDelete {
			return nil
		}
		batch.Delete(record.key)
		pending.add(record.key, record.value)
		if batch.Len() >= pruneDeleteBatchKeys || len(batch.Dump()) >= pruneDeleteBatchBytes {
			return flush()
		}
		return nil
	})
	if err != nil {
		return deleted, err
	}
	if err := flush(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func verifyPruneDatabase(ctx context.Context, p *pruneDatabase, keep ethdb.Database, head bundle.Head, header []byte, state StateResult, before pruneInventory, hooks pruneHooks, execution *pruneExecution) error {
	if err := p.closeDB(); err != nil {
		return err
	}
	if err := hooks.syncFiles(ctx, p.path); err != nil {
		return fmt.Errorf("sync pruned database: %w", err)
	}
	if err := p.reopen(true); err != nil {
		return err
	}
	if err := confirmPruneHead(p.view(), head, header); err != nil {
		return err
	}
	if err := verifyPruneState(ctx, p.view(), head, state, execution); err != nil {
		return err
	}
	if _, _, err := readPruneGenesis(p.view()); err != nil {
		return err
	}
	after, err := scanPruneDB(ctx, p.view(), keep, execution)
	if err != nil {
		return err
	}
	return comparePruneInventory(before, after)
}

func compactPruneDatabase(ctx context.Context, p *pruneDatabase, hooks pruneHooks) error {
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
