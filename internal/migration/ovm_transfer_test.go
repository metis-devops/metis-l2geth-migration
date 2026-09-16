package migration

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

func TestOVMTransferRetentionMembership(t *testing.T) {
	zero, positive, high := common.Hash{}, common.HexToHash("0x01"), common.Hash{0x80}
	for _, tc := range []struct {
		name    string
		amounts []common.Hash
		burn    bool
	}{
		{name: "zero", amounts: []common.Hash{zero}},
		{name: "positive", amounts: []common.Hash{positive}},
		{name: "zero-then-positive", amounts: []common.Hash{zero, positive}},
		{name: "positive-then-zero", amounts: []common.Hash{positive, zero}},
		{name: "repeated-zero", amounts: []common.Hash{zero, zero}},
		{name: "zero-burn", amounts: []common.Hash{zero}, burn: true},
		{name: "positive-burn", amounts: []common.Hash{positive}, burn: true},
		{name: "high-bit", amounts: []common.Hash{high}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := testAllocIndex(t)
			s := ovmHistoryScanner{index: idx, events: sha256.New()}
			from, to := common.Address{1}, common.Address{2}
			if tc.burn {
				to = common.Address{}
			}
			wantDigest := sha256.New()
			var eligible bool
			for n, amount := range tc.amounts {
				log := ovmTestEvent(ovmTransferTopic, from, to, 0)
				log.Data = amount.Bytes()
				if err := s.event(7, 3, uint64(n), log); err != nil {
					t.Fatal(err)
				}
				if err := idx.flush(); err != nil {
					t.Fatal(err)
				}
				eligible = eligible || amount != zero
				if _, got, err := idx.get('f', from[:]); err != nil || got != eligible {
					t.Fatalf("event %d: membership=%t want=%t err=%v", n, got, eligible, err)
				}
				if _, got, err := idx.get('f', to[:]); err != nil || got {
					t.Fatalf("recipient gained membership: %t err=%v", got, err)
				}
				for _, address := range []common.Address{from, to} {
					slot := ovmStorageHash(ovmBalanceSlot(address))
					if got, ok, err := idx.get('b', slot[:]); err != nil || !ok || !bytes.Equal(got, address[:]) {
						t.Fatalf("address discovery: %s %x %t %v", address, got, ok, err)
					}
				}
				var coordinate [24]byte
				binary.BigEndian.PutUint64(coordinate[:8], 7)
				binary.BigEndian.PutUint64(coordinate[8:16], 3)
				binary.BigEndian.PutUint64(coordinate[16:], uint64(n))
				ovmHashFrame(wantDigest, coordinate[:], ovmTransferTopic[:], from[:], to[:], amount[:])
				if s.evidence.Transfers != uint64(n+1) || s.evidence.Approvals != 0 || !bytes.Equal(s.events.Sum(nil), wantDigest.Sum(nil)) {
					t.Fatal("event omitted from counts or ordered digest")
				}
			}
		})
	}
}

func TestOVMZeroTransferStillValidatesEvent(t *testing.T) {
	for _, kind := range []string{"short-data", "long-data", "missing-topic", "extra-topic", "from-padding", "to-padding"} {
		t.Run(kind, func(t *testing.T) {
			idx := testAllocIndex(t)
			s := ovmHistoryScanner{index: idx, events: sha256.New()}
			log := ovmTestEvent(ovmTransferTopic, common.Address{1}, common.Address{2}, 0)
			switch kind {
			case "short-data":
				log.Data = log.Data[:31]
			case "long-data":
				log.Data = append(log.Data, 0)
			case "missing-topic":
				log.Topics = log.Topics[:2]
			case "extra-topic":
				log.Topics = append(log.Topics, common.Hash{})
			case "from-padding":
				log.Topics[1][0] = 1
			case "to-padding":
				log.Topics[2][0] = 1
			}
			if err := s.event(1, 0, 0, log); err == nil || !strings.Contains(err.Error(), "malformed OVM ERC20 event") {
				t.Fatalf("malformed zero event accepted: %v", err)
			}
		})
	}
}

func TestOVMZeroTransferConvertsAllTargets(t *testing.T) {
	f := newOVMFixture(t, nil)
	replaceOVMFixtureLogs(t, f, []*types.Log{
		ovmTestEvent(ovmTransferTopic, f.holders[1], f.holders[3], 0),
		ovmTestEvent(ovmTransferTopic, f.holders[0], f.holders[3], 1),
		ovmTestEvent(ovmApprovalTopic, f.holders[1], f.holders[3], 0x1234),
	})
	before := directoryContentDigest(t, f.source)
	// Independently apply the zero-only sender's conversion to the serial
	// StateDB reference, which otherwise retains this holder's 22 tokens.
	wantRoot := ovmReferenceWithAlloc(t, f, func(s *state.StateDB) {
		s.SetBalance(f.holders[1], uint256.NewInt(22), tracing.BalanceChangeUnspecified)
		s.SetState(ovmETHAddress, ovmBalanceSlot(f.holders[1]), common.Hash{})
		s.SetBalance(ovmETHAddress, uint256.NewInt(77), tracing.BalanceChangeUnspecified)
		s.SetState(ovmETHAddress, common.HexToHash("0x02"), common.HexToHash("0x4d"))
	})
	oldRoot := ovmReferenceRoot(t, f)
	for _, mode := range []TempDBMode{TempDBDisk, TempDBMemory} {
		for _, engine := range []string{"pebble", "leveldb"} {
			for _, scheme := range []string{"hash", "path"} {
				t.Run(string(mode)+"/"+engine+"/"+scheme, func(t *testing.T) {
					opts := f.options(t, engine, scheme, 4)
					opts.TempDB = mode
					result, err := Migrate(t.Context(), opts)
					if err != nil {
						t.Fatal(err)
					}
					r := result.OVMReport
					if r.Target.Root != wantRoot || r.Target.Root == oldRoot || r.History.Transfers != 2 {
						t.Fatalf("unexpected root or event count: %+v", r)
					}
					if r.Balances.MigratedNative.Uint64() != 231 || r.Balances.RemainingSupply.Uint64() != 77 || r.Balances.EOAs != 3 || r.Balances.ConvertedContracts != 3 || r.Balances.RetainedContracts != 0 {
						t.Fatalf("unexpected balances: %+v", r.Balances)
					}
					withArtifactState(t, opts.Output, scheme, wantRoot, true, func(s *state.StateDB) {
						if s.GetBalance(f.holders[1]).Uint64() != 22 || s.GetState(ovmETHAddress, ovmBalanceSlot(f.holders[1])) != (common.Hash{}) {
							t.Fatal("zero-only contract balance was retained")
						}
						if s.GetBalance(ovmETHAddress).Uint64() != 77 || s.GetState(ovmETHAddress, common.HexToHash("0x02")) != common.HexToHash("0x4d") || s.GetState(ovmETHAddress, ovmBalanceSlot(ovmETHAddress)) != common.HexToHash("0x4d") {
							t.Fatal("self holdings, totalSupply or backing mismatch")
						}
					})
					verify := OVMVerifyOptions{TempDB: oppositeTempMode(mode), SourceChaindata: f.source, Artifact: opts.Output, CacheMB: 64, Handles: 64, Workers: 3, OVM: opts.OVM}
					if got, err := VerifyOVM(t.Context(), verify); err != nil || got.Target.Root != wantRoot {
						t.Fatalf("independent verification: %v", err)
					}
					// Reintroduce the former classification evidence: replay must
					// reject it rather than accepting a v1 compatibility fallback.
					old := *r
					old.Target.Root = oldRoot
					old.Checkpoint, err = ovmCheckpoint(old.Source, oldRoot)
					if err != nil {
						t.Fatal(err)
					}
					old.Balances.MigratedNative = uint256.NewInt(209)
					old.Balances.RemainingSupply = uint256.NewInt(99)
					old.Balances.ConvertedContracts = 2
					old.Balances.RetainedContracts = 1
					if err := old.Validate(); err != nil {
						t.Fatalf("former classification report must pass schema validation: %v", err)
					}
					encoded, err := json.Marshal(old)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(opts.Output, VerificationFileName), encoded, 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := VerifyOVM(t.Context(), verify); err == nil || !strings.Contains(err.Error(), "does not match independently replayed") {
						t.Fatalf("expected replay to reject former zero-value retention evidence: %v", err)
					}
				})
			}
		}
	}
	if before != directoryContentDigest(t, f.source) {
		t.Fatal("source changed")
	}
}
