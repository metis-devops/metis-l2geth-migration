# Prune performance measurements — 2026-09-12

The default 4-worker implementation reduced median end-to-end time in all eight
synthetic workloads, by **20.8%–65.8%**
(**1.26×–2.92×** the serial reference's throughput for identical work).
This is a local, page-cache-warm comparison, not a production snapshot or disk-throughput guarantee.

These measurements describe the source fingerprint below, before the subsequent
dry-run LOCK guard and legacy Bloom-filter restoration fixes. They have not been
refreshed for those storage changes; use the runner below to measure current code.

## Method

- Apple M3 Max, darwin/arm64, Go 1.27.1, GOMAXPROCS=16.
- Both versions use total database allowances of 128 MiB cache and 128 handles.
  The reference uses its original 112/16 split; the optimized version uses 86/42.
- Five fresh processes per workload/mode, rotating mode order between repetitions.
  Each process runs the five phases once; page caches are not cleared.
- All databases are generated locally. Fixture creation/copying is outside the
  benchmark timer; the full operation includes database opening, all required
  verification, syncing and temporary-directory cleanup. Component timings have
  their prerequisites prepared outside the timer and are not additive.
- The test-only serial implementation was captured before the refactor. Raw KV,
  roots, counts, protected digests and final deletion results are compared in tests.
- Original staged-diff SHA-256: `dbe170fca2be2d3fabee291149aba2db390455ea2f80f1b489a4d63071f2d5d8`.
- Final measured Go-source/module fingerprint: `0a08ee26b1152ae244810e0f126cc570bb28a7514bc952dbf360bfe535d1c43b`.
- [Raw results](benchmarks/prune-2026-09-12.txt): 200 groups × five samples = 1000 timed observations.

```bash
python3 scripts/benchmark-prune.py --out /tmp/prune-bench.txt --count 5
```

## End-to-end median milliseconds

| Workload | Serial | 2 workers | 4 workers | 8 workers | 16 workers | Speedup at 4 |
|---|---:|---:|---:|---:|---:|---:|
| light | 377.0 | 221.1 | 194.9 | 205.4 | 242.7 | 1.93× |
| dense-storage | 528.3 | 297.3 | 256.2 | 268.9 | 271.7 | 2.06× |
| storage-1024 | 280.2 | 183.0 | 170.1 | 178.2 | 173.5 | 1.65× |
| storage-1025 | 283.6 | 267.0 | 224.6 | 228.8 | 223.4 | 1.26× |
| giant-storage | 993.0 | 489.4 | 406.2 | 432.2 | 424.9 | 2.44× |
| shared-code | 380.5 | 225.5 | 196.8 | 207.8 | 219.9 | 1.93× |
| history-90pct | 233.9 | 146.6 | 141.7 | 146.6 | 147.1 | 1.65× |
| history-99pct | 1245.2 | 435.9 | 425.9 | 436.3 | 435.4 | 2.92× |

The light/shared-code cases contain 12000 accounts; dense storage contains
256 accounts × 64 slots; the threshold cases contain eight accounts × 1024/1025
slots; the giant case has one account with 32768 slots. History cases contain
1000 current accounts plus 16000/160000 obsolete content-addressed records.
Their names indicate approximate historical-state proportions, not exact ratios.

## Component median milliseconds: serial → 4 workers

| Workload | Collect | Verify keep state | Scan | Delete |
|---|---:|---:|---:|---:|
| light | 158.7 → 96.8 | 36.7 → 14.7 | 39.7 → 11.7 | 36.4 → 7.6 |
| dense-storage | 217.9 → 138.6 | 57.9 → 21.2 | 63.9 → 14.2 | 60.3 → 9.8 |
| storage-1024 | 123.9 → 87.0 | 21.1 → 9.2 | 26.4 → 7.6 | 26.1 → 5.2 |
| storage-1025 | 127.9 → 124.0 | 21.4 → 17.6 | 27.1 → 7.3 | 26.3 → 5.2 |
| giant-storage | 380.4 → 211.1 | 106.9 → 45.9 | 135.3 → 28.4 | 127.8 → 23.0 |
| shared-code | 155.6 → 100.6 | 36.8 → 14.2 | 39.2 → 11.9 | 35.9 → 7.6 |
| history-90pct | 59.2 → 55.8 | 1.4 → 1.1 | 17.9 → 9.2 | 95.9 → 19.0 |
| history-99pct | 65.8 → 55.8 | 1.3 → 1.0 | 135.3 → 71.9 | 916.6 → 180.7 |

These history fixtures use small obsolete values. Their scan/deletion gains
primarily measure ordered merge comparison and fewer synchronous write batches,
not parallel hashing of large code blobs. Records below 4 KiB use the direct
path when the ordered queue is empty; large candidates use bounded hash workers.

## Resource observations: serial → 4 workers

| Workload | End-to-end allocated MiB | Sampled peak Go heap MiB | Collect-phase final temp files MiB |
|---|---:|---:|---:|
| light | 111.0 → 105.3 | 92.8 → 91.5 | 3.3 → 2.4 |
| dense-storage | 131.3 → 126.5 | 104.6 → 110.1 | 5.0 → 5.0 |
| storage-1024 | 74.9 → 70.5 | 63.4 → 59.8 | 2.2 → 1.3 |
| storage-1025 | 75.1 → 110.1 | 63.7 → 69.6 | 2.2 → 4.0 |
| giant-storage | 236.1 → 215.4 | 150.1 → 175.4 | 10.2 → 10.6 |
| shared-code | 109.7 → 103.9 | 92.4 → 93.5 | 3.3 → 2.5 |
| history-90pct | 49.4 → 56.3 | 26.0 → 27.8 | 0.2 → 0.2 |
| history-99pct | 188.8 → 190.0 | 102.5 → 161.5 | 0.2 → 0.2 |

Memory usage is not uniformly lower. Larger keep caches, concurrent traversals,
and larger deletion batches trade additional live memory for latency. The
1025-slot case also pays the storage probe and partition startup cost; increasing
workers is not monotonically faster. Keep the default at four and tune using
representative data rather than assuming sixteen is best.

Heap samples are taken every 2 ms after an untimed GC, and report absolute Go
HeapInuse during the phase. They exclude native allocations, process RSS and
kernel caches; instantaneous peaks may be missed. Temporary file sizes are
logical file lengths after collection, not peak compaction space or disk blocks.
The end-to-end operation removes its temporary directory, so its file size is
represented by the separate collection measurement.

Darwin returned zero for both `getrusage` block-I/O counters in this run. Those
raw counters are recorded but provide no useful physical I/O measurement here;
they do not imply that reads, writes or fsync performed no disk traffic. This
report makes no production-disk IOPS or bandwidth claim.

The optimized path retains full preflight, independent reopens, protected-byte
comparison/digest ordering, genesis-root retention, strict CURRENT handling,
LES detection, synced deletion batches and failure/cancellation joins. No
verification pass was removed to obtain these results.
