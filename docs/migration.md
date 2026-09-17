# Migration workflows

[Documentation index](README.md) · [Project overview](../README.md)

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

The explicit `migrate --migrate-ovm-eth` mode is an exception: it converts OVM
balances, replaces the OVM_ETH runtime, and publishes a synthetic empty-block
checkpoint with the new root. Its independent report and six chain-data entries
are described under [OVM ETH conversion](ovm-conversion.md#ovm-eth-conversion). It still does not
produce a bootable full-node database.

The source must be a stopped l2geth database or a point-in-time-consistent
filesystem snapshot. Migration does not read from RPC and never repairs,
compacts, or writes to the source LevelDB. The separate offline `prune` command
described in the [pruning guide](prune.md) intentionally deletes old state in place.

## Choose a workflow

| Workflow               | Use it when                                                                                            | Evidence and storage tradeoff                                                                      |
| ---------------------- | ------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------- |
| `migrate`              | The source snapshot and destination are available together.                                            | Avoids creating a bundle, but later full verification requires the same source snapshot.           |
| `export` then `import` | The environments are separate, the state must be portable, or both schemes may be built from one scan. | Stores a portable record stream with file and ordered-record digests; needs additional disk space. |

Without OVM conversion, both workflows rebuild the same state root, support `hash` and `path`, reopen
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
