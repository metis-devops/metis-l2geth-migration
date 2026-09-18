# Benchmark evidence

One entry point for recorded measurements. [OVM results](ovm.md) combine the
latest migration/verification and alloc comparisons with separately labeled
historical scale/component evidence. [Prune results](prune.md) are historical.
Raw logs are immutable snapshots under `raw/`; their headers identify measured
source fingerprints. A recent date alone does not make a dataset current.

## Retained datasets

| Dataset | Status and purpose | Coverage | Observations |
|---|---|---|---:|
| [Parallel OVM evidence pairs](raw/ovm-evidence-parallel-2026-09-18.txt) | Latest measured concurrency snapshot; serial versus parallel preimages/history on the same fixture | 10k holder preimages; two-block history; disk/memory; 2/8 workers; migrate/verify | 48 |
| [Manual ERC20 retention pairs](raw/ovm-retention-2026-09-16.txt) | Historical retention snapshot; predates the unmeasured AncientRead loop edit; no list versus manual retention on the same source-contract fixture | 10k holders; 1000 listed contracts; disk/memory; 2/8 workers; migrate/verify; alloc/no alloc | 96 |
| [OVM zero-transfer pairs](raw/ovm-zero-transfer-2026-09-16.txt) | Historical; nonzero-Transfer rule versus `c3c52d5` with the same adjusted fixture | 10k holders; disk/memory; 2/8 workers; migrate/verify; alloc/no alloc | 96 |
| [Temporary database modes](raw/temp-db-2026-09-16.txt) | Historical; preserves distinct 100k and component evidence, before the nonzero-Transfer rule | 10k/100k holders; disk/memory; 10k/100k/1m component records | 231 |
| [OVM optimization pairs](raw/ovm-optimization-2026-09-16.txt) | Historical; preserves the `8bd08d4` baseline and ancient-reader evidence | 10k holders; disk; migrate/verify; alloc/no alloc; ancient reads | 54 |
| [Prune serial/parallel](raw/prune-2026-09-12.txt) | Historical; predates dry-run LOCK and Bloom-filter fixes | 8 workloads; serial and 2/4/8/16 workers; 5 phases | 1,000 |

OVM runs have three samples per configuration. Prune has five repetitions per
workload/mode, each in a fresh process executing five separately timed phases.
The prune run used macOS 26.6.2; the OVM runs used macOS 27.0. They are not pooled.
The retention dataset was added after the manual-policy implementation; earlier datasets remain historical.

## Reproduction

Run correctness checks to completion first. Do not run CI or other benchmarks
concurrently. Every output must be a new absolute path; keep raw logs intact.

```bash
# Manual ERC20 retention versus automatic-only classification on the same fixture.
python3 scripts/benchmark-ovm.py --out /absolute/new/ovm-retention.txt \
  --count 3 --holders 10000 --temp-dbs disk memory \
  --with-retain-list --with-alloc --with-verify

# Old/new OVM code with an isolated baseline and the same harness/fixture.
python3 scripts/benchmark-ovm.py --out /absolute/new/ovm-pairs.txt \
  --count 3 --holders 10000 --temp-dbs disk memory \
  --with-alloc --with-verify --baseline-root /absolute/isolated/baseline

# Disk/memory, scale and component comparison of one source snapshot.
python3 scripts/benchmark-ovm.py --out /absolute/new/temp-db.txt \
  --count 3 --holders 10000 100000 --temp-dbs disk memory \
  --with-alloc --with-verify --with-components

# Prune serial/parallel reference comparison.
python3 scripts/benchmark-prune.py --out /absolute/new/prune.txt --count 5
```

`--with-ancient` adds the separate ancient-record microbenchmark to the OVM
runner. Historical source fingerprints and baseline adjustments are documented
in the reports; rerunning these commands on HEAD produces a new dataset, not a
reconstruction of old measurements.

`--with-preimages` stores a valid address preimage for every holder in the
synthetic source, in addition to the existing witness and two-block history.
Use it when measuring evidence concurrency; both baseline and current checkouts
must include the same preimage fixture harness. It does not model long history
or cold source storage.

## Consolidation policy

- Use the newest applicable run for each displayed configuration; do not merge
  samples across source versions, fixtures, machines or measurement methods.
- Keep unique baseline, scale, component and workload evidence with explicit
  historical labels. Preserve raw samples, source fingerprints, regressions,
  setup/RSS distinctions and sampling limitations.
- Replace redundant summaries rather than appending another stage report.
  Store new raw logs here and update this index and the relevant report.
- Removed superseded 2026-09-15 initial OVM/history-fix logs and 2026-09-16
  alloc baseline/initial/whitespace logs, together with their stage reports.
  The latest OVM data covers worker and alloc costs; the retained historical
  datasets preserve their distinct comparisons. Removed files remain in Git
  history. Frozen canary and geth compatibility files are outside this cleanup.

## Raw-file SHA-256

These file checksums validate relocation/copy integrity; they are distinct from
the measured Go-source fingerprints inside each raw log.

| File | SHA-256 |
|---|---|
| [raw/ovm-evidence-parallel-2026-09-18.txt](raw/ovm-evidence-parallel-2026-09-18.txt) | `ac69d1f6a5ed1acfe8c310b808059eaedc6deb29cbbbb395306ea93fcca41e54` |
| [raw/ovm-zero-transfer-2026-09-16.txt](raw/ovm-zero-transfer-2026-09-16.txt) | `a89a5a49ff7eab8c961d996fe6127791856342bd03bdf21b9c4dc3eeea75d31f` |
| [raw/temp-db-2026-09-16.txt](raw/temp-db-2026-09-16.txt) | `147af70d1c80e0b09ecc25caeef7dee84c8bb41bbc408ec1bf97fa1a3651458e` |
| [raw/ovm-optimization-2026-09-16.txt](raw/ovm-optimization-2026-09-16.txt) | `dc6ea5d479af35dcd1cef73c98eb01728bbb8042ea698497097857d4c3b4dd2c` |
| [raw/prune-2026-09-12.txt](raw/prune-2026-09-12.txt) | `364a3546315f5800c8c77616b6e94d12e60e67642c8637483fcd47e03b93d120` |
