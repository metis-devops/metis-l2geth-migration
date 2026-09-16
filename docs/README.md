# Documentation

[Project overview and quick start](../README.md) · [Repository guidance](../AGENTS.md)

Command examples assume the repository root as the working directory.

## Operator guides

| Guide | Contents |
| --- | --- |
| [Migration workflows](migration.md) | Build provenance, direct migration, portable export/import and verification |
| [OVM ETH conversion](ovm-conversion.md) | Runtime requirements, manual ERC20 retention, GenesisAlloc, history, witnesses and checkpoint evidence |
| [Offline pruning](prune.md) | Supported databases, dry-run, deletion, interruption handling, resources and measurements |
| [Artifacts and formats](artifacts.md) | Exact database inventories, bundle/report schemas and compatibility |
| [Operational reference](operations.md) | Temporary storage, worker/cache limits, progress, publication and failures |

## Development

[Development and compatibility](development.md) describes test evidence, the
geth upgrade workflow and temporary-database benchmarks. The following documents
contain the mandatory rules linked from `AGENTS.md`:

| Rules | Scope |
| --- | --- |
| [Immutable contracts](development/contracts.md) | Source, consensus bytes, bundles, target artifacts and publication |
| [Code map and shared implementation](development/implementation.md) | Module responsibilities, temporary backends, streaming and partition workers |
| [OVM implementation](development/ovm.md) | Conversion, retention, alloc, history and OVM validation gates |
| [Prune implementation](development/prune.md) | Locking, physical inventories, deletion and resource bounds |
| [Validation and format evolution](development/validation.md) | Required checks, compatibility corpus, fixtures and change-sensitive gates |

## Performance evidence

The [benchmark index](benchmarks/README.md) catalogs source fingerprints and
immutable raw logs. [OVM results](benchmarks/ovm.md) and [prune results](benchmarks/prune.md)
retain their measurement limits and historical labels.
