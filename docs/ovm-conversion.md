# OVM ETH conversion

[Documentation index](README.md) · [Project overview](../README.md)

```bash
./bin/l2state migrate \
  --source-chaindata /snapshot/chaindata \
  --out /states/metis-converted --scheme path --db-engine pebble \
  --migrate-ovm-eth \
  --wrapped-ether-code /inputs/wrapped-runtime.hex \
  --ovm-state-witness /inputs/ownership.jsonl \
  --workers 8 --cache-mb 512 --handles 256

./bin/l2state verify \
  --source-chaindata /snapshot/chaindata --artifact /states/metis-converted \
  --wrapped-ether-code /inputs/wrapped-runtime.hex \
  --ovm-state-witness /inputs/ownership.jsonl --workers 8
```

`--migrate-ovm-eth` defaults to false. Its input flags are rejected without that
option. Verification selects the independent OVM report automatically and
requires the original code file and, if used, the original witness file.
When migration used `--ovm-genesis-alloc`, verification also requires that exact
original JSON file with the same flag; supplying it for an artifact that did not
use alloc overrides is rejected.
`--source-ancient /snapshot/ancient` overrides the default `source-chaindata/ancient`.
Keep the source, including any separate ancient directory, stopped or frozen for
the entire operation. Outputs must be outside both source directories.

The code file must be a regular non-symlink file, at most 1 MiB, containing one
nonempty, even-length `0x`-prefixed hex string; leading/trailing whitespace is
allowed. Supply **runtime** bytecode compatible with OVM_ETH storage: balances
at slot 0, allowances at slot 1, totalSupply at slot 2, name/symbol at slots 3/4,
and l1Token/l2Bridge at slots 5/6. The tool installs the supplied bytes without
running a constructor. It does not prove arbitrary bytecode implements wrapping
correctly; review the implementation independently. Ordinary WETH9's different
layout cannot be substituted directly. Other storage, including allowances and
name/symbol, is preserved.

Every source account's native balance must be zero. At the selected head,
addresses with empty code (including balance holders without an account leaf)
convert their entire OVM balance to native. Without a manual retention list,
contracts do the same unless the
complete canonical history contains an OVM_ETH `Transfer` with that contract as
`from` and an amount greater than zero. Such contracts retain their ERC20 balance
and zero native balance. Nonzero transferFrom and burns count; zero-value events
neither grant nor revoke retention eligibility, but still contribute addresses,
event counts and history digests. The rule does not prove the calling contract's
identity. Classification uses code at the selected head. Artifacts produced under
the former zero-value eligibility rule must be recreated if independent replay
under this rule produces a different result; the report format remains v1.

At `0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000`, the original self-held ERC20
balance is always retained. The contract's native backing and totalSupply both
become the sum of retained ERC20 balances. Converted balance slots are deleted.
Source balances must sum exactly to the original totalSupply. Any discrepancy,
nonzero source native balance, or uint256 overflow fails the operation; l2state
never repairs the source accounting. The old canary has inconsistent synthetic
supply and is not an eligible conversion snapshot.

### Manual ERC20 retention

Add `--ovm-erc20-retain-list /inputs/retain.txt` to OVM `migrate` to retain the
ERC20 balances of explicitly selected contracts, including newly funded pools
or vaults with no outgoing Transfer history. Supply the same flag and original
file to OVM `verify`. Ordinary migration, bundle workflows and prune do not
accept this policy input. Omitting the file preserves automatic classification.

The file contains one `0x`-prefixed, 40-hex-digit address per line:

```text
0x1000000000000000000000000000000000000001
0x2000000000000000000000000000000000000002
```

Leading/trailing whitespace, CRLF and an unterminated final line are allowed.
Each line is limited to 4095 bytes before LF. Blank lines, comments, malformed
addresses and normalized duplicates (including case aliases) fail. The file
must be regular and not a symlink. An empty file is valid, has no balance effect,
and still requires matching input presence and bytes during verification.

Every listed address must have code at the original canonical LastBlock,
including zero-balance entries. Missing accounts, EOAs, destroyed contracts and
OVM_ETH itself are rejected. OVM_ETH's self-held tokens already have their own
retention rule. GenesisAlloc code overrides cannot make an originally invalid
list entry eligible.

After validating the original migrated state and complete history, conversion
retains a contract's ERC20 balance if it is listed **or** has authenticated
nonzero Transfer-from history. Listed contracts keep zero native balance during
conversion. Addresses satisfying both criteria are counted once. EOAs always
convert, zero-value events never grant automatic eligibility, and the original
supply/accounting checks remain mandatory.

List addresses also identify their balance slots, so they need not be repeated
in the witness file. Eligibility uses a separate index: witness/alloc entries
never grant it and manual entries never fabricate historical events. Lists are
streamed into temporary Pebble with bounded batches and duplicate detection;
both disk and explicit memory temporary modes are supported.

OVM report v1 adds `erc20_retention.file_sha256` only when a list was supplied;
explicit null is rejected. The digest covers raw bytes, including whitespace
and address spelling. The file is rehashed before publication and verification
success. Standalone verification validates the source contracts and replays the
policy before independently checking the actual artifact; it does not create a
second final artifact. Existing retained-contract counts include manual and
automatic retention; history counts/digests continue to describe actual events.

GenesisAlloc still applies afterward. Its converted-state evidence includes
manual retention, and explicit native overrides can change the later total.
It still cannot modify OVM_ETH or repair invalid source state.

### GenesisAlloc account overrides

Optionally add `--ovm-genesis-alloc /inputs/alloc.json` to the OVM `migrate`
command and its subsequent `verify` command. The file is a geth
`core/types.GenesisAlloc` address-to-account JSON object, **not** a complete
genesis document with an `alloc` wrapper:

```json
{
  "0x1000000000000000000000000000000000000001": {
    "code": "0x602a60005260206000f3",
    "storage": {"0x01": "0x2a", "0x02": "0x00"}
  },
  "2000000000000000000000000000000000000002": {
    "balance": "0x1234",
    "nonce": 1
  }
}
```

The source and authenticated history are fully validated first. OVM balance
conversion uses code at the original selected head and must pass all existing
ownership, zero-native-balance and supply checks. Overrides are then merged into
the converted state, before building the final artifact and checkpoint. Alloc
addresses do not serve as ownership witnesses or grant retention eligibility.

| Account input | Effect after OVM conversion |
| --- | --- |
| Omitted field | Keep its converted value |
| `code` | Install these runtime bytes without executing a constructor; `"0x"` clears code |
| `balance` | Set the final native balance, including zero; nonnegative uint256 |
| `nonce` | Set the final nonce, including zero; uint64 |
| `storage` | Merge listed slots; zero deletes a slot, omitted slots remain |
| `storage: {}` or an empty account object | No changes from that input |

For new addresses the starting values are zero balance/nonce, empty code and
empty storage. Applying an explicit scalar field or a storage entry creates an
account leaf, even if all resulting values are zero; empty objects alone do not
create accounts. There is no whole-account deletion or whole-storage replacement.
Unlike geth's genesis decoder, this overlay permits omitting `balance`, so code
and storage can be changed without overwriting a converted balance. Other field
encodings follow pinned geth v1.17.5: 20-byte addresses with or without `0x`,
`0x`-prefixed even-length code hex, hexadecimal or decimal balance/nonce, and
even-length storage hex of at most 32 bytes, optionally prefixed with lowercase
`0x` and left-padded to 32 bytes. Storage JSON fields must be strings.

The OVM_ETH address `0xDeadDeAddeAddEAddeadDEaDDEAdDeaDDeAD0000` is forbidden in
alloc, even with an empty object. Its code continues to come from the required
`--wrapped-ether-code` file; conversion alone determines its backing and supply.
Overriding other accounts' native balances can increase or decrease the final
native currency total. The report's `balances` describes the **conversion stage**,
not the native totals after operator overrides.

The alloc file must be a regular non-symlink file and remain byte-for-byte
unchanged throughout migration or verification. Reject nulls, unknown/repeated
fields, duplicate addresses or storage slots (including equivalent hex spellings),
malformed/out-of-range values and trailing JSON. Each account's code is limited
to 1 MiB. Accounts and storage are streamed to a private disk index, with bounded
batches and JSON tokens; total file size and storage slot count are not capped.
JSON token bounds admit the maximum code size even with JSON Unicode escapes.
Runs of whitespace outside strings are streamed to the decoder as one separator,
so padding cannot grow its buffer with the file size. File digests still cover
every original byte, including whitespace; whitespace-only changes are rejected.

OVM verification remains version 1. With overrides, `genesis_alloc` records
`file_sha256` and `converted_state` (the root and counts before overrides).
`target_state` and `checkpoint` describe the final overridden state. Without
overrides, `genesis_alloc` is omitted; explicit null is invalid. Independent
verification replays both stages using the original inputs and then verifies the
actual target's complete inventory. The extra evidence and temporary index are
exclusive to OVM conversion; ordinary migrate, portable workflows and prune do
not accept the new option.

To measure the additional cost on the synthetic OVM fixture, run
`python3 scripts/benchmark-ovm.py --out /absolute/new/results.txt --with-alloc`.
This pairs ordinary conversion with 1,000 account overrides in fresh processes,
alternating order across samples. Run it separately from CI or other benchmarks.
See [consolidated OVM measurements](benchmarks/ovm.md#alloc-overhead-within-the-latest-source)
for the measured overhead and synthetic-fixture limits.

### History and storage ownership

The tool reads all canonical headers and receipts from genesis through LastBlock,
checks parent links, hashes, receipt roots, bloom and cumulative gas, and rejects
missing/corrupt/conflicting hot or ancient history. It reads legacy freezer files
directly with read-only descriptors; it does not open a repairing freezer or
create locks or metadata. Historical bodies and execution traces are not required.
This validates commitments and event membership, not historical EVM re-execution.

`LastBlock` remains the history cutoff even when fast sync has already stored
higher blocks in ancient or advanced `LastFast`/`LastHeader`; those later events
do not grant retention eligibility. Overlapping hash/header copies must match
byte-for-byte. Overlapping receipts may use different supported legacy storage
encodings: both copies must decode and match the canonical header's receipt root,
gas and bloom. The cold encoding remains the history-digest input. Missing,
empty, malformed or conflicting required history fails without rewriting source
files; both differing receipt copies count toward the queue's byte budget.

Transfer/Approval addresses, valid address preimages and the optional JSONL
witness identify hashed storage slots. Legacy native transfers need not emit
Transfer events, so a witness can be necessary even with complete history.
Each nonblank input line (maximum 4095 bytes excluding newline) must be one of:

```json
{"type":"address","address":"0x1000000000000000000000000000000000000001"}
{"type":"allowance","owner":"0x2000000000000000000000000000000000000002","spender":"0x3000000000000000000000000000000000000003"}
```

Blank lines, nulls, unknown/repeated fields and malformed addresses fail.
Repeated identical records are harmless and deduplicated on disk. Files do not
specify balances or grant retention eligibility. Every nonzero storage slot must
be identified as a balance, allowance, or supported metadata/string slot; unknown
or ambiguous slots fail instead of being silently retained. Add the missing
holder or allowance pair to the witness and restart with a new output path.

### Resources, checkpoint and evidence

Original-state copying to a private hash database, address-preimage collection
and canonical history scanning run concurrently. After all three finish, the
original copy is independently reopened and verified before conversion begins.
The transformed account/storage tries are then rebuilt into a fresh
final target; no obsolete trie nodes or unreferenced old code are published.
Other accounts that still reference the old code keep it. Hash/path and
Pebble/LevelDB remain supported, including path completion metadata and state ID 0.

Account/storage work, preimage scanning, history reading/decoding and balance
classification share the global 2–16 worker limiter. Account and balance queues hold at most twice the
worker count. History queues also cap encoded bytes at cache/8 (1–16 MiB); an
oversize atomic source record drains the queue and is processed synchronously.
Balance readers reuse decoded account-trie paths for at most 32 account reads
per reader, with at most one reader per worker; alloc uses one such bounded
reader. Ancient scanning keeps only the current data-file handle for each of
its three tables and checks file identity/metadata on rotation and close.
These are working-queue limits, not a total RSS or maximum source-record size.
Evidence and patches use disk-backed Pebble by default, or Pebble memory files
with `--temp-db memory`. Cache and handles must each be at least 64;
four concurrent database allowances each receive one quarter. Independent node
inventory checks also use the existing bounded temporary index.

With the default `--temp-db disk`, allow disk space for the original state, conversion scratch nodes, evidence and
final target simultaneously. Standalone verification independently replays the
original state and conversion in a temporary sibling directory, but does not
write another final artifact. Its `replay_converted_state` phase recomputes the
expected root and counts before the actual artifact receives full state and
inventory verification. Source access and scratch space for original state,
conversion nodes, evidence and inventory indexes are still required. All supplied
code, witness and alloc file digests are confirmed over raw bytes immediately
before publication or verification success; confirmation neither reparses the
inputs nor rewrites their evidence index.

See [OVM measurements](benchmarks/ovm.md) for the latest standalone
verification comparison and separately labeled historical ancient-read results.

The checkpoint has height `LastBlock+1`, parent hash equal to LastBlock, the new
state root and timestamp `parent+1`. Its gas limit is inherited; difficulty,
coinbase, nonce, mixDigest, gasUsed and bloom are zero. Transaction/receipt roots
and uncle hash are empty; optional fork fields are omitted. Extra data is
`metis-l2state-ovm/v1`. It has a real empty body. This synthetic migration boundary
does not claim normal consensus validity. Only its header, hash/number mappings,
LastBlock, LastHeader and empty body are stored; no old header, receipts, TD,
chain configuration or historical state is copied.

`verification.json` uses `metis-l2state-ovm-verification` v1. It records source and
checkpoint header evidence, original/final roots and counts, code/input digests,
complete-history/event digests, classified holder counts, migrated amount,
remaining supply and self-held balance. Ordinary direct and bundle reports retain
their existing v1 contracts and source-root equality checks. Progress remains on
stderr; stdout contains a single final JSON value. Cancellation removes only this
invocation's staging data; publication durability failures retain the final path.

Use `scripts/benchmark-ovm.py --out /absolute/new/results.txt --count 3` for repeated
paired synthetic worker measurements, without concurrent CI or benchmarks.
The test runtime and logical source fixture live in `testdata/ovm-conversion`
under the migration package; ordinary tests never regenerate them.
