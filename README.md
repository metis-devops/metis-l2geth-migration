# l2state

`l2state` migrates the latest executed state from a legacy Optimism/Metis
l2geth LevelDB into a state database compatible with go-ethereum v1.17.5.

> [!IMPORTANT]
> The output is a state-only artifact, not bootable geth `chaindata`. It
> contains the accounts, storage, and code committed by the canonical
> `LastBlock`, plus exactly five chain-metadata entries: the selected header,
> its hash-to-number mapping, its canonical number-to-hash mapping,
> `LastBlock`, and `LastHeader`. It does not contain block bodies,
> transactions, receipts, total difficulty, `LastFast`, chain configuration,
> historical headers or state, or trie preimages.

The source must be a stopped l2geth database or a point-in-time-consistent
filesystem snapshot. Migration does not read from RPC and never repairs,
compacts, or writes to the source LevelDB. The separate offline `prune` command
described below intentionally deletes old state in place.

## Choose a workflow

| Workflow               | Use it when                                                                                            | Evidence and storage tradeoff                                                                      |
| ---------------------- | ------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------- |
| `migrate`              | The source snapshot and destination are available together.                                            | Avoids creating a bundle, but later full verification requires the same source snapshot.           |
| `export` then `import` | The environments are separate, the state must be portable, or both schemes may be built from one scan. | Stores a portable record stream with file and ordered-record digests; needs additional disk space. |

Both workflows rebuild the same state root, support `hash` and `path`, reopen
the target for independent verification, and refuse to overwrite an existing
output.

Both `migrate` and `import` use the geth state layout and accept independent
target choices: `--db-engine pebble|leveldb` (default Pebble) and an explicit
`--scheme hash|path`, for four supported combinations. The `--state-layout`
flag has been removed; passing it, including `--state-layout geth`, is an error.
Remove that flag from existing scripts.

`verify` reads the engine and layout from the report, checks the physical engine,
and independently validates geth keys. Reports declaring `legacy-l2geth` are
rejected; recreate those artifacts from the source snapshot or portable bundle.
Verification never falls back to another layout or repairs a damaged LevelDB.

## Offline legacy state pruning

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
are not flushed. See [measured results](docs/prune-performance.md).

## Build

Go 1.27 is required:

```bash
make build
./bin/l2state version
```

The version command reports the main-module and go-ethereum module versions
embedded by the Go toolchain. Local development builds may report a generated
pseudo-version or `(devel)` when VCS metadata is unavailable. Container builds
without VCS metadata report `(devel)` for the main module. The linked
go-ethereum module version is recorded in manifests and verification reports.
There is no manually maintained runtime geth commit value.

## Direct migration

Use `migrate` when the source snapshot can remain attached for the entire
operation:

```bash
./bin/l2state migrate \
  --source-chaindata /snapshot/geth/chaindata \
  --out /states/metis-hash \
  --scheme hash \
  --workers 4
```

Use `--scheme path` and a different output path to build a path-scheme
artifact. Direct migration does not create a bundle, record stream, or flat
staging pass. It divides the account-hash space into the 16 first-nibble MPT
partitions, preserves ascending order within each partition, and uses geth's
`PartialStackTrie` assembly to reproduce the canonical root and exact node
layout. Storage tries with at most 1024 slots stay on the serial streaming fast
path; larger tries are rebuilt through the same 16-way partitioning after a
bounded, uncommitted probe. Hash migration never creates temporary flat state;
path migration writes the required current flat state alongside the generated
path nodes before adoption.

`--workers` is a global account/storage work limit. Values below 2 are raised
to 2, values above 16 are rejected, and the default is the smaller of 4 and
`GOMAXPROCS`, with a minimum of 2. Increase it only when the source and target
storage can sustain the additional concurrent reads and writes. Within each
account partition, accounts without storage and without new code stay on a
serial zero-copy fast path. Encountering storage or the first reference to a
code hash starts a bounded burst: storage and code reads are processed in
parallel, then account and code writes are merged into the partition writer
and `PartialStackTrie` in strict hash order. A global window of twice
`--workers` bounds accounts and code bytes that have been read but not yet
merged. Light-account scans reuse their worker slot while no burst is pending,
then yield it so queued storage or code work cannot wait behind a complete
light-only partition. Waiting coordinators and accounts waiting for
large-storage subtasks do not hold worker capacity. The generated database is
still reopened and verified by the independent serial verifier before
publication; `--workers` affects construction only.

To repeat the full source-to-target proof later, retain the original snapshot
or a content-identical copy:

```bash
./bin/l2state verify \
  --source-chaindata /snapshot/geth/chaindata \
  --artifact /states/metis-hash
```

Direct verification recomputes the canonical source head and state, then
reopens and checks the target without modifying either database.

## Portable bundle migration

Export the legacy state once. Records are compressed with zstd by default:

```bash
./bin/l2state export \
  --source-chaindata /snapshot/geth/chaindata \
  --out /exports/metis-state-12345
```

Use `--compression none` for an uncompressed `state.records` file. A bundle
contains only the manifest and record stream:

```text
metis-state-12345/
├── manifest.json
└── state.records.zst
```

Account records use geth v1.17.5's canonical slim encoding: an empty storage
root and empty code hash are encoded as empty byte strings. Import and verify
strictly expand every account back to full consensus RLP before rebuilding the
account trie. In `manifest.json`, `state_file.record_payload_bytes` measures
the compact record payloads, while `counts.payload_bytes` measures the expanded
consensus payloads used to compare portable and direct migration results.
Portable import consumes this ordered stream with the same `StackTrie` rebuild
used for root validation and writes target trie nodes during that scan. Hash
imports never create temporary flat state; path imports write only the current
flat state required by geth before adopting the generated path trie.

Import the bundle into either scheme:

```bash
./bin/l2state import \
  --bundle /exports/metis-state-12345 \
  --out /states/metis-hash \
  --scheme hash

./bin/l2state import \
  --bundle /exports/metis-state-12345 \
  --out /states/metis-path \
  --scheme path
```

Verify the bundle by itself, or verify it together with an imported artifact:

```bash
./bin/l2state verify --bundle /exports/metis-state-12345

./bin/l2state verify \
  --bundle /exports/metis-state-12345 \
  --artifact /states/metis-path
```

Bundle verification recomputes the state-file SHA-256 digest, ordered
record-chain hash, account storage roots, overall state root, and code hashes.
Artifact verification additionally checks every reachable state entry, the
scheme-specific metadata, the selected header and head markers, and the exact
database inventory. Unexpected trie nodes, code, chain data, or metadata cause
verification to fail.

## Artifact contract

Both `migrate` and `import` publish the same top-level layout:

```text
metis-hash/
├── chaindata/          # Pebble v2 or LevelDB state database
└── verification.json  # Source, engine, layout, scheme, counts, root evidence
```

The database subdirectory is always `chaindata/`, matching geth naming.
`--out` and `verify --artifact` still refer to the artifact root containing
`chaindata/` and `verification.json`. Existing artifacts using `db/` need that
subdirectory renamed to `chaindata/` while the database is closed before
verification; there is no automatic fallback to `db/`.

The schemes differ inside `chaindata/`:

- `hash` contains the current hash-trie nodes and referenced contract code.
  Direct and portable migration write those nodes without temporary flat
  state.
- `path` retains current flat state and path-trie nodes, then records geth's
  completed snapshot metadata and state ID 0. It has no historical layers.

Consumers must open the database with its explicit `hash` or `path` scheme;
they must not rely on normal `chaindata` auto-detection. The artifact's root
and selected scheme are recorded in `verification.json`; `manifest.json`
records the bundle root and its supported schemes.

The geth layout stores code at `c + codeHash`, preserving consensus bytes.
Verification rejects unreferenced code, orphan trie nodes, and bare-hash code
entries from other layouts.

LevelDB targets are closed, every regular database file is synced, and the
database directory is synced before independent read-only verification and
atomic publication. This explicitly supplies durability because the pinned
geth LevelDB adapter's `SyncKeyValue` does not sync data.

Artifact reports record `db_engine` as `pebble-v2` or `leveldb` and explicitly
record `state_layout` as `geth`. Old reports omitting `state_layout` mean `geth`;
explicit `legacy-l2geth`, empty, null, and unknown values fail validation. Direct and bundle-backed reports use v1.
Pure bundle verification has neither target field; target engine and layout
choices do not alter the portable bundle format.

`UsingOVM` changes legacy execution and RPC interpretation, not MPT encoding.
The tool does not convert OVM balances into ordinary account balances or
execute a state transition. Source and target account RLP, storage-value RLP,
`OVM_ETH` storage, and contract code remain identical consensus data; only the
portable bundle's account payload uses the reversible slim representation.

## Formats and compatibility

- Bundles use `metis-l2state` format version 1.
- Bundle-backed verification reports use
  `metis-l2state-verification` version 1.
- Direct migrations use `metis-l2state-direct-verification` version 1.

During active development, all current schemas remain v1. Validators reject
versions 0, 2, and 3; there is no historical-format compatibility mode. Recreate
older bundles with the current `export` command and artifacts with `import` or
`migrate`. Changing a JSON version field is insufficient: the record-chain
domain is now `metis-l2state-record-chain/v1`, while the current slim-account
encoding and record framing are retained. A v1 label alone does not make an
older development format compatible.

`geth_version` is optional provenance, not an acceptance gate. Readers accept
any string, omission, empty string, or null for that field; other JSON types are
rejected. `geth_commit` has been removed from both inputs and outputs and is
rejected as an unknown JSON field. Recreate older development artifacts that
contain it. `tool_version` remains required and non-empty. These rules do not
change the pinned geth dependency or imply support for other database layouts.
Input-stability checks still compare provenance before and after verification.

Current validators also require the exact selected header RLP
and matching hash-to-number, canonical, `LastBlock`, and `LastHeader` metadata.
Artifacts produced by older builds without that evidence are rejected even if
they use the same report version; recreate them with the current `import` or
`migrate` command.

Inputs also have exact top-level layouts. Bundle and artifact roots,
their required files, and the artifact `chaindata` entry must not be symbolic links.
Extra top-level entries, including `.DS_Store`, README, or checksum files, are
rejected. Manifest and verification JSON files are limited to 1 MiB. An import
output must be outside its input bundle.

Generated JSON hashes and digests are lowercase, 32-byte, `0x`-prefixed geth
hashes. Header RLP is encoded as `0x`-prefixed bytes. Missing, malformed,
wrong-length, or unprefixed values are rejected.

The bundle record chain is unsigned integrity evidence. It detects missing,
corrupt, reordered, or modified records and binds them to the supplied header,
block hash, and state root. It does not establish a trusted timestamp, signer,
external canonicality, or L1 finality.

## Operational behavior

- `-h` and `--help` on every subcommand print that subcommand's usage to
  standard error and exit successfully without starting an operation or
  emitting JSON.
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
  keep this verification index inside the current `.partial-*` directory.
  Standalone artifact verification uses the operating system temporary
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
