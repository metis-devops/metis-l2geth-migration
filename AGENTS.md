# Repository guidance

## Mission and boundaries

This repository builds `l2state`, a deliberately narrow migration tool for the
latest executed state in a legacy Optimism/Metis l2geth LevelDB.

Preserve the state-only boundary. The tool migrates the accounts, storage, and
code committed by the canonical `LastBlock`, plus exactly five chain-metadata
entries: the selected header, its hash-to-number mapping, its canonical
number-to-hash mapping, `LastBlock`, and `LastHeader`. It does not create
bootable geth `chaindata` or migrate bodies, transactions, receipts, total
difficulty, `LastFast`, chain configuration, historical headers or state, or
trie preimages.

The independent `prune` command is an explicit exception to source immutability:
it deletes old legacy state in place in a stopped full-node LevelDB. It does
not widen migration artifact contracts or restore legacy target generation.

## Code map

- `cmd/l2state` owns CLI validation, progress-log setup, and final JSON output.
- `internal/readonlydb` is the strict read-only adapter for legacy LevelDB.
- `internal/migration/prune*.go` owns offline pruning, the independently held
  LevelDB lock, raw physical-state collection, and protected-inventory checks.
- `internal/bundle` defines the v1 manifest, canonical slim-account codec, and
  deterministic account, storage, and code record stream.
- `internal/migration/export.go`, `import.go`, `migrate.go`,
  `migrate_partitioned.go`, and `direct_writer.go` implement the portable and
  direct workflows. Portable import persists nodes from its ordered
  `StackTrie`. Direct migrate uses 16 first-nibble `PartialStackTrie`
  partitions, with a serial fast path for storage tries of at most 1024 slots,
  and assembles the same canonical root and target layout. Portable import
  retains a test-only pinned `GenerateTrie` reference builder for independent
  layout comparison.
- `internal/migration/verify.go` and `direct_verify.go` independently verify
  bundle-backed and direct artifacts; their report formats are intentionally
  distinct.
- `internal/migration/head_metadata.go`, `progress.go`, and `atomicdir.go` own
  the minimal header inventory, stderr progress, and no-replace publication.
- `internal/migration/testdata` contains the committed legacy canary and
  expected evidence.
- `testdata/legacyfixturegen` is a separate, maintenance-only module for
  intentional canary regeneration.
- `testdata/legacyprune` independently tests the prune CLI, startup at the
  preserved head and subsequent block import with pinned legacy l2geth APIs.

## Immutable contracts

### Source and consensus data

- For migration/export/verification, treat the legacy source as immutable. Never enable recovery, compaction,
  repair, or any write path. Require a stopped database or a
  point-in-time-consistent filesystem snapshot.
- Validate `LastBlock`, its number mapping, canonical mapping, header RLP,
  header hash, block number, and state root before traversal. Confirm the same
  canonical head after traversal and fail if it changed.
- Preserve source and target consensus bytes. Bundle v1 represents accounts
  with canonical geth slim RLP, but must restore full canonical account RLP
  before trie reconstruction or target writes. Do not reinterpret OVM balances,
  synthesize preimages, or transform storage-value RLP or code. `UsingOVM`
  affects execution/RPC semantics, not trie encoding.
- Fail closed on missing trie nodes or code, non-canonical RLP, malformed or
  duplicate records, digest/count/order mismatches, and recomputed-root
  mismatches.

### Bundles and evidence

- Keep account, storage, and unique-code records in deterministic semantic
  order. Preserve their framing, validation, and header-seeded Keccak record
  chain.
- Bundle and bundle-backed verification formats are version 1. Direct
  verification is a separate version 1 format and must not acquire bundle-only
  digests or manifest fields. During active development, keep the current schemas
  at v1; do not add historical-format compatibility. Reject v2/v3 artifacts and
  recreate them instead of editing their version fields. The record-chain domain
  is `metis-l2state-record-chain/v1`.
- `geth_version` is optional provenance, not a compatibility gate: accept any
  string, omission, empty string, or null, but reject other JSON types.
  `geth_commit` is removed from input and output schemas; reject it as an unknown
  field. Do not restore a hardcoded runtime commit or an old-field reader. Keep
  `tool_version` non-empty and retain before/after input comparisons.
- In v1 account payloads, only zero-length or 32-byte roots and code hashes are
  valid. Zero length expands to `EmptyRootHash` or `EmptyCodeHash`; explicitly
  encoding either empty constant is non-canonical and must fail. Keep
  `state_file.record_payload_bytes` as the compact wire total and
  `counts.payload_bytes` as the expanded consensus total.
- Exact selected-header evidence is mandatory within those versions. Do not
  accept older reports that omit it or silently add a compatibility mode.
- Keep fixed hashes as `common.Hash` and arbitrary header bytes as
  `hexutil.Bytes`; malformed, zero, unprefixed, or wrong-length values must
  continue to fail strict JSON decoding or validation.
- Bundle roots must contain exactly `manifest.json` and their one record file;
  reject root/entry symlinks, extra entries, non-regular files, metadata over
  1 MiB, and import outputs inside the input bundle.

### Target artifacts

- Support `--db-engine pebble|leveldb` (default Pebble) with explicit `hash`
  and `path` schemes using pinned geth v1.17.5. The target layout is always geth.
  Do not restore the removed `--state-layout` flag or legacy target generation.
- Reports record `db_engine` as `pebble-v2|leveldb` and new artifact reports
  explicitly record `state_layout` as `geth`. An omitted layout in an old report
  means geth; explicit legacy-l2geth/empty/null/unknown values fail.
  Keep existing versions and bundle encoding. Pure bundle verification carries
  neither target field. Verification uses the declared engine without fallback
  and opens LevelDB with the strict, recovery-disabled read-only adapter.
- Target code keys use `c + codeHash`. Reject bare-hash code, unreferenced code,
  and orphan trie nodes. Never classify code by RLP shape. Legacy source code
  reading remains supported; source and target conventions are distinct.
- Close LevelDB writers, sync every regular database file and then the directory
  before reopening and publishing. The pinned LevelDB SyncKeyValue is a no-op.
  Check cancellation and propagate file-sync errors; never recover a target
  during verification or reuse an existing writable target directory.
- After state generation, store only the selected header and the four matching
  lookup/head entries. Reject additional headers, head markers, bodies,
  receipts, history, orphan trie nodes, unreferenced code, malformed path
  metadata, and any other unexpected database key.
- Hash artifacts must contain no temporary flat state. Path artifacts must
  preserve geth v1.17.5 completion metadata, `SnapshotRoot`, and state ID 0,
  with no historical layers.
- Direct-migrate partition roots must reproduce the serial trie node set.
  Handle empty, single-populated, and multi-populated partitions with the
  pinned `MountPartitionRoot`/`AssembleBranch` APIs, including deletion of a
  folded single-partition orphan. Do not leave partition-only nodes behind.
- Deduplicate contract code hashes exactly with an operation-local
  `map[common.Hash]struct{}`. The supported assumption is fewer than one
  million unique code hashes; do not add a pre-count scan, hard limit, or disk
  fallback without an explicit contract change.
- Keep exact reachable hash-node tracking disk-backed and bounded. Do not
  replace its operation-local Pebble index with a set whose memory grows with
  the full state trie.
- Independently reopen and verify every artifact before publication. Do not
  trust `verification.json` as the source of truth for bundle verification.
- Artifact roots must contain exactly the real `chaindata/` directory and real
  `verification.json` file. Reject root or entry symlinks and extra top-level
  entries before and after standalone verification. Both migrate and import use
  this name; verification does not fall back to the former `db/` name.

### Publication and compatibility

- Output paths must be new and non-aliasing. Export and direct-migration
  outputs must also be outside the legacy source. Stage work in a sibling
  `.partial-*` directory; sync and verify it before an atomic no-replace
  rename. Cleanup must target only the partial directory created by the
  current invocation.
- If rename succeeds but syncing the parent fails, retain the final directory
  and return `PublicationDurabilityError`. Never report success, remove it, or
  attempt an automatic rollback; operators must verify it before deciding what
  to retain.
- The root module requires Go 1.27 and pins go-ethereum v1.17.5 at commit
  `9621c6ad10934a01b5514886fb6fbd87640b6c05`. Do not change the geth pin,
  bundle/report versions, record encoding, database layout, supported schemes,
  or compression choices as incidental cleanup.

## Implementation rules

### Opt-in OVM balance conversion

- `migrate --migrate-ovm-eth` is an explicit exception to consensus-byte and
  original-head preservation. Default migrate, portable workflows and prune
  retain their existing boundaries. `ovm_*.go` owns the conversion implementation.
- Require an operator-supplied storage-compatible wrappedEther runtime hex file;
  never install the test runtime by default or execute a constructor. All source
  native balances must be zero. Reconcile every identified balance against the
  original totalSupply; unknown storage and accounting discrepancies fail closed.
- Classify by code at the selected head. Only authenticated canonical OVM_ETH
  Transfer-from membership retains a contract's ERC20 balance (including zero
  transfers, transferFrom and burns). EOA balances always convert. OVM_ETH self
  holdings always remain, and its native backing equals retained totalSupply.
- Read complete legacy headers/receipts, including freezer files, physically
  read-only. No restoring freezer constructors, recovery, writes or RPC fallback.
  Stop at the selected `LastBlock` even when ancient or fast/header heads are
  further ahead. Hash/header overlaps must match bytes; differently encoded
  legacy receipt overlaps must both match the canonical receipt root, gas and
  bloom. Use the cold encoding for history evidence and budget both raw copies;
  never ignore malformed/empty copies or rewrite source receipts to normalize them.
  Witnesses provide addresses and allowance pairs, never balances or eligibility.
  Keep evidence/patches disk-backed and reject unclassified storage slots.
- Complete and independently reopen the original migrated state before applying
  balance changes. Build the final artifact afresh with the partitioned core.
  Share the worker limiter with history and balance workers; join all jobs on
  errors/cancellation. Preserve record/byte queue bounds and path state ID 0.
- The synthetic checkpoint uses parent height/time +1, the new root, inherited
  gas limit, empty body/transaction/receipt/uncle commitments, zero execution and
  consensus fields, no optional fork fields, and extra `metis-l2state-ovm/v1`.
  Its six chain-data KVs are the only metadata exception; it is not a bootable
  database or a normally validated consensus block.
- Keep `metis-l2state-ovm-verification` v1 independent of ordinary reports.
  Standalone OVM verification replays source/history/operator inputs before
  checking the actual target's full inventory. Never relax ordinary root equality.
- Exercise four target combinations, independent serial state conversion,
  legacy-module receipt/state reading, wrapped runtime execution, continuation,
  history failures, input/report tampering, source immutability, concurrency and
  cancellation. Do not regenerate the old canary or geth compatibility corpus.
  The old canary's inconsistent synthetic supply must fail conversion validation.
  Finish with `make ci`, `make test-race`, and paired OVM measurements in isolated
  processes; report synthetic measurement limits and any regressions.

### Offline prune

- Support only stopped legacy LevelDB/hash full-node databases that have never
  enabled LES service. Reject detected LES metadata and geth/mixed state layouts
  before any deletion. Do not infer that all 32-byte keys are state.
  Match LES balance keys by their version prefix and complete ID/address shape;
  never classify ordinary hash-index keys by substrings inside their hashes.
- Retain the complete canonical `LastBlock` state plus the genesis root-node
  KV (unless the genesis trie is empty). Preserve every non-state record,
  head marker and ancient file; no genesis rewriting or recent-root window.
- Record actual raw state reads in an operation-local, bounded-cache Pebble
  keep database; never use generated geth code keys or StackTrie output as the
  original key inventory. Keep shared physical node/code records exactly once.
  Use the shared partitioned execution core with a validation-only output;
  migrate retains its ordinary target writer. Both modes retain the global
  worker limiter, twice-workers account window and 1024-slot storage probe.
  Shard keep batches by key, validate every captured content hash, and latch
  capture/flush failures independently of trie-reader error propagation.
  Do not restore per-record keep-database Has/Get calls or a state-sized node set.
- Validate the whole current state and independently reopen the keep database
  before deletion. Delete only absent 32-byte keys whose values hash to the key.
  Retain and count unknown records; fail on corruption in the retained state.
- Hold the LevelDB filesystem lock across read-only preflight, writable deletion
  and independent read-only verification. Keep the general readonlydb adapter
  immutable, including borrowed views. Never call Recover/RecoverFile; normal
  strict WAL replay on writable open is permitted for prune only.
  All prune opens must resolve only the exact valid `CURRENT` manifest without
  calling goleveldb's restoring/fallback GetMeta. Reject missing or malformed
  CURRENT/manifest and pending CURRENT.N publications; do not use CURRENT.bak.
- Use bounded, synced deletion batches. Verify head, state, genesis root-node
  bytes and the ordered, length-framed digest/count of all protected KV after
  deletion. Close and sync regular database files plus directory before success.
  Compare source/keep via ordered cursors, rejecting missing keep keys. Preserve
  the exact v1 digest framing/order through parallel hashing and ordered output.
  Limit scan queues to twice-workers records and cache/8 bytes (1–16 MiB), with
  oversize records processed synchronously after draining. Reserve a coordinator
  slot and keep candidate hashing within the worker allowance. Sync delete
  batches at 16384 keys or 1 MiB encoded size; report only committed deletions.
  Workers follow migrate's normalization/defaults. Allocate one third of cache
  and handles to the keep database (minimum 16 each), with the remainder assigned
  to the source; these are database allowances, not a total RSS limit.
- Default to no explicit full compaction; `--compact` opts in. Do not promise
  instantaneous cancellation during CompactRange or physical bytes reclaimed.
- `--dry-run` is physically read-only and incompatible with `--compact`.
  Require an existing regular LOCK before a dry-run storage open: goleveldb's
  read-only opener otherwise creates it. Keep filter.NewBloomFilter(10) enabled
  in prune's LevelDB options so reads use, and compaction preserves, legacy filters.
  Interrupted pruning restarts by building a fresh keep set, without reusing
  incomplete sets or rolling back deletions. Only clean this invocation's
  temporary directory; never write progress journals into chaindata.
- Prune result v1 is independent from existing bundle and verification formats.
  Preserve stdout JSON/stderr progress separation and the uint256 boundary.

### Shared implementation rules

- Preserve `context.Context` cancellation through source scans, chunked bundle
  reads/writes, path adoption, and verification.
- Wrap errors with operation context and `%w`. Check and combine relevant
  close, sync, abort, and cleanup errors instead of discarding them.
- Keep human-readable progress on standard error and the single final JSON
  value on standard output. `--quiet` suppresses progress, not diagnostics or
  the final error.
- Keep long operations streaming. The operation-local codehash set is the only
  state-sized in-memory exception; keep other working state bounded by the
  configured cache and handle allowances. Do not add a pre-count scan merely
  to report a percentage or ETA.
- Direct migrate always uses the partitioned builder. Normalize workers below
  2 to 2, reject values above 16, and share one limiter across account and
  nested storage partitions. Bound accounts read but not yet merged globally
  to twice the normalized worker count. Keep each account-partition iterator
  serial and retain a zero-copy serial fast path for accounts without storage
  or newly claimed code. Storage or a first code-hash claim starts a bounded,
  non-blocking burst; read and validate code in its worker, but write the
  returned bounded code bytes through the ordered partition writer. Merge
  account/code writes and `PartialStackTrie.Update` calls in sequence order.
  Iterator advancement, account work, and trie merging borrow the shared
  limiter only while active. A light-account scan may retain its lease while
  the global account window is empty, but must yield after each account while
  a burst is pending. Release an account lease before waiting for a large
  storage partition and join every account and nested task before closing
  either database. Keep the 1024-slot storage probe uncommitted so crossing the
  threshold can discard it without leaking target keys or duplicate counts.
- Add regression coverage for behavior changes. Exercise both `hash` and
  `path`, both `zstd` and `none`, and both direct and bundle-backed paths
  when the changed behavior applies to them.
- Update operator documentation and agent guidance whenever a public command,
  report contract, artifact invariant, or validation gate changes.

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

Run focused tests while iterating, then finish every change with:

```bash
make ci
git diff --check
```

`make ci` runs formatting and module-tidiness checks, lint, all root-module
tests (including the geth compatibility gate), fixture-module and legacy-prune
module tidy/verify/test/vet, and the build. `make test-race` also runs the
legacy-prune module with a race-enabled CLI.
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
