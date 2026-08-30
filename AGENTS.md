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

## Code map

- `cmd/l2state` owns CLI validation, progress-log setup, and final JSON output.
- `internal/readonlydb` is the strict read-only adapter for legacy LevelDB.
- `internal/bundle` defines the v2 manifest and deterministic account,
  storage, and code record stream.
- `internal/migration/export.go`, `import.go`, `migrate.go`, and
  `direct_writer.go` implement the portable and direct workflows. Direct
  migration persists target trie nodes from the same ordered `StackTrie`
  rebuild that validates the source root; portable import uses the pinned
  `GenerateTrie` API over its streamed flat state.
- `internal/migration/verify.go` and `direct_verify.go` independently verify
  bundle-backed and direct artifacts; their report formats are intentionally
  distinct.
- `internal/migration/head_metadata.go`, `progress.go`, and `atomicdir.go` own
  the minimal header inventory, stderr progress, and no-replace publication.
- `internal/migration/testdata` contains the committed legacy canary and
  expected evidence.
- `testdata/legacyfixturegen` is a separate, maintenance-only module for
  intentional canary regeneration.

## Immutable contracts

### Source and consensus data

- Treat the legacy source as immutable. Never enable recovery, compaction,
  repair, or any write path. Require a stopped database or a
  point-in-time-consistent filesystem snapshot.
- Validate `LastBlock`, its number mapping, canonical mapping, header RLP,
  header hash, block number, and state root before traversal. Confirm the same
  canonical head after traversal and fail if it changed.
- Preserve consensus bytes. Do not reinterpret OVM balances, synthesize
  preimages, or transform account RLP, storage-value RLP, or code. `UsingOVM`
  affects execution/RPC semantics, not trie encoding.
- Fail closed on missing trie nodes or code, non-canonical RLP, malformed or
  duplicate records, digest/count/order mismatches, and recomputed-root
  mismatches.

### Bundles and evidence

- Keep account, storage, and unique-code records in deterministic semantic
  order. Preserve their framing, validation, and header-seeded Keccak record
  chain.
- Bundle and bundle-backed verification formats are version 2. Direct
  verification is a separate version 1 format and must not acquire bundle-only
  digests or manifest fields.
- Exact selected-header evidence is mandatory within those versions. Do not
  accept older reports that omit it or silently add a compatibility mode.
- Keep fixed hashes as `common.Hash` and arbitrary header bytes as
  `hexutil.Bytes`; malformed, zero, unprefixed, or wrong-length values must
  continue to fail strict JSON decoding or validation.

### Target artifacts

- Support only `hash` and `path`, both backed by the pinned geth v1.17.5 APIs
  and Pebble v2. Require callers and reports to select the scheme explicitly.
- After state generation, store only the selected header and the four matching
  lookup/head entries. Reject additional headers, head markers, bodies,
  receipts, history, orphan trie nodes, unreferenced code, malformed path
  metadata, and any other unexpected database key.
- Hash artifacts must contain no temporary flat state. Path artifacts must
  preserve geth v1.17.5 completion metadata, `SnapshotRoot`, and state ID 0,
  with no historical layers.
- Deduplicate contract code hashes exactly with an operation-local
  `map[common.Hash]struct{}`. The supported assumption is fewer than one
  million unique code hashes; do not add a pre-count scan, hard limit, or disk
  fallback without an explicit contract change.
- Keep exact reachable hash-node tracking disk-backed and bounded. Do not
  replace its operation-local Pebble index with a set whose memory grows with
  the full state trie.
- Independently reopen and verify every artifact before publication. Do not
  trust `verification.json` as the source of truth for bundle verification.

### Publication and compatibility

- Output paths must be new and non-aliasing. Export and direct-migration
  outputs must also be outside the legacy source. Stage work in a sibling
  `.partial-*` directory; sync and verify it before an atomic no-replace
  rename. Cleanup must target only the partial directory created by the
  current invocation.
- The root module requires Go 1.27 and pins go-ethereum v1.17.5 at commit
  `9621c6ad10934a01b5514886fb6fbd87640b6c05`. Do not change the geth pin,
  bundle/report versions, record encoding, database layout, supported schemes,
  or compression choices as incidental cleanup.

## Implementation rules

- Preserve `context.Context` cancellation through source scans, bundle scans,
  trie generation, verification, and compaction.
- Wrap errors with operation context and `%w`. Check and combine relevant
  close, sync, abort, and cleanup errors instead of discarding them.
- Keep human-readable progress on standard error and the single final JSON
  value on standard output. `--quiet` suppresses progress, not diagnostics or
  the final error.
- Keep long operations streaming. The operation-local codehash set is the only
  state-sized in-memory exception; keep other working state bounded by the
  configured cache and handle allowances. Do not add a pre-count scan merely
  to report a percentage or ETA.
- Add regression coverage for behavior changes. Exercise both `hash` and
  `path`, both `zstd` and `none`, and both direct and bundle-backed paths
  when the changed behavior applies to them.
- Update operator documentation and agent guidance whenever a public command,
  report contract, artifact invariant, or validation gate changes.

## Validation

Run focused tests while iterating, then finish every change with:

```bash
make ci
git diff --check
```

`make ci` runs formatting and module-tidiness checks, lint, all root-module
tests, and the build. Also run `make test-race` when changing concurrency,
cancellation, progress reporting, database lifecycle, atomic publication, or
shared state.

Use the following change-sensitive checks:

- CLI or progress changes: verify flags and stdout/stderr separation in
  `cmd/l2state` tests.
- Bundle or report changes: cover strict JSON, both compression modes,
  corruption, trailing data, counts, ordering, and direct/bundle format
  separation.
- Import or database-layout changes: test both schemes, exact inventory,
  independent reopening, and a subsequent state commit/read through geth
  v1.17.5 APIs.
- Source traversal or direct-migration changes: run the committed legacy
  canary through direct and portable workflows and confirm the source content
  remains unchanged.

Do not regenerate the golden fixture during ordinary test maintenance. If
regeneration is intentional, use the pinned module in
`testdata/legacyfixturegen`, generate into a new temporary directory, and run
that module's tests, vet, and module verification. Compare every generated
output with the committed fixture and document any changed l2geth or
`OVM_ETH` provenance before replacing it. A canary pass is not evidence that a
real production snapshot has been accepted.
