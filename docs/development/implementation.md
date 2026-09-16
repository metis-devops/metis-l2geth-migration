# Code map and shared implementation rules

[Documentation index](../README.md) · [Repository guidance](../../AGENTS.md)

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

## Shared implementation rules

- `migrate`, `import`, and `verify` support `--temp-db disk|memory`; empty Go
  options mean disk, explicit empty/unknown CLI values fail. Export has no
  temporary database; prune remains disk-only. Do not add temp settings to
  reports or change artifact engines, layouts, versions or compression.
- Memory temporary databases use the pinned Pebble v2 filesystem and internal
  ethdb adapter. Keep one private filesystem alive across OVM original-state
  writer close, independent read-only reopen/complete verification and writable
  reopen. Artifact verification still opens physical storage through its
  declared engine without fallback. Join workers, close all handles and release
  private filesystems on success, failure and cancellation. Never claim memory
  sync as disk durability or skip final target sync/publication.
- Memory mode has no hard RAM cap or automatic spill. Cache allowances do not
  include memory files, compaction peaks or total RSS. Keep geth memorydb in
  benchmarks only. Preserve error sentinels, value ownership, iterator ordering,
  batch replay and propagation of resource-release errors in the adapter.
- Temporary-backend changes require four-target logical/reference comparisons,
  both bundle compression modes, cross-mode standalone verification, OVM alloc,
  runtime/continuation, corruption, input tampering, source immutability, no
  physical temporary DB files, and cancellation/cleanup checks. Run `make ci`,
  `make test-race`, and isolated alternating measurements with
  `scripts/benchmark-ovm.py --holders 10000 100000 --temp-dbs disk memory
  --with-alloc --with-verify --with-components --count 3 --out /absolute/new/file`.
  Report RSS/setup and sampled heap/file limitations and any regressions.

- Preserve `context.Context` cancellation through source scans, chunked bundle
  reads/writes, path adoption, and verification.
- Wrap errors with operation context and `%w`. Check and combine relevant
  close, sync, abort, and cleanup errors instead of discarding them.
- Keep human-readable progress on standard error and the single final JSON
  value on standard output. `--quiet` suppresses progress, not diagnostics or
  the final error.
- Keep long operations streaming. With default disk temporary storage, the operation-local codehash set is the only
  state-sized in-memory exception; explicit memory mode also retains temporary
  Pebble files. Keep remaining working state bounded by the
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
