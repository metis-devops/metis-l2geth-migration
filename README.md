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
filesystem snapshot. `l2state` does not migrate from RPC and never recovers,
compacts, or writes to the source LevelDB.

## Choose a workflow

| Workflow | Use it when | Evidence and storage tradeoff |
| --- | --- | --- |
| `migrate` | The source snapshot and destination are available together. | Avoids creating a bundle, but later full verification requires the same source snapshot. |
| `export` then `import` | The environments are separate, the state must be portable, or both schemes may be built from one scan. | Stores a portable record stream with file and ordered-record digests; needs additional disk space. |

Both workflows rebuild the same state root, support `hash` and `path`, reopen
the target for independent verification, and refuse to overwrite an existing
output.

## Build

Go 1.27 is required:

```bash
make build
./bin/l2state version
```

The version command reports the main-module and go-ethereum module versions
embedded by the Go toolchain. Local development builds may report a generated
pseudo-version or `(devel)` when VCS metadata is unavailable.
Official images exclude `.git` from their build context and inject
`git-<full GitHub SHA>` explicitly, so manifests and verification reports retain
exact production-image provenance. Ad hoc Docker builds default to
`container-devel` unless `TOOL_VERSION` is supplied.

The module pins go-ethereum to v1.17.5 at commit
`9621c6ad10934a01b5514886fb6fbd87640b6c05`. It also pins a newer compatible
`cockroachdb/swiss` revision because the version selected by geth does not
build with Go 1.27; this does not change the pinned geth storage APIs.

## Direct migration

Use `migrate` when the source snapshot can remain attached for the entire
operation:

```bash
./bin/l2state migrate \
  --source-chaindata /snapshot/geth/chaindata \
  --out /states/metis-hash \
  --scheme hash
```

Use `--scheme path` and a different output path to build a path-scheme
artifact. Direct migration reads the legacy state once and does not create a
bundle or record stream. The same ordered `StackTrie` rebuild that validates
the source root writes the target trie nodes directly. Hash migration never
creates temporary flat state; path migration writes the required current flat
state alongside the generated path nodes before adoption.

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

Both import paths publish the same top-level layout:

```text
metis-hash/
├── db/                 # Pebble v2 state database
└── verification.json  # Source, scheme, counts, and recomputed-root evidence
```

The schemes differ inside `db/`:

- `hash` contains the current hash-trie nodes and referenced contract code.
  Direct and portable migration write those nodes without temporary flat
  state.
- `path` retains current flat state and path-trie nodes, then records geth's
  completed snapshot metadata and state ID 0. It has no historical layers.

Consumers must open the database with its explicit `hash` or `path` scheme;
they must not rely on normal `chaindata` auto-detection. The artifact's root
and selected scheme are recorded in `verification.json`; `manifest.json`
records the bundle root and its supported schemes.

`UsingOVM` changes legacy execution and RPC interpretation, not MPT encoding.
The tool does not convert OVM balances into ordinary account balances or
execute a state transition. Source and target account RLP, storage-value RLP,
`OVM_ETH` storage, and contract code remain identical consensus data; only the
portable bundle's account payload uses the reversible slim representation.

## Formats and compatibility

- Bundles use `metis-l2state` format version 3.
- Bundle-backed verification reports use
  `metis-l2state-verification` version 3.
- Direct migrations use `metis-l2state-direct-verification` version 1.

Version 3 validators reject version 2 bundles and bundle-backed reports; there
is no legacy compatibility mode, so recreate older bundles with the current
`export` command. Current validators also require the exact selected header RLP
and matching hash-to-number, canonical, `LastBlock`, and `LastHeader` metadata.
Artifacts produced by older builds without that evidence are rejected even if
they use the same report version; recreate them with the current `import` or
`migrate` command.

Version 3 inputs also have exact top-level layouts. Bundle and artifact roots,
their required files, and the artifact `db` entry must not be symbolic links.
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

## Development and test evidence

```bash
make ci
make test-race
```

The tests cover both schemes, zstd and uncompressed bundles, direct and
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
