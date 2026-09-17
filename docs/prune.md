# Offline legacy state pruning

[Documentation index](README.md) · [Project overview](../README.md)

`prune` operates on an existing legacy l2geth full-node LevelDB. Stop the node
for the entire operation and retain a backup or consistent snapshot before
deleting historical state. Databases that have served LES clients are outside
the supported scope; detected LES service records cause pruning to fail before
deletion. Light-client databases, geth code/path layouts and mixed layouts are
unsupported. No migration artifact or bootstrapping data is generated.

```bash
# Inspect and verify without modifying the source database.
./bin/l2state prune --chaindata /data/geth/chaindata --dry-run

# Delete old state in place; no explicit full-database compaction by default.
./bin/l2state prune --chaindata /data/geth/chaindata --workers 4

# Also compact the database to reclaim disk space.
./bin/l2state prune --chaindata /data/geth/chaindata --compact
```

The retained state is the latest **executed** canonical `LastBlock` root, not
potentially newer `LastHeader` state. A temporary Pebble database holds the
original account/storage trie nodes and referenced code, using legacy bare
hash keys. Pruning also retains the single genesis root-node KV, because the
pinned legacy node opens that root during genesis initialization. It does not
retain the complete genesis state or a recent-state rollback window. Historical
state queries and reorgs requiring discarded state are not supported.

Only 32-byte, content-addressed records absent from the verified keep database
are deleted. Unknown 32-byte records whose values do not hash to their keys are
retained and counted. Blocks, receipts, transaction metadata, chain configuration,
rollup indexes, head markers, preimages and ancient files are preserved. The
genesis header and canonical mapping must be present in LevelDB; the legacy
freezer normally retains them there. Pruning never opens or changes ancient files.

The database lock remains held across collection, deletion and independent
reopening. Both current state and the complete protected KV inventory are
verified after deletion. Failure or cancellation after deletion starts leaves
completed deletions in place; rerun the same command while the node remains
stopped to build a fresh keep database and finish. There is no rollback or saved
deletion cursor. Corrupt databases are rejected without repair. Normal strict
LevelDB journal replay on writable open is distinct from database repair.
Every open reads only `CURRENT`, which must name an existing, regular
`MANIFEST` file. Prune never falls back to `CURRENT.bak` or restores `CURRENT`;
pending `CURRENT.N` publications cause rejection, including during dry-run.
An interrupted manifest publication therefore needs separate investigation
before pruning can resume. An unused backup is allowed when `CURRENT` is valid.

`--temp-dir` selects an existing parent outside chaindata (default: chaindata's
parent), with space for approximately the current state's physical KV data plus
database overhead. A unique `.l2state-prune-*` child is removed on normal exit;
after a hard kill, identify and remove the abandoned child while no prune is
using it. Reruns do not reuse old children or delete other tasks' directories.
Paths and database entries must not be symlinks. `--cache-mb` (default 512) and
`--handles` (default 256) require at least 32 each, including 16 reserved for
each database. The keep database receives one third of each allowance (at least
16); the source receives the remainder. `--workers` uses migrate's default
`min(4, GOMAXPROCS)` with a minimum of 2; values below 2 normalize to 2 and values
above 16 are rejected. These limits govern execution workers and database
caches/file-handle caches, not total process RSS or database background threads.

Collection and independent state verification share migrate's 16-partition
scheduler, global account window and 1024-slot storage probe. Prune records
original reads into key-owned batch shards; generated partition nodes are used
only to validate roots. Each shard buffers at most approximately
`ethdb.IdealBatchSize` (plus one record); the existing operation-local codehash
set remains the only state-sized in-memory set.

The source and keep database are compared by ordered iterators rather than
per-record point lookups. Candidate hashes of at least 4 KiB use parallel
workers; small records avoid dispatch overhead when the ordered queue is empty.
The scan reserves one worker slot for ordered digest/deletion processing and
keeps at most twice the worker count queued, with a byte allowance of one eighth
of the configured cache (clamped to 1–16 MiB). Oversize records drain the queue
and run synchronously without copying the whole record into it. Deletion batches
are synced after 16384 keys or 1 MiB of encoded batch data, whichever comes first.
Preflight, deletion and post-verification remain separate complete passes.

`--dry-run` and `--compact` are mutually exclusive. Ordinary LevelDB writes can
still trigger internal compaction without `--compact`; logical deletion does
not promise immediate disk recovery. Explicit compaction requires temporary
disk space and cannot be interrupted instantly: cancellation waits for the
active compaction call to return before closing the database.
Dry-run requires the source's existing regular `LOCK` file and rejects a missing
one instead of letting LevelDB create it. Prune uses the legacy 10-bit Bloom
filter policy for both reads and rewritten SSTs, including explicit compaction.

Progress goes to stderr; `--quiet` suppresses progress. Success produces one
`metis-l2state-prune` v1 JSON result on stdout with the selected head, genesis
root, state counts, retained/protected/candidate/deleted/unknown KV counts and
logical bytes, protected-inventory digest, and verification/compaction status.
Dry-run reports candidates and zero deletions. Byte counts include keys and
values; they are not measurements of freed disk space. Errors identify the
phase and whether deletion started, and do not emit a success JSON result.

`make legacy-prune-check` independently tests the actual CLI with l2geth
`e795a258d3f2`: pruning a generated archive database, preserving the startup
head, importing the next block and reopening its state. This gate runs in
`make ci`, and `make test-race` also exercises it with a race-enabled CLI.
These are synthetic local tests, not production-snapshot acceptance, live
rollup synchronization or reorg validation.

For paired performance measurements against the frozen serial implementation:

```bash
python3 scripts/benchmark-prune.py --out /tmp/prune-bench.txt --count 5
```

The runner uses fresh processes per workload/mode/repetition, with 128 MiB of
configured cache and 128 handles in both implementations. It tests workers
2/4/8/16 and measures collection, verification, scan, deletion and end-to-end
time. Fixture generation and copying are excluded from phase timers. It also
reports Go allocations, sampled peak Go heap, temporary database file sizes for
the component phases and OS block-I/O counters. Go heap excludes native memory
and kernel cache; the OS counters may be uninformative (including zero) on some
platforms and must not be interpreted as proof of zero disk traffic. Page caches
are not flushed. See [historical measurements](benchmarks/prune.md).
