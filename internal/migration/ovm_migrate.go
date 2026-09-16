package migration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func cryptoCodeHash(in ovmInputs) common.Hash { return crypto.Keccak256Hash(in.code) }

func migrateOVM(ctx context.Context, opts MigrateOptions) (result MigrateResult, retErr error) {
	reporter := newProgressReporter("migrate", opts.Progress, "ovm_eth", true, "workers", normalizeMigrateWorkers(opts.Workers))
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
	phase := reporter.StartPhase("publish_artifact", nil, "output", opts.Output)
	defer func() { phase.Finish(retErr) }()
	if err := writeOVMReport(output.Path(), report); err != nil {
		return result, err
	}
	stored, err := loadOVMReport(output.Path())
	if err != nil {
		return result, err
	}
	if !sameOVMReport(stored, report) {
		return result, errors.New("reopened OVM report differs from generated report")
	}
	if err := rejectOVMOutputs(opts, opts.Output); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := confirmOVMReportInputs(ctx, opts.OVM, report); err != nil {
		return result, err
	}
	if err := output.Commit(); err != nil {
		return result, err
	}
	return MigrateResult{ArtifactPath: opts.Output, OVMReport: &report}, nil
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
	w := ovmWork{ctx: ctx, opts: opts, path: filepath.Join(root, ".ovm-work"), reporter: reporter, limiter: newMigrateWorkLimiter(opts.Workers)}
	if err := os.Mkdir(w.path, 0700); err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, w.close(), os.RemoveAll(w.path)) }()
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
	baseConfig := targetConfig{engine: "pebble-v2", layout: LayoutGeth}
	w.base, err = openOVMDatabase(baseConfig, filepath.Join(w.path, "base"), cache, handles)
	if err != nil {
		return err
	}
	w.indexDB, err = openOVMDatabase(baseConfig, filepath.Join(w.path, "evidence"), cache, handles)
	if err != nil {
		return err
	}
	w.index = newOVMIndex(w.indexDB)
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
	var historyErr error
	wg.Go(func() {
		lease := newMigrateWorkLease(w.limiter)
		historyErr = lease.acquire(ctx)
		if historyErr == nil {
			historyErr = collectOVMPreimages(ctx, w.source.db, w.index)
		}
		lease.release()
		if historyErr == nil {
			w.history, historyErr = scanOVMHistory(ctx, w.source, w.opts, w.index, w.limiter, w.reporter)
		}
		if historyErr != nil {
			cancel()
		}
	})
	phase := w.reporter.StartPhase("migrate_original_state", nil, "root", w.source.head.StateRoot)
	var stateErr error
	w.original, stateErr = runOVMPartitioned(ctx, w.source.db, w.base, w.source.head.StateRoot, "hash", w.opts.Workers, w.limiter, nil, true)
	phase.Finish(stateErr)
	if stateErr != nil {
		cancel()
	}
	wg.Wait()
	if err := errors.Join(stateErr, historyErr); err != nil {
		return err
	}
	cache, handles := w.opts.CacheMB/4, w.opts.Handles/4
	config := targetConfig{engine: "pebble-v2", layout: LayoutGeth}
	_, closed, err := finalizeAndVerifyTarget(w.ctx, w.base, filepath.Join(w.path, "base"), "hash", config, w.sourceEvidence(), w.original, cache, handles, w.reporter)
	if closed {
		w.base = nil
	}
	if err != nil {
		return err
	}
	// This is this invocation's private scratch database, never a reused target.
	kv, err := config.open(filepath.Join(w.path, "base"), cache, handles, false)
	if err != nil {
		return err
	}
	w.base = rawdb.NewDatabase(kv)
	return nil
}

func runOVMPartitioned(ctx context.Context, source, target ethdb.Database, root common.Hash, scheme string, workers int, limiter *migrateWorkLimiter, readCode codeReader, zeroNative bool) (result StateResult, retErr error) {
	tdb := triedb.NewDatabase(source, triedb.HashDefaults)
	defer func() { retErr = errors.Join(retErr, tdb.Close()) }()
	m := partitionedStateMigrator{ctx: ctx, source: source, trieDB: tdb, target: target, scheme: scheme, root: root, limiter: limiter, accounts: newMigrateAccountWindow(workers), codeHashes: newConcurrentHashSet(), readCode: readCode, requireZeroNative: zeroNative}
	if target == nil {
		m.outputFactory = func(bool) partitionStateOutput { return validationStateOutput{} }
	}
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
	state, err := runOVMPartitioned(w.ctx, w.base, nil, newRoot, "hash", w.opts.Workers, w.limiter, target.readCode, false)
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
	final, err = runOVMPartitioned(w.ctx, w.base, db, newRoot, w.opts.Scheme, w.opts.Workers, w.limiter, target.readCode, false)
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
	verified, err := verifyTargetDatabase(w.ctx, path, w.opts.Scheme, target, checkpoint, final, cache, handles, w.reporter, w.path, ovmBodyMetadata(checkpoint))
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
