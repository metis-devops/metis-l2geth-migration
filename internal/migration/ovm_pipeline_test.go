package migration

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestOVMHistoryOrderedCompletionAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "out-of-order", true: "worker-error"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			db := rawdb.NewMemoryDatabase()
			defer func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			idx := newOVMIndex(db)
			defer idx.batch.Close()
			s := ovmHistoryScanner{index: idx, history: sha256.New(), events: sha256.New()}
			batch := ovmHistoryBatch{ctx: ctx, cancel: cancel, limiter: newMigrateWorkLimiter(2)}
			secondDone := make(chan struct{})
			var active, peak atomic.Int32
			injected := errors.New("injected history decode")
			batch.decode = func(b *ovmHistoryBlock) error {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				if b.number == 0 {
					<-secondDone
				} else {
					close(secondDone)
					if fail {
						return injected
					}
				}
				b.receipts = types.Receipts{&types.Receipt{Logs: []*types.Log{ovmTestEvent(ovmTransferTopic, common.Address{byte(b.number + 1)}, common.Address{}, 0)}}}
				return nil
			}
			for n := range uint64(2) {
				batch.submit(&ovmHistoryBlock{number: n, raw: []byte{byte(n)}}, 1)
			}
			var completed atomic.Uint64
			err := s.drainHistory(&batch, &completed)
			batch.jobs.Wait()
			if active.Load() != 0 || peak.Load() > 2 {
				t.Fatal("history workers exceeded allowance or failed to join")
			}
			if fail {
				if !errors.Is(err, injected) || completed.Load() != 0 {
					t.Fatalf("lost error or accepted partial history: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if completed.Load() != 2 || batch.bytes != 0 || len(batch.blocks) != 0 {
				t.Fatal("history queue did not drain")
			}
			want := ovmHistoryScanner{index: idx, history: sha256.New(), events: sha256.New()}
			for n := range uint64(2) {
				b := &ovmHistoryBlock{number: n, raw: []byte{byte(n)}, receipts: types.Receipts{&types.Receipt{Logs: []*types.Log{ovmTestEvent(ovmTransferTopic, common.Address{byte(n + 1)}, common.Address{}, 0)}}}}
				if err := want.acceptBlock(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			if string(s.history.Sum(nil)) != string(want.history.Sum(nil)) || string(s.events.Sum(nil)) != string(want.events.Sum(nil)) {
				t.Fatal("parallel history changed serial digest ordering")
			}
		})
	}
}

func TestOVMGoldenSupplyMismatch(t *testing.T) {
	source := loadGoldenLegacyKV(t)
	s, err := openLegacySource(source, 16, 16, newProgressReporter("test", ProgressOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	indexDB := rawdb.NewMemoryDatabase()
	defer func() {
		if err := indexDB.Close(); err != nil {
			t.Error(err)
		}
	}()
	idx := newOVMIndex(indexDB)
	defer idx.batch.Close()
	for _, a := range []common.Address{ovmETHAddress, common.HexToAddress("0x1000000000000000000000000000000000000001"), common.HexToAddress("0x2000000000000000000000000000000000000002"), common.HexToAddress("0x3000000000000000000000000000000000000003"), common.HexToAddress("0x4000000000000000000000000000000000000004")} {
		if err := idx.address(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := idx.flush(); err != nil {
		t.Fatal(err)
	}
	_, _, err = transformOVMState(t.Context(), s.db, s.head.StateRoot, idx, ovmInputs{code: []byte{0}}, 2, newMigrateWorkLimiter(2))
	if err == nil || !strings.Contains(err.Error(), "totalSupply mismatch") {
		t.Fatalf("golden canary must fail source supply reconciliation: %v", err)
	}
}
