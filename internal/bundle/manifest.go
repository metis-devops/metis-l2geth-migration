package bundle

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
	"github.com/metis-devops/metis-l2geth-migration/internal/version"
)

const (
	// FormatName identifies the bundle format in a manifest.
	FormatName = "metis-l2state"
	// FormatVersion is the supported bundle format version.
	FormatVersion = 3
	// ManifestFileName is the fixed name of the bundle manifest.
	ManifestFileName = "manifest.json"
	// CompressionZstd identifies the canonical zstd-compressed record stream.
	CompressionZstd = "zstd"
	// CompressionNone identifies the uncompressed record stream.
	CompressionNone = "none"
	// RecordsFileZstd is the fixed name of a zstd-compressed record stream.
	RecordsFileZstd = "state.records.zst"
	// RecordsFileRaw is the fixed name of an uncompressed record stream.
	RecordsFileRaw = "state.records"
)

// Head identifies the legacy canonical block whose state is exported.
type Head struct {
	BlockNumber uint64      `json:"block_number"`
	BlockHash   common.Hash `json:"block_hash"`
	StateRoot   common.Hash `json:"state_root"`
}

// SourceEvidence binds a bundle to the stable source head and its encoded header.
type SourceEvidence struct {
	HeadBefore Head          `json:"head_before"`
	HeadAfter  Head          `json:"head_after"`
	HeaderRLP  hexutil.Bytes `json:"header_rlp"`
}

// Validate checks that the encoded header and stable source heads agree.
func (s SourceEvidence) Validate() error {
	_, err := s.ValidatedHeader()
	return err
}

// ValidatedHeader checks that the encoded header and stable source heads agree
// and returns the decoded header.
func (s SourceEvidence) ValidatedHeader() (*types.Header, error) {
	if s.HeadBefore != s.HeadAfter {
		return nil, errors.New("source head changed during traversal")
	}
	if s.HeadBefore.BlockHash == (common.Hash{}) {
		return nil, errors.New("source block hash is empty")
	}
	if s.HeadBefore.StateRoot == (common.Hash{}) {
		return nil, errors.New("source state root is empty")
	}
	if len(s.HeaderRLP) == 0 {
		return nil, errors.New("source header RLP is empty")
	}
	var header types.Header
	if err := rlp.DecodeBytes(s.HeaderRLP, &header); err != nil {
		return nil, fmt.Errorf("decode header RLP as geth v1.17.5 header: %w", err)
	}
	if header.Number == nil || !header.Number.IsUint64() {
		return nil, errors.New("header block number is missing or exceeds uint64")
	}
	head := s.HeadBefore
	if header.Number.Uint64() != head.BlockNumber {
		return nil, fmt.Errorf("header block number mismatch: have %d want %d", header.Number.Uint64(), head.BlockNumber)
	}
	if hash := crypto.Keccak256Hash(s.HeaderRLP); hash != head.BlockHash {
		return nil, fmt.Errorf("header block hash mismatch: have %s want %s", hash, head.BlockHash)
	}
	if header.Root != head.StateRoot {
		return nil, fmt.Errorf("header state root mismatch: have %s want %s", header.Root, head.StateRoot)
	}
	return &header, nil
}

// Counts summarizes semantic records and their expanded consensus payload bytes.
type Counts struct {
	Accounts       uint64 `json:"accounts"`
	StorageSlots   uint64 `json:"storage_slots"`
	CodeReferences uint64 `json:"code_references"`
	CodeRecords    uint64 `json:"code_records,omitempty"`
	Records        uint64 `json:"records"`
	PayloadBytes   uint64 `json:"payload_bytes"`
}

// Validate checks relationships between semantic state counts.
func (c Counts) Validate() error {
	if c.CodeReferences > c.Accounts {
		return errors.New("code reference count exceeds account count")
	}
	if c.CodeRecords > c.CodeReferences {
		return errors.New("code record count exceeds code reference count")
	}
	if c.CodeReferences > 0 && c.CodeRecords == 0 {
		return errors.New("state has code references but no code records")
	}
	expected, ok := sumCounts(c.Accounts, c.StorageSlots, c.CodeRecords)
	if !ok {
		return errors.New("record type counts overflow uint64")
	}
	if c.Records != expected {
		return errors.New("record count does not match record type counts")
	}
	if c.Records == 0 {
		if c.PayloadBytes != 0 {
			return errors.New("empty state has non-zero payload bytes")
		}
		return nil
	}
	if c.Accounts == 0 {
		return errors.New("non-empty state has no account records")
	}
	if c.PayloadBytes == 0 {
		return errors.New("non-empty state has zero payload bytes")
	}
	return nil
}

// StateFile describes the encoded record stream and its integrity evidence.
type StateFile struct {
	Name               string      `json:"name"`
	Compression        string      `json:"compression"`
	Size               int64       `json:"size"`
	RecordPayloadBytes uint64      `json:"record_payload_bytes"`
	SHA256             common.Hash `json:"sha256"`
	RecordChainHash    common.Hash `json:"record_chain_hash"`
}

// Manifest describes a versioned state bundle and its source evidence.
type Manifest struct {
	Format           string         `json:"format"`
	Version          uint64         `json:"version"`
	CreatedAt        time.Time      `json:"created_at"`
	ToolVersion      string         `json:"tool_version"`
	GethVersion      string         `json:"geth_version"`
	GethCommit       string         `json:"geth_commit"`
	Source           SourceEvidence `json:"source"`
	Counts           Counts         `json:"counts"`
	StateFile        StateFile      `json:"state_file"`
	SupportedSchemes []string       `json:"supported_schemes"`
}

// NewManifest constructs a manifest for a completed record stream.
func NewManifest(source SourceEvidence, counts Counts, stateFile StateFile) Manifest {
	return Manifest{
		Format:           FormatName,
		Version:          FormatVersion,
		CreatedAt:        time.Now().UTC(),
		ToolVersion:      version.ToolVersion,
		GethVersion:      version.GethVersion,
		GethCommit:       version.GethCommit,
		Source:           source,
		Counts:           counts,
		StateFile:        stateFile,
		SupportedSchemes: []string{"hash", "path"},
	}
}

// LoadManifest reads and strictly validates the manifest in dir.
func LoadManifest(dir string) (manifest Manifest, data []byte, retErr error) {
	root, err := strictio.OpenRoot(dir)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("open bundle: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	data, err = root.ReadRegular(ManifestFileName, strictio.MaxMetadataSize)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("read manifest: %w", err)
	}
	manifest, err = strictio.DecodeJSON[Manifest](data, "manifest")
	if err != nil {
		return Manifest{}, nil, err
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, nil, err
	}
	if err := root.RequireExactLayout(map[string]strictio.EntryKind{
		ManifestFileName:        strictio.RegularFile,
		manifest.StateFile.Name: strictio.RegularFile,
	}); err != nil {
		return Manifest{}, nil, fmt.Errorf("validate bundle layout: %w", err)
	}
	return manifest, data, nil
}

// WriteManifest validates and writes a canonical JSON manifest into dir.
func WriteManifest(dir string, manifest Manifest) ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, ManifestFileName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}
	return data, nil
}

// Validate checks the manifest format, source header, counts, and digests.
func (m Manifest) Validate() error {
	if m.Format != FormatName || m.Version != FormatVersion {
		return fmt.Errorf("unsupported bundle format %q version %d", m.Format, m.Version)
	}
	if m.ToolVersion == "" {
		return errors.New("manifest tool_version is empty")
	}
	if m.GethVersion != version.GethVersion || m.GethCommit != version.GethCommit {
		return fmt.Errorf("bundle requires geth %s (%s), got %s (%s)", version.GethVersion, version.GethCommit, m.GethVersion, m.GethCommit)
	}
	if m.CreatedAt.IsZero() {
		return errors.New("manifest created_at is empty")
	}
	if err := m.Source.Validate(); err != nil {
		return err
	}
	if m.StateFile.Name != RecordsFileZstd && m.StateFile.Name != RecordsFileRaw {
		return fmt.Errorf("invalid state file name %q", m.StateFile.Name)
	}
	if filepath.Base(m.StateFile.Name) != m.StateFile.Name {
		return errors.New("state file name must not contain a path")
	}
	if m.StateFile.Compression != CompressionZstd && m.StateFile.Compression != CompressionNone {
		return fmt.Errorf("unsupported compression %q", m.StateFile.Compression)
	}
	if m.StateFile.Compression == CompressionZstd && m.StateFile.Name != RecordsFileZstd {
		return errors.New("zstd bundle must use state.records.zst")
	}
	if m.StateFile.Compression == CompressionNone && m.StateFile.Name != RecordsFileRaw {
		return errors.New("uncompressed bundle must use state.records")
	}
	if m.StateFile.Size <= 0 {
		return errors.New("state file size must be positive")
	}
	if m.StateFile.SHA256 == (common.Hash{}) {
		return errors.New("state file sha256 is empty")
	}
	if m.StateFile.RecordChainHash == (common.Hash{}) {
		return errors.New("record chain hash is empty")
	}
	if err := m.Counts.Validate(); err != nil {
		return err
	}
	if m.Counts.Records == 0 && m.StateFile.RecordPayloadBytes != 0 {
		return errors.New("empty record stream has non-zero record payload bytes")
	}
	if m.Counts.Records != 0 && m.StateFile.RecordPayloadBytes == 0 {
		return errors.New("non-empty record stream has zero record payload bytes")
	}
	if m.StateFile.RecordPayloadBytes > m.Counts.PayloadBytes {
		return errors.New("record payload bytes exceed expanded consensus payload bytes")
	}
	if len(m.SupportedSchemes) != 2 || m.SupportedSchemes[0] != "hash" || m.SupportedSchemes[1] != "path" {
		return errors.New("manifest supported_schemes must be [hash, path]")
	}
	return nil
}

func sumCounts(values ...uint64) (uint64, bool) {
	var total uint64
	for _, value := range values {
		if math.MaxUint64-total < value {
			return 0, false
		}
		total += value
	}
	return total, true
}

// ManifestSHA256 returns the SHA-256 digest of manifest bytes.
func ManifestSHA256(data []byte) common.Hash {
	sum := sha256.Sum256(data)
	return common.Hash(sum)
}
