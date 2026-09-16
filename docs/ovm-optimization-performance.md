# OVM migration and verification optimizations — 2026-09-16

## Implementation and preserved checks

The comparison baseline is `8bd08d4` (GenesisAlloc support). Six changes preserve
report/bundle v1, the pinned geth version, consensus conversion rules, codehash
classification at the original head, and all four target engine/scheme layouts:

1. Account lookups reuse exclusive trie readers for at most 32 reads. Balance
   classification borrows readers only under the global worker lease and retains
   at most the normalized worker count; alloc uses one reader. Dropping each
   reader's trie at the window boundary bounds decoded nodes and prevalue traces.
2. Input confirmation hashes raw bytes in 32 KiB chunks. It does not parse the
   already-validated witness again or rewrite evidence keys. All supplied runtime,
   witness and alloc files are checked again immediately before publication or
   successful standalone verification. This also fixes acceptance of runtime/
   witness changes after replay and during actual artifact verification.
3. Ancient scans retain one current read-only data descriptor per table (three
   total), rotating when the file number changes. Rotation and final close check
   identity, regular-file type, size and modification time. Truncation, changed
   or replaced files, and close errors still fail the operation.
4. Standalone verification replays the expected root/counts with the existing
   validation-only partitioned output. It does not create, sync or verify a second
   final artifact. It still independently reopens/verifies original migrated
   state and fully validates the actual artifact and its inventory.
5. Hash writers skip slim-account encoding before returning from the flat-state
   no-op; path writers retain the same encoding.
6. The Pebble evidence index uses one Get per lookup and recognizes only Pebble's
   not-found error as absence. EOAs do not query Transfer-from membership; contracts
   still do and propagate lookup errors.

The new regressions cover bounded-reader reuse against fresh reads, mutation of
returned account values, absent accounts, EOA/contract query behavior, index I/O
errors, zero-allocation hash account writes, 1,200 holders and 128 alloc overrides
across all four target combinations against an independent StateDB reference,
late runtime/witness mutation at publication and actual verification, raw-byte
hashing/read errors/cancellation, validation-only replay evidence and cleanup,
and ancient handle reuse/rotation, file mutation/replacement and physical
immutability. Existing state/layout, corruption, history overlap, runtime,
continuation, legacy canary and geth compatibility gates remain in place.

## Reproduction and measurement limits

Run CI/race to completion before measurements. Use an isolated checkout of the
baseline and copy only these benchmark harness files from the final checkout:

- `internal/migration/ovm_benchmark_test.go`
- `internal/migration/ovm_optimization_benchmark_test.go`

No production code, old canary or compatibility corpus is changed in the baseline.
Then run from the final checkout (OUT must be a new path):

```bash
python3 scripts/benchmark-ovm.py \
  --out /absolute/new/ovm-optimization-pairs.txt --count 3 \
  --with-alloc --with-verify --with-ancient \
  --baseline-root /absolute/isolated/baseline
```

The script alternates baseline/current order, workers and alloc modes. Every
sample uses a fresh process. End-to-end migration/verification uses the existing
synthetic 10,000-holder, two-block fixture on Pebble/hash, with 128 MiB cache and
128 handles. Alloc overrides 1,000 holders with code, native balance and one slot.
Standalone verification timing excludes the initial fixture migration. The ancient
microbenchmark reads 60,000 synthetic 32-byte records across three tables and four
files per table, including two compressed tables; it is not a full history scan.

Allocated bytes are cumulative Go allocations, not RSS. RSS includes fixture setup
and, for verification, the initial migration. The migration-only disk metric is
sampled every 20 ms, can miss short peaks and excludes source/input files. OS caches
are not flushed. Three samples, tiny history and synthetic records do not establish
production throughput, worst-case memory/disk usage, or LevelDB/path performance.

## Validation and results

Validation on the final Go source passed:

- `make ci`: formatting/tidiness, lint (0 issues), root tests including frozen
  geth compatibility, both legacy modules' checks, and CLI build.
- `make test-race`: fresh root migration/CLI race results and legacy-prune tests
  with the race-enabled CLI.
- `git diff --check`.

The previous late-input acceptance is now rejected by permanent tests for both
runtime and witness changes, at both publication and actual artifact verification.
No golden canary or compatibility corpus was regenerated.

Measurements ran after CI/race finished on Apple M3 Max, macOS 27.0 arm64,
Go 1.27.1, 16 reported CPUs. Each cell is the median of three independent fresh
processes. [Raw samples and source SHA-256 values](ovm-optimization-benchmark-2026-09-16.txt)
record all 54 runs. The baseline is unmodified production code at `8bd08d4` with
only the two shared benchmark harness files copied in.

| Operation | Workers | Baseline ms | Current ms | Time change | Baseline MiB/op | Current MiB/op | Allocation change |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Migrate | 2 | 938.80 | 824.52 | -12.17% | 208.62 | 158.98 | -23.79% |
| Migrate | 8 | 767.30 | 759.35 | -1.04% | 208.01 | 158.89 | -23.61% |
| Migrate + alloc | 2 | 986.15 | 911.90 | -7.53% | 249.76 | 195.99 | -21.53% |
| Migrate + alloc | 8 | 900.29 | 855.50 | -4.98% | 255.62 | 202.27 | -20.87% |
| Verify | 2 | 973.45 | 792.56 | -18.58% | 219.10 | 152.31 | -30.48% |
| Verify | 8 | 817.86 | 671.80 | -17.86% | 218.23 | 150.47 | -31.05% |
| Verify + alloc | 2 | 1066.89 | 852.31 | -20.11% | 264.18 | 187.83 | -28.90% |
| Verify + alloc | 8 | 957.38 | 767.95 | -19.79% | 270.50 | 192.58 | -28.81% |

The 8-worker no-alloc migration time ranges overlap (baseline 763.32–788.72 ms,
current 746.48–780.04 ms), so its 1.04% median difference does not establish a
throughput improvement. The reduction in cumulative allocations is much clearer.
These are combined-change measurements, not attribution of a speedup to each
individual optimization.

| Operation | Workers | Baseline peak RSS MiB | Current peak RSS MiB |
| --- | ---: | ---: | ---: |
| Migrate | 2 | 82.30 | 80.28 |
| Migrate | 8 | 80.94 | 79.44 |
| Migrate + alloc | 2 | 86.27 | 85.92 |
| Migrate + alloc | 8 | 92.06 | 89.95 |
| Verify | 2 | 90.06 | 88.53 |
| Verify | 8 | 90.14 | 93.03 |
| Verify + alloc | 2 | 92.22 | 93.66 |
| Verify + alloc | 8 | 102.66 | 103.52 |

Peak RSS increased by 2.89 MiB (+3.21%) for 8-worker verification without alloc,
1.44 MiB (+1.56%) for 2-worker verification with alloc, and 0.86 MiB (+0.84%) for
8-worker verification with alloc. These regressions are retained in the report:
lower cumulative allocations do not guarantee lower peak RSS, and verification
RSS includes its setup migration.

Migration sampled disk peaks changed from 14.085/10.352 MiB to 13.942/9.941 MiB
without alloc (2/8 workers), and from 13.878/16.634 MiB to 13.981/16.759 MiB with
alloc. The alloc increases are about 0.10/0.13 MiB (+0.74%/+0.75%). The sampler does
not measure verification scratch peaks; avoiding a second final artifact is
verified structurally, without claiming a measured production disk reduction.

The separate 60,000-record ancient microbenchmark changed from **890.56 ms to
45.67 ms** (-94.87%), with allocations from **66.45 to 2.47 MiB/op** (-96.29%) and
peak RSS from 28.42 to 27.13 MiB. This demonstrates the cost of repeated descriptor
opening on small cached records. Real history includes larger records, receipt
RLP decoding, commitment validation, event indexing and storage latency, so this
is not a claim of a comparable end-to-end history speedup.
