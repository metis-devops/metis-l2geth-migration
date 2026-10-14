package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

// These wire types describe mvm's existing audit v1 output. Configuration and
// witness hashes are provenance, not migration configuration or witness hashes.
type ovmAuditIdentity struct {
	Version        int            `json:"version"`
	Genesis        common.Hash    `json:"genesis"`
	ConfigHash     common.Hash    `json:"configHash"`
	WitnessHash    common.Hash    `json:"witnessHash"`
	To             uint64         `json:"to"`
	ToHash         common.Hash    `json:"toHash"`
	DeSeqBlock     uint64         `json:"deSeqBlock"`
	SeqValidHeight uint64         `json:"seqValidHeight"`
	FirstSequencer common.Address `json:"firstSequencer"`
}

type ovmAuditScan struct {
	Root         common.Hash `json:"root"`
	CodeHash     common.Hash `json:"ovmCodeHash"`
	BalanceSlots uint64      `json:"balanceSlots"`
	Balances     string      `json:"balancesWei"`
	Supply       string      `json:"supplyWei"`
	Difference   string      `json:"differenceWei"`
}

type ovmAuditDelta struct {
	Balance     string `json:"balanceDeltaWei"`
	Supply      string `json:"supplyDeltaWei"`
	Difference  string `json:"discrepancyDeltaWei"`
	Gas         string `json:"gasShortfallWei"`
	Suicide     string `json:"legacySelfdestructWei"`
	Unexplained string `json:"unexplainedDeltaWei"`
}

type ovmAuditSummary struct {
	Version                      int              `json:"version"`
	Identity                     ovmAuditIdentity `json:"identity"`
	ClientVersion                string           `json:"clientVersion"`
	From                         uint64           `json:"from"`
	VerifiedBlocks               uint64           `json:"verifiedBlocks"`
	Genesis                      ovmAuditScan     `json:"genesisState"`
	Target                       ovmAuditScan     `json:"targetState"`
	Totals                       ovmAuditDelta    `json:"totals"`
	ExecutionVerified            bool             `json:"executionVerified"`
	AccountingReconciled         bool             `json:"accountingReconciled"`
	IntervalCausesFullyExplained bool             `json:"intervalCausesFullyExplained"`
	UnexplainedEvents            uint64           `json:"unexplainedEvents"`
	GenesisDifferenceExplained   bool             `json:"genesisDifferenceExplained"`
	PreviousDiagnosisDifference  *string          `json:"previousDiagnosisDifferenceWei,omitempty"`
	MatchesPreviousDiagnosis     bool             `json:"matchesPreviousDiagnosis,omitempty"`
}

// OVMAuditEvidence binds exact operator input to independently computed amounts.
type OVMAuditEvidence struct {
	FileSHA256       common.Hash  `json:"file_sha256"`
	BalanceSum       *uint256.Int `json:"balance_sum"`
	SupplyDifference string       `json:"supply_difference"`
	AuditHead        *bundle.Head `json:"audit_head,omitempty"`
}

// UnmarshalJSON rejects incomplete, null, duplicate and unknown evidence fields.
func (e *OVMAuditEvidence) UnmarshalJSON(data []byte) error {
	type plain OVMAuditEvidence
	if err := auditJSONFields(data, reflect.TypeFor[plain]()); err != nil {
		return err
	}
	v, err := strictio.DecodeJSON[plain](data, "OVM audit evidence")
	if err == nil {
		*e = OVMAuditEvidence(v)
	}
	return err
}

// Enforce exact field names, presence, duplicate rejection and non-null values
// recursively for these small, fixed schemas. Typed decoding checks scalars.
func auditJSONFields(data []byte, schema reflect.Type) error {
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("OVM audit requires an object")
	}
	fields := make(map[string]reflect.StructField, schema.NumField())
	for f := range schema.Fields() {
		fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f
	}
	seen := make(map[string]bool, len(fields))
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := tok.(string)
		if !ok {
			return errors.New("invalid OVM audit field")
		}
		f, ok := fields[name]
		if !ok || seen[name] {
			return fmt.Errorf("unknown or duplicate OVM audit field %q", name)
		}
		seen[name] = true
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("OVM audit field %s must not be null", name)
		}
		fieldType := f.Type
		if fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Struct {
			if err := auditJSONFields(raw, fieldType); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing OVM audit data")
	}
	for name, f := range fields {
		if !seen[name] && !strings.Contains(f.Tag.Get("json"), ",omitempty") {
			return fmt.Errorf("missing OVM audit field %s", name)
		}
	}
	return nil
}

func auditInteger(s string, unsigned bool) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.String() != s || (unsigned && (v.Sign() < 0 || v.BitLen() > 256)) {
		return nil, fmt.Errorf("invalid OVM audit integer %q", s)
	}
	return v, nil
}

func (a *ovmAuditSummary) validate() error {
	if a.Version != 1 || a.Identity.Version != 1 {
		return errors.New("unsupported OVM audit version")
	}
	if a.ClientVersion == "" || a.From != 1 || a.Identity.To == 0 || a.VerifiedBlocks != a.Identity.To {
		return errors.New("incomplete OVM audit coverage or provenance")
	}
	if slices.Contains([]common.Hash{a.Identity.Genesis, a.Identity.ToHash, a.Identity.ConfigHash, a.Identity.WitnessHash, a.Genesis.Root, a.Genesis.CodeHash, a.Target.Root, a.Target.CodeHash}, common.Hash{}) {
		return errors.New("empty OVM audit hash")
	}
	if !a.ExecutionVerified || !a.AccountingReconciled || !a.IntervalCausesFullyExplained || !a.GenesisDifferenceExplained || a.UnexplainedEvents != 0 || a.Totals.Unexplained != "0" || a.Genesis.Difference != "0" {
		return errors.New("OVM audit causes are not fully explained")
	}
	for _, scan := range []ovmAuditScan{a.Genesis, a.Target} {
		b, err := auditInteger(scan.Balances, true)
		if err != nil {
			return err
		}
		s, err := auditInteger(scan.Supply, true)
		if err != nil {
			return err
		}
		d, err := auditInteger(scan.Difference, false)
		if err != nil {
			return err
		}
		if new(big.Int).Sub(b, s).Cmp(d) != 0 {
			return errors.New("OVM audit scan arithmetic mismatch")
		}
	}
	for _, pair := range [][3]string{{a.Genesis.Balances, a.Target.Balances, a.Totals.Balance}, {a.Genesis.Supply, a.Target.Supply, a.Totals.Supply}, {a.Genesis.Difference, a.Target.Difference, a.Totals.Difference}} {
		before, _ := auditInteger(pair[0], false)
		after, _ := auditInteger(pair[1], false)
		delta, err := auditInteger(pair[2], false)
		if err != nil {
			return err
		}
		if new(big.Int).Sub(after, before).Cmp(delta) != 0 {
			return errors.New("OVM audit cumulative arithmetic mismatch")
		}
	}
	gas, err := auditInteger(a.Totals.Gas, false)
	if err != nil {
		return err
	}
	suicide, err := auditInteger(a.Totals.Suicide, false)
	if err != nil {
		return err
	}
	if gas.Sign() < 0 || suicide.Sign() < 0 || new(big.Int).Add(gas, suicide).String() != a.Totals.Difference {
		return errors.New("OVM audit cause arithmetic mismatch")
	}
	if a.PreviousDiagnosisDifference != nil {
		if _, err := auditInteger(*a.PreviousDiagnosisDifference, false); err != nil {
			return err
		}
	}
	return nil
}

func readOVMAuditSummary(ctx context.Context, path string) (summary *ovmAuditSummary, digest common.Hash, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, digest, err
	}
	f, err := openOVMInput(path)
	if err != nil {
		return nil, digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	data, err := io.ReadAll(io.LimitReader(f, strictio.MaxMetadataSize+1))
	if err != nil {
		return nil, digest, err
	}
	if int64(len(data)) > strictio.MaxMetadataSize {
		return nil, digest, errors.New("OVM audit summary exceeds 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return nil, digest, err
	}
	if err := auditJSONFields(data, reflect.TypeFor[ovmAuditSummary]()); err != nil {
		return nil, digest, err
	}
	a, err := strictio.DecodeJSON[ovmAuditSummary](data, "OVM audit summary")
	if err != nil {
		return nil, digest, err
	}
	if err := a.validate(); err != nil {
		return nil, digest, err
	}
	return &a, common.Hash(sha256.Sum256(data)), nil
}

func (a *ovmAuditSummary) matchHead(head bundle.Head) error {
	if a.Identity.To > head.BlockNumber {
		return errors.New("OVM audit target is ahead of source head")
	}
	if a.Identity.To == head.BlockNumber && (a.Identity.ToHash != head.BlockHash || a.Target.Root != head.StateRoot) {
		return errors.New("OVM audit target does not match source head")
	}
	return nil
}

func confirmOVMAuditEvidence(ctx context.Context, path string, evidence *OVMAuditEvidence) error {
	if (path != "") != (evidence != nil) {
		return errors.New("OVM verification requires the original --ovm-audit-summary exactly when recorded")
	}
	if evidence == nil {
		return ctx.Err()
	}
	digest, err := hashOVMInput(ctx, path)
	if err != nil {
		return fmt.Errorf("confirm OVM audit summary: %w", err)
	}
	if digest != evidence.FileSHA256 {
		return errors.New("OVM audit summary changed or differs from recorded digest")
	}
	return nil
}

func (a *ovmAuditSummary) matchAccounting(height uint64, code common.Hash, slots uint64, balances, supply *uint256.Int) error {
	if a.Identity.To > height {
		return errors.New("OVM audit target is ahead of source accounting")
	}
	type comparison struct{ name, expected, actual string }
	fields := []comparison{
		{"ovmCodeHash", a.Target.CodeHash.Hex(), code.Hex()},
		{"differenceWei", a.Target.Difference, new(big.Int).Sub(balances.ToBig(), supply.ToBig()).String()},
	}
	if a.Identity.To == height {
		fields = append(fields, comparison{"balanceSlots", fmt.Sprint(a.Target.BalanceSlots), fmt.Sprint(slots)},
			comparison{"balancesWei", a.Target.Balances, balances.Dec()},
			comparison{"supplyWei", a.Target.Supply, supply.Dec()})
	}
	for _, field := range fields {
		if field.expected != field.actual {
			return fmt.Errorf("OVM audit target %s mismatch: summary %s, source %s", field.name, field.expected, field.actual)
		}
	}
	return nil
}

// Build report amounts from conversion at L, never from the historical state at H.
func (a *ovmAuditSummary) conversionEvidence(digest common.Hash, head bundle.Head, balances OVMBalanceEvidence) (*OVMAuditEvidence, error) {
	if err := a.matchHead(head); err != nil {
		return nil, err
	}
	if balances.OriginalSupply == nil || balances.MigratedNative == nil || balances.RemainingSupply == nil {
		return nil, errors.New("missing OVM conversion amounts")
	}
	sum, overflow := new(uint256.Int).AddOverflow(balances.MigratedNative, balances.RemainingSupply)
	if overflow {
		return nil, errors.New("OVM converted balance sum overflows uint256")
	}
	difference := new(big.Int).Sub(sum.ToBig(), balances.OriginalSupply.ToBig()).String()
	if difference != a.Target.Difference {
		return nil, errors.New("OVM conversion difference does not match audit")
	}
	evidence := &OVMAuditEvidence{FileSHA256: digest, BalanceSum: sum, SupplyDifference: difference}
	if a.Identity.To < head.BlockNumber {
		evidence.AuditHead = &bundle.Head{BlockNumber: a.Identity.To, BlockHash: a.Identity.ToHash, StateRoot: a.Target.Root}
	}
	return evidence, nil
}
