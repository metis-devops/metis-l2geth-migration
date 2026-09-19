# OVM benchmark results

Updated on 2026-09-19. [Dataset index and raw-file checksums](README.md).

## Measurement boundaries

- The latest measured source is the ordered-balance/evidence-cursor optimization on top of the uncommitted input-reader and review fixes, identified by the fingerprint below. Parallel-evidence, retention and earlier optimization datasets remain separate historical snapshots; these measurements do not apply to arbitrary future HEAD.
- All OVM end-to-end results use an Apple M3 Max (16 logical CPUs), macOS 27.0 arm64, a two-block synthetic history, Pebble/hash targets, 128 MiB cache allowance and 128 handles. Alloc adds code, native balance and one storage override to 1,000 holders.
- The locality comparison uses six fresh-process samples per configuration with alternating order. Historical end-to-end comparisons use three, and input-reader components use five. Paired runs follow CI/race without concurrent benchmarks. Tables are medians calculated from their named raw dataset. Samples from different runs or source versions are never pooled.
- Operation time and allocation counters exclude setup. Process RSS includes setup and, for verification, the initial disk-mode migration. `setup-s` is separate. OS caches were not flushed.
- Heap and file lengths are sampled every 20 ms and can miss peaks. Heap excludes some native allocations; memory-file lengths exclude allocation capacity. Temporary disk excludes the final artifact. Neither cache allowances nor these samples bound total memory or disk requirements.
- No production throughput, long-history, cold-disk, LevelDB or path-scheme performance claim follows from these fixtures. Historical results below have not been rerun on the latest source.

## Ordered balance reads and evidence cursors — 2026-09-19

The comparison baseline is the working-tree snapshot at the start of this task,
including the already staged review fixes and input-reader optimizations. It is
**not Git HEAD**. Both binaries use identical end-to-end benchmark/profile harnesses.
The new isolated cursor benchmark and regression tests exist only in current.

- Baseline: `3f7990c3aee37b3b6e6e789204a3935ccd08b66b603e55468f77383e492d0788`
- End-to-end/profile current: `c0aa4a18cb30e5287c0975aa9a4880365c36c5a71680e3cdf36ca78d7b95247b`
- Final/component source: `5d253e788c55d40c66135bb48c877be76628d182b37977076d196b6efc501a64`

The final fingerprint differs only by adding `b.StopTimer()` before deferred
database cleanup in the isolated evidence component benchmark. Production code
and the end-to-end/profile harness are byte-identical between those two current
snapshots. Component observations including cleanup were excluded and rerun.

### Diagnosis and implementation

The prior +1.7% 100k migration result and +7.6% review-fix verification result are
observed regressions from three-sample datasets, not proof of a persistent regression
or its cause. This investigation profiles the current working tree and compares
explicit changes against that same baseline; it does not attribute historical
variation to a particular commit.

Go CPU, allocation, mutex and blocking profiles identify substantial trie-node
reads and decoding. Account lookups visit balance-slot-hash order, which scrambles
account hashes. Resetting each decoded trie after 32 reads then repeatedly decodes
upper branches. Database locks contribute contention, but summed blocking times
also include idle Pebble workers and must not be treated as elapsed critical-path
time. Simply raising worker count or expanding trie-cache windows is not justified.

Two changes address measurable repeated work:

1. Storage ownership checks reuse three ordered cursors instead of three independent
   point seeks per slot. All three namespaces remain checked for ambiguity. After
   eight forward steps a cursor reseeks, bounding work for sparse witnesses.
2. Fixed-size balance jobs are staged in the existing temporary Pebble index in
   account-hash order. Bounded readers then reuse nearby trie paths. The 32-read
   windows, two-times-worker in-flight bound, shared limiter, ordered result
   application, source classification, conservation and independent verification
   remain intact.

This trades temporary storage for locality: each indexed balance slot adds a
33-byte key and 84-byte value before compression/database overhead. WALs, snapshots
and compaction can raise physical peaks. Memory temporary mode holds these files
in RAM; fewer cumulative allocated bytes do not guarantee lower RSS.

### Paired end-to-end results

[10k raw data](raw/ovm-locality-pairs-2026-09-19.txt) contains 192 observations;
[100k raw data](raw/ovm-locality-scale-2026-09-19.txt) contains 48. Each configuration
has six baseline and six current fresh-process observations. Both use the existing
two-block synthetic history, 128 MiB/128-handle allowances, hash/Pebble targets,
no holder preimages and no manual retain list. Only the 10k matrix includes memory
scratch and alloc; no 100k memory/alloc result is implied.

The [benchstat analysis](raw/ovm-locality-benchstat-2026-09-19.txt) keeps every
holder/operation/worker/temp configuration separate. At 10k, all eight 2-worker
time reductions are significant (`p=0.002`), ranging from 6.6% to 14.0%. None of
the eight 8-worker time differences is significant (`p=0.065–0.937`). Positive
median changes up to +1.8% are retained below; this is not a regression-free claim.
Tests use benchstat's default 0.05 threshold without multiple-comparison adjustment.
Only per-configuration comparisons are interpreted; the raw tool's aggregate
geomeans span heterogeneous workloads and are not used as a speedup claim.

At 100k, 2-worker migration improves 9.9% (`p=0.015`) and verification 10.3%
(`p=0.004`). The 8-worker median reductions (2.1%/3.7%) are **not** significant
(`p=0.240`/`0.394`). Timing ranges are wide; do not advertise those medians as
established throughput gains. Allocated bytes fall 13.7–16.5% in all four 100k
configurations (`p=0.002`).

All table values are medians. Timers/allocations exclude setup; RSS includes
setup and the initial disk migration used to prepare verification. Disk/heap/file
peaks are sampled every 20 ms and can miss peaks. Full raw metrics remain available.

| Holders | Operation | Workers | Temp | Baseline ms | Current ms | Time change | B/op change | RSS MiB baseline/current | Temp disk MiB baseline/current |
|---:|---|---:|---|---:|---:|---:|---:|---:|---:|
| 10000 | Migration | 2 | disk | 876.9 | 805.0 | -8.2% | -11.6% | 81.0 / 80.1 | 13.0 / 14.4 |
| 10000 | Migration | 2 | memory | 531.1 | 464.4 | -12.6% | -3.8% | 106.0 / 104.5 | 0.0 / 0.0 |
| 10000 | Migration | 8 | disk | 743.3 | 720.0 | -3.1% | -8.9% | 83.0 / 81.9 | 7.9 / 9.3 |
| 10000 | Migration | 8 | memory | 426.4 | 434.1 | +1.8% | -2.4% | 100.7 / 100.4 | 0.0 / 0.0 |
| 10000 | MigrationAlloc | 2 | disk | 920.5 | 860.2 | -6.6% | -9.8% | 85.5 / 83.9 | 13.7 / 14.7 |
| 10000 | MigrationAlloc | 2 | memory | 617.0 | 546.7 | -11.4% | -4.1% | 118.3 / 113.1 | 0.0 / 0.0 |
| 10000 | MigrationAlloc | 8 | disk | 834.9 | 838.2 | +0.4% | -7.5% | 89.4 / 90.0 | 16.5 / 17.8 |
| 10000 | MigrationAlloc | 8 | memory | 517.2 | 518.8 | +0.3% | -2.7% | 121.1 / 122.9 | 0.0 / 0.0 |
| 10000 | Verification | 2 | disk | 793.2 | 728.0 | -8.2% | -12.8% | 88.7 / 87.1 | 12.2 / 13.9 |
| 10000 | Verification | 2 | memory | 456.2 | 392.3 | -14.0% | -5.9% | 115.5 / 117.9 | 0.0 / 0.0 |
| 10000 | Verification | 8 | disk | 663.0 | 649.2 | -2.1% | -10.6% | 91.7 / 89.6 | 7.9 / 9.3 |
| 10000 | Verification | 8 | memory | 362.2 | 361.5 | -0.2% | -2.6% | 110.0 / 106.8 | 0.0 / 0.0 |
| 10000 | VerificationAlloc | 2 | disk | 855.3 | 796.1 | -6.9% | -10.2% | 91.9 / 89.8 | 13.8 / 14.9 |
| 10000 | VerificationAlloc | 2 | memory | 530.7 | 464.8 | -12.4% | -1.5% | 121.0 / 118.8 | 0.0 / 0.0 |
| 10000 | VerificationAlloc | 8 | disk | 762.9 | 766.8 | +0.5% | -7.5% | 104.8 / 98.5 | 16.5 / 17.6 |
| 10000 | VerificationAlloc | 8 | memory | 443.7 | 442.9 | -0.2% | -2.4% | 132.3 / 133.8 | 0.0 / 0.0 |

| Holders | Operation | Workers | Temp | Baseline ms | Current ms | Time change | B/op change | RSS MiB baseline/current | Temp disk MiB baseline/current |
|---:|---|---:|---|---:|---:|---:|---:|---:|---:|
| 100000 | Migration | 2 | disk | 8477.7 | 7635.8 | -9.9% | -16.3% | 296.6 / 298.8 | 128.1 / 136.6 |
| 100000 | Migration | 8 | disk | 7457.8 | 7298.1 | -2.1% | -13.7% | 286.2 / 290.4 | 128.2 / 138.6 |
| 100000 | Verification | 2 | disk | 8149.6 | 7306.6 | -10.3% | -16.5% | 361.9 / 359.5 | 127.1 / 136.1 |
| 100000 | Verification | 8 | disk | 7302.4 | 7034.4 | -3.7% | -13.9% | 376.2 / 373.4 | 128.5 / 138.8 |

The 100k scratch-disk median increases by about 8.5–10.4 MiB. RSS does not
consistently decrease: for example, 100k/8-worker migration rises from 286.2 to
290.4 MiB, and 10k/2-worker memory verification rises from 115.5 to 117.9 MiB.
Cumulative allocation reductions and sampled heap peaks are not RSS guarantees.

The [final scoped profiles](raw/ovm-locality-profile-2026-09-19.txt) show account
reader allocation samples falling from 462.05 to 184.14 MB (pprof units), about
60%. This diagnoses the reduced path-decoding work; it is one diagnostic pair,
not a statistical timing result. Initial exploratory profiles/prototypes are not
pooled into any published timing dataset.

### Evidence lookup components

[Component raw data](raw/ovm-locality-components-2026-09-19.txt) contains six
alternating fresh processes per variant/configuration, with three iterations per
process. Each process creates the same 100,000-key disk Pebble index (16 MiB cache,
16 handles); keys have a one-byte namespace and 32-byte suffix, values are 32 bytes.
The dense trace queries every key; the sparse trace queries 100 keys at stride 1,000.
Both exclude setup and deferred database cleanup; Pebble background work stays
enabled and OS caches are not flushed. This isolates one namespace, not all storage
classification, trie reading or the complete migration.

| Trace | Point reads ms | Cursor ms | Time change | Point B/op | Cursor B/op |
|---|---:|---:|---:|---:|---:|
| Dense: 100,000 lookups | 96.383 | 11.939 | -87.6% | 8209409 | 3417764 |
| Sparse: 100 lookups | 0.267 | 0.466 | +74.6% | 32937 | 47264 |

The sparse component is a measured regression: cursor setup and up to eight
forward steps add about 0.20 ms per 100 lookups here. The reseek bound prevents
work growing with the number of skipped witnesses, but does not make sparse
queries faster than point reads. Dense evidence benefits; sparse production
workloads and cold disks require their own end-to-end acceptance measurements.
No regression-free or universal speedup claim follows from these results.

Validation: `make ci` and aggregate `make test-race` passed for the production
implementation, covering all target combinations, temporary modes, independent
replay/inventories, retention, alloc, source immutability and cancellation. After
the component timer-only correction, `make ci` passed again. New focused coverage
checks cursor EOF/gaps/reseeks, ambiguity, malformed addresses/jobs, iteration
failures, cancellation, iterator release, worker joining and borrowed-byte ownership.

### Reproduction and profiling boundaries

Run validation to completion before timing; never overlap CI, profiles or benchmark
processes. Use a separate baseline checkout containing the same benchmark harness.

```bash
python3 scripts/benchmark-ovm.py --count 6 --holders 10000 \
  --temp-dbs disk memory --with-alloc --with-verify \
  --baseline-root /absolute/isolated/baseline --out /absolute/new/locality-pairs.txt
python3 scripts/benchmark-ovm.py --count 6 --holders 100000 \
  --temp-dbs disk --with-verify \
  --baseline-root /absolute/isolated/baseline --out /absolute/new/locality-scale.txt

go test -c -o /absolute/migration.test ./internal/migration
# Run from internal/migration. The profile directory must not exist.
L2STATE_BENCH_HOLDERS=100000 L2STATE_BENCH_TEMP_DB=disk \
L2STATE_BENCH_PROFILE_DIR=/absolute/new/profile \
  /absolute/migration.test -test.run='^$' \
  -test.bench='^BenchmarkOVMMigration$/^workers=8$' -test.benchtime=1x
go tool pprof -top /absolute/new/profile/cpu.pprof
go tool pprof -alloc_space -base=/absolute/new/profile/alloc-before.pprof \
  /absolute/new/profile/alloc-after.pprof
```

The opt-in benchmark helper starts CPU/block/mutex profiling after fixture setup.
Allocation analysis subtracts the pre-operation profile from the post-operation
profile. Forced GC and profiling instrumentation affect these diagnostic runs;
they are excluded from timing datasets. Allocations are sampled, CPU profiles
include runtime/background work, and aggregate blocking delay is not wall time.
Do not combine this helper with Go's `-test.cpuprofile` flag.

## Historical witness parsing and hot-history lookups — 2026-09-19

This change removes a second JSON decoding pass per witness line and the
Has-before-Get lookup per present hot-history record. It preserves strict witness
acceptance, raw-byte digests, canonical-history checks, empty-record rejection,
not-found versus I/O-error handling, reader/queue bounds and independent reopening.
There is no larger trie cache or skipped state validation.

The baseline is a snapshot of the **uncommitted six-review-fix implementation**,
not Git HEAD. The only production changes in this comparison are the witness
parser and hot-history reader. Source fingerprints:

- Baseline: `f45845978e3e5024d251f80e73cceda8bba716f3451965f98ad3a53ca8312c34`
- Current: `251ba6d6ce5dda94a4b06250e1e779849d1d32de6366e34302438b5bc6990721`

`make ci`, aggregate `make test-race` and a 20-second differential witness fuzz
run (175,020 executions) passed. The frozen two-pass parser remains a test oracle.
Paired measurements ran sequentially after validation, without concurrent CI or other
benchmarks. Components, 10k end-to-end pairs, and 100k scale checks are separate
datasets; no historical samples or differently scoped measurements are pooled.

### Why these changes

An [exploratory CPU/allocation profile](raw/ovm-input-profile-2026-09-19.txt)
of 100k-holder, 2-worker, disk migration identified repeated witness decoding
(about 138 MiB sampled allocations in that decoder) and substantial trie/database
read work. This single profile includes fixture setup and test/runtime overhead;
it is diagnostic evidence, not a paired operation-time measurement. Account
reader caches were not expanded: the pinned hashdb clean cache has a 32 MiB
minimum, requiring a separate resource-budget decision. Existing 32-read decoded
trie windows remain intact.

### Isolated component medians

[Component raw log](raw/ovm-input-components-2026-09-19.txt): 30 successful fresh
processes, five samples for each component/variant, alternating reference/current
order. References are frozen pre-optimization implementations in the test binary.
An earlier exploratory run and a prototype runner check were excluded from these
medians. The archived dataset was run with `scripts/benchmark-ovm-inputs.py`.

| Component | Reference time | Current time | Time change | Reference B/op | Current B/op | Reference allocs/op | Current allocs/op |
|---|---:|---:|---:|---:|---:|---:|---:|
| One address witness | 1287.0 ns | 689.0 ns | -46.5% | 1485 | 650 | 25 | 15 |
| One allowance witness | 1768.0 ns | 960.6 ns | -45.7% | 1605 | 698 | 30 | 18 |
| 60k hot-history reads | 89.30 ms | 50.57 ms | -43.4% | 103844920 | 68995016 | 1338413 | 720009 |

Witnesses use the existing address and allowance encodings. The history component
reads 60,000 sequential present keys, each with a synthetic 512-byte value, through
strict read-only LevelDB with 16 MiB cache allowance and 16 handles. Fixture setup
and database opening are outside the timer; OS caches are not flushed. This
component does **not** measure receipt/header authentication, cold-history fallback,
freezer reads, or end-to-end history scanning. Missing hot keys already needed one
lookup before this change; the two-to-one reduction applies to present records.

### End-to-end medians: 10k holders

[10k raw log](raw/ovm-input-pairs-2026-09-19.txt): 96 successful observations,
three per source/configuration; 2/8 workers, disk/memory, migration/verification,
with and without 1,000 alloc overrides. Targets are Pebble/hash; cache and handle
allowances are 128 each. The two-block fixture has no holder preimages or manual
retention list, so it cannot represent the hot-read savings on long history.

| Operation | Workers | Temp DB | Baseline ms | Current ms | Time change | Baseline RSS MiB | Current RSS MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migration | 2 | disk | 885.6 | 870.9 | -1.7% | 80.9 | 81.4 |
| Migration | 2 | memory | 544.6 | 534.1 | -1.9% | 109.5 | 97.3 |
| Migration | 8 | disk | 729.6 | 720.7 | -1.2% | 80.3 | 81.2 |
| Migration | 8 | memory | 434.1 | 453.8 | +4.5% | 101.8 | 99.8 |
| MigrationAlloc | 2 | disk | 945.7 | 908.0 | -4.0% | 84.9 | 85.5 |
| MigrationAlloc | 2 | memory | 618.8 | 610.5 | -1.3% | 115.1 | 118.2 |
| MigrationAlloc | 8 | disk | 850.3 | 857.4 | +0.8% | 88.8 | 88.3 |
| MigrationAlloc | 8 | memory | 533.2 | 529.6 | -0.7% | 121.5 | 122.5 |
| Verification | 2 | disk | 802.4 | 797.1 | -0.7% | 89.3 | 89.4 |
| Verification | 2 | memory | 468.0 | 459.5 | -1.8% | 116.4 | 114.7 |
| Verification | 8 | disk | 668.5 | 667.6 | -0.1% | 92.8 | 89.7 |
| Verification | 8 | memory | 365.8 | 358.7 | -2.0% | 110.5 | 110.1 |
| VerificationAlloc | 2 | disk | 866.0 | 850.4 | -1.8% | 92.3 | 92.0 |
| VerificationAlloc | 2 | memory | 537.8 | 523.3 | -2.7% | 119.6 | 125.9 |
| VerificationAlloc | 8 | disk | 766.5 | 767.2 | +0.1% | 102.5 | 101.8 |
| VerificationAlloc | 8 | memory | 448.0 | 442.5 | -1.2% | 134.0 | 135.5 |

Median time changes range from −4.0% to +4.5%. The 8-worker/memory ordinary
migration regressed by 4.5%; small increases also appear in two alloc cases.
These regressions are retained. There is no consistent end-to-end speedup claim.
Median allocated bytes fell across the measured configurations (0.8%–13.4%), but
that is not equivalent to lower RSS or a proportional time improvement.

### Scale check: 100k holders

[100k raw log](raw/ovm-input-scale-2026-09-19.txt): 12 successful fresh-process
observations, three per operation/source, alternating order. This narrower check
uses 2 workers, disk scratch, no alloc, and the same 128 MiB/128-handle allowances.
It does not extrapolate to unmeasured 100k memory/8-worker/alloc configurations.

| Operation | Baseline ms | Current ms | Time change | Baseline RSS MiB | Current RSS MiB |
|---|---:|---:|---:|---:|---:|
| Migration | 8332.0 | 8476.4 | +1.7% | 299.0 | 294.8 |
| Verification | 8065.6 | 7836.9 | -2.8% | 350.8 | 372.4 |

Migration time regressed 1.7%; verification improved 2.8%. Median allocated bytes
fell about 4.2% in both cases. Verification RSS increased despite fewer allocated
bytes; RSS includes setup and garbage-collection timing effects. Three samples
with unflushed caches cannot establish a persistent timing trend or its cause.

All end-to-end operation timers exclude setup. RSS includes fixture setup and,
for verification, the initial disk-mode migration. Raw logs retain `setup-s`,
allocation counts, GC and 20 ms heap/file samples. Sampling can miss peaks,
memory-file lengths omit allocation capacity, and cache allowances are not RSS
limits. No production snapshot acceptance, long-history throughput, cold-disk,
LevelDB-target or path-target performance is established here.

### Reproduction

```bash
python3 scripts/benchmark-ovm-inputs.py --count 5 --out /absolute/new/input-components.txt
python3 scripts/benchmark-ovm.py --count 3 --holders 10000 --temp-dbs disk memory \
  --with-alloc --with-verify --baseline-root /absolute/isolated/baseline \
  --out /absolute/new/input-pairs.txt
```

For the 100k scale check, compile each source with `go test -c` and alternate the
following two benchmark selectors between baseline/current binaries in fresh
processes, three times each. Run from that source's `internal/migration` directory:

```bash
L2STATE_BENCH_HOLDERS=100000 L2STATE_BENCH_TEMP_DB=disk \
L2STATE_BENCH_PREIMAGES=0 L2STATE_BENCH_RETAIN_FIXTURE=0 /usr/bin/time -l \
  /absolute/variant.test -test.run='^$' \
  -test.bench='^BenchmarkOVMMigration$/^workers=2$' -test.benchtime=1x -test.count=1
# Repeat with ^BenchmarkOVMVerification$/^workers=2$.
```

## Historical review fixes and verification workspace — 2026-09-19

Raw evidence: [review-fix pairs](raw/ovm-review-fixes-2026-09-19.txt),
462 successful observations: 192 end-to-end and 270 temporary-database component
samples. Each variant/configuration has three fresh-process samples. Baseline is
an unchanged archive of `9eff764ee4e72c1cf7fc78d09126e8b9db981c23`.
`make ci` and aggregate `make test-race` passed before measurement; no CI or other
benchmark ran concurrently.

This source contains all six review fixes. These timings exercise OVM migration
and verification, including scratch selection, traversal decomposition and common
publication. Bundle-artifact preflight and ordinary-result accessors are covered
by correctness tests, not by these OVM timings. The verification harness now explicitly
selects the artifact parent as `TempDir`, matching the baseline's implicit scratch
location and preserving the existing disk sampler's coverage. It does not measure
moving scratch to a different device. Fixtures and measured operations are unchanged:
10k/100k holders, two-block history, no holder preimages or manual retain list,
Pebble/hash targets, 128 MiB cache allowance, 128 handles, 2/8 workers and both
scratch backends. Alloc changes 1,000 accounts as described above.

Source fingerprints:

- Baseline: `385a967cb254e781b93d381cf59e8f1510ae051bc0b2cc05fcc1b5ddfb9bec82`
- Current: `f45845978e3e5024d251f80e73cceda8bba716f3451965f98ad3a53ca8312c34`

### End-to-end medians

Times exclude setup; RSS includes setup and the initial disk migration for
verification. Positive time changes are observed regressions. Across the 32
configurations, median time changes range from **−7.0% to +7.6%**; this does not
establish a consistent speedup or regression-free performance.

| Operation | Workers | Holders | Temp DB | Baseline ms | Current ms | Time change | Baseline RSS MiB | Current RSS MiB |
|---|---:|---:|---|---:|---:|---:|---:|---:|
| Migration | 2 | 10000 | disk | 850.0 | 857.3 | +0.9% | 80.7 | 81.1 |
| Migration | 2 | 10000 | memory | 552.9 | 552.6 | -0.1% | 108.2 | 111.8 |
| Migration | 2 | 100000 | disk | 8854.4 | 8973.5 | +1.3% | 290.1 | 289.8 |
| Migration | 2 | 100000 | memory | 7787.9 | 7423.5 | -4.7% | 565.0 | 557.9 |
| Migration | 8 | 10000 | disk | 739.0 | 718.9 | -2.7% | 82.9 | 80.7 |
| Migration | 8 | 10000 | memory | 468.8 | 436.2 | -7.0% | 102.7 | 101.8 |
| Migration | 8 | 100000 | disk | 7586.7 | 7751.8 | +2.2% | 293.8 | 294.7 |
| Migration | 8 | 100000 | memory | 6394.5 | 6471.6 | +1.2% | 559.6 | 552.8 |
| MigrationAlloc | 2 | 10000 | disk | 928.4 | 914.4 | -1.5% | 86.6 | 86.4 |
| MigrationAlloc | 2 | 10000 | memory | 620.9 | 628.4 | +1.2% | 116.3 | 120.3 |
| MigrationAlloc | 2 | 100000 | disk | 10563.0 | 10585.9 | +0.2% | 310.7 | 311.5 |
| MigrationAlloc | 2 | 100000 | memory | 8710.7 | 8857.4 | +1.7% | 601.1 | 596.8 |
| MigrationAlloc | 8 | 10000 | disk | 827.2 | 839.2 | +1.4% | 90.8 | 92.4 |
| MigrationAlloc | 8 | 10000 | memory | 548.4 | 549.1 | +0.1% | 119.4 | 123.8 |
| MigrationAlloc | 8 | 100000 | disk | 9360.2 | 9316.0 | -0.5% | 294.0 | 306.5 |
| MigrationAlloc | 8 | 100000 | memory | 7563.8 | 7497.4 | -0.9% | 590.9 | 611.0 |
| Verification | 2 | 10000 | disk | 791.5 | 785.6 | -0.7% | 88.0 | 90.3 |
| Verification | 2 | 10000 | memory | 477.8 | 477.8 | +0.0% | 119.6 | 116.4 |
| Verification | 2 | 100000 | disk | 8271.9 | 8574.6 | +3.7% | 370.4 | 369.0 |
| Verification | 2 | 100000 | memory | 7150.5 | 7249.8 | +1.4% | 580.6 | 586.6 |
| Verification | 8 | 10000 | disk | 664.5 | 663.8 | -0.1% | 92.4 | 97.7 |
| Verification | 8 | 10000 | memory | 370.4 | 377.8 | +2.0% | 110.7 | 109.5 |
| Verification | 8 | 100000 | disk | 7505.5 | 8078.4 | +7.6% | 367.0 | 360.7 |
| Verification | 8 | 100000 | memory | 6162.9 | 6165.3 | +0.0% | 575.0 | 586.4 |
| VerificationAlloc | 2 | 10000 | disk | 846.6 | 860.0 | +1.6% | 93.2 | 92.0 |
| VerificationAlloc | 2 | 10000 | memory | 545.2 | 546.3 | +0.2% | 120.6 | 121.2 |
| VerificationAlloc | 2 | 100000 | disk | 10136.6 | 10044.7 | -0.9% | 369.0 | 367.7 |
| VerificationAlloc | 2 | 100000 | memory | 8489.4 | 8370.4 | -1.4% | 619.5 | 625.2 |
| VerificationAlloc | 8 | 10000 | disk | 751.8 | 767.4 | +2.1% | 104.8 | 102.5 |
| VerificationAlloc | 8 | 10000 | memory | 459.4 | 465.7 | +1.4% | 131.0 | 135.9 |
| VerificationAlloc | 8 | 100000 | disk | 9721.8 | 9072.4 | -6.7% | 377.0 | 374.5 |
| VerificationAlloc | 8 | 100000 | memory | 7331.5 | 7158.7 | -2.4% | 604.9 | 616.1 |

The largest observed end-to-end regression is ordinary verification with 100k
holders, 8 workers and disk scratch (+7.6%). Baseline operation samples were
7844.7, 7505.5 and 7008.5 ms; current samples were 8078.4, 8078.5 and 7020.2 ms.
Three samples and unflushed OS caches do not identify the cause or establish a
persistent production regression; the measured increase is retained rather than
normalized away. The 100k/2-worker/disk ordinary-verification median also increased
3.7%. No throughput improvement is claimed for this correctness/refactoring change.

Current end-to-end setup spans 0.138–11.77 s. Sampled heap peaks span 22.8–387.3 MiB,
memory-file lengths 0–120.0 MiB, and scratch disk lengths 0–141.3 MiB across the
configurations. These are ranges of individual observations, not pooled medians.
The 20 ms sampler can miss peaks; file lengths omit allocation capacity and
heap/RSS measure different things. Raw logs retain every configuration's setup,
allocation, GC and file measurements. Cache allowances are not RSS limits.

### Component medians

These components use 10k/100k/1m records and the existing disk Pebble, memory Pebble
and benchmark-only geth memorydb traces. Adapter implementations were unchanged.
Positive changes are retained, including geth memorydb's 1m-record prefix case
(+14.1%); they do not establish end-to-end migration throughput or attribute a
change to a particular refactor. Components are not added to end-to-end times.

| Operation | Records | Backend | Baseline ms | Current ms | Time change |
|---|---:|---|---:|---:|---:|
| batch | 10000 | disk | 2.712 | 2.797 | +3.1% |
| batch | 10000 | geth | 0.407 | 0.412 | +1.4% |
| batch | 10000 | memory | 2.704 | 2.576 | -4.7% |
| batch | 100000 | disk | 63.113 | 62.899 | -0.3% |
| batch | 100000 | geth | 4.841 | 5.514 | +13.9% |
| batch | 100000 | memory | 25.793 | 25.766 | -0.1% |
| batch | 1000000 | disk | 1550.264 | 1588.049 | +2.4% |
| batch | 1000000 | geth | 107.244 | 110.693 | +3.2% |
| batch | 1000000 | memory | 277.109 | 271.112 | -2.2% |
| dedup | 10000 | disk | 2.548 | 2.782 | +9.2% |
| dedup | 10000 | geth | 0.378 | 0.393 | +4.0% |
| dedup | 10000 | memory | 2.478 | 2.197 | -11.3% |
| dedup | 100000 | disk | 54.425 | 56.637 | +4.1% |
| dedup | 100000 | geth | 3.567 | 3.570 | +0.1% |
| dedup | 100000 | memory | 27.078 | 27.154 | +0.3% |
| dedup | 1000000 | disk | 866.260 | 920.257 | +6.2% |
| dedup | 1000000 | geth | 94.136 | 86.996 | -7.6% |
| dedup | 1000000 | memory | 299.144 | 287.746 | -3.8% |
| get | 10000 | disk | 8.351 | 8.752 | +4.8% |
| get | 10000 | geth | 0.313 | 0.349 | +11.4% |
| get | 10000 | memory | 7.189 | 6.537 | -9.1% |
| get | 100000 | disk | 122.800 | 124.114 | +1.1% |
| get | 100000 | geth | 6.061 | 6.089 | +0.5% |
| get | 100000 | memory | 113.797 | 114.592 | +0.7% |
| get | 1000000 | disk | 3729.091 | 3652.781 | -2.0% |
| get | 1000000 | geth | 169.422 | 176.460 | +4.2% |
| get | 1000000 | memory | 2925.904 | 2949.946 | +0.8% |
| prefix | 10000 | disk | 2.053 | 2.284 | +11.2% |
| prefix | 10000 | geth | 109.877 | 114.216 | +3.9% |
| prefix | 10000 | memory | 1.894 | 1.722 | -9.1% |
| prefix | 100000 | disk | 9.049 | 9.233 | +2.0% |
| prefix | 100000 | geth | 913.927 | 907.677 | -0.7% |
| prefix | 100000 | memory | 9.284 | 9.555 | +2.9% |
| prefix | 1000000 | disk | 126.415 | 138.417 | +9.5% |
| prefix | 1000000 | geth | 19376.077 | 22109.029 | +14.1% |
| prefix | 1000000 | memory | 100.594 | 100.467 | -0.1% |
| scan | 10000 | disk | 0.527 | 0.576 | +9.2% |
| scan | 10000 | geth | 1.050 | 1.033 | -1.6% |
| scan | 10000 | memory | 0.497 | 0.459 | -7.5% |
| scan | 100000 | disk | 6.848 | 6.678 | -2.5% |
| scan | 100000 | geth | 12.898 | 13.326 | +3.3% |
| scan | 100000 | memory | 7.098 | 6.754 | -4.9% |
| scan | 1000000 | disk | 122.851 | 114.885 | -6.5% |
| scan | 1000000 | geth | 220.894 | 214.491 | -2.9% |
| scan | 1000000 | memory | 108.344 | 105.968 | -2.2% |

Reproduction (use a new output and isolated baseline; do not run alongside CI):

```bash
python3 scripts/benchmark-ovm.py --holders 10000 100000 --temp-dbs disk memory \
  --with-alloc --with-verify --with-components --count 3 \
  --baseline-root /absolute/isolated/baseline --out /absolute/new/review-fixes.txt
```

These synthetic measurements do not prove production-snapshot acceptance,
long-history behavior, cold-disk throughput, or LevelDB/path-scheme performance.

## Historical parallel evidence collection

Raw evidence: [preimage/history concurrency pairs](raw/ovm-evidence-parallel-2026-09-18.txt),
48 successful fresh-process observations on 2026-09-18, three per configuration.
`make ci` and `make test-race` passed before measurement; no tests or other
benchmarks ran concurrently. An earlier interrupted attempt was excluded entirely.

The baseline is `67db0b4` with only the identical preimage benchmark harness and
its `testing.TB` fixture helper adjustment copied in. It retains serial preimage
then history collection. Current runs collect both concurrently with separate
batches, and preimage scanning yields its shared worker lease between records.
Both variants have 10,000 holder address preimages, the same witness, and the
same two-block history. Alloc and manual retention are disabled. This compares
the complete scheduling change, including per-record limiter overhead.

Source fingerprints:

- Baseline: `14fd1d6723f40f0d7609e6e7c9bccb61ea9d18bc8cb12c524f85d3a44b2ae8b2`
- Current: `385a967cb254e781b93d381cf59e8f1510ae051bc0b2cc05fcc1b5ddfb9bec82`

| Operation | Workers | Temp DB | Baseline ms | Parallel ms | Time change | Baseline RSS MiB | Parallel RSS MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migration | 2 | disk | 866.1 | 873.8 | +0.9% | 92.6 | 92.1 |
| Migration | 2 | memory | 560.2 | 566.1 | +1.1% | 124.5 | 123.2 |
| Migration | 8 | disk | 736.7 | 722.8 | -1.9% | 98.2 | 94.7 |
| Migration | 8 | memory | 457.3 | 468.0 | +2.3% | 118.1 | 116.2 |
| Verification | 2 | disk | 776.8 | 813.6 | +4.7% | 112.3 | 115.9 |
| Verification | 2 | memory | 477.6 | 493.2 | +3.3% | 152.8 | 142.0 |
| Verification | 8 | disk | 641.1 | 655.6 | +2.3% | 120.8 | 123.4 |
| Verification | 8 | memory | 380.3 | 393.4 | +3.4% | 138.5 | 147.4 |

Positive changes are measured regressions. This fixture shows no consistent
end-to-end speedup; its two-block history offers little work to overlap with
preimage scanning. Three short samples do not establish a persistent performance
trend or production acceptance. In particular, they do not measure long-history,
cold-disk or source-I/O contention behavior. RSS includes setup; heap/file peaks
are sampled and may miss peaks, as described above. Raw logs also retain setup,
allocation and scratch-storage measurements.

Reproduce with the same harness in an isolated baseline checkout:

```bash
python3 scripts/benchmark-ovm.py --holders 10000 --temp-dbs disk memory \
  --with-preimages --with-verify --count 3 \
  --baseline-root /absolute/isolated/baseline --out /absolute/new/evidence.txt
```

## Historical manual ERC20 retention comparison

Raw evidence: [manual retention pairs](raw/ovm-retention-2026-09-16.txt), 96 fresh-process observations, three per configuration. This source passed `make ci`, `make test-race` and `git diff --check` before measurement; no CI or other benchmark ran concurrently.

The measured fingerprint is `93558a23a849d24ca277cdf1853174e9e8b17b459265238ed0df5e04d876f758`.
After measurement, a separate working-tree edit changed `BenchmarkOVMAncientRead`
from `b.N` to `b.Loop`. That unmeasured benchmark is outside this dataset. An
in-memory comparison accounting for only that edit reproduces the recorded
fingerprint exactly; the retention implementation and measured cases are unchanged.
Do not treat these logs as measurements of that later source revision.

Both absent/present-list cases use the same modified 10k-holder source fixture: the first 1000 existing holder accounts have code. Retain cases list those contracts; no-list cases use only nonzero Transfer-from history. The list therefore changes the retained balances and final trie inventory as well as adding input/eligibility checks. These are whole-feature costs, not an isolated lookup benchmark or an old/new revision speedup. No samples are pooled with the historical fixtures below.

Reproduce with `python3 scripts/benchmark-ovm.py --holders 10000 --temp-dbs disk memory --with-retain-list --with-alloc --with-verify --count 3 --out /absolute/new/file`.

| Operation | Workers | Temp DB | No list ms | With list ms | Time change | No list RSS MiB | With list RSS MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 873.4 | 865.3 | -0.9% | 82.7 | 81.6 |
| Migrate | 2 | memory | 541.2 | 565.5 | +4.5% | 100.2 | 100.9 |
| Migrate | 8 | disk | 735.2 | 773.9 | +5.3% | 80.7 | 82.6 |
| Migrate | 8 | memory | 436.6 | 481.2 | +10.2% | 105.0 | 100.1 |
| Migrate + alloc | 2 | disk | 951.5 | 961.4 | +1.0% | 86.8 | 86.1 |
| Migrate + alloc | 2 | memory | 652.4 | 666.2 | +2.1% | 116.4 | 118.0 |
| Migrate + alloc | 8 | disk | 831.2 | 847.7 | +2.0% | 89.6 | 89.9 |
| Migrate + alloc | 8 | memory | 546.0 | 557.5 | +2.1% | 120.1 | 122.3 |
| Verify | 2 | disk | 790.5 | 817.5 | +3.4% | 88.5 | 92.3 |
| Verify | 2 | memory | 482.0 | 520.3 | +7.9% | 106.5 | 115.0 |
| Verify | 8 | disk | 675.8 | 698.6 | +3.4% | 91.3 | 93.2 |
| Verify | 8 | memory | 360.9 | 379.6 | +5.2% | 110.2 | 115.2 |
| Verify + alloc | 2 | disk | 903.5 | 897.9 | -0.6% | 95.6 | 94.6 |
| Verify + alloc | 2 | memory | 534.8 | 546.7 | +2.2% | 126.0 | 120.4 |
| Verify + alloc | 8 | disk | 746.1 | 773.5 | +3.7% | 101.5 | 103.9 |
| Verify + alloc | 8 | memory | 473.2 | 502.0 | +6.1% | 132.2 | 131.0 |

Median elapsed changes range from -0.9% to +10.2%; positive values are measured regressions in the list-enabled configuration. Three short samples do not establish a persistent trend. In particular, this two-block, hash/Pebble fixture does not establish production throughput, long-history costs, or LevelDB/path performance.

### List-enabled setup and resource detail

RSS includes setup; the following operation samples use the same limitations described above.

| Operation | Workers | Temp DB | Setup ms | Allocated MiB/op | Heap MiB | Temp disk MiB | Memory files MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 146.8 | 165.3 | 25.3 | 13.0 | 0.0 |
| Migrate | 2 | memory | 150.1 | 237.7 | 41.6 | 0.0 | 7.9 |
| Migrate | 8 | disk | 142.9 | 163.3 | 27.3 | 8.0 | 0.0 |
| Migrate | 8 | memory | 154.7 | 236.7 | 39.0 | 0.0 | 7.9 |
| Migrate + alloc | 2 | disk | 151.3 | 202.4 | 25.8 | 13.1 | 0.0 |
| Migrate + alloc | 2 | memory | 155.0 | 322.2 | 48.7 | 0.0 | 13.3 |
| Migrate + alloc | 8 | disk | 157.3 | 208.8 | 27.8 | 16.7 | 0.0 |
| Migrate + alloc | 8 | memory | 152.1 | 326.3 | 48.6 | 0.0 | 12.3 |
| Verify | 2 | disk | 1078.0 | 159.1 | 25.0 | 12.1 | 0.0 |
| Verify | 2 | memory | 1066.0 | 231.8 | 36.4 | 0.0 | 7.9 |
| Verify | 8 | disk | 942.5 | 157.4 | 27.0 | 8.0 | 0.0 |
| Verify | 8 | memory | 905.0 | 229.7 | 37.6 | 0.0 | 7.9 |
| Verify + alloc | 2 | disk | 1075.0 | 195.0 | 25.3 | 13.2 | 0.0 |
| Verify + alloc | 2 | memory | 1104.0 | 317.6 | 47.9 | 0.0 | 11.3 |
| Verify + alloc | 8 | disk | 1007.0 | 199.4 | 27.7 | 16.5 | 0.0 |
| Verify + alloc | 8 | memory | 1017.0 | 319.5 | 50.2 | 0.0 | 12.3 |

## Historical 10k-holder nonzero-Transfer comparison

Raw evidence: [zero-transfer pairs](raw/ovm-zero-transfer-2026-09-16.txt), 96 samples.
The baseline is isolated `c3c52d5` production code. Its test fixture was changed to use amount 1 for the qualifying contract Transfer, matching the final fixture. Both sides therefore use the same input and expected conversion result. This measures the added eligibility check, not the benefit of filtering a zero-heavy history.
The final source passed `make ci`, `make test-race` and `git diff --check`; zero-only eligibility is tested separately against the independent state reference and standalone replay.

| Operation | Workers | Temp DB | Baseline ms | Final ms | Time change | Baseline RSS MiB | Final RSS MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 849.9 | 856.2 | +0.7% | 81.4 | 81.9 |
| Migrate | 2 | memory | 536.7 | 537.0 | +0.1% | 109.9 | 111.1 |
| Migrate | 8 | disk | 737.8 | 709.4 | -3.8% | 82.6 | 80.5 |
| Migrate | 8 | memory | 435.5 | 452.4 | +3.9% | 101.1 | 104.8 |
| Migrate + alloc | 2 | disk | 921.1 | 923.9 | +0.3% | 85.8 | 87.1 |
| Migrate + alloc | 2 | memory | 614.3 | 617.5 | +0.5% | 114.3 | 115.3 |
| Migrate + alloc | 8 | disk | 843.5 | 850.0 | +0.8% | 88.1 | 89.3 |
| Migrate + alloc | 8 | memory | 525.0 | 522.2 | -0.5% | 120.0 | 120.0 |
| Verify | 2 | disk | 789.5 | 795.4 | +0.7% | 90.0 | 88.6 |
| Verify | 2 | memory | 466.9 | 465.9 | -0.2% | 117.6 | 118.3 |
| Verify | 8 | disk | 649.7 | 649.4 | -0.0% | 94.3 | 89.1 |
| Verify | 8 | memory | 356.3 | 359.7 | +1.0% | 111.8 | 111.1 |
| Verify + alloc | 2 | disk | 843.8 | 882.0 | +4.5% | 90.7 | 92.9 |
| Verify + alloc | 2 | memory | 536.5 | 534.6 | -0.4% | 120.7 | 119.6 |
| Verify + alloc | 8 | disk | 743.4 | 782.0 | +5.2% | 101.7 | 101.4 |
| Verify + alloc | 8 | memory | 447.7 | 446.5 | -0.3% | 134.5 | 134.9 |

Median time changes range from -3.8% to +5.2%; RSS changes range from -5.5% to +3.7%. Positive changes are measured regressions. In particular, disk verification with alloc increased 4.5%/5.2% at 2/8 workers. Three short samples do not establish a persistent trend.

### Historical nonzero-Transfer setup and resource detail

Final-source medians only. Allocated bytes are cumulative per operation; heap/files are sampled peaks. Baseline values and individual samples remain in the raw log.

| Operation | Workers | Temp DB | Setup ms | Allocated MiB/op | Heap MiB | Temp disk MiB | Memory files MiB |
|---|---:|---|---:|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 140.4 | 159.4 | 26.3 | 13.0 | 0.0 |
| Migrate | 2 | memory | 140.1 | 259.5 | 45.1 | 0.0 | 9.0 |
| Migrate | 8 | disk | 142.1 | 158.7 | 25.5 | 7.9 | 0.0 |
| Migrate | 8 | memory | 138.2 | 230.6 | 37.6 | 0.0 | 7.9 |
| Migrate + alloc | 2 | disk | 143.3 | 196.1 | 25.4 | 13.4 | 0.0 |
| Migrate + alloc | 2 | memory | 140.4 | 308.8 | 45.7 | 0.0 | 11.4 |
| Migrate + alloc | 8 | disk | 140.7 | 201.8 | 26.9 | 16.5 | 0.0 |
| Migrate + alloc | 8 | memory | 139.5 | 319.3 | 52.8 | 0.0 | 11.9 |
| Verify | 2 | disk | 999.9 | 153.5 | 25.0 | 13.0 | 0.0 |
| Verify | 2 | memory | 1003.0 | 252.7 | 48.4 | 0.0 | 9.0 |
| Verify | 8 | disk | 841.1 | 152.7 | 25.9 | 7.9 | 0.0 |
| Verify | 8 | memory | 884.1 | 224.7 | 38.1 | 0.0 | 7.9 |
| Verify + alloc | 2 | disk | 1068.0 | 189.4 | 24.6 | 13.7 | 0.0 |
| Verify + alloc | 2 | memory | 1094.0 | 308.7 | 50.6 | 0.0 | 11.4 |
| Verify + alloc | 8 | disk | 972.4 | 193.4 | 30.3 | 16.2 | 0.0 |
| Verify + alloc | 8 | memory | 970.1 | 313.1 | 38.0 | 0.0 | 11.9 |

### Alloc overhead within the latest source

These compare alloc/no-alloc modes of the same final binary, not old/new code.

| Operation | Workers | Temp DB | Without alloc ms | With alloc ms | Added time |
|---|---:|---|---:|---:|---:|
| Migrate | 2 | disk | 856.2 | 923.9 | +7.9% |
| Migrate | 2 | memory | 537.0 | 617.5 | +15.0% |
| Migrate | 8 | disk | 709.4 | 850.0 | +19.8% |
| Migrate | 8 | memory | 452.4 | 522.2 | +15.4% |
| Verify | 2 | disk | 795.4 | 882.0 | +10.9% |
| Verify | 2 | memory | 465.9 | 534.6 | +14.7% |
| Verify | 8 | disk | 649.4 | 782.0 | +20.4% |
| Verify | 8 | memory | 359.7 | 446.5 | +24.1% |

## Historical 100k-holder and component measurements

Raw evidence: [temporary database modes](raw/temp-db-2026-09-16.txt), 231 samples (96 end-to-end and 135 component). This source predates the nonzero-Transfer rule. It compares disk/memory modes of one implementation, not two code revisions. Keep its 100k and component evidence separate from the latest 10k run; redundant older 10k tables are omitted here but their samples remain intact.

### 100k holders

| Operation | Workers | Disk ms | Memory ms | Time change | Disk RSS MiB | Memory RSS MiB |
|---|---:|---:|---:|---:|---:|---:|
| Migrate | 2 | 8724.5 | 7408.7 | -15.1% | 281.3 | 555.4 |
| Migrate | 8 | 7837.2 | 6523.5 | -16.8% | 290.5 | 554.5 |
| Migrate + alloc | 2 | 10449.7 | 8561.0 | -18.1% | 310.3 | 597.5 |
| Migrate + alloc | 8 | 9325.3 | 7593.7 | -18.6% | 302.6 | 608.0 |
| Verify | 2 | 8534.7 | 6969.5 | -18.3% | 372.8 | 585.7 |
| Verify | 8 | 7692.8 | 6144.7 | -20.1% | 371.6 | 597.8 |
| Verify + alloc | 2 | 10047.6 | 8916.3 | -11.3% | 376.6 | 619.7 |
| Verify + alloc | 8 | 9302.5 | 7344.0 | -21.1% | 374.4 | 611.4 |

Memory-mode RSS medians were 554–620 MiB versus 281–377 MiB for disk. Every end-to-end memory-mode sample reported zero physical temporary database bytes. Memory mode remains opt-in, with no hard RAM cap or spill; the final artifact still needs disk space and sync.

### One-million-record components

Components use deterministic high-entropy 32-byte keys/values (nil node-marker values) in 1,000 prefixes. Preparation is outside the timer; input arrays remain live. Each row is a whole trace: batched overwrites, repeated markers, permuted point reads, a full scan, or 1,000 prefix scans. One foreground caller drives each trace; Pebble background work is enabled. Disk/memory Pebble use identical adapter settings. Geth memorydb is a component reference, not a supported CLI backend.

| Trace | Disk Pebble ms | Memory Pebble ms | Geth memorydb ms | Disk RSS MiB | Memory RSS MiB | Geth RSS MiB |
|---|---:|---:|---:|---:|---:|---:|
| batch | 1562.3 | 263.1 | 102.3 | 169.5 | 615.5 | 352.0 |
| dedup | 949.7 | 285.8 | 91.3 | 151.8 | 410.6 | 283.8 |
| get | 3734.0 | 3082.8 | 184.0 | 215.3 | 459.9 | 324.0 |
| scan | 129.0 | 111.5 | 220.5 | 160.0 | 498.0 | 330.2 |
| prefix | 127.9 | 106.3 | 22740.8 | 165.2 | 474.5 | 598.9 |

Geth memorydb used less RSS and time for several point-read/write traces, but its repeated full-map scan/sort made the prefix trace much slower. This is not evidence that Pebble always uses less RAM.

Preserved small-trace regressions for memory Pebble versus disk Pebble:

- 10,000 records, `batch`: 2.454 → 2.575 ms (+5.0%).
- 100,000 records, `scan`: 6.273 → 6.881 ms (+9.7%).
- 100,000 records, `prefix`: 8.816 → 9.967 ms (+13.1%).

## Historical ancient-reader optimization

Raw evidence: [optimization baseline/final pairs](raw/ovm-optimization-2026-09-16.txt), 54 samples. Baseline production code is `8bd08d4`; only `ovm_benchmark_test.go` and `ovm_optimization_benchmark_test.go` were copied from the measured final checkout to share the harness. This predates temporary-memory support and the nonzero-Transfer rule.
The raw log retains all original end-to-end before/after observations, including RSS regressions. Those older end-to-end tables are superseded by the latest report above; only the distinct ancient-reader comparison is summarized here.

The ancient microbenchmark reads 60,000 synthetic 32-byte records across three tables and four files/table, with two compressed tables. It isolates cached small-record descriptor reuse, not receipt decoding, commitment validation or a complete history scan.

| Measurement | Baseline | Optimized | Change |
|---|---:|---:|---:|
| Time ms | 890.56 | 45.67 | -94.87% |
| Allocated MiB/op | 66.45 | 2.47 | -96.29% |
| Process RSS MiB | 28.42 | 27.12 | -4.56% |

These historical changes combined bounded account-reader reuse, raw-byte input confirmation, ancient descriptor reuse, validation-only verification replay and index/write-path optimizations. They do not attribute the end-to-end gains to individual changes or establish a comparable production history speedup.
