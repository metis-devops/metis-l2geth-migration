# Frozen geth contracts

These are new test-only baselines captured with the dependency recorded in
`provenance.json`. The legacy canary alongside this directory is unchanged.

- `bundle-vN.json`: portable records, normalized manifests, codec inputs and
  expected acceptance/expanded bytes, and empty consensus constants.
- `verification-vN-bundle-vM.json`: normalized bundle-only/artifact reports,
  logical database inventories and canary continuation for bundle version M.
- `direct-vN.json`: normalized direct reports, logical database inventories and
  canary continuation, independently versioned from portable formats.

Each artifact case references a SHA-256-addressed database blob in its own file.
Blob entries contain strictly sorted raw hex keys and values; the blob ID hashes
compact JSON of that entry array. The comparison expands references so failures
identify the actual changed key, not just a database digest. Long values remain
complete in this corpus but are shortened with length and SHA-256 in diagnostics.

Only times and build provenance are normalized. The historical `geth_commit`
field is removed in memory before comparison and replay, with dependent manifest
hashes recomputed. The frozen files remain unchanged. Runtime readers reject
that removed field; there is no old-field compatibility mode. Report manifest hashes are
recomputed over the normalized manifest JSON (two-space indentation and a final
newline). No roots, counts, record bytes or database metadata are normalized.

Existing version files are immutable review evidence. An intentional format
change adds new version files, rather than replacing the prior contract. The
current reader still accepts only current format versions. `provenance.json`
records the initial corpus provenance; provenance for any newly accepted files
must also be retained with their review/change record.

Run `make geth-compat` from the repository root. Use the documented
`make geth-compat-candidate OUT=/absolute/new/path` only to export an unapproved
candidate. Ordinary tests never generate or modify these files.
