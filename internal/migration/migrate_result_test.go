package migration

import (
	"encoding/json"
	"testing"
)

func requireDirectReport(t testing.TB, result MigrateResult) DirectVerificationReport {
	t.Helper()
	report, ok := result.DirectReport()
	if !ok {
		t.Fatal("expected ordinary migration report")
	}
	return report
}

func requireOVMReport(t testing.TB, result MigrateResult) *OVMVerificationReport {
	t.Helper()
	report, ok := result.OVMReport()
	if !ok {
		t.Fatal("expected OVM migration report")
	}
	return &report
}

func TestMigrateResultReportDiscriminatorAndJSON(t *testing.T) {
	for _, ovm := range []bool{false, true} {
		var result MigrateResult
		var report any
		if ovm {
			r := OVMVerificationReport{Format: OVMVerificationFormat}
			result, report = newOVMMigrateResult("artifact", r), r
		} else {
			r := DirectVerificationReport{Format: DirectVerificationFormat}
			result, report = newDirectMigrateResult("artifact", r), r
		}
		if _, ok := result.DirectReport(); ok == ovm {
			t.Fatal("wrong ordinary discriminator")
		}
		if _, ok := result.OVMReport(); ok != ovm {
			t.Fatal("wrong OVM discriminator")
		}
		got, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(struct {
			Artifact     string `json:"artifact"`
			Verification any    `json:"verification"`
		}{"artifact", report})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("JSON changed: %s != %s", got, want)
		}
	}
	var zero MigrateResult
	if _, ok := zero.DirectReport(); ok {
		t.Fatal("zero result has ordinary report")
	}
	if _, ok := zero.OVMReport(); ok {
		t.Fatal("zero result has OVM report")
	}
	if _, err := json.Marshal(zero); err == nil {
		t.Fatal("zero result serialized successfully")
	}
}
