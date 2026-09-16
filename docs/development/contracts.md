# Immutable contracts

[Documentation index](../README.md) · [Repository guidance](../../AGENTS.md)

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
- Keep exact reachable hash-node tracking in its operation-local Pebble index.
  Disk-backed bounded-cache storage remains the default. Explicit `--temp-db
  memory` is the only exception: use Pebble vfs.NewMem, not a state-sized map.
  Preserve batch/queue/worker bounds; memory files grow with scratch data.
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
