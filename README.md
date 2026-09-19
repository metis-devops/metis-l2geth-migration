# l2state

`l2state` migrates the latest executed state from a legacy Optimism/Metis
l2geth LevelDB into a state database compatible with go-ethereum v1.17.5.

> [!IMPORTANT]
> The output is a **state-only artifact**, not bootable geth `chaindata`.
> Ordinary migration preserves the canonical `LastBlock` accounts, storage,
> code and five selected-header/head metadata entries. It does not migrate
> block bodies, transactions, receipts or historical state.

Use a stopped source database or a point-in-time-consistent filesystem snapshot.
Migration never repairs, compacts or writes to the source, and does not use RPC.
Output paths must be new and outside the source.

## Build

Go 1.27 is required. Run commands from the repository root:

```bash
make build
./bin/l2state version
```

## Choose a workflow

| Workflow | Use it when | Guide |
| --- | --- | --- |
| `migrate` | Source and destination are available together; retain the source for later verification | [Direct migration](docs/migration.md#direct-migration) |
| `export` → `import` | You need a portable bundle or separate source/target environments | [Portable migration](docs/migration.md#portable-bundle-migration) |
| `migrate --migrate-ovm-eth` | You explicitly need OVM balances converted and a synthetic checkpoint | [OVM conversion and required inputs](docs/ovm-conversion.md) |
| `prune` | You need to delete old state in a stopped legacy full-node LevelDB | [Pruning, dry-run and recovery](docs/prune.md) |

Ordinary direct and portable workflows preserve the source state root and
independently verify the result before publication. Both support
`--db-engine pebble|leveldb` (default Pebble) and require `--scheme hash|path`.
The target layout is always geth; the former `--state-layout` flag is rejected.

## Quick start

Migrate a snapshot, then independently verify the artifact against that source:

```bash
./bin/l2state migrate \
  --source-chaindata /snapshot/geth/chaindata \
  --out /states/metis-hash \
  --scheme hash --workers 4

./bin/l2state verify \
  --source-chaindata /snapshot/geth/chaindata \
  --artifact /states/metis-hash
```

The result contains exactly:

```text
metis-hash/
├── chaindata/
└── verification.json
```

`--out` and `verify --artifact` refer to this artifact root. Consumers must open
`chaindata/` with the explicit scheme. See the [artifact contract](docs/artifacts.md)
for inventory, report and compatibility requirements.

Progress goes to stderr; stdout contains one final JSON result. Use `--quiet`
to suppress progress, or `<command> --help` for flags. Temporary databases use
disk by default; `--temp-db memory` retains scratch files in RAM with no hard
memory cap or automatic spill. Independent verification accepts `--temp-dir PATH`
to select an existing scratch parent; the default is the system temporary directory
(including for OVM verification). See [resource and failure handling](docs/operations.md)
before large runs.

OVM conversion requires a reviewed, storage-compatible runtime and complete
canonical history; it changes balances and the head. Pruning deletes old state
in place: stop the node and retain a backup before running it. Read the
corresponding guide before using either mode.

## Documentation and development

The [documentation index](docs/README.md) links all operator guides and development
rules. Start with [AGENTS.md](AGENTS.md) for repository guidance.

```bash
make ci
git diff --check
```

Run `make test-race` for changes covered by the [validation rules](docs/development/validation.md).
See [development and geth compatibility](docs/development.md) for test evidence,
dependency upgrades and benchmark commands, and the [benchmark index](docs/benchmarks/README.md)
for measured results and their limitations.
