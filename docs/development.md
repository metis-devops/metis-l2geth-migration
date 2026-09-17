# Development and compatibility

[Documentation index](README.md) · [Project overview](../README.md)

## Geth dependency upgrades

Compatibility is determined by the tool's frozen contracts and validation gates,
not by geth's major/minor version number. The current accepted baseline was
captured with v1.17.5. This checks the migration tool's dependencies on geth,
not every behavior in geth or production-snapshot acceptance.

```bash
make ci
make test-race
```

`make ci` includes the geth compatibility tests through its `test` target.
The root `fixture-check` and `legacy-prune-check` targets delegate to `make check`
in `testdata/legacyfixturegen` and `testdata/legacyprune`, respectively. The latter
also provides `make test-race`, including a race-enabled CLI. GitHub CI runs the
root module (`make ci-root test-race-root`) and these two modules in separate jobs
with separate Go cache keys. Local `make ci` and `make test-race` retain full
coverage across the modules.
For a focused, uncached run, use `make geth-compat`. These tests compare current
behavior against the committed corpus under
`internal/migration/testdata/geth-compat`, then import its frozen records and
restore its frozen logical database entries for independent verification and
continued state commits on disposable copies. The corpus covers both compression
modes, all four target combinations, direct and portable workflows, empty and
single/multiple-partition tries, and fixed 1024/1025-slot boundaries. Canary
continuation updates nonce, balance, storage and code, closes, reopens, and checks
the committed state. Existing GenerateTrie/rawdb comparisons remain additional
checks.

Database baselines contain sorted logical key/value bytes, not SST or WAL file
layouts. JSON comparisons normalize only timestamps and build provenance;
manifest-dependent report hashes are recomputed from normalized manifest bytes.
The test normalizer removes historical `geth_commit` metadata in memory and
recomputes dependent manifest hashes before comparison and replay. The frozen
corpus stays unchanged; runtime readers have no old-field compatibility branch.
Before comparison and replay, tests exclude exactly the 21 retired legacy-target
cases from expected evidence in memory. All bundle cases and supported geth
target cases retain strict comparison; missing or changed geth cases still fail.
Candidate captures contain only the four supported geth target combinations.
Format version, encoding, records, counts, roots and database metadata are not
normalized. Failures identify the contract, scenario, target and changed field or
key, with bounded previews and hashes for long values. Comparison failures retain
the complete candidate corpus in a printed temporary path for inspection.

These three migration versions and the separate prune-result version live in
`internal/formatversion/version.go`.
The existing bundle/report constants are aliases, and the record-chain domain is
derived from the bundle version. Bundle-backed report baselines also identify
the input bundle version: a changed input digest alone does not require bumping
the report schema. Missing baselines fail; ordinary tests never generate them.

For an intentional dependency upgrade:

1. Update the geth dependency and sums. Module-version provenance is automatic;
   there is no runtime commit constant to synchronize. Historical baseline
   provenance retains its original commit as generation evidence.
2. Run the commands above. Compilation failures identify changed APIs; adapt
   those calls and keep format versions unchanged when the external contract is
   preserved. Major version upgrades follow exactly the same process.
3. Inspect any contract differences. Correctness failures such as changed
   consensus bytes, wrong state roots or acceptance of malformed input must be
   fixed, never approved by a format bump.
4. If deliberately accepting an incompatible representation or layout, change
   only the affected format versions and implementation. Bundle encoding changes
   also change its chain domain; target layout changes affect both artifact report
   formats. Export a candidate, review it, and add the corresponding new version
   files alongside historical baselines. Do not overwrite an existing version's
   contract to make a failing upgrade pass. No historical-format reader is added
   implicitly.

To export an unapproved candidate, supply a new **absolute** directory whose
parent already exists, outside the committed baseline directory:

```bash
make geth-compat-candidate OUT=/tmp/l2state-geth-candidate
```

This runs capture probes and exports data; it does not compare against the
accepted baseline and does not prove compatibility. Existing output directories
and symlinks are rejected. The original legacy canary is never regenerated.

## Development and test evidence

```bash
make ci
make test-race
```

The tests cover both engines and all supported layout/scheme combinations,
zstd and uncompressed bundles, direct and
portable migrations, independent verification, continued state access through
geth v1.17.5 APIs, corruption and ordering failures, exact database inventory,
strict top-level layouts, read-only source handling, cancellation, atomic
publication fault injection, a GenerateTrie reference build, parser fuzzing,
and supported cross-build targets.

The committed canary was generated with legacy l2geth commit `e795a258d3f2`,
default `UsingOVM=true`, and no trie preimages. It includes the complete
Andromeda `OVM_ETH` allocation pinned to `metis-networks` commit `696b5613df9c`:
ordinary account balances are zero and positive user balances live in
`OVM_ETH` storage. This fixture is deterministic regression evidence, not a
production-snapshot or production-scale acceptance result.

### Temporary database performance measurements

Compare disk and memory temporary storage with identical final Pebble/hash output:

```bash
python3 scripts/benchmark-ovm.py --out /absolute/new/temp-db-results.txt \
  --count 3 --holders 10000 100000 --temp-dbs disk memory \
  --with-alloc --with-verify --with-components
```

This alternates fresh processes for workers 2/8, conversion/alloc and
migration/verification. The optional component traces compare disk Pebble,
Pebble memory files and geth memorydb at 10k/100k/1m records: batched overwrites,
repeated node markers, random reads, ordered scans and 1000 prefix scans. Geth
memorydb is a benchmark reference only, not a supported CLI backend.
Component traces use fixed high-entropy 32-byte keys/values (empty values for
node markers); retained input arrays contribute equally to measured memory.
Component disk and memory Pebble use the same adapter/settings; end-to-end disk
runs use the existing production disk adapter. Do not run CI or other benchmarks
concurrently. The harness preserves `--baseline-root` comparisons when both
checkouts contain the same benchmark harness.

`ns/op`, allocation counters and GC counts cover the measured operation; `setup-s`
is separate. OS peak RSS includes setup and the initial disk migration used to
prepare verification artifacts. Heap and file metrics sample every 20 ms and
can miss short peaks. Memory-file lengths exclude capacity/allocator overhead;
Go heap excludes some Pebble allocations and OS cache. Physical temporary-file
bytes exclude the final artifact, while total disk bytes include it. OS caches
are not flushed. The synthetic fixtures do not establish production throughput,
maximum memory requirements or LevelDB/path performance.

Measured results and limitations: [benchmark index](benchmarks/README.md) and
[OVM scale/component results](benchmarks/ovm.md#historical-100k-holder-and-component-measurements).
