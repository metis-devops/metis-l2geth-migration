package migration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	ovmAllocMaxCode = 1 << 20
	// Account fields and storage live in separate, operation-local namespaces.
	ovmAllocAccountPrefix byte = 'G'
	ovmAllocStoragePrefix byte = 'H'
	ovmAllocPatchedPrefix byte = 'I'
)

const (
	ovmAllocCodeField uint8 = 1 << iota
	ovmAllocBalanceField
	ovmAllocNonceField
	ovmAllocStorageField
)

type ovmAllocAccount struct {
	Fields  uint8
	Code    []byte
	Balance []byte
	Nonce   uint64
}

// ovmAllocReader bounds a single JSON token before encoding/json buffers it and
// collapses each run of whitespace outside strings to one space. Keeping that
// separator preserves token boundaries without letting the decoder buffer an
// arbitrarily long whitespace run. Hash raw bytes upstream of this reader.
// The limit admits a maximum-sized code string even with every character escaped
// as \uXXXX. Storage objects and account collections themselves have no size cap.
type ovmAllocReader struct {
	ctx     context.Context
	r       io.Reader
	length  int
	quoted  bool
	escaped bool
	space   bool
	err     error
}

func (r *ovmAllocReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.err != nil {
			return 0, r.err
		}
		n, err := r.r.Read(p[:min(len(p), 32<<10)])
		r.err = err // Retain errors even when a decoder consumes bytes first.
		n, filterErr := r.filter(p[:n])
		if filterErr != nil {
			r.err = errors.Join(filterErr, r.err)
		}
		if n != 0 || r.err != nil {
			return n, r.err
		}
		// A chunk containing only an already-started whitespace run produces no
		// output. Continue reading with cancellation checks and the same buffer.
	}
}

func (r *ovmAllocReader) filter(p []byte) (int, error) {
	written := 0
	for _, b := range p {
		if !r.quoted && (b == ' ' || b == '\t' || b == '\r' || b == '\n') {
			r.length = 0
			if !r.space {
				p[written] = ' '
				written++
				r.space = true
			}
			continue
		}
		r.space = false
		if err := r.tokenByte(b); err != nil {
			return written, err
		}
		p[written] = b
		written++
	}
	return written, nil
}

func (r *ovmAllocReader) tokenByte(b byte) error {
	r.length++
	if r.length > 6*(2*ovmAllocMaxCode+2)+2 {
		return errors.New("GenesisAlloc JSON token exceeds bounded code allowance")
	}
	if r.quoted {
		switch {
		case r.escaped:
			r.escaped = false
		case b == '\\':
			r.escaped = true
		case b == '"':
			r.quoted, r.length = false, 0
		}
	} else if b == '"' {
		r.quoted, r.length = true, 1
	} else if strings.ContainsRune("{}[],:", rune(b)) {
		r.length = 0
	}
	return nil
}

type ovmAllocLoader struct {
	ctx     context.Context
	decoder *json.Decoder
	index   *ovmIndex
	// Only unflushed keys are retained here; older duplicates are found on disk.
	pending map[string]struct{}
}

func loadOVMGenesisAlloc(ctx context.Context, path string, index *ovmIndex) (digest common.Hash, retErr error) {
	f, err := openOVMInput(path)
	if err != nil {
		return digest, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	h := sha256.New()
	decoder := json.NewDecoder(&ovmAllocReader{ctx: ctx, r: io.TeeReader(f, h)})
	decoder.UseNumber()
	l := ovmAllocLoader{ctx: ctx, decoder: decoder, index: index, pending: make(map[string]struct{})}
	if err := l.object(l.account); err != nil {
		return digest, fmt.Errorf("decode GenesisAlloc: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return digest, errors.Join(errors.New("GenesisAlloc has trailing data"), err)
	}
	if err := index.flush(); err != nil {
		return digest, err
	}
	copy(digest[:], h.Sum(nil))
	return digest, ctx.Err()
}

func (l *ovmAllocLoader) object(visit func(string) error) error {
	if err := l.delimiter('{'); err != nil {
		return err
	}
	for l.decoder.More() {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		token, err := l.decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("expected JSON object key")
		}
		if err := visit(key); err != nil {
			return fmt.Errorf("GenesisAlloc key %.80q: %w", key, err)
		}
	}
	return l.delimiter('}')
}

func (l *ovmAllocLoader) delimiter(want json.Delim) error {
	token, err := l.decoder.Token()
	if err != nil {
		return err
	}
	if token != want {
		return fmt.Errorf("expected JSON delimiter %c", want)
	}
	return nil
}

func (l *ovmAllocLoader) scalar() ([]byte, error) {
	token, err := l.decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token.(type) {
	case string, json.Number:
		return json.Marshal(token)
	default:
		return nil, errors.New("expected non-null JSON string or number")
	}
}

func (l *ovmAllocLoader) account(key string) error {
	var address common.UnprefixedAddress
	if err := address.UnmarshalText([]byte(key)); err != nil {
		return err
	}
	if common.Address(address) == ovmETHAddress {
		return errors.New("GenesisAlloc must not modify OVM_ETH")
	}
	hash := crypto.Keccak256Hash(address[:])
	var patch ovmAllocAccount
	seen := make(map[string]bool, 4)
	err := l.object(func(field string) error {
		if seen[field] {
			return errors.New("duplicate account field")
		}
		seen[field] = true
		return l.field(hash, &patch, field)
	})
	if err != nil {
		return err
	}
	blob, err := rlp.EncodeToBytes(&patch)
	if err != nil {
		return err
	}
	return l.unique(prefixedKey([]byte{ovmAllocAccountPrefix}, hash[:]), blob)
}

func (l *ovmAllocLoader) field(owner common.Hash, patch *ovmAllocAccount, field string) error {
	if field == "storage" {
		return l.object(func(key string) error {
			patch.Fields |= ovmAllocStorageField
			return l.storage(owner, key)
		})
	}
	if field != "code" && field != "balance" && field != "nonce" {
		return errors.New("unknown account field")
	}
	data, err := l.scalar()
	if err != nil {
		return err
	}
	switch field {
	case "code":
		var code hexutil.Bytes
		if err := code.UnmarshalJSON(data); err != nil {
			return err
		}
		if len(code) > ovmAllocMaxCode {
			return errors.New("GenesisAlloc code exceeds 1 MiB")
		}
		patch.Code, patch.Fields = code, patch.Fields|ovmAllocCodeField
	case "balance":
		var value gethmath.HexOrDecimal256
		if err := value.UnmarshalJSON(data); err != nil {
			return err
		}
		n := (*big.Int)(&value)
		if n.Sign() < 0 || n.BitLen() > 256 {
			return errors.New("GenesisAlloc balance must be a nonnegative uint256")
		}
		patch.Balance, patch.Fields = n.Bytes(), patch.Fields|ovmAllocBalanceField
	case "nonce":
		var value gethmath.HexOrDecimal64
		if err := value.UnmarshalJSON(data); err != nil {
			return err
		}
		patch.Nonce, patch.Fields = uint64(value), patch.Fields|ovmAllocNonceField
	}
	return nil
}

// This matches geth v1.17.5 types.storageJSON: optional lowercase 0x, even hex
// length, at most 32 bytes, left padded. Account/storage keys are hashed once.
func decodeOVMAllocWord(text string) (word common.Hash, err error) {
	text = strings.TrimPrefix(text, "0x")
	if len(text) > 64 {
		return word, errors.New("GenesisAlloc storage key/value exceeds 32 bytes")
	}
	_, err = hex.Decode(word[len(word)-len(text)/2:], []byte(text))
	return word, err
}

func (l *ovmAllocLoader) storage(owner common.Hash, key string) error {
	slot, err := decodeOVMAllocWord(key)
	if err != nil {
		return err
	}
	data, err := l.scalar()
	if err != nil {
		return err
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	value, err := decodeOVMAllocWord(text)
	if err != nil {
		return err
	}
	var blob []byte
	if value != (common.Hash{}) {
		blob, err = rlp.EncodeToBytes(common.TrimLeftZeroes(value[:]))
		if err != nil {
			return err
		}
	}
	hash := crypto.Keccak256Hash(slot[:])
	prefix := prefixedKey([]byte{ovmAllocStoragePrefix}, owner[:])
	return l.unique(prefixedKey(prefix, hash[:]), blob)
}

func (l *ovmAllocLoader) unique(key, value []byte) error {
	if _, ok := l.pending[string(key)]; ok {
		return errors.New("duplicate GenesisAlloc address or storage slot")
	}
	exists, err := l.index.db.Has(key)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("duplicate GenesisAlloc address or storage slot")
	}
	if err := l.index.batch.Put(key, value); err != nil {
		return err
	}
	l.pending[string(key)] = struct{}{}
	if l.index.batch.ValueSize() >= ethdb.IdealBatchSize {
		if err := l.index.flush(); err != nil {
			return err
		}
		clear(l.pending)
	}
	return nil
}

func hashOVMGenesisAlloc(ctx context.Context, path string) (common.Hash, error) {
	return hashOVMInput(ctx, path)
}

func confirmOVMAllocEvidence(ctx context.Context, path string, evidence *OVMGenesisAllocEvidence) error {
	if (evidence != nil) != (path != "") {
		return errors.New("OVM verification requires the original --ovm-genesis-alloc input exactly when recorded")
	}
	if evidence == nil {
		return ctx.Err()
	}
	digest, err := hashOVMGenesisAlloc(ctx, path)
	if err != nil {
		return err
	}
	if digest != evidence.FileSHA256 {
		return errors.New("GenesisAlloc input changed or differs from recorded digest")
	}
	return nil
}
