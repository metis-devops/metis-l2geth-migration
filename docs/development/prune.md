# Prune implementation rules

[Documentation index](../README.md) · [Repository guidance](../../AGENTS.md)

## Offline prune

- Support only stopped legacy LevelDB/hash full-node databases that have never
  enabled LES service. Reject detected LES metadata and geth/mixed state layouts
  before any deletion. Do not infer that all 32-byte keys are state.
  Match LES balance keys by their version prefix and complete ID/address shape;
  never classify ordinary hash-index keys by substrings inside their hashes.
- Retain the complete canonical `LastBlock` state plus the genesis root-node
  KV (unless the genesis trie is empty). Preserve every non-state record,
  head marker and ancient file; no genesis rewriting or recent-root window.
- Record actual raw state reads in an operation-local, bounded-cache Pebble
  keep database; never use generated geth code keys or StackTrie output as the
  original key inventory. Keep shared physical node/code records exactly once.
  Use the shared partitioned execution core with a validation-only output;
  migrate retains its ordinary target writer. Both modes retain the global
  worker limiter, twice-workers account window and 1024-slot storage probe.
  Shard keep batches by key, validate every captured content hash, and latch
  capture/flush failures independently of trie-reader error propagation.
  Do not restore per-record keep-database Has/Get calls or a state-sized node set.
- Validate the whole current state and independently reopen the keep database
  before deletion. Delete only absent 32-byte keys whose values hash to the key.
  Retain and count unknown records; fail on corruption in the retained state.
- Hold the LevelDB filesystem lock across read-only preflight, writable deletion
  and independent read-only verification. Keep the general readonlydb adapter
  immutable, including borrowed views. Never call Recover/RecoverFile; normal
  strict WAL replay on writable open is permitted for prune only.
  All prune opens must resolve only the exact valid `CURRENT` manifest without
  calling goleveldb's restoring/fallback GetMeta. Reject missing or malformed
  CURRENT/manifest and pending CURRENT.N publications; do not use CURRENT.bak.
- Use bounded, synced deletion batches. Verify head, state, genesis root-node
  bytes and the ordered, length-framed digest/count of all protected KV after
  deletion. Close and sync regular database files plus directory before success.
  Compare source/keep via ordered cursors, rejecting missing keep keys. Preserve
  the exact v1 digest framing/order through parallel hashing and ordered output.
  Limit scan queues to twice-workers records and cache/8 bytes (1–16 MiB), with
  oversize records processed synchronously after draining. Reserve a coordinator
  slot and keep candidate hashing within the worker allowance. Sync delete
  batches at 16384 keys or 1 MiB encoded size; report only committed deletions.
  Workers follow migrate's normalization/defaults. Allocate one third of cache
  and handles to the keep database (minimum 16 each), with the remainder assigned
  to the source; these are database allowances, not a total RSS limit.
- Default to no explicit full compaction; `--compact` opts in. Do not promise
  instantaneous cancellation during CompactRange or physical bytes reclaimed.
- `--dry-run` is physically read-only and incompatible with `--compact`.
  Require an existing regular LOCK before a dry-run storage open: goleveldb's
  read-only opener otherwise creates it. Keep filter.NewBloomFilter(10) enabled
  in prune's LevelDB options so reads use, and compaction preserves, legacy filters.
  Interrupted pruning restarts by building a fresh keep set, without reusing
  incomplete sets or rolling back deletions. Only clean this invocation's
  temporary directory; never write progress journals into chaindata.
- Prune result v1 is independent from existing bundle and verification formats.
  Preserve stdout JSON/stderr progress separation and the uint256 boundary.
