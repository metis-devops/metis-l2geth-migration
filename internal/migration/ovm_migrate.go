package migration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func cryptoCodeHash(in ovmInputs) common.Hash { return crypto.Keccak256Hash(in.code) }

func migrateOVM(ctx context.Context, opts MigrateOptions) (result MigrateResult, retErr error) {
	reporter := newProgressReporter("migrate", opts.Progress, "temp_db", opts.TempDB.normalized(), "ovm_eth", true, "workers", normalizeMigrateWorkers(opts.Workers))
	defer func() { reporter.Finish(retErr, "artifact", result.ArtifactPath) }()
	if err := validateMigrateOptions(opts); err != nil {
		return result, err
	}
	if err := validateOVMResources(opts); err != nil {
		return result, err
	}
	if err := rejectOVMOutputs(opts, opts.Output); err != nil {
		return result, err
	}
	output, err := newAtomicDir(opts.Output)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, output.Abort()) }()
	if err := rejectOVMOutputs(opts, output.Path()); err != nil {
		return result, err
	}
	report, err := replayOVMState(ctx, opts, output.Path(), reporter, true)
	if err != nil {
		return result, err
	}
	err = publishArtifact(ctx, output, report, artifactReportCodec[OVMVerificationReport]{
		label: "OVM verification report", write: writeOVMReport,
		load: loadOVMReport, equal: sameOVMReport,
	}, func() error {
		if err := rejectOVMOutputs(opts, opts.Output); err != nil {
			return err
		}
		return confirmOVMReportInputs(ctx, opts.OVM, report)
	}, reporter)
	if err != nil {
		return result, err
	}
	return newOVMMigrateResult(opts.Output, report), nil
}

func validateOVMResources(opts MigrateOptions) error {
	if opts.CacheMB < 64 || opts.Handles < 64 {
		return errors.New("OVM conversion requires --cache-mb and --handles of at least 64 for its concurrent databases")
	}
	return nil
}

func rejectOVMOutputs(opts MigrateOptions, output string) error {
	if err := rejectOutputInsideSource(opts.SourceChaindata, output); err != nil {
		return err
	}
	if opts.OVM.SourceAncient != "" {
		if err := rejectOutputInsideDirectory(opts.OVM.SourceAncient, output, "output must be outside source ancient", "output aliases source ancient"); err != nil {
			return err
		}
	}
	return nil
}

type ovmWork struct {
	storage  *temporaryStorage
	ctx      context.Context
	opts     MigrateOptions
	path     string
	reporter *progressReporter
	source   *legacySource
	base     ethdb.Database
	indexDB  ethdb.Database
	index    *ovmIndex
	inputs   ovmInputs
	original StateResult
	history  OVMHistoryEvidence
	limiter  *migrateWorkLimiter
}

func replayOVMState(ctx context.Context, opts MigrateOptions, root string, reporter *progressReporter, persistTarget bool) (report OVMVerificationReport, retErr error) {
	w := ovmWork{storage: newTemporaryStorage(opts.TempDB), ctx: ctx, opts: opts, path: filepath.Join(root, ".ovm-work"), reporter: reporter, limiter: newMigrateWorkLimiter(opts.Workers)}
	observeTemporaryStorage(ctx, w.storage)
	if err := os.Mkdir(w.path, 0700); err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, w.close(), w.storage.remove(w.path), os.RemoveAll(w.path)) }()
	if err := w.prepare(); err != nil {
		return report, err
	}
	if err := w.migrateOriginalAndHistory(); err != nil {
		return report, err
	}
	phase := reporter.StartPhase("convert_ovm_balances", nil)
	newRoot, balances, err := transformOVMState(ctx, w.base, w.original.Root, w.index, w.inputs, opts.Workers, w.limiter)
	phase.Finish(err)
	if err != nil {
		return report, err
	}
	newRoot, allocEvidence, err := w.applyGenesisAlloc(newRoot)
	if err != nil {
		return report, err
	}
	provisional := w.sourceEvidence()
	checkpoint, err := ovmCheckpoint(provisional, newRoot)
	if err != nil {
		return report, err
	}
	final, target, err := w.finishState(root, newRoot, checkpoint, persistTarget)
	if err != nil {
		return report, err
	}
	if err := w.confirmInputs(); err != nil {
		return report, err
	}
	sourceEvidence, err := w.source.ConfirmStableAndClose("OVM migration")
	if err != nil {
		return report, err
	}
	report = newOVMReport(sourceEvidence, w.original, final, checkpoint, w.inputs, w.history, balances, target, opts.Scheme)
	report.GenesisAlloc = allocEvidence
	return report, report.Validate()
}

func (w *ovmWork) close() error {
	var err error
	if w.index != nil {
		w.index.batch.Close()
		w.index = nil
	}
	if w.indexDB != nil {
		err = errors.Join(err, w.indexDB.Close())
		w.indexDB = nil
	}
	if w.base != nil {
		err = errors.Join(err, w.base.Close())
		w.base = nil
	}
	if w.source != nil {
		err = errors.Join(err, w.source.Close())
	}
	return err
}

func (w *ovmWork) prepare() error {
	var err error
	w.inputs.code, w.inputs.codeFileDigest, err = readWrappedCode(w.ctx, w.opts.OVM.WrappedEtherCode)
	if err != nil {
		return err
	}
	// Four database allowances: source, original state, evidence, final target.
	cache, handles := w.opts.CacheMB/4, w.opts.Handles/4
	w.source, err = openLegacySource(w.opts.SourceChaindata, cache, handles, w.reporter)
	if err != nil {
		return err
	}
	if _, err := ovmCheckpoint(w.sourceEvidence(), w.source.head.StateRoot); err != nil {
		return err
	}
	w.base, err = w.storage.create(filepath.Join(w.path, "base"), cache, handles)
	if err != nil {
		return err
	}
	w.indexDB, err = w.storage.create(filepath.Join(w.path, "evidence"), cache, handles)
	if err != nil {
		return err
	}
	w.index = newOVMIndex(w.indexDB)
	if w.opts.OVM.ERC20RetainList != "" {
		digest, err := loadOVMERC20RetainList(w.ctx, w.opts.OVM.ERC20RetainList, w.index)
		if err != nil {
			return err
		}
		w.inputs.retention = &OVMERC20RetentionEvidence{FileSHA256: digest}
	}
	if w.opts.OVM.GenesisAlloc != "" {
		w.inputs.allocDigest, err = loadOVMGenesisAlloc(w.ctx, w.opts.OVM.GenesisAlloc, w.index)
		if err != nil {
			return err
		}
	}
	if err := w.index.address(ovmETHAddress); err != nil {
		return err
	}
	w.inputs.witnessDigest, err = loadOVMWitness(w.ctx, w.opts.OVM.StateWitness, w.index)
	if err != nil {
		return err
	}
	return nil
}

func openOVMDatabase(target targetConfig, path string, cache, handles int) (ethdb.Database, error) {
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, err
	}
	kv, err := target.open(path, cache, handles, false)
	if err != nil {
		return nil, err
	}
	return rawdb.NewDatabase(kv), nil
}

func (w *ovmWork) sourceEvidence() bundle.SourceEvidence {
	return bundle.SourceEvidence{HeadBefore: w.source.head, HeadAfter: w.source.head, HeaderRLP: w.source.headerRLP}
}

func (w *ovmWork) migrateOriginalAndHistory() error {
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	var wg sync.WaitGroup
	var preimageErr, historyErr error
	wg.Go(func() {
		// Share the database, never the mutable batch. Overlapping address
		// discoveries write identical keys and values in either order.
		index := newOVMIndex(w.indexDB)
		defer index.batch.Close()
		preimageErr = collectOVMPreimages(ctx, w.source.db, index, w.limiter)
		if preimageErr != nil {
			cancel()
		}
	})
	wg.Go(func() {
		// This lane exclusively owns w.index, including any pending inputs.
		w.history, historyErr = scanOVMHistory(ctx, w.source, w.opts, w.index, w.limiter, w.reporter)
		if historyErr != nil {
			cancel()
		}
	})
	phase := w.reporter.StartPhase("migrate_original_state", nil, "root", w.source.head.StateRoot)
	var stateErr error
	w.original, stateErr = runOVMPartitioned(ctx, w.source.db, w.source.head.StateRoot, w.opts.Workers, w.limiter, nil, requireZeroOVMNative, persistentPartitionOutput(w.base, "hash"))
	phase.Finish(stateErr)
	if stateErr != nil {
		cancel()
	}
	wg.Wait()
	if err := errors.Join(stateErr, preimageErr, historyErr); err != nil {
		return err
	}
	return w.reopenOriginal()
}

func (w *ovmWork) reopenOriginal() (retErr error) {
	cache, handles := w.opts.CacheMB/4, w.opts.Handles/4
	config := targetConfig{engine: DBEnginePebble, layout: LayoutGeth}
	path := filepath.Join(w.path, "base")
	if w.storage.fs == nil {
		_, closed, err := finalizeAndVerifyTarget(w.ctx, w.base, path, "hash", config, w.sourceEvidence(), w.original, cache, handles, w.reporter, w.opts.TempDB)
		if closed {
			w.base = nil
		}
		if err != nil {
			return err
		}
	} else {
		if err := persistHeadMetadata(w.ctx, w.base, w.sourceEvidence(), w.reporter); err != nil {
			return err
		}
		if err := w.base.SyncKeyValue(); err != nil {
			return err
		}
		err := w.base.Close()
		w.base = nil
		if err != nil {
			return err
		}
		if err := w.verifyMemoryOriginal(path, config, cache, handles); err != nil {
			return err
		}
	}
	var err error
	w.base, err = w.storage.open(path, cache, handles, false)
	return err
}

func (w *ovmWork) verifyMemoryOriginal(path string, config targetConfig, cache, handles int) (retErr error) {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	db, err := w.storage.open(path, cache, handles, true)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()) }()
	_, err = verifyOpenedTarget(w.ctx, db, path, "hash", config, w.sourceEvidence(), w.original, w.reporter,
		trieNodeIndexOptions{Mode: w.opts.TempDB, CacheMB: cache, Handles: handles})
	return err
}

func runOVMPartitioned(ctx context.Context, source ethdb.Database, root common.Hash, workers int, limiter *migrateWorkLimiter, readCode codeReader, validateAccount func(common.Hash, *types.StateAccount) error, output partitionOutputFactory) (result StateResult, retErr error) {
	tdb := triedb.NewDatabase(source, triedb.HashDefaults)
	defer func() { retErr = errors.Join(retErr, tdb.Close()) }()
	m := partitionedStateMigrator{ctx: ctx, source: source, trieDB: tdb, outputFactory: output, root: root, limiter: limiter, accounts: newMigrateAccountWindow(workers), codeHashes: newConcurrentHashSet(), readCode: readCode, validateAccount: validateAccount}
	result, writer, err := m.run()
	if writer != nil {
		defer writer.Abort()
	}
	if err != nil {
		return result, err
	}
	return result, writer.CloseContext(ctx)
}

// Verification needs replayed root/counts, not a second final artifact. The
// original migrated database is still independently reopened and verified, and
// the supplied artifact receives the full engine/scheme inventory verification.
func (w *ovmWork) finishState(root string, newRoot common.Hash, checkpoint bundle.SourceEvidence, persistTarget bool) (StateResult, targetConfig, error) {
	if persistTarget {
		return w.finalTarget(root, newRoot, checkpoint)
	}
	target, err := targetOptions(w.opts.DBEngine, w.opts.Scheme)
	if err != nil {
		return StateResult{}, target, err
	}
	phase := w.reporter.StartPhase("replay_converted_state", nil, "root", newRoot)
	state, err := runOVMPartitioned(w.ctx, w.base, newRoot, w.opts.Workers, w.limiter, target.readCode, nil, newValidationOutput)
	phase.Finish(err)
	return state, target, err
}

func (w *ovmWork) finalTarget(root string, newRoot common.Hash, checkpoint bundle.SourceEvidence) (final StateResult, target targetConfig, retErr error) {
	target, err := targetOptions(w.opts.DBEngine, w.opts.Scheme)
	if err != nil {
		return final, target, err
	}
	cache, handles := w.opts.CacheMB/4, w.opts.Handles/4
	path := filepath.Join(root, artifactDatabaseDirName)
	db, err := openOVMDatabase(target, path, cache, handles)
	if err != nil {
		return final, target, err
	}
	closed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, db.Close())
		}
	}()
	phase := w.reporter.StartPhase("build_converted_state", nil, "root", newRoot)
	final, err = runOVMPartitioned(w.ctx, w.base, newRoot, w.opts.Workers, w.limiter, target.readCode, nil, persistentPartitionOutput(db, w.opts.Scheme))
	phase.Finish(err)
	if err != nil {
		return final, target, err
	}
	if err := adoptPathState(w.ctx, db, w.opts.Scheme, newRoot, w.reporter); err != nil {
		return final, target, err
	}
	if err := writeHeadMetadata(db, checkpoint); err != nil {
		return final, target, err
	}
	batch := db.NewBatch()
	defer batch.Close()
	for key, value := range ovmBodyMetadata(checkpoint) {
		if err := batch.Put([]byte(key), value); err != nil {
			return final, target, err
		}
	}
	if err := batch.Write(); err != nil {
		return final, target, err
	}
	closed, err = finalizeTargetDatabase(w.ctx, db, path, target, w.reporter)
	if err != nil {
		return final, target, err
	}
	verified, err := verifyTargetDatabase(w.ctx, path, w.opts.Scheme, target, checkpoint, final, cache, handles, w.reporter, trieNodeIndexOptions{Mode: w.opts.TempDB, Parent: w.path, CacheMB: cache, Handles: handles}, ovmBodyMetadata(checkpoint))
	if err != nil {
		return final, target, err
	}
	if verified != final {
		return final, target, errors.New("converted target differs from rebuilt state")
	}
	return final, target, nil
}

func (w *ovmWork) confirmInputs() error {
	return confirmOVMInputs(w.ctx, w.opts.OVM, w.inputs)
}

// requireZeroOVMNative is conversion policy, independent of traversal scheduling.
func requireZeroOVMNative(hash common.Hash, account *types.StateAccount) error {
	if !account.Balance.IsZero() {
		return fmt.Errorf("OVM migration requires zero source native balance: account %s has %s", hash, account.Balance)
	}
	return nil
}
