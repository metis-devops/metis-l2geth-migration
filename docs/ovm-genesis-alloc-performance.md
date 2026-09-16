# OVM GenesisAlloc validation and measurements — 2026-09-16

## Correctness and baseline

The overlay is applied after the original-state/history validation and OVM
conversion. Tests compare all four engine/scheme combinations with an independent
StateDB conversion/overlay followed by the pinned GenerateTrie layout builder,
including every logical target record. They also exercise runtime execution and
a subsequent state commit/read, sparse fields, zero/deletion semantics, 10,000
storage slots, bounded duplicate tracking, malformed inputs, source immutability,
input/report/target tampering, cancellation during storage merging, cleanup and
input changes immediately before publication or during actual target verification.

The initial repository HEAD, `156f5853f35483f8ffe63273d2169c215997f2b1`, had an
inverted contract test in `ovmBalanceBatch.inspect`: equality with EmptyCodeHash
classified an EOA as a contract. Existing four-target reference tests failed with
converted/retained amounts 220/88 rather than 209/99. This change fixes that
predicate to inequality and adds a focused original-head classification test.
The performance baseline below is that HEAD **with only this predicate fix**,
in a separate temporary checkout, so incorrect balance classification is not
used as the feature's performance baseline. No canary or compatibility corpus was
regenerated.

Validation completed on the final Go source:

- `make ci`: formatting/tidiness, lint (0 issues), all root tests including geth
  compatibility, fixture and legacy-prune module verification/test/vet, and build.
- `make test-race`: all root tests and legacy-prune tests with a race-enabled CLI.
- `git diff --check`.

## Method

- Apple M3 Max, darwin/arm64, Go 1.27.1, geth v1.17.5; 16 reported CPUs.
- Synthetic 10,000-holder state, two-block history; Pebble/hash only.
- Cache allowance 128 MiB, handles 128; workers 2 and 8.
- Three samples per configuration; one migration per fresh process. Workers and
  alloc/no-alloc order alternate. No CI or other benchmark runs concurrently.
- The alloc fixture overrides the first 1,000 holders with the same five-byte
  runtime, balance 10, and storage slot 1 = 1; omitted nonces remain unchanged.
- Fixture construction is outside ns/op, but process RSS includes construction.
  OS caches are not flushed. Disk use includes private scratch and target files,
  sampled every 20 ms; it can miss short peaks and excludes source/input files.
- The corrected baseline was measured earlier in a separate run. Its before/after
  comparisons are descriptive, not statistically controlled speedup claims.

Commands (each OUT must be a new path):

```bash
# In the isolated baseline checkout containing only the classification fix:
python3 scripts/benchmark-ovm.py --out /absolute/new/baseline.txt --count 3

# In this checkout, after CI/race have finished:
python3 scripts/benchmark-ovm.py --out /absolute/new/alloc-pairs.txt --count 3 --with-alloc
```

Raw outputs, including source SHA-256, individual samples and process statistics:

- [Corrected baseline](ovm-genesis-alloc-baseline-2026-09-16.txt)
- [Final alloc/no-alloc pairs](ovm-genesis-alloc-benchmark-2026-09-16.txt)

## Initial results (before the whitespace-reader fix)

Each cell is the median of three measurements. Allocated bytes are cumulative Go
allocations per operation, not resident memory. Database cache allowances are not
a total RSS cap.

| Workers | Configuration | Time (ms) | Allocated (MiB/op) | Peak RSS (MiB) | Sampled disk peak (MiB) |
| --- | --- | ---: | ---: | ---: | ---: |
| 2 | Corrected baseline | 946.77 | 208.35 | 80.58 | 14.13 |
| 2 | Final, no alloc | 940.01 | 208.67 | 82.11 | 14.22 |
| 2 | Final, 1,000 overrides | 982.38 | 249.64 | 85.20 | 14.00 |
| 8 | Corrected baseline | 815.49 | 207.64 | 83.77 | 11.54 |
| 8 | Final, no alloc | 800.72 | 207.01 | 81.00 | 11.63 |
| 8 | Final, 1,000 overrides | 894.65 | 255.00 | 92.41 | 16.56 |

No-alloc median time changed by -0.7% (2 workers) and -1.8% (8 workers), with
overlapping sample ranges; these measurements show no no-alloc time regression
but do not establish a speedup. No-alloc RSS increased by 1.53 MiB at 2 workers
and decreased by 2.77 MiB at 8 workers; sampled disk rose by about 0.09 MiB in
both configurations.

Enabling the overlay increased median time by **4.5%** (2 workers) and **11.7%**
(8 workers) relative to the same final code without alloc. Cumulative allocations
rose by 40.97/47.99 MiB, and peak RSS by 3.09/11.41 MiB. At 8 workers the sampled
disk peak increased by 4.93 MiB (42.4%); the slightly lower sampled 2-worker disk
peak is not evidence of lower required disk capacity.

These costs include parsing/indexing and digest rechecks, validation of the
converted state for its separate evidence, rebuilding the overlay tries, and
generating/verifying the final artifact. They are expected additional work when
the flag is enabled. Three small synthetic samples do not predict production
snapshot throughput, long-history cost, worst-case alloc size, LevelDB/path
performance, or a maximum process memory/disk requirement.

## Whitespace-reader fix and follow-up measurements

Review reproduced an unbounded JSON decoder buffer for long runs of whitespace:
`{` followed by 16 MiB of spaces and `}` grew the Go 1.27.1 decoder buffer to
32 MiB. The reader now folds each whitespace run outside strings into one
separator before decoding. The same diagnostic reports a 64-byte decoder buffer
after the fix; this describes that particular input, not the buffer needed for
large code strings or the process RSS.

Permanent regression tests generate whitespace without allocating input-sized
strings. Leading, inter-token and trailing runs of 1/16 MiB deliver exactly five
bytes (` { } `) to the real decoder. A 48 MiB padded alloc file verifies that
initial and confirmation SHA-256 values both cover every raw input byte. Other
tests preserve quoted spaces/escapes across single-byte reads, reject separated
numeric tokens and invalid hex, check cancellation during whitespace-only chunks,
and retain read errors accompanying otherwise valid JSON bytes. `make ci`,
`make test-race` and `git diff --check` passed after the fix.

The same three-sample, fresh-process paired benchmark was rerun after CI/race
finished. [Follow-up raw output](ovm-genesis-alloc-whitespace-benchmark-2026-09-16.txt)
records the source SHA-256 and individual measurements. This benchmark retains
the ordinary 1,000-account override fixture; the long-padding cases are covered
by the dedicated regression tests above.

| Workers | After whitespace fix | Time (ms) | Allocated (MiB/op) | Peak RSS (MiB) | Sampled disk peak (MiB) |
| --- | --- | ---: | ---: | ---: | ---: |
| 2 | No alloc | 935.01 | 208.45 | 82.47 | 14.14 |
| 2 | 1,000 overrides | 1001.09 | 250.34 | 86.77 | 14.00 |
| 8 | No alloc | 821.17 | 207.23 | 82.73 | 11.84 |
| 8 | 1,000 overrides | 866.94 | 254.98 | 90.00 | 16.86 |

Relative to the prior implementation run, no-alloc median time changed by
-0.5%/+2.6% at 2/8 workers; with alloc it changed by +1.9%/-3.1%. The increases
are reported rather than treated as a guaranteed zero-cost fix. These small,
separately collected samples do not establish which differences come from the
reader change versus runtime/system variability. All earlier synthetic-fixture,
RSS and disk-sampling limitations still apply.
