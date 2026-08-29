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
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

const (
	// DirectVerificationFormat identifies reports produced without a bundle.
	DirectVerificationFormat = "metis-l2state-direct-verification"
	// DirectVerificationVersion is the supported direct verification report version.
	DirectVerificationVersion = 1
)

// DirectVerificationReport records independently recomputed source and artifact evidence.
type DirectVerificationReport struct {
	Format         string                `json:"format"`
	Version        uint64                `json:"version"`
	VerifiedAt     time.Time             `json:"verified_at"`
	Verified       bool                  `json:"verified"`
	Scheme         string                `json:"scheme"`
	DBEngine       string                `json:"db_engine"`
	ToolVersion    string                `json:"tool_version"`
	GethVersion    string                `json:"geth_version"`
	GethCommit     string                `json:"geth_commit"`
	Source         bundle.SourceEvidence `json:"source"`
	Counts         bundle.Counts         `json:"counts"`
	RecomputedRoot common.Hash           `json:"recomputed_state_root"`
}

func newDirectVerificationReport(source bundle.SourceEvidence, state StateResult, scheme string) DirectVerificationReport {
	return DirectVerificationReport{
		Format:         DirectVerificationFormat,
		Version:        DirectVerificationVersion,
		VerifiedAt:     time.Now().UTC(),
		Verified:       true,
		Scheme:         scheme,
		DBEngine:       "pebble-v2",
		ToolVersion:    version.ToolVersion,
		GethVersion:    version.GethVersion,
		GethCommit:     version.GethCommit,
		Source:         source,
		Counts:         state.Counts,
		RecomputedRoot: state.Root,
	}
}

// Validate checks the report format, source evidence, counts, and target root.
func (r DirectVerificationReport) Validate() error {
	if r.Format != DirectVerificationFormat || r.Version != DirectVerificationVersion {
		return fmt.Errorf("unsupported direct verification report %q version %d", r.Format, r.Version)
	}
	if !r.Verified {
		return errors.New("direct verification report is not marked verified")
	}
	if r.VerifiedAt.IsZero() {
		return errors.New("direct verification report timestamp is empty")
	}
	if r.Scheme != rawdb.HashScheme && r.Scheme != rawdb.PathScheme {
		return fmt.Errorf("invalid direct verification scheme %q", r.Scheme)
	}
	if r.DBEngine != "pebble-v2" {
		return fmt.Errorf("invalid database engine %q", r.DBEngine)
	}
	if r.ToolVersion == "" || r.GethVersion != version.GethVersion || r.GethCommit != version.GethCommit {
		return errors.New("direct verification report tool/geth version mismatch")
	}
	if err := r.Source.Validate(); err != nil {
		return fmt.Errorf("validate direct source evidence: %w", err)
	}
	if err := r.Counts.Validate(); err != nil {
		return fmt.Errorf("validate direct state counts: %w", err)
	}
	if r.RecomputedRoot == (common.Hash{}) {
		return errors.New("direct verification report root is empty")
	}
	if r.RecomputedRoot != r.Source.HeadBefore.StateRoot {
		return errors.New("direct verification report root does not match source head state root")
	}
	return nil
}

func writeDirectVerificationReport(dir string, report DirectVerificationReport) ([]byte, error) {
	if err := report.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode direct verification report: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, VerificationFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("write direct verification report: %w", err)
	}
	if err := syncFile(path); err != nil {
		return nil, err
	}
	return data, nil
}

func loadDirectVerificationReport(dir string) (DirectVerificationReport, error) {
	data, err := os.ReadFile(filepath.Join(dir, VerificationFileName))
	if err != nil {
		return DirectVerificationReport{}, fmt.Errorf("read direct verification report: %w", err)
	}
	var report DirectVerificationReport
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		return DirectVerificationReport{}, fmt.Errorf("decode direct verification report: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return DirectVerificationReport{}, errors.New("decode direct verification report: trailing JSON value")
		}
		return DirectVerificationReport{}, fmt.Errorf("decode direct verification report trailing data: %w", err)
	}
	if err := report.Validate(); err != nil {
		return DirectVerificationReport{}, err
	}
	return report, nil
}
