# OVM implementation rules

[Documentation index](../README.md) · [Repository guidance](../../AGENTS.md)

## Opt-in OVM balance conversion

- `migrate --migrate-ovm-eth` is an explicit exception to consensus-byte and
  original-head preservation. Default migrate, portable workflows and prune
  retain their existing boundaries. `ovm_*.go` owns the conversion implementation.
- Require an operator-supplied storage-compatible wrappedEther runtime hex file;
  never install the test runtime by default or execute a constructor. All source
  native balances must be zero. Reconcile every identified balance against the
  original totalSupply by default. The only discrepancy exception is the exact,
  fully explained `--ovm-audit-summary` contract below; unknown storage and
  unmatched/unexplained accounting discrepancies fail closed.
- An empty or omitted `--ovm-audit-summary` means no audit input; it must not
  bypass strict supply equality or verification of recorded audit evidence.
  A nonempty path accepts only the existing mvm summary/identity v1,
  as a regular non-symlink file of at most 1 MiB with strict required fields,
  duplicate/unknown/null rejection and canonical integer strings. Require full
  execution/accounting/cause completion, zero genesis discrepancy, zero unexplained
  amount AND event count, and exact component/cause arithmetic. For audit endpoint
  H and current LastBlock L, reject H > L early. Authenticate genesis and H's
  hash/root in the existing canonical hot/ancient history lane and require that
  H was seen before conversion; keep scanning through L. Do not load historical
  state tries or add another history scan. H == L retains full snapshot matching;
  H < L requires matching current OVM code hash and exact B_L - S_L == D_H, but
  does not compare current balance sum, supply or slot count to historical values.
  Select the branch by height, including when roots are equal. Never hardcode a
  Shanghai boundary or relax current unknown-storage/native/code/root checks.
  Never use diagnostic prior amounts as allowances, audit
  witnesses as migration witnesses, or audit addresses as retention policy.
- With accepted audit input, conserve actual B = M + R instead of S = M + R;
  preserve every holder's policy-determined amount, original S in the report, and
  R as target ERC20 supply/native backing. Never subtract an arbitrary balance or
  award the difference twice. Keep alloc after conversion. Default conversion
  still requires B = S; zero-difference audit inputs retain explicit presence.
- OVM report v1 optionally adds `audit_summary` with `file_sha256`, `balance_sum`
  (uint256) and `supply_difference` (canonical signed decimal), computed from the
  current conversion before alloc, not historical summary amounts. For H < L,
  include optional `audit_head` using block_number/block_hash/state_root; omit it
  for H == L to preserve existing same-height encoding. Validate 0 < H < L and
  strict required hash/number fields; verify must reconstruct it from the input
  and canonical history and reject removal/tampering. The audit's complete cause
  explanation ends at H; matching net discrepancy does not rule out later
  offsetting anomalies. Reject null or incomplete evidence. Reject duplicate top-level report fields (including
  case-folded aliases) before typed decoding can merge them. Verify requires
  identical input presence/bytes, replays
  source matching, and independently inventories the target. Confirm raw input
  hashes before success/publication. Do not copy files into the artifact, modify
  mvm, depend on its module at runtime, scan historical state again, or claim an
  input hash proves historical execution. Keep config/witness hashes as provenance.
  Exercise four targets, both temp modes, M > S, retained/self balances, alloc,
  independent reference inventory, runtime/continuation, tampering and cleanup.
  Benchmark no input, same-height and historical input on an identical current
  source via `--with-historical-audit` in the isolated, alternating harness.
  Retain the old same-height-only `--with-audit-summary` measurements separately.
- Classify by code at the selected head. Authenticated canonical OVM_ETH
  nonzero Transfer-from membership or an explicit validated ERC20 retain-list
  entry retains a contract's ERC20 balance (including nonzero transferFrom and
  burns for automatic eligibility). Zero-value events still discover addresses
  and enter event counts/digests, but never grant or revoke membership. EOA
  balances always convert. OVM_ETH self
  holdings always remain, and its native backing equals retained totalSupply.
- `--ovm-erc20-retain-list` is an OVM-only manual retention policy, separate from
  witnesses, history and GenesisAlloc. Stream one 0x-prefixed 20-byte address per
  line (at most 4095 bytes before LF), accepting surrounding whitespace/CRLF and
  missing final newline; reject blank/comment/malformed lines, normalized
  duplicates and non-regular/symlink files. Empty files have explicit presence.
  Store eligibility and bounded duplicate detection in temporary Pebble, in a
  namespace separate from Transfer-from evidence; addresses also identify balance
  slots. Never put manual entries into the historical membership index.
- Independently verify the original migrated state before checking every retain
  address against its original code, including zero-balance entries. Reject EOAs,
  nonexistent/destroyed accounts and OVM_ETH; alloc cannot fix list eligibility.
  Check manual membership only for contracts and only when a list was supplied.
  Preserve conservation and source validation, count overlapping eligibility once,
  and apply alloc afterward. Reuse bounded account readers and worker allowances.
- OVM report v1 optionally records `erc20_retention.file_sha256`; omit it when
  unused and reject null/invalid digests. Verify input presence and raw bytes,
  replay policy with validation-only output, and rehash before publication or
  verification success. Witnesses never grant manual eligibility. Cover four
  targets, both temporary modes, source-head classification, independent inventory,
  runtime/continuation, alloc interaction, duplicates across batches, tampering,
  source immutability, cancellation and cleanup. Use benchmark `--with-retain-list`
  to compare absent/present lists on the same source-contract fixture.
- Read complete legacy headers/receipts, including freezer files, physically
  read-only. No restoring freezer constructors, recovery, writes or RPC fallback.
  Stop at the selected `LastBlock` even when ancient or fast/header heads are
  further ahead. Hash/header overlaps must match bytes; differently encoded
  legacy receipt overlaps must both match the canonical receipt root, gas and
  bloom. Use the cold encoding for history evidence and budget both raw copies;
  never ignore malformed/empty copies or rewrite source receipts to normalize them.
  Witnesses provide addresses and allowance pairs, never balances or eligibility.
  Keep evidence/patches in operation-local Pebble (disk by default, or explicit
  `--temp-db memory`) and reject unclassified storage slots.
  Hot-history reads use one LevelDB Get; only its not-found sentinel is absence.
  Empty records and other read failures must still fail before ancient fallback.
  Witness lines use one typed JSON decoding pass with explicit field/duplicate/null
  and end-of-input checks. Keep the frozen two-pass decoder as a differential test
  oracle; raw input hashing and the 4095-byte line bound remain unchanged.
- Complete and independently reopen the original migrated state before applying
  balance changes. Build the final artifact afresh with the partitioned core.
  Run original-state copying, preimage collection and canonical history scanning
  concurrently. Evidence lanes share the database but own separate bounded
  batches; only identical address entries may overlap. Preimage iteration,
  hashing and writes borrow the global limiter and yield between records.
  Preserve ordered history acceptance and digests. Cancel siblings on failure,
  join all three lanes and require both evidence flushes before reopening the
  original state or starting conversion; close each batch before its database.
  Share the worker limiter with history and balance workers; join all jobs on
  errors/cancellation. Preserve record/byte queue bounds and path state ID 0.
- The synthetic checkpoint uses parent height/time +1, the new root, inherited
  gas limit, empty body/transaction/receipt/uncle commitments, zero execution and
  consensus fields, no optional fork fields, and extra `metis-l2state-ovm/v1`.
  Its six chain-data KVs are the only metadata exception; it is not a bootable
  database or a normally validated consensus block.
- Keep `metis-l2state-ovm-verification` v1 independent of ordinary reports.
  Standalone OVM verification replays source/history/operator inputs before
  checking the actual target's full inventory. Never relax ordinary root equality.
- Verification replay must independently reopen/verify the original migrated
  state, then compute final root/counts with validation-only partitioned output;
  do not materialize a second final artifact. Preserve the actual artifact's
  independent engine/scheme/inventory verification and scratch cleanup.
  Use the common standalone verification workspace (`--temp-dir`, default system
  temporary directory), outside source/ancient/artifact including aliases. Do not
  create scratch beside the artifact implicitly; read-only artifact parents work
  in both temporary modes. Zero-native-balance validation remains OVM policy
  injected into the shared traversal before account scheduling.
- Storage classification reuses three ordered evidence cursors (balance,
  allowance and metadata), checking every namespace for ambiguous ownership.
  Evidence is flushed and immutable in these namespaces during the scan. Bound
  each cursor to eight forward steps before reseeking so sparse witnesses do not
  cause an unbounded scan. Release iterators and propagate their errors.
- Stage fixed-size balance jobs in the existing temporary Pebble index, keyed by
  account hash, then dispatch in that order. This improves trie-path locality
  without increasing decoded-trie windows or keeping a state-sized memory map.
  Queue records contain address, storage hash and uint256 value (84 bytes plus a
  33-byte key before database overhead); memory temporary mode keeps these files
  in RAM too. Preserve all slot checks, conservation, eligibility and source-head
  classification. Keep the two-times-workers job bound, ordered application,
  cancellation and joins; borrow the shared limiter while advancing the job
  iterator and release it before waiting for balance jobs.
- Account readers may reuse decoded trie paths for at most 32 reads before
  dropping the trie. Balance readers are exclusive to a worker lease, with no
  more than the worker count retained; alloc uses one bounded reader. Never
  share a mutable trie concurrently or retain an operation-wide decoded trie.
- The Pebble evidence index uses single Get lookups and treats only its
  not-found sentinel as absence. EOA conversion does not query Transfer-from
  membership. Input confirmation hashes bounded raw-byte chunks without
  reparsing or rewriting evidence, and rechecks all supplied runtime/witness/
  alloc/retain-list/audit-summary inputs immediately before publication or verification success.
- Ancient readers retain at most one current read-only data handle per table
  (three total), verify file identity/size/mtime on rotation and close, and
  propagate validation/close errors. Do not introduce freezer constructors.
- `--ovm-genesis-alloc` is an optional OVM-only post-conversion overlay. Accept a
  GenesisAlloc address map with geth v1.17.6 field encodings, additionally allowing
  omitted balance. Preserve omitted fields, merge storage by slot (zero deletes),
  apply explicit code/balance/nonce including empty code and zero, and reject
  OVM_ETH even for empty entries. Empty account/storage objects do nothing;
  explicit scalar fields or storage entries create absent accounts even when the
  result is empty. No whole-account deletion or storage replacement is supported.
  Never use alloc addresses as witnesses or classify by overridden code. Finish
  all original-state/history/accounting validation and conversion before applying
  alloc; overrides cannot repair invalid source state.
- Stream alloc and its normalized duplicate detection into the operation-local
  disk index with bounded batches/tokens and independent patch namespaces. Do not
  retain a full GenesisAlloc map or whole-account storage in memory. Limit each
  runtime to 1 MiB. Reject null, unknown/duplicate fields, address/slot aliases,
  malformed/overflow values, trailing data and symlink/non-regular inputs. Hash
  the original file and compare it again before publication or verification
  success. Explicit native balance overrides may change the final currency total.
  Consume whitespace outside JSON strings with bounded memory, preserving token
  separation and all string bytes; compute both initial and confirmation digests
  over raw input before any whitespace folding. Cover long leading/inter-token/
  trailing whitespace, cross-read escapes, read errors and cancellation.
- When alloc is used, OVM report v1 includes `genesis_alloc.file_sha256` and
  `genesis_alloc.converted_state` (root/counts before overrides); `balances` remains
  conversion-stage evidence and `target_state`/checkpoint describe the final
  overlay. Omit `genesis_alloc` when unused and reject explicit null. Verification
  requires matching input presence and bytes, replays both stages and inventories
  the actual artifact. Keep ordinary report and bundle schemas unchanged.
- Exercise four target combinations, independent serial state conversion,
  legacy-module receipt/state reading, wrapped runtime execution, continuation,
  history failures, input/report tampering, source immutability, concurrency and
  cancellation. Do not regenerate the old canary or geth compatibility corpus.
  The old canary's inconsistent synthetic supply must fail conversion validation.
  Finish with `make ci`, `make test-race`, and paired OVM measurements in isolated
  processes; report synthetic measurement limits and any regressions.
  Alloc changes additionally cover geth JSON interoperability, sparse/zero
  fields, new/empty accounts, large storage and normalized duplicates across
  batches, original-head classification, reference StateDB/GenerateTrie inventory,
  runtime execution/continuation, input/report tampering and cancellation/cleanup.
  Use `scripts/benchmark-ovm.py --with-alloc` for paired conversion/overlay costs.
  Optimization measurements can add `--baseline-root /absolute/isolated/checkout`
  (same benchmark harness), `--with-verify` and `--with-ancient`; alternate
  baseline/current in fresh processes without concurrent CI or benchmarks.
