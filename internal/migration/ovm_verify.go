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
	reporter := newProgressReporter("verify", opts.Progress, "ovm_eth", true, "artifact", opts.Artifact)
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
	migrate := MigrateOptions{SourceChaindata: opts.SourceChaindata, Scheme: stored.Scheme, DBEngine: stored.DBEngine, CacheMB: opts.CacheMB, Handles: opts.Handles, Workers: opts.Workers, OVM: opts.OVM, Progress: opts.Progress}
	if migrate.DBEngine == "pebble-v2" {
		migrate.DBEngine = "pebble"
	}
	if err := validateOVMResources(migrate); err != nil {
		return report, err
	}
	if opts.Workers > maxMigrateWorkers {
		return report, fmt.Errorf("workers must not exceed %d", maxMigrateWorkers)
	}
	// A sibling scratch tree makes the extra disk requirement visible and avoids
	// ever writing temporary databases inside the artifact being verified.
	parent := filepath.Dir(opts.Artifact)
	if err := rejectOVMOutputs(migrate, parent); err != nil {
		return report, err
	}
	scratch, err := os.MkdirTemp(parent, ".l2state-ovm-verify-")
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
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
	_, err = verifyTargetDatabase(ctx, filepath.Join(opts.Artifact, artifactDatabaseDirName), stored.Scheme, target, recomputed.Checkpoint, state, opts.CacheMB/4, opts.Handles/4, reporter, scratch, ovmBodyMetadata(recomputed.Checkpoint))
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
