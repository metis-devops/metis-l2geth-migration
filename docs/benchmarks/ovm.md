# OVM benchmark results

Consolidated on 2026-09-16. [Dataset index and raw-file checksums](README.md).

## Measurement boundaries

- The latest measured source is the nonzero-Transfer eligibility change on top of `c3c52d5`. These are recorded measurements of identified source snapshots, not a claim that future HEAD has been benchmarked.
- All OVM end-to-end results use an Apple M3 Max (16 logical CPUs), macOS 27.0 arm64, a two-block synthetic history, Pebble/hash targets, 128 MiB cache allowance and 128 handles. Alloc adds code, native balance and one storage override to 1,000 holders.
- Each configuration has three fresh-process samples with alternating order, after CI/race and without concurrent benchmarks. Tables are medians calculated from their named raw dataset. Samples from different runs or source versions are never pooled.
- Operation time and allocation counters exclude setup. Process RSS includes setup and, for verification, the initial disk-mode migration. `setup-s` is separate. OS caches were not flushed.
- Heap and file lengths are sampled every 20 ms and can miss peaks. Heap excludes some native allocations; memory-file lengths exclude allocation capacity. Temporary disk excludes the final artifact. Neither cache allowances nor these samples bound total memory or disk requirements.
- No production throughput, long-history, cold-disk, LevelDB or path-scheme performance claim follows from these fixtures. Historical results below have not been rerun on the latest source.

## Latest manual ERC20 retention comparison

Raw evidence: [manual retention pairs](raw/ovm-retention-2026-09-16.txt), 96 fresh-process observations, three per configuration. This source passed `make ci`, `make test-race` and `git diff --check` before measurement; no CI or other benchmark ran concurrently.

The measured fingerprint is `93558a23a849d24ca277cdf1853174e9e8b17b459265238ed0df5e04d876f758`.
After measurement, a separate working-tree edit changed `BenchmarkOVMAncientRead`
from `b.N` to `b.Loop`. That unmeasured benchmark is outside this dataset. An
in-memory comparison accounting for only that edit reproduces the recorded
fingerprint exactly; the retention implementation and measured cases are unchanged.
Do not treat these logs as measurements of that later source revision.

Both absent/present-list cases use the same modified 10k-holder source fixture: the first 1000 existing holder accounts have code. Retain cases list those contracts; no-list cases use only nonzero Transfer-from history. The list therefore changes the retained balances and final trie inventory as well as adding input/eligibility checks. These are whole-feature costs, not an isolated lookup benchmark or an old/new revision speedup. No samples are pooled with the historical fixtures below.

Reproduce with `python3 scripts/benchmark-ovm.py --holders 10000 --temp-dbs disk memory --with-retain-list --with-alloc --with-verify --count 3 --out /absolute/new/file`.

| Operation | Workers | Temp DB | No list ms | With list ms | Time change | No list RSS MiB | With list RSS MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 873.4 | 865.3 | -0.9% | 82.7 | 81.6 |
| Migrate | 2 | memory | 541.2 | 565.5 | +4.5% | 100.2 | 100.9 |
| Migrate | 8 | disk | 735.2 | 773.9 | +5.3% | 80.7 | 82.6 |
| Migrate | 8 | memory | 436.6 | 481.2 | +10.2% | 105.0 | 100.1 |
| Migrate + alloc | 2 | disk | 951.5 | 961.4 | +1.0% | 86.8 | 86.1 |
| Migrate + alloc | 2 | memory | 652.4 | 666.2 | +2.1% | 116.4 | 118.0 |
| Migrate + alloc | 8 | disk | 831.2 | 847.7 | +2.0% | 89.6 | 89.9 |
| Migrate + alloc | 8 | memory | 546.0 | 557.5 | +2.1% | 120.1 | 122.3 |
| Verify | 2 | disk | 790.5 | 817.5 | +3.4% | 88.5 | 92.3 |
| Verify | 2 | memory | 482.0 | 520.3 | +7.9% | 106.5 | 115.0 |
| Verify | 8 | disk | 675.8 | 698.6 | +3.4% | 91.3 | 93.2 |
| Verify | 8 | memory | 360.9 | 379.6 | +5.2% | 110.2 | 115.2 |
| Verify + alloc | 2 | disk | 903.5 | 897.9 | -0.6% | 95.6 | 94.6 |
| Verify + alloc | 2 | memory | 534.8 | 546.7 | +2.2% | 126.0 | 120.4 |
| Verify + alloc | 8 | disk | 746.1 | 773.5 | +3.7% | 101.5 | 103.9 |
| Verify + alloc | 8 | memory | 473.2 | 502.0 | +6.1% | 132.2 | 131.0 |

Median elapsed changes range from -0.9% to +10.2%; positive values are measured regressions in the list-enabled configuration. Three short samples do not establish a persistent trend. In particular, this two-block, hash/Pebble fixture does not establish production throughput, long-history costs, or LevelDB/path performance.

### List-enabled setup and resource detail

RSS includes setup; the following operation samples use the same limitations described above.

| Operation | Workers | Temp DB | Setup ms | Allocated MiB/op | Heap MiB | Temp disk MiB | Memory files MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 146.8 | 165.3 | 25.3 | 13.0 | 0.0 |
| Migrate | 2 | memory | 150.1 | 237.7 | 41.6 | 0.0 | 7.9 |
| Migrate | 8 | disk | 142.9 | 163.3 | 27.3 | 8.0 | 0.0 |
| Migrate | 8 | memory | 154.7 | 236.7 | 39.0 | 0.0 | 7.9 |
| Migrate + alloc | 2 | disk | 151.3 | 202.4 | 25.8 | 13.1 | 0.0 |
| Migrate + alloc | 2 | memory | 155.0 | 322.2 | 48.7 | 0.0 | 13.3 |
| Migrate + alloc | 8 | disk | 157.3 | 208.8 | 27.8 | 16.7 | 0.0 |
| Migrate + alloc | 8 | memory | 152.1 | 326.3 | 48.6 | 0.0 | 12.3 |
| Verify | 2 | disk | 1078.0 | 159.1 | 25.0 | 12.1 | 0.0 |
| Verify | 2 | memory | 1066.0 | 231.8 | 36.4 | 0.0 | 7.9 |
| Verify | 8 | disk | 942.5 | 157.4 | 27.0 | 8.0 | 0.0 |
| Verify | 8 | memory | 905.0 | 229.7 | 37.6 | 0.0 | 7.9 |
| Verify + alloc | 2 | disk | 1075.0 | 195.0 | 25.3 | 13.2 | 0.0 |
| Verify + alloc | 2 | memory | 1104.0 | 317.6 | 47.9 | 0.0 | 11.3 |
| Verify + alloc | 8 | disk | 1007.0 | 199.4 | 27.7 | 16.5 | 0.0 |
| Verify + alloc | 8 | memory | 1017.0 | 319.5 | 50.2 | 0.0 | 12.3 |

## Historical 10k-holder nonzero-Transfer comparison

Raw evidence: [zero-transfer pairs](raw/ovm-zero-transfer-2026-09-16.txt), 96 samples.
The baseline is isolated `c3c52d5` production code. Its test fixture was changed to use amount 1 for the qualifying contract Transfer, matching the final fixture. Both sides therefore use the same input and expected conversion result. This measures the added eligibility check, not the benefit of filtering a zero-heavy history.
The final source passed `make ci`, `make test-race` and `git diff --check`; zero-only eligibility is tested separately against the independent state reference and standalone replay.

| Operation | Workers | Temp DB | Baseline ms | Final ms | Time change | Baseline RSS MiB | Final RSS MiB |
|---|---:|---|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 849.9 | 856.2 | +0.7% | 81.4 | 81.9 |
| Migrate | 2 | memory | 536.7 | 537.0 | +0.1% | 109.9 | 111.1 |
| Migrate | 8 | disk | 737.8 | 709.4 | -3.8% | 82.6 | 80.5 |
| Migrate | 8 | memory | 435.5 | 452.4 | +3.9% | 101.1 | 104.8 |
| Migrate + alloc | 2 | disk | 921.1 | 923.9 | +0.3% | 85.8 | 87.1 |
| Migrate + alloc | 2 | memory | 614.3 | 617.5 | +0.5% | 114.3 | 115.3 |
| Migrate + alloc | 8 | disk | 843.5 | 850.0 | +0.8% | 88.1 | 89.3 |
| Migrate + alloc | 8 | memory | 525.0 | 522.2 | -0.5% | 120.0 | 120.0 |
| Verify | 2 | disk | 789.5 | 795.4 | +0.7% | 90.0 | 88.6 |
| Verify | 2 | memory | 466.9 | 465.9 | -0.2% | 117.6 | 118.3 |
| Verify | 8 | disk | 649.7 | 649.4 | -0.0% | 94.3 | 89.1 |
| Verify | 8 | memory | 356.3 | 359.7 | +1.0% | 111.8 | 111.1 |
| Verify + alloc | 2 | disk | 843.8 | 882.0 | +4.5% | 90.7 | 92.9 |
| Verify + alloc | 2 | memory | 536.5 | 534.6 | -0.4% | 120.7 | 119.6 |
| Verify + alloc | 8 | disk | 743.4 | 782.0 | +5.2% | 101.7 | 101.4 |
| Verify + alloc | 8 | memory | 447.7 | 446.5 | -0.3% | 134.5 | 134.9 |

Median time changes range from -3.8% to +5.2%; RSS changes range from -5.5% to +3.7%. Positive changes are measured regressions. In particular, disk verification with alloc increased 4.5%/5.2% at 2/8 workers. Three short samples do not establish a persistent trend.

### Historical nonzero-Transfer setup and resource detail

Final-source medians only. Allocated bytes are cumulative per operation; heap/files are sampled peaks. Baseline values and individual samples remain in the raw log.

| Operation | Workers | Temp DB | Setup ms | Allocated MiB/op | Heap MiB | Temp disk MiB | Memory files MiB |
|---|---:|---|---:|---:|---:|---:|---:|---:|
| Migrate | 2 | disk | 140.4 | 159.4 | 26.3 | 13.0 | 0.0 |
| Migrate | 2 | memory | 140.1 | 259.5 | 45.1 | 0.0 | 9.0 |
| Migrate | 8 | disk | 142.1 | 158.7 | 25.5 | 7.9 | 0.0 |
| Migrate | 8 | memory | 138.2 | 230.6 | 37.6 | 0.0 | 7.9 |
| Migrate + alloc | 2 | disk | 143.3 | 196.1 | 25.4 | 13.4 | 0.0 |
| Migrate + alloc | 2 | memory | 140.4 | 308.8 | 45.7 | 0.0 | 11.4 |
| Migrate + alloc | 8 | disk | 140.7 | 201.8 | 26.9 | 16.5 | 0.0 |
| Migrate + alloc | 8 | memory | 139.5 | 319.3 | 52.8 | 0.0 | 11.9 |
| Verify | 2 | disk | 999.9 | 153.5 | 25.0 | 13.0 | 0.0 |
| Verify | 2 | memory | 1003.0 | 252.7 | 48.4 | 0.0 | 9.0 |
| Verify | 8 | disk | 841.1 | 152.7 | 25.9 | 7.9 | 0.0 |
| Verify | 8 | memory | 884.1 | 224.7 | 38.1 | 0.0 | 7.9 |
| Verify + alloc | 2 | disk | 1068.0 | 189.4 | 24.6 | 13.7 | 0.0 |
| Verify + alloc | 2 | memory | 1094.0 | 308.7 | 50.6 | 0.0 | 11.4 |
| Verify + alloc | 8 | disk | 972.4 | 193.4 | 30.3 | 16.2 | 0.0 |
| Verify + alloc | 8 | memory | 970.1 | 313.1 | 38.0 | 0.0 | 11.9 |

### Alloc overhead within the latest source

These compare alloc/no-alloc modes of the same final binary, not old/new code.

| Operation | Workers | Temp DB | Without alloc ms | With alloc ms | Added time |
|---|---:|---|---:|---:|---:|
| Migrate | 2 | disk | 856.2 | 923.9 | +7.9% |
| Migrate | 2 | memory | 537.0 | 617.5 | +15.0% |
| Migrate | 8 | disk | 709.4 | 850.0 | +19.8% |
| Migrate | 8 | memory | 452.4 | 522.2 | +15.4% |
| Verify | 2 | disk | 795.4 | 882.0 | +10.9% |
| Verify | 2 | memory | 465.9 | 534.6 | +14.7% |
| Verify | 8 | disk | 649.4 | 782.0 | +20.4% |
| Verify | 8 | memory | 359.7 | 446.5 | +24.1% |

## Historical 100k-holder and component measurements

Raw evidence: [temporary database modes](raw/temp-db-2026-09-16.txt), 231 samples (96 end-to-end and 135 component). This source predates the nonzero-Transfer rule. It compares disk/memory modes of one implementation, not two code revisions. Keep its 100k and component evidence separate from the latest 10k run; redundant older 10k tables are omitted here but their samples remain intact.

### 100k holders

| Operation | Workers | Disk ms | Memory ms | Time change | Disk RSS MiB | Memory RSS MiB |
|---|---:|---:|---:|---:|---:|---:|
| Migrate | 2 | 8724.5 | 7408.7 | -15.1% | 281.3 | 555.4 |
| Migrate | 8 | 7837.2 | 6523.5 | -16.8% | 290.5 | 554.5 |
| Migrate + alloc | 2 | 10449.7 | 8561.0 | -18.1% | 310.3 | 597.5 |
| Migrate + alloc | 8 | 9325.3 | 7593.7 | -18.6% | 302.6 | 608.0 |
| Verify | 2 | 8534.7 | 6969.5 | -18.3% | 372.8 | 585.7 |
| Verify | 8 | 7692.8 | 6144.7 | -20.1% | 371.6 | 597.8 |
| Verify + alloc | 2 | 10047.6 | 8916.3 | -11.3% | 376.6 | 619.7 |
| Verify + alloc | 8 | 9302.5 | 7344.0 | -21.1% | 374.4 | 611.4 |

Memory-mode RSS medians were 554–620 MiB versus 281–377 MiB for disk. Every end-to-end memory-mode sample reported zero physical temporary database bytes. Memory mode remains opt-in, with no hard RAM cap or spill; the final artifact still needs disk space and sync.

### One-million-record components

Components use deterministic high-entropy 32-byte keys/values (nil node-marker values) in 1,000 prefixes. Preparation is outside the timer; input arrays remain live. Each row is a whole trace: batched overwrites, repeated markers, permuted point reads, a full scan, or 1,000 prefix scans. One foreground caller drives each trace; Pebble background work is enabled. Disk/memory Pebble use identical adapter settings. Geth memorydb is a component reference, not a supported CLI backend.

| Trace | Disk Pebble ms | Memory Pebble ms | Geth memorydb ms | Disk RSS MiB | Memory RSS MiB | Geth RSS MiB |
|---|---:|---:|---:|---:|---:|---:|
| batch | 1562.3 | 263.1 | 102.3 | 169.5 | 615.5 | 352.0 |
| dedup | 949.7 | 285.8 | 91.3 | 151.8 | 410.6 | 283.8 |
| get | 3734.0 | 3082.8 | 184.0 | 215.3 | 459.9 | 324.0 |
| scan | 129.0 | 111.5 | 220.5 | 160.0 | 498.0 | 330.2 |
| prefix | 127.9 | 106.3 | 22740.8 | 165.2 | 474.5 | 598.9 |

Geth memorydb used less RSS and time for several point-read/write traces, but its repeated full-map scan/sort made the prefix trace much slower. This is not evidence that Pebble always uses less RAM.

Preserved small-trace regressions for memory Pebble versus disk Pebble:

- 10,000 records, `batch`: 2.454 → 2.575 ms (+5.0%).
- 100,000 records, `scan`: 6.273 → 6.881 ms (+9.7%).
- 100,000 records, `prefix`: 8.816 → 9.967 ms (+13.1%).

## Historical ancient-reader optimization

Raw evidence: [optimization baseline/final pairs](raw/ovm-optimization-2026-09-16.txt), 54 samples. Baseline production code is `8bd08d4`; only `ovm_benchmark_test.go` and `ovm_optimization_benchmark_test.go` were copied from the measured final checkout to share the harness. This predates temporary-memory support and the nonzero-Transfer rule.
The raw log retains all original end-to-end before/after observations, including RSS regressions. Those older end-to-end tables are superseded by the latest report above; only the distinct ancient-reader comparison is summarized here.

The ancient microbenchmark reads 60,000 synthetic 32-byte records across three tables and four files/table, with two compressed tables. It isolates cached small-record descriptor reuse, not receipt decoding, commitment validation or a complete history scan.

| Measurement | Baseline | Optimized | Change |
|---|---:|---:|---:|
| Time ms | 890.56 | 45.67 | -94.87% |
| Allocated MiB/op | 66.45 | 2.47 | -96.29% |
| Process RSS MiB | 28.42 | 27.12 | -4.56% |

These historical changes combined bounded account-reader reuse, raw-byte input confirmation, ancient descriptor reuse, validation-only verification replay and index/write-path optimizations. They do not attribute the end-to-end gains to individual changes or establish a comparable production history speedup.
