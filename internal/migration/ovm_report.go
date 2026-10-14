package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
	"github.com/metis-devops/metis-l2geth-migration/internal/formatversion"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

// OVMVerificationFormat identifies balance-converted migration checkpoints.
const OVMVerificationFormat = "metis-l2state-ovm-verification"

// OVMStateEvidence contains independently traversed consensus state evidence.
type OVMStateEvidence struct {
	Root   common.Hash   `json:"state_root"`
	Counts bundle.Counts `json:"counts"`
}

// OVMGenesisAllocEvidence separates balance conversion from operator overrides.
type OVMGenesisAllocEvidence struct {
	FileSHA256 common.Hash      `json:"file_sha256"`
	Converted  OVMStateEvidence `json:"converted_state"`
}

// OVMERC20RetentionEvidence binds manual eligibility to an exact operator input.
type OVMERC20RetentionEvidence struct {
	FileSHA256 common.Hash `json:"file_sha256"`
}

// OVMVerificationReport is independent of root-preserving direct reports.
type OVMVerificationReport struct {
	Format          string                     `json:"format"`
	Version         uint64                     `json:"version"`
	VerifiedAt      time.Time                  `json:"verified_at"`
	Verified        bool                       `json:"verified"`
	Scheme          string                     `json:"scheme"`
	DBEngine        string                     `json:"db_engine"`
	StateLayout     StateLayout                `json:"state_layout"`
	ToolVersion     string                     `json:"tool_version"`
	GethVersion     string                     `json:"geth_version"`
	Source          bundle.SourceEvidence      `json:"source"`
	Original        OVMStateEvidence           `json:"original_state"`
	Target          OVMStateEvidence           `json:"target_state"`
	Checkpoint      bundle.SourceEvidence      `json:"checkpoint"`
	WrappedCodeHash common.Hash                `json:"wrapped_code_hash"`
	CodeFileSHA256  common.Hash                `json:"code_file_sha256"`
	WitnessSHA256   common.Hash                `json:"witness_sha256"`
	History         OVMHistoryEvidence         `json:"history"`
	Balances        OVMBalanceEvidence         `json:"balances"`
	GenesisAlloc    *OVMGenesisAllocEvidence   `json:"genesis_alloc,omitempty"`
	AuditSummary    *OVMAuditEvidence          `json:"audit_summary,omitempty"`
	ERC20Retention  *OVMERC20RetentionEvidence `json:"erc20_retention,omitempty"`
}

// UnmarshalJSON rejects ambiguous top-level fields before typed decoding and
// distinguishes omitted optional evidence from explicit null.
func (r *OVMVerificationReport) UnmarshalJSON(data []byte) error {
	type reportJSON OVMVerificationReport
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("OVM verification report must be an object")
	}
	var names []string
	for field := range reflect.TypeFor[reportJSON]().Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		names = append(names, name)
	}
	seen := make(map[string]bool, len(names))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("invalid OVM report field")
		}
		// Match encoding/json's case folding, so aliases cannot evade duplicate
		// detection. Unknown keys fail immediately, bounding the seen set.
		key := ""
		for _, candidate := range names {
			if strings.EqualFold(name, candidate) {
				key = candidate
				break
			}
		}
		if key == "" || seen[key] {
			return fmt.Errorf("unknown or duplicate OVM report field %q", name)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if (key == "genesis_alloc" || key == "erc20_retention" || key == "audit_summary") && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("OVM %s evidence must not be null", key)
		}
	}
	// Typed decoding also validates the closing delimiter and end of input.
	decoded, err := strictio.DecodeJSON[reportJSON](data, "OVM verification report")
	if err != nil {
		return err
	}
	*r = OVMVerificationReport(decoded)
	return nil
}

func newOVMReport(source bundle.SourceEvidence, original, final StateResult, checkpoint bundle.SourceEvidence, inputs ovmInputs, history OVMHistoryEvidence, balances OVMBalanceEvidence, target targetConfig, scheme string) OVMVerificationReport {
	return OVMVerificationReport{
		Format: OVMVerificationFormat, Version: formatversion.OVMVerification, VerifiedAt: time.Now().UTC(), Verified: true,
		Scheme: scheme, DBEngine: target.engine, StateLayout: target.layout, ToolVersion: version.ToolVersion, GethVersion: version.GethVersion,
		Source: source, Original: OVMStateEvidence(original), Target: OVMStateEvidence(final), Checkpoint: checkpoint,
		WrappedCodeHash: cryptoCodeHash(inputs), CodeFileSHA256: inputs.codeFileDigest, WitnessSHA256: inputs.witnessDigest, History: history, Balances: balances,
		ERC20Retention: inputs.retention, AuditSummary: inputs.auditEvidence,
	}
}

// Validate checks this report's independent checkpoint and balance invariants.
func (r OVMVerificationReport) Validate() error {
	if r.Format != OVMVerificationFormat || r.Version != formatversion.OVMVerification {
		return errors.New("unsupported OVM verification format/version")
	}
	if !r.Verified || r.VerifiedAt.IsZero() || r.ToolVersion == "" {
		return errors.New("incomplete OVM verification provenance")
	}
	if r.StateLayout != LayoutGeth {
		return errors.New("OVM report requires explicit geth state layout")
	}
	if _, err := reportTarget(r.DBEngine, r.StateLayout, r.Scheme); err != nil {
		return err
	}
	if err := r.Source.Validate(); err != nil {
		return err
	}
	if err := r.Checkpoint.Validate(); err != nil {
		return err
	}
	if r.Original.Root != r.Source.HeadBefore.StateRoot || r.Target.Root != r.Checkpoint.HeadBefore.StateRoot {
		return errors.New("OVM report state roots disagree with headers")
	}
	if err := r.Original.Counts.Validate(); err != nil {
		return err
	}
	if err := r.Target.Counts.Validate(); err != nil {
		return err
	}
	if r.ERC20Retention != nil && r.ERC20Retention.FileSHA256 == (common.Hash{}) {
		return errors.New("OVM ERC20 retention file digest is missing")
	}
	if r.GenesisAlloc != nil {
		if r.GenesisAlloc.FileSHA256 == (common.Hash{}) || r.GenesisAlloc.Converted.Root == (common.Hash{}) || r.GenesisAlloc.Converted.Counts.Accounts == 0 {
			return errors.New("OVM GenesisAlloc evidence is incomplete")
		}
		if err := r.GenesisAlloc.Converted.Counts.Validate(); err != nil {
			return fmt.Errorf("OVM converted state counts: %w", err)
		}
	}
	expected, err := ovmCheckpoint(r.Source, r.Target.Root)
	if err != nil {
		return err
	}
	if !sameSourceEvidence(expected, r.Checkpoint) {
		return errors.New("OVM checkpoint is not canonical for the migration inputs")
	}
	if slices.Contains([]common.Hash{r.WrappedCodeHash, r.CodeFileSHA256, r.WitnessSHA256, r.History.HistorySHA256, r.History.EventsSHA256}, common.Hash{}) {
		return errors.New("OVM report has an empty digest")
	}
	if r.History.FirstBlock != 0 || r.History.LastBlock != r.Source.HeadBefore.BlockNumber || r.History.Blocks != r.History.LastBlock+1 {
		return errors.New("OVM report history range is incomplete")
	}
	if r.AuditSummary != nil && r.AuditSummary.AuditHead != nil {
		head := r.AuditSummary.AuditHead
		if head.BlockNumber == 0 || head.BlockNumber >= r.Source.HeadBefore.BlockNumber || head.BlockHash == (common.Hash{}) || head.StateRoot == (common.Hash{}) {
			return errors.New("OVM audit_head must identify an earlier nonzero canonical block")
		}
	}
	return validateOVMBalances(r.Balances, r.AuditSummary)
}

func validateOVMBalances(b OVMBalanceEvidence, audit *OVMAuditEvidence) error {
	if b.OriginalSupply == nil || b.MigratedNative == nil || b.RemainingSupply == nil || b.SelfBalance == nil {
		return errors.New("OVM report lacks balance amounts")
	}
	expected := b.OriginalSupply
	if audit != nil {
		delta, err := auditInteger(audit.SupplyDifference, false)
		if err != nil {
			return err
		}
		if audit.FileSHA256 == (common.Hash{}) || audit.BalanceSum == nil || delta.Sign() < 0 || new(big.Int).Sub(audit.BalanceSum.ToBig(), b.OriginalSupply.ToBig()).Cmp(delta) != 0 {
			return errors.New("OVM audit report accounting mismatch")
		}
		expected = audit.BalanceSum
	}
	sum, overflow := new(uint256.Int).AddOverflow(b.MigratedNative, b.RemainingSupply)
	if overflow || !sum.Eq(expected) || b.SelfBalance.Cmp(b.RemainingSupply) > 0 {
		return errors.New("OVM report balance conservation mismatch")
	}
	return nil
}

func ovmCheckpoint(source bundle.SourceEvidence, root common.Hash) (bundle.SourceEvidence, error) {
	parent, err := source.ValidatedHeader()
	if err != nil {
		return bundle.SourceEvidence{}, err
	}
	if parent.Number.Uint64() == math.MaxUint64 || parent.Time == math.MaxUint64 {
		return bundle.SourceEvidence{}, errors.New("migration checkpoint height or timestamp overflows")
	}
	h := &types.Header{ParentHash: source.HeadBefore.BlockHash, UncleHash: types.EmptyUncleHash, Root: root, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
		Difficulty: new(big.Int), Number: new(big.Int).Add(parent.Number, big.NewInt(1)), GasLimit: parent.GasLimit, Time: parent.Time + 1, Extra: []byte("metis-l2state-ovm/v1")}
	blob, err := rlp.EncodeToBytes(h)
	if err != nil {
		return bundle.SourceEvidence{}, err
	}
	head := bundle.Head{BlockNumber: h.Number.Uint64(), BlockHash: h.Hash(), StateRoot: root}
	return bundle.SourceEvidence{HeadBefore: head, HeadAfter: head, HeaderRLP: hexutil.Bytes(blob)}, nil
}

func ovmBodyMetadata(checkpoint bundle.SourceEvidence) headMetadataEntries {
	entries := make(headMetadataEntries)
	rawdb.WriteBody(entries, checkpoint.HeadBefore.BlockHash, checkpoint.HeadBefore.BlockNumber, &types.Body{})
	return entries
}

func writeOVMReport(dir string, report OVMVerificationReport) error {
	if err := report.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, VerificationFileName)
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return fmt.Errorf("write OVM verification: %w", err)
	}
	return syncFile(path)
}

func loadOVMReport(dir string) (OVMVerificationReport, error) {
	return loadArtifactJSON(dir, "OVM verification report", func(r OVMVerificationReport) error { return r.Validate() })
}

func sameOVMReport(a, b OVMVerificationReport) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && bytes.Equal(x, y)
}
