# Repository guidance

## Purpose and compatibility

This repository builds `l2state`, a deliberately narrow migration tool for the latest executed state in a legacy Optimism/Metis l2geth LevelDB. Preserve the documented boundary: the tool migrates accounts, storage, and code committed by `LastBlock`; it does not create bootable geth `chaindata` or migrate blocks, transactions, receipts, chain configuration, history, or trie preimages.

The root module requires Go 1.27 and pins go-ethereum to v1.17.5 at commit `9621c6ad10934a01b5514886fb6fbd87640b6c05`. Do not change the geth version, bundle format, database layout, or supported state schemes as incidental dependency cleanup.

## Code map

- `cmd/l2state`: CLI parsing and JSON output.
- `internal/readonlydb`: strict read-only adapter for legacy LevelDB.
- `internal/bundle`: versioned manifest and ordered record-stream encoding.
- `internal/migration`: export, import, state traversal, atomic publication, and independent verification.
- `internal/migration/testdata`: committed legacy l2geth canary and expected evidence.
- `testdata/legacyfixturegen`: separate maintenance-only module used to regenerate the canary.

## Correctness and safety invariants

- Treat the legacy source as immutable. Never enable LevelDB recovery, compaction, or any write path, and require a stopped database or a point-in-time-consistent filesystem snapshot.
- Fail closed on missing trie nodes or code, a changing or non-canonical head, non-canonical RLP, digest/count/order mismatches, unexpected database keys, or a recomputed root mismatch.
- Preserve consensus bytes. Do not reinterpret OVM balances or transform account, storage, or code payloads during export.
- Keep record ordering deterministic and bind evidence to the exact header, block hash, and state root.
- Output paths must be new. Publish through the sibling `.partial-*` directory only after verification and syncing; cleanup must never target unrelated paths.
- Hash and path artifacts are state-only databases. Keep hash artifacts free of temporary flat state, and keep the path snapshot metadata and state ID semantics compatible with geth v1.17.5.

## Development workflow

Run commands from the repository root:

```bash
make fmt-check
make vet
make lint
make test
make build
```

Use `make ci` for the standard local gate and `make test-race` when changing concurrency, cancellation, database lifecycle, or shared state. Run focused `go test` commands while iterating, but finish with the relevant Make targets.

Format Go code with `gofmt`. Wrap errors with context and `%w`, handle close/sync/abort errors, and preserve `context.Context` cancellation through long scans. Add regression coverage for behavior changes, including both `hash` and `path` schemes and both `zstd` and `none` compression where applicable.

Do not regenerate the golden legacy fixture during ordinary test maintenance. If fixture regeneration is intentional, use the pinned generator in `testdata/legacyfixturegen`, write to a new temporary directory, compare all generated outputs, and document any provenance change.
