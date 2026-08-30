package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

const (
	// VerificationFormat identifies the verification report format.
	VerificationFormat = "metis-l2state-verification"
	// VerificationVersion is the supported verification report version.
	VerificationVersion = 3
	// VerificationFileName is the fixed report name in a state artifact.
	VerificationFileName = "verification.json"
)

// VerificationReport records independently recomputed bundle and artifact evidence.
type VerificationReport struct {
	Format          string        `json:"format"`
	Version         uint64        `json:"version"`
	VerifiedAt      time.Time     `json:"verified_at"`
	Verified        bool          `json:"verified"`
	Scheme          string        `json:"scheme"`
	DBEngine        string        `json:"db_engine,omitempty"`
	ToolVersion     string        `json:"tool_version"`
	GethVersion     string        `json:"geth_version"`
	GethCommit      string        `json:"geth_commit"`
	ManifestSHA256  common.Hash   `json:"manifest_sha256"`
	StateFileSHA256 common.Hash   `json:"state_file_sha256"`
	RecordChainHash common.Hash   `json:"record_chain_hash"`
	Head            bundle.Head   `json:"head"`
	Counts          bundle.Counts `json:"counts"`
	RecomputedRoot  common.Hash   `json:"recomputed_state_root"`
}

func newVerificationReport(bundleResult BundleResult, scheme string) VerificationReport {
	dbEngine := ""
	if scheme == "hash" || scheme == "path" {
		dbEngine = "pebble-v2"
	}
	return VerificationReport{
		Format:          VerificationFormat,
		Version:         VerificationVersion,
		VerifiedAt:      time.Now().UTC(),
		Verified:        true,
		Scheme:          scheme,
		DBEngine:        dbEngine,
		ToolVersion:     version.ToolVersion,
		GethVersion:     version.GethVersion,
		GethCommit:      version.GethCommit,
		ManifestSHA256:  bundleResult.ManifestSHA256,
		StateFileSHA256: bundleResult.Records.FileSHA256,
		RecordChainHash: bundleResult.Records.RecordChainHash,
		Head:            bundleResult.Manifest.Source.HeadBefore,
		Counts:          bundleResult.State.Counts,
		RecomputedRoot:  bundleResult.State.Root,
	}
}

// Validate checks the report format, versions, digests, and recomputed root.
func (r VerificationReport) Validate() error {
	if r.Format != VerificationFormat || r.Version != VerificationVersion {
		return fmt.Errorf("unsupported verification report %q version %d", r.Format, r.Version)
	}
	if !r.Verified {
		return errors.New("verification report is not marked verified")
	}
	if r.VerifiedAt.IsZero() {
		return errors.New("verification report timestamp is empty")
	}
	if r.Scheme != "bundle" && r.Scheme != "hash" && r.Scheme != "path" {
		return fmt.Errorf("invalid verification scheme %q", r.Scheme)
	}
	if (r.Scheme == "hash" || r.Scheme == "path") && r.DBEngine != "pebble-v2" {
		return fmt.Errorf("invalid database engine %q", r.DBEngine)
	}
	if r.ToolVersion == "" || r.GethVersion != version.GethVersion || r.GethCommit != version.GethCommit {
		return errors.New("verification report tool/geth version mismatch")
	}
	if r.ManifestSHA256 == (common.Hash{}) || r.StateFileSHA256 == (common.Hash{}) || r.RecordChainHash == (common.Hash{}) {
		return errors.New("verification report digest is empty")
	}
	if r.RecomputedRoot != r.Head.StateRoot {
		return errors.New("verification report root does not match head state root")
	}
	return nil
}

func writeVerificationReport(dir string, report VerificationReport) ([]byte, error) {
	if err := report.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode verification report: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, VerificationFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("write verification report: %w", err)
	}
	if err := syncFile(path); err != nil {
		return nil, err
	}
	return data, nil
}

func loadVerificationReport(dir string) (VerificationReport, error) {
	data, err := os.ReadFile(filepath.Join(dir, VerificationFileName))
	if err != nil {
		return VerificationReport{}, fmt.Errorf("read verification report: %w", err)
	}
	var report VerificationReport
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return VerificationReport{}, fmt.Errorf("decode verification report: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return VerificationReport{}, errors.New("decode verification report: trailing JSON value")
		}
		return VerificationReport{}, fmt.Errorf("decode verification report trailing data: %w", err)
	}
	if err := report.Validate(); err != nil {
		return VerificationReport{}, err
	}
	return report, nil
}
