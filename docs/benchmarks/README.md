# Benchmark evidence

One entry point for recorded measurements. [OVM results](ovm.md) combine the
latest migration/verification and alloc comparisons with separately labeled
historical scale/component evidence. [Prune results](prune.md) include the current
serial/parallel comparison and its separate historical baseline.
Raw logs are immutable snapshots under `raw/`; their headers identify measured
source fingerprints. A recent date alone does not make a dataset current.

## Retained datasets

| Dataset | Status and purpose | Coverage | Observations |
|---|---|---|---:|
| [Locality OVM pairs](raw/ovm-locality-pairs-2026-09-19.txt) | Latest ordered balance reads and evidence cursors versus the starting working tree | 10k holders; disk/memory; 2/8 workers; migrate/verify; alloc/no alloc; 6 alternating samples | 192 |
| [Locality scale pairs](raw/ovm-locality-scale-2026-09-19.txt) | Separate latest 100k comparison; no memory/alloc scale claim | 100k holders; disk; 2/8 workers; migrate/verify; 6 alternating samples | 48 |
| [Locality components](raw/ovm-locality-components-2026-09-19.txt) | Isolated dense/sparse evidence lookups versus point reads | 100k indexed keys; full scan or stride 1000; 6 alternating processes, 3 iterations/process | 24 |
| [Locality scoped profiles](raw/ovm-locality-profile-2026-09-19.txt) | Diagnostic operation-only CPU, allocation, block and mutex summaries, excluded from timing | 100k holders; disk; 8 workers; migrate; baseline/current | 2 |
| [Locality benchstat](raw/ovm-locality-benchstat-2026-09-19.txt) | Derived statistical comparison, with holder/mode/worker/operation kept separate | The 240 locality end-to-end observations above; no additional samples | — |
| [Input reader components](raw/ovm-input-components-2026-09-19.txt) | Latest isolated parser and hot-record lookup comparison versus frozen references | Address/allowance decoding; 60k hot LevelDB reads; 5 alternating samples | 30 |
| [Input optimization OVM pairs](raw/ovm-input-pairs-2026-09-19.txt) | Historical input-reader changes versus the uncommitted review-fix snapshot | 10k holders; disk/memory; 2/8 workers; migrate/verify; alloc/no alloc | 96 |
| [Input optimization scale check](raw/ovm-input-scale-2026-09-19.txt) | Historical 100k check; do not extrapolate to the entire matrix | 100k holders; disk; 2 workers; migrate/verify; no alloc | 12 |
| [Input optimization profile](raw/ovm-input-profile-2026-09-19.txt) | Diagnostic pre-optimization CPU/allocation profile including setup, not paired timing evidence | 100k holders; disk; 2 workers; migrate | 1 |
| [Review-fix prune pairs](raw/prune-review-fixes-2026-09-19.txt) | Latest shared-traversal snapshot versus the frozen serial reference, not a before/after revision comparison | 8 workloads; serial and 2/4/8/16 workers; 3 repetitions; 5 phases | 600 |
| [Review-fix OVM pairs](raw/ovm-review-fixes-2026-09-19.txt) | Historical review fixes versus `9eff764`; verification scratch is explicitly colocated for sampler parity | 10k/100k holders; disk/memory; 2/8 workers; migrate/verify; alloc/no alloc; 10k/100k/1m component records | 462 |
| [Parallel OVM evidence pairs](raw/ovm-evidence-parallel-2026-09-18.txt) | Historical concurrency snapshot; serial versus parallel preimages/history on the same fixture | 10k holder preimages; two-block history; disk/memory; 2/8 workers; migrate/verify | 48 |
| [Manual ERC20 retention pairs](raw/ovm-retention-2026-09-16.txt) | Historical retention snapshot; predates the unmeasured AncientRead loop edit; no list versus manual retention on the same source-contract fixture | 10k holders; 1000 listed contracts; disk/memory; 2/8 workers; migrate/verify; alloc/no alloc | 96 |
| [OVM zero-transfer pairs](raw/ovm-zero-transfer-2026-09-16.txt) | Historical; nonzero-Transfer rule versus `c3c52d5` with the same adjusted fixture | 10k holders; disk/memory; 2/8 workers; migrate/verify; alloc/no alloc | 96 |
| [Temporary database modes](raw/temp-db-2026-09-16.txt) | Historical; preserves distinct 100k and component evidence, before the nonzero-Transfer rule | 10k/100k holders; disk/memory; 10k/100k/1m component records | 231 |
| [OVM optimization pairs](raw/ovm-optimization-2026-09-16.txt) | Historical; preserves the `8bd08d4` baseline and ancient-reader evidence | 10k holders; disk; migrate/verify; alloc/no alloc; ancient reads | 54 |
| [Prune serial/parallel](raw/prune-2026-09-12.txt) | Historical; predates dry-run LOCK and Bloom-filter fixes | 8 workloads; serial and 2/4/8/16 workers; 5 phases | 1,000 |

Latest locality runs have six samples per configuration. Historical OVM
end-to-end runs have three, and input components have five samples per variant.
Diagnostic profiles are separate invocations and never timing samples. The current prune run has three
repetitions per workload/mode; the historical prune run has five. Each process
executes five separately timed phases. Historical prune used macOS 26.6.2; current
prune and OVM used macOS 27.0. Sources, fixtures and OS versions are not pooled.
The retention dataset was added after the manual-policy implementation; earlier datasets remain historical.

## Reproduction

Run correctness checks to completion first. Do not run CI or other benchmarks
concurrently. Every output must be a new absolute path; keep raw logs intact.

```bash
# Input parsing and hot-history lookup components versus frozen references.
python3 scripts/benchmark-ovm-inputs.py --out /absolute/new/ovm-inputs.txt --count 5

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
| [raw/ovm-locality-pairs-2026-09-19.txt](raw/ovm-locality-pairs-2026-09-19.txt) | `060380c680b4b7703241a907e3c580c16b49227f1d4e4806767956d2417a0572` |
| [raw/ovm-locality-scale-2026-09-19.txt](raw/ovm-locality-scale-2026-09-19.txt) | `003978fd3b39b60e56533061ee932d5e01c1c5f7495383852183cf4b16b46edb` |
| [raw/ovm-locality-components-2026-09-19.txt](raw/ovm-locality-components-2026-09-19.txt) | `6311f546982aaa498aedb93b35d2728c6703c48820c6945d06e933fedfc33785` |
| [raw/ovm-locality-profile-2026-09-19.txt](raw/ovm-locality-profile-2026-09-19.txt) | `fd6b5c6ee94b98d0830aa93784adc1bec036bff0870c5c61a0475e337238628a` |
| [raw/ovm-locality-benchstat-2026-09-19.txt](raw/ovm-locality-benchstat-2026-09-19.txt) | `67ba4e4e1cf68718021a91003f2141b77f6d9b9785e4e34e837e68ed7d4cc409` |
| [raw/ovm-input-components-2026-09-19.txt](raw/ovm-input-components-2026-09-19.txt) | `58f33f4370c035dd64b6d31fbc9f1a791b402df89586538b901959ec3bf427d5` |
| [raw/ovm-input-pairs-2026-09-19.txt](raw/ovm-input-pairs-2026-09-19.txt) | `ebe0ef7999c1630a5512c7a28eefa30b4247032ed3a4213856f40f4312d698d3` |
| [raw/ovm-input-scale-2026-09-19.txt](raw/ovm-input-scale-2026-09-19.txt) | `3f8c19cebd5443fd8477da6b6c71ecd7fabee33afdbb8ac358622819cd18d882` |
| [raw/ovm-input-profile-2026-09-19.txt](raw/ovm-input-profile-2026-09-19.txt) | `433d00c80e576476716ff0f7a8f5eb1d4068e33271059ed61f2147e0bdee2585` |
| [raw/prune-review-fixes-2026-09-19.txt](raw/prune-review-fixes-2026-09-19.txt) | `f68cf62df405c17602635f735e6992a8d345366f03338fa94ec89b0ddb2cf107` |
| [raw/ovm-review-fixes-2026-09-19.txt](raw/ovm-review-fixes-2026-09-19.txt) | `e331ac17717434d140a2024c7b1954093947db9f6747ba0c49cef4d8dddbe679` |
| [raw/ovm-evidence-parallel-2026-09-18.txt](raw/ovm-evidence-parallel-2026-09-18.txt) | `ac69d1f6a5ed1acfe8c310b808059eaedc6deb29cbbbb395306ea93fcca41e54` |
| [raw/ovm-zero-transfer-2026-09-16.txt](raw/ovm-zero-transfer-2026-09-16.txt) | `a89a5a49ff7eab8c961d996fe6127791856342bd03bdf21b9c4dc3eeea75d31f` |
| [raw/temp-db-2026-09-16.txt](raw/temp-db-2026-09-16.txt) | `147af70d1c80e0b09ecc25caeef7dee84c8bb41bbc408ec1bf97fa1a3651458e` |
| [raw/ovm-optimization-2026-09-16.txt](raw/ovm-optimization-2026-09-16.txt) | `dc6ea5d479af35dcd1cef73c98eb01728bbb8042ea698497097857d4c3b4dd2c` |
| [raw/prune-2026-09-12.txt](raw/prune-2026-09-12.txt) | `364a3546315f5800c8c77616b6e94d12e60e67642c8637483fcd47e03b93d120` |
