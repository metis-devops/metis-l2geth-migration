package migration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	cpebble "github.com/cockroachdb/pebble/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2geth-migration/internal/strictio"
)

var ovmETHAddress = common.HexToAddress("0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000")

// OVMOptions configures the explicitly enabled OVM balance conversion.
type OVMOptions struct {
	Enabled          bool
	WrappedEtherCode string
	SourceAncient    string
	StateWitness     string
	GenesisAlloc     string
}

type ovmInputs struct {
	code           []byte
	codeFileDigest common.Hash
	witnessDigest  common.Hash
	allocDigest    common.Hash
}

func validateOVMOptions(o OVMOptions) error {
	if !o.Enabled {
		if o.WrappedEtherCode != "" || o.SourceAncient != "" || o.StateWitness != "" || o.GenesisAlloc != "" {
			return errors.New("OVM input flags require --migrate-ovm-eth")
		}
		return nil
	}
	if o.WrappedEtherCode == "" {
		return errors.New("--migrate-ovm-eth requires --wrapped-ether-code")
	}
	return nil
}

func openOVMInput(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect OVM input: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("OVM input must be a regular file, not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open OVM input: %w", err)
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("OVM input changed while opening"), err, f.Close())
	}
	return f, nil
}

func readWrappedCode(ctx context.Context, path string) (code []byte, digest common.Hash, retErr error) {
	f, err := openOVMInput(path)
	if err != nil {
		return nil, digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	// Runtime code is deliberately a small operator input, not an unbounded blob.
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return nil, digest, fmt.Errorf("read wrapped code: %w", err)
	}
	if len(data) > 1024*1024 {
		return nil, digest, errors.New("wrapped code file exceeds 1 MiB")
	}
	if err := ctx.Err(); err != nil {
		return nil, digest, err
	}
	encoded := strings.TrimSpace(string(data))
	if !strings.HasPrefix(encoded, "0x") {
		return nil, digest, errors.New("wrapped runtime bytecode requires a 0x prefix")
	}
	code, err = hexutil.Decode(encoded)
	if err != nil {
		return nil, digest, fmt.Errorf("decode wrapped runtime bytecode: %w", err)
	}
	if len(code) == 0 {
		return nil, digest, errors.New("wrapped runtime bytecode is empty")
	}
	return code, common.Hash(sha256.Sum256(data)), nil
}

type ovmWitnessRecord struct {
	Type    string          `json:"type"`
	Address *common.Address `json:"address,omitempty"`
	Owner   *common.Address `json:"owner,omitempty"`
	Spender *common.Address `json:"spender,omitempty"`
}

func loadOVMWitness(ctx context.Context, path string, index *ovmIndex) (digest common.Hash, retErr error) {
	if path == "" {
		return common.Hash(sha256.Sum256(nil)), nil
	}
	f, err := openOVMInput(path)
	if err != nil {
		return digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	h := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(f, h))
	scanner.Buffer(make([]byte, 4096), 4096)
	for line := uint64(1); scanner.Scan(); line++ {
		if err := ctx.Err(); err != nil {
			return digest, err
		}
		record, err := decodeOVMWitness(scanner.Bytes())
		if err == nil {
			err = index.addWitness(record)
		}
		if err != nil {
			return digest, fmt.Errorf("OVM witness line %d: %w", line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return digest, fmt.Errorf("read OVM witness: %w", err)
	}
	copy(digest[:], h.Sum(nil))
	return digest, index.flush()
}

func decodeOVMWitness(data []byte) (ovmWitnessRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ovmWitnessRecord{}, errors.New("OVM witness must be a JSON object")
	}
	seen := make(map[string]bool, 4)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return ovmWitnessRecord{}, err
		}
		key, ok := token.(string)
		if !ok {
			return ovmWitnessRecord{}, errors.New("invalid witness field")
		}
		if seen[key] {
			return ovmWitnessRecord{}, fmt.Errorf("duplicate OVM witness field %q", key)
		}
		if key != "type" && key != "address" && key != "owner" && key != "spender" {
			return ovmWitnessRecord{}, fmt.Errorf("unknown OVM witness field %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ovmWitnessRecord{}, err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ovmWitnessRecord{}, errors.New("null OVM witness field")
		}
	}
	return strictio.DecodeJSON[ovmWitnessRecord](data, "OVM witness record")
}

func (i *ovmIndex) addWitness(r ovmWitnessRecord) error {
	switch r.Type {
	case "address":
		if r.Address == nil || r.Owner != nil || r.Spender != nil {
			return errors.New("address record requires only type and address")
		}
		return i.address(*r.Address)
	case "allowance":
		if r.Address != nil || r.Owner == nil || r.Spender == nil {
			return errors.New("allowance record requires only type, owner and spender")
		}
		return i.allowance(*r.Owner, *r.Spender)
	default:
		return fmt.Errorf("unknown OVM witness type %q", r.Type)
	}
}

// Keys: b+hashed balance slot => address; a+hashed allowance slot => owner/spender;
// f+address => Transfer-from membership; p+account hash => replacement account RLP;
// s+hashed storage slot => replacement storage RLP (empty value means deletion).
type ovmIndex struct {
	db    ethdb.Database
	batch ethdb.Batch
}

func newOVMIndex(db ethdb.Database) *ovmIndex { return &ovmIndex{db: db, batch: db.NewBatch()} }

func (i *ovmIndex) put(prefix byte, key, value []byte) error {
	if err := i.batch.Put(prefixedKey([]byte{prefix}, key), value); err != nil {
		return err
	}
	if i.batch.ValueSize() >= ethdb.IdealBatchSize {
		return i.flush()
	}
	return nil
}

func (i *ovmIndex) flush() error {
	if err := i.batch.Write(); err != nil {
		return fmt.Errorf("flush OVM evidence index: %w", err)
	}
	i.batch.Reset()
	return nil
}

func (i *ovmIndex) get(prefix byte, key []byte) ([]byte, bool, error) {
	k := prefixedKey([]byte{prefix}, key)
	v, err := i.db.Get(k)
	// The operation-local evidence database always uses Pebble. Only its
	// not-found sentinel is absence; corruption and I/O errors must propagate.
	if errors.Is(err, cpebble.ErrNotFound) {
		return nil, false, nil
	}
	return v, err == nil, err
}

func ovmBalanceSlot(address common.Address) common.Hash {
	return crypto.Keccak256Hash(common.LeftPadBytes(address[:], 32), make([]byte, 32))
}

func ovmStorageHash(slot common.Hash) common.Hash { return crypto.Keccak256Hash(slot[:]) }

func (i *ovmIndex) address(address common.Address) error {
	slot := ovmStorageHash(ovmBalanceSlot(address))
	return i.put('b', slot[:], address[:])
}

func (i *ovmIndex) allowance(owner, spender common.Address) error {
	if err := i.address(owner); err != nil {
		return err
	}
	if err := i.address(spender); err != nil {
		return err
	}
	inner := crypto.Keccak256Hash(common.LeftPadBytes(owner[:], 32), common.LeftPadBytes([]byte{1}, 32))
	slot := crypto.Keccak256Hash(common.LeftPadBytes(spender[:], 32), inner[:])
	hash := ovmStorageHash(slot)
	return i.put('a', hash[:], append(bytes.Clone(owner[:]), spender[:]...))
}

func collectOVMPreimages(ctx context.Context, db ethdb.Database, index *ovmIndex) error {
	prefix := []byte("secure-key-")
	it := db.NewIterator(prefix, nil)
	defer it.Release()
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, value := it.Key(), it.Value()
		if len(key) != len(prefix)+32 || !bytes.Equal(crypto.Keccak256(value), key[len(prefix):]) {
			return errors.New("invalid legacy preimage hash")
		}
		if len(value) == common.AddressLength {
			if err := index.address(common.Address(value)); err != nil {
				return err
			}
		}
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("read legacy preimages: %w", err)
	}
	return index.flush()
}
