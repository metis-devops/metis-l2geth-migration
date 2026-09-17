# Validation and format evolution

[Documentation index](../README.md) · [Repository guidance](../../AGENTS.md)

## Geth compatibility and format evolution

- `internal/formatversion` owns the independent bundle, verification, direct
  verification and prune-result versions;
  existing bundle/report constants are aliases. The record-chain domain derives
  from the bundle version. All currently remain v1.
- Explicit dependency upgrades are governed by frozen compatibility evidence,
  not major/minor geth version equality. Module versions are reported
  automatically; no runtime geth commit needs manual synchronization. Preserve
  historical commit evidence in frozen baselines. Only the test normalizer
  removes their old commit field in memory and recomputes dependent manifest
  hashes before comparison/replay; runtime readers stay strict.
- `make geth-compat` compares the fixed corpus in
  `internal/migration/testdata/geth-compat`, restores frozen logical databases,
  imports frozen record streams, and checks continuation on disposable copies.
  Existing current-geth reference comparisons and source canary checks are still
  required. This gate covers tool contracts, not all geth functionality.
  Exclude only the 21 explicitly retired legacy-target cases from expected
  baselines in memory before comparison/replay; never filter actual output,
  rewrite frozen files, or hide missing/changed geth cases.
- The Pebble engine identifier is uniformly `pebble` in options and reports.
  Keep current report versions at v1 for this intentional identifier replacement;
  runtime readers reject the retired identifier. Only expected frozen artifact
  reports translate the retired identifier to `pebble` in memory before comparison
  and replay. Never normalize current output or change other report/database evidence.
- Treat accepted version files as immutable. Ordinary tests never regenerate
  them. `make geth-compat-candidate OUT=/absolute/new/path` only exports an
  unapproved candidate outside the corpus; review differences before adding a
  new version. Never overwrite the legacy canary as part of this process.
- Keep format versions for API-only adaptations with identical external behavior.
  Deliberate incompatible bundle changes update its version/domain; report or
  target-layout changes update affected report versions. Bundle-backed baselines
  include the input bundle version to avoid conflating input digests with report
  schema changes. Do not introduce historical readers without authorization.
- Never use a version bump to accept consensus corruption, root mismatches,
  malformed input acceptance, or relaxed source/publication invariants.

## Validation

Keep benchmark evidence under `docs/benchmarks/`: one dataset index, consolidated
OVM/prune reports, and immutable raw logs in `raw/`. Use source fingerprints and
explicit historical labels; never pool samples across revisions or fixtures.
Replace superseded stage summaries while retaining unique baseline/scale/component
evidence and measured regressions. Update README links when reorganizing results.

Run focused tests while iterating, then finish every change with:

```bash
make ci
git diff --check
```

`make ci` runs formatting and module-tidiness checks, lint, all root-module
tests (including the geth compatibility gate), fixture-module and legacy-prune
module tidy/verify/test/vet, and the build. `make test-race` also runs the
legacy-prune module with a race-enabled CLI.
Keep the fixture and legacy-prune check recipes in their module-local Makefiles;
root targets delegate through `$(MAKE) -C`. GitHub CI runs `ci-root test-race-root`
and separate fixture/legacy-prune jobs with disjoint Go cache key and restore
prefixes. The legacy-prune cache includes root dependencies because its tests
build the CLI. Preserve aggregate local `ci` and `test-race` coverage.
`make geth-compat` remains available for a focused, uncached compatibility run;
it is not a separate CI prerequisite because `test` already covers it. Also run
`make test-race` when changing concurrency, cancellation, progress reporting,
database lifecycle, atomic publication, or shared state.

Use the following change-sensitive checks:

- Prune changes: exact state/protected key inventories, genesis root-only
  startup, physical dry-run immutability, mixed layouts and LES rejection,
  shared code/node keys, interrupted deletion and rerun, locking, sync and
  compaction failure, plus legacy startup/next-block/restart validation.
  Compare partitioned collection and ordered scanning against the frozen serial
  test reference across all worker settings. Cover out-of-order hashing, byte
  and record bounds, oversize inputs, swallowed reader errors and worker joins.
  Performance changes require repeated paired measurements; use
  `scripts/benchmark-prune.py` without concurrent CI/benchmarks, and report
  regressions and measurement limits rather than asserting a fixed speedup.

- CLI or progress changes: verify flags and stdout/stderr separation in
  `cmd/l2state` tests.
- Bundle or report changes: cover strict JSON, both compression modes,
  corruption, trailing data, counts, ordering, and direct/bundle format
  separation.
- Import or database-layout changes: test both schemes, exact inventory,
  independent reopening, full logical comparison with the test-only
  `GenerateTrie` reference, and a subsequent state commit/read through geth
  v1.17.5 APIs.
- Backend/layout changes: cover all four engine/scheme combinations,
  both compression modes, report field strictness, old omitted-layout reports,
  physical-engine mismatch, rejected legacy reports, RLP-shaped code,
  bare-hash code injection, corrupt LevelDB read-only behavior, and file-sync failure.
- Source traversal or direct-migration changes: run the committed legacy
  canary through direct and portable workflows and confirm the source content
  remains unchanged.
- Partitioned direct-migration changes: compare hash and path node inventories
  with the serial and test-only `GenerateTrie` references; cover exact range
  boundaries, empty/single/multiple partitions, 1024/1025 storage slots,
  shared code across partitions, out-of-order account completion, the global
  account window, global worker bounds, cancellation, and nested-worker race
  behavior.

Do not regenerate the golden fixture during ordinary test maintenance. If
regeneration is intentional, use the pinned module in
`testdata/legacyfixturegen`, generate into a new temporary directory, and run
that module's tests, vet, and module verification. Compare every generated
output with the committed fixture and document any changed l2geth or
`OVM_ETH` provenance before replacing it. A canary pass is not evidence that a
real production snapshot has been accepted.
