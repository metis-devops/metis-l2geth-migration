package migration

import (
	"encoding/json"
	"errors"
)

// MigrateResult contains exactly one successful migration's report. Use the
// typed accessors to distinguish ordinary state preservation from OVM conversion.
type MigrateResult struct {
	ArtifactPath string `json:"artifact"`
	report       migrationReport
}

type migrationReport interface{ migrationReport() }

func (DirectVerificationReport) migrationReport() {}
func (OVMVerificationReport) migrationReport()    {}

func newDirectMigrateResult(path string, report DirectVerificationReport) MigrateResult {
	return MigrateResult{ArtifactPath: path, report: report}
}

func newOVMMigrateResult(path string, report OVMVerificationReport) MigrateResult {
	return MigrateResult{ArtifactPath: path, report: report}
}

// DirectReport returns the ordinary report only for a direct migration result.
func (r MigrateResult) DirectReport() (DirectVerificationReport, bool) {
	report, ok := r.report.(DirectVerificationReport)
	return report, ok
}

// OVMReport returns the conversion report only for an OVM migration result.
func (r MigrateResult) OVMReport() (OVMVerificationReport, bool) {
	report, ok := r.report.(OVMVerificationReport)
	return report, ok
}

// MarshalJSON retains the original wire shape without serializing an absent
// report as a successful migration. The report formats remain independent.
func (r MigrateResult) MarshalJSON() ([]byte, error) {
	switch r.report.(type) {
	case DirectVerificationReport, OVMVerificationReport:
	default:
		return nil, errors.New("migration result has no report")
	}
	return json.Marshal(struct {
		Artifact     string          `json:"artifact"`
		Verification migrationReport `json:"verification"`
	}{r.ArtifactPath, r.report})
}
