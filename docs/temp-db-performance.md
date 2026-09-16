# Optional in-memory temporary Pebble databases

## Implementation and validation

`migrate`, `import`, and `verify` accept `--temp-db disk|memory`, defaulting to
`disk`. This setting selects storage for reachable-node indexes and OVM
original-state/evidence/patch databases. It does not select the final artifact
engine and is not serialized in reports. Export has no temporary database;
prune retains its existing disk-only keep database.

Memory mode uses the pinned Pebble v2 `vfs.NewMem`, with an operation-owned
filesystem spanning original-state writer close, independent read-only reopen
and complete verification, and the subsequent writable conversion phase. A
small internal ethdb adapter preserves geth byte ownership, batches, ordered
prefix iterators, error sentinels and resource-release errors. Its cache,
memtable, WAL, compression and compaction settings follow the pinned geth
adapter. The default disk adapter and final artifact publication are unchanged.
Both storage modes use the same full logical state/inventory verifier.

The regression suite covers four target engine/scheme combinations, both bundle
compression modes, independent reference inventories, disk/memory cross-mode
verification, OVM alloc, runtime execution and continuation, original-state
corruption, target/input/history tampering, source immutability, physical
scratch absence, cancellation and cleanup. Memory adapter tests inject read,
iterator and close failures and exercise key/value ownership, batch replay,
range deletion, concurrent writes and read-only reopening.

Validation completed with `make ci`, `make test-race` (including the legacy-prune
race CLI), and `git diff --check`. No geth pin, format version, compression,
legacy canary or frozen compatibility corpus was changed.

## Reproduction

Host: Apple M3 Max, 16 logical CPUs, 48 GiB RAM, macOS 27.0 arm64,
Go 1.27.1. Development started from commit `f18eaad`; timings compare the two modes of
the current implementation, not that historical commit.
The raw output records the measured Go-source SHA-256 and system timing/RSS.

```bash
python3 -B scripts/benchmark-ovm.py \
  --out /absolute/new/temp-db-results.txt --count 3 \
  --holders 10000 100000 --temp-dbs disk memory \
  --with-alloc --with-verify --with-components
```

Each configuration runs three times in fresh processes, alternating backend,
worker and operation order. No CI or other benchmark from this task runs during
measurement. End-to-end fixtures have two blocks, 10k/100k holders, workers 2/8,
128 MiB configured cache, 128 handles, and a final Pebble/hash artifact. Alloc
adds code, a native-balance override and one storage override to 1,000 holders.
Verification setup always prepares the artifact using disk mode, so only replay
and actual artifact verification use the selected mode.

Components use 10k/100k/1m deterministic high-entropy 32-byte keys/values (nil
values for node markers), grouped into 1,000 prefixes. Preparation populates the
database before timing: `batch` measures batched overwrites, `dedup` repeated
node markers, `get` a deterministic permutation of point reads, `scan` one full
ordered scan, and `prefix` 1,000 prefix scans collectively visiting every record.
Disk and memory Pebble components use the same adapter/settings. Geth memorydb
is measured only at this component level, with no claim of full migration or
independent reopen support. Component operations have one foreground caller;
Pebble background work remains enabled, so these are not concurrency-scaling
measurements for geth memorydb.

## Interpretation limits

- Tables use medians of three samples. These synthetic workloads do not prove
  production speedups or worst-case RAM/disk requirements. Only correctness,
  not performance, was checked for final LevelDB/path combinations.
- `ns/op` and Go allocation counters exclude setup. End-to-end `setup-s` is
  reported separately. External peak RSS includes fixture setup and, for
  verification, the initial disk migration. Component input arrays remain live
  and contribute the same baseline memory to all three backends.
- Heap and file sizes are sampled every 20 ms and may miss short peaks. Heap
  values include live setup objects and exclude some native Pebble allocations.
  Memory-file sizes measure logical file lengths, not slice capacity or total
  memory. They do not replace RSS. Sampling itself adds overhead.
- Physical temporary-file bytes exclude the final artifact. Total disk bytes
  include it. OS page caches are not flushed and are not part of process RSS;
  no claim of cold-disk throughput or total machine RAM is made.
- `--cache-mb` does not cap RAM used by memory files, compaction or the process.
  Memory mode deliberately retains all temporary data; there is no automatic
  spill or hard RAM limit. Final output still requires disk space and sync.

Raw evidence: [temp-db-benchmark-2026-09-16.txt](temp-db-benchmark-2026-09-16.txt).

## Measured results

All 231 samples completed: 96 end-to-end and 135 component samples, covering
77 configurations three times each. Percent changes below compare medians,
not a claim of statistical significance. Every memory-mode end-to-end sample
reported zero physical temporary database bytes.

### End-to-end time and process RSS

| Operation | Holders | Workers | Disk seconds | Memory seconds | Time change | Disk RSS MiB | Memory RSS MiB |
|---|---:|---:|---:|---:|---:|---:|---:|
| Migration | 10,000 | 2 | 0.862 | 0.554 | -35.7% | 80.8 | 111.5 |
| Migration | 100,000 | 2 | 8.724 | 7.409 | -15.1% | 281.3 | 555.4 |
| Verification | 10,000 | 2 | 0.780 | 0.473 | -39.3% | 88.8 | 117.9 |
| Verification | 100,000 | 2 | 8.535 | 6.969 | -18.3% | 372.8 | 585.7 |
| Migration + alloc | 10,000 | 2 | 0.930 | 0.627 | -32.5% | 85.4 | 115.4 |
| Migration + alloc | 100,000 | 2 | 10.450 | 8.561 | -18.1% | 310.3 | 597.5 |
| Verification + alloc | 10,000 | 2 | 0.845 | 0.562 | -33.5% | 93.2 | 120.7 |
| Verification + alloc | 100,000 | 2 | 10.048 | 8.916 | -11.3% | 376.6 | 619.7 |
| Migration | 10,000 | 8 | 0.774 | 0.461 | -40.5% | 81.4 | 100.9 |
| Migration | 100,000 | 8 | 7.837 | 6.524 | -16.8% | 290.5 | 554.5 |
| Verification | 10,000 | 8 | 0.649 | 0.386 | -40.5% | 93.6 | 110.7 |
| Verification | 100,000 | 8 | 7.693 | 6.145 | -20.1% | 371.6 | 597.8 |
| Migration + alloc | 10,000 | 8 | 0.839 | 0.542 | -35.4% | 93.4 | 121.6 |
| Migration + alloc | 100,000 | 8 | 9.325 | 7.594 | -18.6% | 302.6 | 608.0 |
| Verification + alloc | 10,000 | 8 | 0.766 | 0.458 | -40.2% | 109.5 | 133.3 |
| Verification + alloc | 100,000 | 8 | 9.302 | 7.344 | -21.1% | 374.4 | 611.4 |

All measured end-to-end median times improved. At 100k holders, memory-mode
RSS medians were approximately 554–620 MiB, versus 281–377 MiB for disk mode.
At 10k holders, memory-mode medians were approximately 101–133 MiB, versus
81–110 MiB for disk mode. This supports an opt-in memory mode, not changing
defaults or sizing production machines from these fixtures.

### End-to-end memory/file detail

Values are per-configuration medians. Heap/files are sampled peaks; allocated
bytes are cumulative Go allocations during the operation, not live RAM.

| Operation | Holders | Workers | Disk temp files MiB | Memory files MiB | Disk heap MiB | Memory heap MiB | Disk allocated MiB | Memory allocated MiB |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Migration | 10,000 | 2 | 13.0 | 9.8 | 25.8 | 43.6 | 159.5 | 257.3 |
| Migration | 100,000 | 2 | 127.4 | 109.3 | 138.7 | 344.9 | 1919.0 | 4827.3 |
| Verification | 10,000 | 2 | 13.0 | 12.7 | 26.4 | 48.1 | 153.6 | 251.3 |
| Verification | 100,000 | 2 | 123.6 | 108.4 | 213.9 | 348.8 | 1912.4 | 4813.0 |
| Migration + alloc | 10,000 | 2 | 13.1 | 11.4 | 24.3 | 49.1 | 196.0 | 307.9 |
| Migration + alloc | 100,000 | 2 | 136.9 | 119.1 | 140.8 | 378.6 | 2107.6 | 6303.0 |
| Verification + alloc | 10,000 | 2 | 13.6 | 11.4 | 25.8 | 42.8 | 189.3 | 310.8 |
| Verification + alloc | 100,000 | 2 | 137.5 | 119.6 | 211.5 | 361.4 | 2100.2 | 6298.1 |
| Migration | 10,000 | 8 | 7.9 | 7.9 | 27.8 | 39.4 | 158.6 | 231.0 |
| Migration | 100,000 | 8 | 128.8 | 111.6 | 145.4 | 344.2 | 1920.9 | 4783.2 |
| Verification | 10,000 | 8 | 7.9 | 7.9 | 28.1 | 36.3 | 153.0 | 223.9 |
| Verification | 100,000 | 8 | 128.9 | 111.9 | 213.3 | 349.5 | 1912.2 | 4810.9 |
| Migration + alloc | 10,000 | 8 | 16.5 | 14.1 | 31.1 | 52.7 | 201.0 | 320.9 |
| Migration + alloc | 100,000 | 8 | 135.2 | 118.6 | 144.7 | 375.6 | 2111.8 | 6315.4 |
| Verification + alloc | 10,000 | 8 | 16.5 | 15.0 | 28.5 | 51.5 | 193.7 | 312.9 |
| Verification + alloc | 100,000 | 8 | 134.9 | 118.6 | 215.6 | 375.0 | 2104.7 | 6311.2 |

Raw evidence also includes allocation counts, GC cycles, setup time, total
disk bytes and OS resource counters for each sample.

### Components: one million records

Each row is one complete trace, not one key operation. Point reads visit one
million records; the prefix trace opens 1,000 iterators.

| Trace | Disk Pebble seconds | Memory Pebble seconds | Geth memorydb seconds | Disk RSS MiB | Memory Pebble RSS MiB | Geth memorydb RSS MiB |
|---|---:|---:|---:|---:|---:|---:|
| batch | 1.5623 | 0.2631 | 0.1023 | 169.5 | 615.5 | 352.0 |
| dedup | 0.9497 | 0.2858 | 0.0913 | 151.8 | 410.6 | 283.8 |
| get | 3.7340 | 3.0828 | 0.1840 | 215.3 | 459.9 | 324.0 |
| scan | 0.1290 | 0.1115 | 0.2205 | 160.0 | 498.0 | 330.2 |
| prefix | 0.1279 | 0.1063 | 22.7408 | 165.2 | 474.5 | 598.9 |

Geth memorydb had faster point reads and overwrites, with lower RSS in those
one-million-record traces. For the 1,000-prefix trace, however, its median was
22.741 seconds versus 0.106 seconds for memory Pebble (about 214×), and its RSS
was approximately 599 MiB versus 475 MiB. The repeated full-map scan/sort and
full-database iterator slice allocations make it a poor fit for per-account
alloc patch scans. These results do not establish that Pebble always uses less
RAM; most other component traces measured the opposite.

### Regressions and small-trace variability

Observed memory-Pebble median regressions relative to disk Pebble:

- 10,000 records, `batch`: 2.454 → 2.575 ms (+5.0%).
- 100,000 records, `scan`: 6.273 → 6.881 ms (+9.7%).
- 100,000 records, `prefix`: 8.816 → 9.967 ms (+13.1%).

These short traces retain LSM/cache/compaction work and add Go-memory filesystem
overhead. The larger end-to-end improvements do not erase these regressions.
No optimization was applied to selectively improve these measured cases.
