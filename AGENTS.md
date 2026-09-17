# Repository guidance

## Mission and boundaries

`l2state` migrates the latest executed state from legacy Optimism/Metis l2geth
LevelDB. Ordinary migration preserves the canonical `LastBlock` accounts,
storage and code, plus exactly five chain-metadata entries: the selected header,
its hash-to-number mapping, its canonical number-to-hash mapping, `LastBlock`
and `LastHeader`. The output is a state-only artifact, not bootable chaindata.

Migration, export and verification must never write, repair or compact the
source. Require a stopped database or a consistent filesystem snapshot.
Two explicit exceptions have separate contracts:

- `migrate --migrate-ovm-eth` converts balances and publishes a synthetic
  checkpoint; it does not change ordinary or portable migration semantics.
- `prune` deletes old state in place in a stopped legacy full-node LevelDB;
  it does not generate migration artifacts.

## Required reading

The linked development documents are mandatory repository rules, not optional
background. Before editing, read the common rules and every topic relevant to
the change. Paths in these documents are relative to the repository root unless
stated otherwise; run command examples from that root.

| Scope | Required rules |
| --- | --- |
| Every change | [Immutable contracts](docs/development/contracts.md), [validation and format evolution](docs/development/validation.md) |
| Implementation changes | [Code map and shared rules](docs/development/implementation.md) |
| OVM conversion, history, witnesses, retain lists or alloc | [OVM rules](docs/development/ovm.md) plus shared rules |
| Offline pruning | [Prune rules](docs/development/prune.md) plus shared rules |
| Operator-facing behavior | Relevant guide in the [documentation index](docs/README.md) |

## Core safeguards

- Fail closed on corrupt or ambiguous input, missing state/code, non-canonical
  encoding, inventory mismatches and recomputed-root mismatches. Preserve
  consensus bytes outside the explicit OVM conversion mode.
- Require new, non-aliasing outputs. Stage in this invocation's sibling partial
  directory, sync and independently reopen/verify before atomic no-replace
  publication. If parent sync fails after rename, retain the final directory
  and return `PublicationDurabilityError`.
- Keep streaming work bounded, preserve context cancellation, join workers
  before closing databases, and propagate close/sync/cleanup failures.
  Disk-backed temporary storage remains the default; memory mode is explicit.
- Preserve stdout JSON / stderr progress separation and strict artifact layouts.
- Do not incidentally change the Go 1.27 requirement, geth v1.17.5 pin,
  format v1 contracts, encoding, supported engines/schemes or compression.
  Do not restore legacy target generation or historical-format readers.
- Do not regenerate the legacy canary or frozen compatibility corpus during
  ordinary maintenance. Follow the documented intentional regeneration process.
- Update operator documentation and applicable development rules when public
  commands, reports, artifact invariants or validation gates change.

## Validation

Run focused checks while iterating. Finish every change with:

```bash
make ci
git diff --check
```

Also run `make test-race` for concurrency, cancellation, progress, database
lifecycle, publication or shared-state changes. Follow the topic-specific gates
and performance measurement requirements in the linked rules. Preserve aggregate
root/fixture/legacy-prune coverage; do not claim synthetic tests or benchmarks
prove production-snapshot acceptance.
