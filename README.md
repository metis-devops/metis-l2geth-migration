# l2state

`l2state` exports the latest executed state from a legacy Optimism/Metis l2geth database into an auditable bundle, then imports that bundle into a standalone hash- or path-scheme state database readable by `go-ethereum v1.17.5`.

It migrates only the current accounts, storage, and contract code committed to by the `LastBlock` header. It does not migrate historical state, block bodies, transactions, receipts, chain configuration, or trie preimages. The imported artifact is not a complete `chaindata` directory from which an unmodified geth node can boot.

## Build

Go 1.27 is required:

```bash
make build
./bin/l2state version
```

geth is pinned to `v1.17.5`, commit `9621c6ad10934a01b5514886fb6fbd87640b6c05`. The older `cockroachdb/swiss` release pulled in by geth v1.17.5 does not declare support for Go 1.27, so the root module pins a later commit that is compatible with Go 1.27. The geth version and storage APIs are unchanged.

## Export

Stop l2geth first, or copy a point-in-time-consistent LVM, EBS, ZFS, or equivalent snapshot to the local machine. The tool accepts a LevelDB `chaindata` directory directly; it does not read from a live RPC endpoint:

```bash
./bin/l2state export \
  --source-chaindata /snapshot/geth/chaindata \
  --out /exports/metis-state-12345
```

Records use zstd compression by default. To produce an uncompressed record stream:

```bash
./bin/l2state export \
  --source-chaindata /snapshot/geth/chaindata \
  --out /exports/metis-state-12345-raw \
  --compression none
```

The source LevelDB is opened through a dedicated adapter in strict read-only mode. The adapter never performs recovery, compaction, or writeback. Export fails if another l2geth process still holds the database, the database is corrupt, or the latest trie is missing nodes.

Bundle layout:

```text
metis-state-12345/
├── manifest.json
└── state.records.zst
```

JSON evidence uses geth's hexadecimal conventions: `header_rlp` is encoded as `hexutil.Bytes`, and every 32-byte hash or digest is encoded as a `common.Hash`. Generated values are lowercase and `0x`-prefixed; unprefixed digest strings are rejected.

The record stream contains only:

- an account hash and the complete consensus account RLP;
- an account hash, slot hash, and the original MPT storage-value RLP;
- a code hash and its bytecode.

Address and slot preimages are outside the export scope. Ordinary geth state queries still work because `StateTrie` hashes the addresses and slots supplied by the caller.

`UsingOVM` controls how a legacy client executes transactions and interprets state through RPC; it is not a state-trie encoding parameter. The migration tool neither reinterprets OVM balances as ordinary account balances nor executes a state transition. It preserves account leaves, `OVM_ETH` storage, and code byte for byte, so the OVM semantics captured in a real database are not lost merely because the main tool does not reference `rcfg`.

## Import

The same bundle can be imported separately for each scheme. The output directory must not exist; the tool never overwrites existing data.

Hash scheme:

```bash
./bin/l2state import \
  --bundle /exports/metis-state-12345 \
  --out /states/metis-hash \
  --scheme hash
```

Path scheme:

```bash
./bin/l2state import \
  --bundle /exports/metis-state-12345 \
  --out /states/metis-path \
  --scheme path
```

Artifact layout:

```text
metis-hash/
├── db/                 # Pebble v2 state database
└── verification.json  # Recomputed import evidence
```

Both import modes use `triedb.GenerateTrie` from v1.17.5 to build a canonical MPT from the flat records:

- `hash` deletes and compacts the temporary flat state after root verification, leaving only the hash-trie nodes reachable from the current root and prefixed contract code;
- `path` retains the current flat state and path-trie nodes, then uses `AdoptSyncedState` to write the completion marker, `SnapshotRoot`, and state ID 0. It does not create historical layers.

Because a hash artifact intentionally contains no genesis header, geth's automatic scheme detection cannot identify it. Library callers must select `triedb.HashDefaults` explicitly. Explicit path configuration is also recommended for path artifacts. For either scheme, obtain the state root from `manifest.json` or `verification.json`.

## Independent verification

Verify a bundle only:

```bash
./bin/l2state verify --bundle /exports/metis-state-12345
```

Verify a bundle and an imported database together:

```bash
./bin/l2state verify \
  --bundle /exports/metis-state-12345 \
  --artifact /states/metis-path
```

Verification does not trust the conclusions stored in `verification.json`. It rereads the bundle, reopens the target Pebble database in read-only mode, and performs the following checks:

1. Recompute the block hash from the header RLP and check the block number and state root.
2. Verify the compressed-file SHA-256 digest and the ordered-record Keccak chain.
3. Recompute every account's storage root.
4. Recompute the overall state root from all account leaves.
5. Verify every non-empty code hash.
6. For path mode, compare every flat-state entry with the trie.
7. Inspect the database key inventory and reject block, head, receipt, history, or other non-state data.

The record chain is seeded by the header evidence:

```text
H0 = keccak256(domain || blockNumber || blockHash || stateRoot || keccak256(headerRLP))
Hi = keccak256(Hi-1 || canonicalRecordFrame)
```

This is an unsigned, self-verifying evidence chain. It detects missing, corrupt, reordered, and semantically modified records, and proves that the imported state matches the supplied header and block hash. It does not provide a trusted timestamp or signer identity, and it does not prove that the block hash is canonical according to an external RPC endpoint or L1.

## Progress logs

The `export`, `import`, and `verify` commands write human-readable geth-style progress logs to standard error. The final machine-readable result remains the only value written to standard output, so the two streams can be captured independently:

```bash
./bin/l2state import \
  --bundle /exports/metis-state-12345 \
  --out /states/metis-hash \
  --scheme hash \
  >import-result.json \
  2>import-progress.log
```

Phase transitions are logged immediately, and a long-running phase emits an update every 30 seconds. Bundle import and verification report exact record progress against the manifest totals. Trie-generation percentages are estimates based on progress through the uniformly distributed account-hash keyspace. Export reports processed accounts, storage slots, code references, records, payload bytes, and throughput, but not a percentage or ETA because discovering the total would require an additional full source-state traversal.

Use `--quiet` with any of the three commands to suppress progress logs. It does not suppress flag diagnostics or the final error printed when an operation fails.

## Resources and failure recovery

- `--cache-mb` defaults to 512 MiB and `--handles` defaults to 256; adjust them to the machine's limits.
- Export needs at least enough free space for the bundle. Import needs space for the temporary flat state, target trie, and compaction at the same time.
- If the operation is canceled or any verification step fails, the tool removes only the sibling `.partial-*` directory created by that invocation. The final output path never appears.
- Resume is not supported. Rerun the operation with an output path that does not exist.

## Tests

```bash
make ci
make test-race
```

The default test suite covers:

- zstd and uncompressed formats, digests, trailing data, and semantic ordering;
- legacy database to bundle to hash/path database migrations followed by v1.17.5 API queries;
- committing new state on both imported roots and reopening it for reads;
- fail-closed paths such as missing code, a non-canonical head, a corrupt archive, and an existing output;
- a fixed KV canary generated by `github.com/MetisProtocol/mvm/l2geth` with the default `UsingOVM=true` and with preimages removed. Ordinary account balances remain explicitly zero, while user balances are written only to `OVM_ETH` storage when `ovmBalance > 0`;
- an `OVM_ETH` account containing the complete ERC-20 bytecode and non-zero base storage from the official Andromeda state dump. The fixture is pinned to `metis-networks` commit `696b5613df9c`, with code hash `0xcaf944c2…57f44`.

The canary generator lives in `testdata/legacyfixturegen` and pins only `github.com/MetisProtocol/mvm/l2geth` commit `e795a258d3f2`. Its embedded `ovm-eth-andromeda.json` records the official OVM_ETH allocation and its fixed provenance, and the generator decodes it directly through the legacy `core.GenesisAccount` type. The generator is used only when maintaining the fixture; it is not part of the root module or its runtime dependencies. Generate into a fresh temporary directory, then compare the result with the committed fixture:

```bash
cd testdata/legacyfixturegen
go run -mod=mod . /tmp/l2state-legacy-fixture
```
