package migration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

func TestOVMOrderedBalanceInputFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		key, value []byte
		want       string
	}{
		{"short-key", []byte{1}, make([]byte, 84), "invalid indexed"},
		{"short-value", make([]byte, 32), make([]byte, 83), "invalid indexed"},
		{"wrong-hash", make([]byte, 32), make([]byte, 84), "account hash mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			index := testAllocIndex(t)
			if err := index.put('q', test.key, test.value); err != nil {
				t.Fatal(err)
			}
			tr := ovmTransformer{index: index, limiter: newMigrateWorkLimiter(2)}
			batch := ovmBalanceBatch{ctx: t.Context()}
			if err := tr.inspectOrderedBalances(&batch); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestOVMOrderedBalanceIteratorFailureJoins(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		index := testAllocIndex(t)
		injected := errors.New("ordered iterator failure")
		db := &cursorCountingDB{Database: index.db, failAt: failAt, failure: injected}
		index.db = db
		ctx, cancel := context.WithCancel(t.Context())
		tr := ovmTransformer{index: index, limiter: newMigrateWorkLimiter(2), workers: 2}
		// Zero values still exercise dispatch but need no account lookup.
		address := common.Address{1}
		slot := ovmStorageHash(ovmBalanceSlot(address))
		if err := tr.queueBalance(address[:], slot[:], new(uint256.Int)); err != nil {
			t.Fatal(err)
		}
		batch := ovmBalanceBatch{ctx: ctx, cancel: cancel, transformer: &tr}
		err := tr.inspectOrderedBalances(&batch)
		cancel()
		batch.jobs.Wait()
		if !errors.Is(err, injected) {
			t.Fatalf("lost iterator failure: %v", err)
		}
		if db.opens != db.releases || len(tr.limiter.tokens) != 0 {
			t.Fatal("iterator or worker lease leaked")
		}
	}
}

func TestOVMOrderedBalanceRecordsOwnIteratorBytes(t *testing.T) {
	index := testAllocIndex(t)
	tr := ovmTransformer{index: index, limiter: newMigrateWorkLimiter(2), workers: 2,
		evidence: OVMBalanceEvidence{SelfBalance: new(uint256.Int), RemainingSupply: new(uint256.Int)}}
	// Self and zero jobs do not need source account reads. Their queued copies
	// must survive subsequent iterator advancement and ordered application.
	for n := byte(1); n < 40; n++ {
		address := common.Address{n}
		slot := ovmStorageHash(ovmBalanceSlot(address))
		if err := tr.queueBalance(address[:], slot[:], new(uint256.Int)); err != nil {
			t.Fatal(err)
		}
	}
	slot := ovmStorageHash(ovmBalanceSlot(ovmETHAddress))
	if err := tr.queueBalance(ovmETHAddress[:], slot[:], uint256.NewInt(7)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batch := ovmBalanceBatch{ctx: ctx, cancel: cancel, transformer: &tr}
	defer batch.jobs.Wait()
	if err := tr.inspectOrderedBalances(&batch); err != nil {
		t.Fatal(err)
	}
	if tr.total.Uint64() != 7 || tr.evidence.SelfBalance.Uint64() != 7 || tr.evidence.RemainingSupply.Uint64() != 7 {
		t.Fatal("ordered jobs changed balances")
	}
	hash := crypto.Keccak256Hash(ovmETHAddress[:])
	if _, ok, err := index.get('q', hash[:]); err != nil || !ok {
		t.Fatalf("missing queued self balance: %v", err)
	}
}
