package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

// ArtifactVerificationFormat inspects the report discriminator without relaxing
// either report decoder. The selected decoder still validates every field.
func ArtifactVerificationFormat(dir string) (format string, retErr error) {
	if err := validateArtifactLayout(dir); err != nil {
		return "", err
	}
	root, err := strictio.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	data, err := root.ReadRegular(VerificationFileName, strictio.MaxMetadataSize)
	if err != nil {
		return "", err
	}
	var envelope struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("decode report discriminator: %w", err)
	}
	return envelope.Format, nil
}

// OVMVerifyOptions supplies the source and operator inputs for independent replay.
type OVMVerifyOptions struct {
	TempDir         string
	TempDB          TempDBMode
	SourceChaindata string
	Artifact        string
	CacheMB         int
	Handles         int
	Workers         int
	OVM             OVMOptions
	Progress        ProgressOptions
}

// VerifyOVM independently regenerates the expected checkpoint from source state
// and authenticated history, then opens and inventories the supplied artifact.
func VerifyOVM(ctx context.Context, opts OVMVerifyOptions) (report OVMVerificationReport, retErr error) {
	if err := opts.TempDB.validate(); err != nil {
		return report, err
	}
	reporter := newProgressReporter("verify", opts.Progress, "temp_db", opts.TempDB.normalized(), "ovm_eth", true, "artifact", opts.Artifact)
	defer func() { reporter.Finish(retErr) }()
	stored, err := loadOVMReport(opts.Artifact)
	if err != nil {
		return report, err
	}
	opts.OVM.Enabled = true
	if err := validateOVMOptions(opts.OVM); err != nil {
		return report, err
	}
	if err := confirmOVMAllocEvidence(ctx, opts.OVM.GenesisAlloc, stored.GenesisAlloc); err != nil {
		return report, err
	}
	if err := confirmOVMRetentionEvidence(ctx, opts.OVM.ERC20RetainList, stored.ERC20Retention); err != nil {
		return report, err
	}
	migrate := MigrateOptions{TempDB: opts.TempDB, SourceChaindata: opts.SourceChaindata, Scheme: stored.Scheme, DBEngine: stored.DBEngine, CacheMB: opts.CacheMB, Handles: opts.Handles, Workers: opts.Workers, OVM: opts.OVM, Progress: opts.Progress}
	if err := validateOVMResources(migrate); err != nil {
		return report, err
	}
	if opts.Workers > maxMigrateWorkers {
		return report, fmt.Errorf("workers must not exceed %d", maxMigrateWorkers)
	}
	ancient := opts.OVM.SourceAncient
	if ancient == "" {
		ancient = filepath.Join(opts.SourceChaindata, "ancient")
		if _, err := os.Lstat(ancient); errors.Is(err, os.ErrNotExist) {
			ancient = ""
		} else if err != nil {
			return report, fmt.Errorf("inspect source ancient: %w", err)
		}
	}
	workspace, err := prepareVerificationWorkspace(opts.TempDir, opts.SourceChaindata, ancient, opts.Artifact)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, workspace.Close()) }()
	scratch, err := workspace.create(ctx)
	if err != nil {
		return report, err
	}
	recomputed, err := replayOVMState(ctx, migrate, scratch, reporter, false)
	if err != nil {
		return report, err
	}
	expected := recomputed
	expected.VerifiedAt = stored.VerifiedAt
	expected.ToolVersion = stored.ToolVersion
	expected.GethVersion = stored.GethVersion
	if !sameOVMReport(expected, stored) {
		return report, errors.New("OVM artifact evidence does not match independently replayed source and inputs")
	}
	target, err := reportTarget(stored.DBEngine, stored.StateLayout, stored.Scheme)
	if err != nil {
		return report, err
	}
	state := StateResult{Root: recomputed.Target.Root, Counts: recomputed.Target.Counts}
	_, err = verifyTargetDatabase(ctx, filepath.Join(opts.Artifact, artifactDatabaseDirName), stored.Scheme, target, recomputed.Checkpoint, state, opts.CacheMB/4, opts.Handles/4, reporter, trieNodeIndexOptions{Mode: opts.TempDB, Parent: scratch, CacheMB: opts.CacheMB / 4, Handles: opts.Handles / 4}, ovmBodyMetadata(recomputed.Checkpoint))
	if err != nil {
		return report, err
	}
	after, err := loadOVMReport(opts.Artifact)
	if err != nil {
		return report, err
	}
	if !sameOVMReport(stored, after) {
		return report, errors.New("OVM report changed during verification")
	}
	if err := confirmOVMReportInputs(ctx, opts.OVM, stored); err != nil {
		return report, err
	}
	return recomputed, nil
}
