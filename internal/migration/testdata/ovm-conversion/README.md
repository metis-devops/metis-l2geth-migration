# OVM conversion test runtime

`wrapped.hex` is the deployed runtime of `WrappedEther.sol`, compiled with
solc `0.8.26+commit.8a97fa7a`, optimizer enabled (200 runs), EVM version `paris`,
and metadata bytecode hash disabled. The compiler source key is `WrappedEther.sol`.
Ordinary tests read this frozen runtime; they never invoke a compiler.

This deliberately small contract tests the migration's storage preservation and
native backing through ERC20 queries, transfers, deposit and withdrawal. It is
not a production contract supplied by l2state. Operators must provide their own
reviewed, storage-compatible runtime.

`source.json` freezes a small synthetic logical legacy database (hex key/value
pairs), with a canonical two-header history and legacy-encoded receipts. It covers
EOAs, retained and converted contracts, an address without an account leaf,
shared old code, self-held tokens and allowances. Its source totalSupply is 308;
209 converts to native and 99 remains backed ERC20. `witness.jsonl` supplies the
addresses not recoverable from the three history events. This is synthetic
commitment/codec evidence, not historical execution or a production snapshot.
The separately pinned legacy fixture module reads its state and receipts through
legacy l2geth APIs. Existing canary files are unaffected.

For intentional maintenance, `L2STATE_OVM_FIXTURE_OUT=/absolute/new/path go test
./internal/migration -run '^TestWriteOVMFixtureCandidate$' -count=1` writes an
unapproved candidate outside testdata. Compare every file before replacing this
fixture. Ordinary tests never write these inputs.
