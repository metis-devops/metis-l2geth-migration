# Artifacts and formats

[Documentation index](README.md) · [Project overview](../README.md)

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

Artifact reports record `db_engine` as `pebble` (Pebble v2) or `leveldb` and explicitly
record `state_layout` as `geth`. Reports using the retired engine identifier
are rejected; recreate those artifacts. Old reports omitting `state_layout` mean `geth`;
explicit `legacy-l2geth`, empty, null, and unknown values fail validation. Direct and bundle-backed reports use v1.
Pure bundle verification has neither target field; target engine and layout
choices do not alter the portable bundle format.

`UsingOVM` changes legacy execution and RPC interpretation, not MPT encoding.
Without the explicit OVM conversion option, the tool does not convert balances or
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
