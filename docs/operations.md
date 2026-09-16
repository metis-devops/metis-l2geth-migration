# Operational reference

[Documentation index](README.md) · [Project overview](../README.md)

- `-h` and `--help` on every subcommand print that subcommand's usage to
  standard error and exit successfully without starting an operation or
  emitting JSON.
- `migrate`, `import`, and `verify` accept `--temp-db disk|memory` (default
  `disk`). `memory` uses Pebble's in-memory filesystem for reachable-node
  indexes and OVM original-state/evidence/patch databases, including standalone
  OVM verification replay. It never changes `--db-engine`, final artifact
  storage, report fields, or format versions. Export has no temporary database
  and does not accept this flag; prune retains its disk-backed keep database.
  Pure bundle verification accepts the option but needs no temporary database.
- Memory mode is an explicit space-for-speed option, not a throughput guarantee.
  All temporary database files remain in RAM; memory grows with the full scratch
  data, in addition to caches, memtables, compaction and Go allocations.
  `--cache-mb` is not a memory limit, and there is no automatic spill to disk or
  hard RAM cap. Choose a machine with capacity for peak scratch data and the
  remaining process overhead. Existing streaming queues and worker bounds remain.
- OVM memory mode still closes and independently reopens the original migrated
  database read-only on the same operation-local filesystem before conversion.
  Every final artifact is independently reopened from physical storage and
  verified, synced and published with the usual rules. Private staging
  directories may exist on disk, but temporary database files do not. Memory
  mode does not provide restart/resume persistence; interrupted work is rerun.
- `--cache-mb` defaults to 512 MiB and `--handles` defaults to 256 for every
  state operation. Direct migration keeps source and target databases open
  together, so account for both allowances.
- `migrate --workers` defaults to `max(2, min(4, GOMAXPROCS))`, raises smaller
  values to 2, and rejects values above 16. Account and large-storage partition
  tasks share this single limit; it does not multiply at nested storage tries.
  Worker count does not change the final serial verification pass.
- Contract code hashes are deduplicated exactly with an operation-local
  in-memory set. Its memory use grows with the number of unique code hashes;
  the supported operating assumption is fewer than one million, without a
  pre-count scan, hard limit, or disk fallback.
- While verifying `hash` artifacts, reachable trie-node hashes use a separate
  operation-local Pebble index with at most 16 MiB of cache and 16 file handles
  (or the lower positive configured allowances). Import and direct migration
  keep this verification index inside the current `.partial-*` directory in
  disk mode. Memory mode retains it in a private in-memory filesystem.
  In disk mode, standalone artifact verification uses the operating system temporary
  directory; set `TMPDIR` to place it on a disk with enough capacity. `path`
  verification does not create this index. The index is removed before a
  successful output is published or a verification command returns.
- Progress logs go to standard error; the final JSON result is the only output
  on standard output. Phase changes appear immediately and long phases update
  every 30 seconds. Use `--quiet` to suppress progress logs.
- Import and verification have exact record totals. Export and direct
  migration do not report a percentage or ETA because finding the source total
  would require another full traversal.
- The final output path must not exist. Export and direct migration also reject
  the source chaindata path and paths inside it. Work is staged in a sibling
  `.partial-*` directory, synced, verified, and published with an
  operating-system no-replace rename.
- Cancellation or any failure before publication removes only the partial
  directory created by that invocation, and the final path never appears. If
  the no-replace rename succeeds but syncing the parent directory fails, the
  command returns a `PublicationDurabilityError` and retains the final path
  with durability explicitly unknown. Do not rerun with that path; verify it
  against the original bundle or source before deciding whether to retain it.
- Resume is not supported; ordinary failures must be rerun with a new output
  path.
- Plan disk space for the bundle when using export/import. `hash` artifact
  verification additionally needs the temporary trie-node index. Both import
  workflows generate target trie nodes during their single source or bundle
  scan and need no second flat-state staging copy; path artifacts retain their
  required current flat state.

To capture machine output and progress separately:

```bash
./bin/l2state import \
  --bundle /exports/metis-state-12345 \
  --out /states/metis-hash \
  --scheme hash \
  >import-result.json \
  2>import-progress.log
```
