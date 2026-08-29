package migration

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/metis-devops/metis-l2geth-migration/internal/bundle"
)

func TestProgressReporterPeriodicAndStops(t *testing.T) {
	output := newWaitBuffer()
	logger := log.NewLogger(log.LogfmtHandlerWithLevel(output, log.LevelInfo))
	reporter := newProgressReporter("import", ProgressOptions{Logger: logger, Interval: time.Millisecond}, "bundle", "/bundle")
	counts := new(progressCounts)
	total := bundle.Counts{Accounts: 10, Records: 10, PayloadBytes: 100}
	phase := reporter.StartPhase("scan_bundle", countProgressSnapshot(counts, &total))
	counts.accounts.Store(5)
	counts.records.Store(5)
	counts.payloadBytes.Store(50)
	waitForLog(t, output, "status=progress")

	counts.accounts.Store(10)
	counts.records.Store(10)
	counts.payloadBytes.Store(100)
	phase.Finish(nil)
	reporter.Finish(nil)
	logged := output.String()
	for _, want := range []string{
		"operation=import",
		"phase=scan_bundle",
		"status=progress",
		"status=completed",
		"progress=100.0%",
		"records=10",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("progress output does not contain %q: %s", want, logged)
		}
	}

	size := output.Len()
	time.Sleep(5 * time.Millisecond)
	if output.Len() != size {
		t.Fatalf("phase ticker wrote after Finish: before=%d after=%d output=%s", size, output.Len(), output.String())
	}
}

func TestProgressReporterCancellationAndFailure(t *testing.T) {
	output := newWaitBuffer()
	logger := log.NewLogger(log.LogfmtHandlerWithLevel(output, log.LevelInfo))
	reporter := newProgressReporter("verify", ProgressOptions{Logger: logger})
	phase := reporter.StartPhase("verify_state", nil)
	phase.Finish(context.Canceled)
	reporter.Finish(context.Canceled)
	failed := newProgressReporter("export", ProgressOptions{Logger: logger})
	failed.Finish(errors.New("export failed"))
	logged := output.String()
	for _, want := range []string{
		"Migration phase canceled",
		"Migration operation canceled",
		"status=canceled",
		"Migration operation failed",
		"status=failed",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("status output does not contain %q: %s", want, logged)
		}
	}
}

func TestProgressReporterNilLoggerIsNoop(t *testing.T) {
	reporter := newProgressReporter("export", ProgressOptions{})
	phase := reporter.StartPhase("export_state", nil)
	phase.Finish(nil)
	reporter.Info("ignored")
	reporter.Finish(nil)
}

func TestProgressReporterDoesNotReplaceGethRoot(t *testing.T) {
	root := log.Root()
	output := newWaitBuffer()
	logger := log.NewLogger(log.LogfmtHandlerWithLevel(output, log.LevelInfo))
	reporter := newProgressReporter("export", ProgressOptions{Logger: logger})
	reporter.Finish(nil)
	if log.Root() != root {
		t.Fatal("progress reporter replaced the global geth logger")
	}
}

func TestProgressPercentSemantics(t *testing.T) {
	t.Run("zero total completes at one hundred percent", func(t *testing.T) {
		output := newWaitBuffer()
		logger := log.NewLogger(log.LogfmtHandlerWithLevel(output, log.LevelInfo))
		reporter := newProgressReporter("verify", ProgressOptions{Logger: logger})
		counts := new(progressCounts)
		total := bundle.Counts{}
		phase := reporter.StartPhase("scan_bundle", countProgressSnapshot(counts, &total))
		phase.Finish(nil)
		if logged := output.String(); !strings.Contains(logged, "progress=100.0%") {
			t.Fatalf("zero-total completion is missing 100%% progress: %s", logged)
		}
	})

	t.Run("unknown total omits percentage", func(t *testing.T) {
		output := newWaitBuffer()
		logger := log.NewLogger(log.LogfmtHandlerWithLevel(output, log.LevelInfo))
		reporter := newProgressReporter("export", ProgressOptions{Logger: logger})
		counts := new(progressCounts)
		counts.records.Store(1)
		phase := reporter.StartPhase("export_state", countProgressSnapshot(counts, nil))
		phase.Finish(nil)
		if logged := output.String(); strings.Contains(logged, "progress=") {
			t.Fatalf("unknown-total export reported a percentage: %s", logged)
		}
	})
}

func TestCountingStateVisitor(t *testing.T) {
	counts := new(progressCounts)
	visitor := newCountingStateVisitor(nil, counts)
	account := types.NewEmptyStateAccount()
	if err := visitor.Account(common.HexToHash("0x01"), account, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := visitor.Storage(common.HexToHash("0x01"), common.HexToHash("0x02"), []byte{4, 5}); err != nil {
		t.Fatal(err)
	}
	if err := visitor.Code(common.HexToHash("0x01"), common.HexToHash("0x03"), []byte{6, 7, 8, 9}); err != nil {
		t.Fatal(err)
	}
	want := bundle.Counts{Accounts: 1, StorageSlots: 1, CodeReferences: 1, Records: 3, PayloadBytes: 9}
	if got := counts.snapshot(); got != want {
		t.Fatalf("unexpected progress counts: got %+v want %+v", got, want)
	}
}

func TestVerifyDatabaseInventoryHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := rawdb.NewMemoryDatabase()
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close memory database: %v", err)
		}
	}()
	if err := verifyDatabaseInventory(ctx, db, rawdb.HashScheme, bundle.Counts{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

type waitBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	notify chan struct{}
}

func newWaitBuffer() *waitBuffer {
	return &waitBuffer{notify: make(chan struct{}, 1)}
}

func (b *waitBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buffer.Write(data)
	b.mu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default:
	}
	return n, err
}

func (b *waitBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func (b *waitBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Len()
}

func waitForLog(t *testing.T, output *waitBuffer, fragment string) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		if strings.Contains(output.String(), fragment) {
			return
		}
		select {
		case <-output.notify:
		case <-timer.C:
			t.Fatalf("timed out waiting for %q in progress output: %s", fragment, output.String())
		}
	}
}
