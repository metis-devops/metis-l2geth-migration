package migration

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	pruneDeleteBatchBytes  = 1 << 20
	pruneDeleteBatchKeys   = 16384
	pruneParallelHashBytes = 4096
)

type pruneExecution struct {
	workers                    int
	sourceCache, keepCache     int
	sourceHandles, keepHandles int
	scanBytes                  int
	progress                   *progressCounts
	scan                       *pruneScanProgress
	classifyRecord             func(context.Context, *pruneScanRecord) error
}

type pruneScanProgress struct{ keys, bytes atomic.Uint64 }

func newPruneExecution(o PruneOptions) *pruneExecution {
	keepCache := max(16, o.CacheMB/3)
	keepHandles := max(16, o.Handles/3)
	return &pruneExecution{workers: normalizeMigrateWorkers(o.Workers),
		sourceCache: o.CacheMB - keepCache, keepCache: keepCache,
		sourceHandles: o.Handles - keepHandles, keepHandles: keepHandles,
		scanBytes: max(1<<20, min(16<<20, o.CacheMB*(1<<20)/8))}
}

func (e *pruneExecution) phaseSnapshot() progressSnapshot {
	return func(elapsed time.Duration, _ bool) []any {
		c := e.progress.snapshot()
		keys, size := e.scan.keys.Load(), e.scan.bytes.Load()
		return []any{"accounts", c.Accounts, "storage_slots", c.StorageSlots, "records", c.Records, "records_per_second", float64(c.Records) / max(elapsed.Seconds(), 0.001), "scanned_keys", keys, "scanned_bytes", size, "keys_per_second", float64(keys) / max(elapsed.Seconds(), 0.001)}
	}
}
