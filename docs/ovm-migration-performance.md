# OVM migration measurements — 2026-09-15

Command: `python3 scripts/benchmark-ovm.py --out /absolute/new/results.txt --count 3`.
The [raw results](ovm-benchmark-2026-09-15.txt) include the source digest, machine,
individual samples, allocations, process RSS and OS timing.

On this Apple M3 Max, three alternating paired runs used fresh processes,
10,000 synthetic holder addresses, a two-block history, hash/Pebble targets,
128 MiB cache and 128 handles. No CI or other benchmark ran concurrently.

| Measurement | 2 workers | 8 workers |
| --- | ---: | ---: |
| Migration elapsed range | 0.911–0.927 s | 0.737–0.754 s |
| Migration elapsed median | 0.920 s | 0.740 s |
| Process maximum RSS median | 80.80 MiB | 81.69 MiB |
| Sampled scratch + target apparent disk peak median | 14.31 MiB | 10.47 MiB |
| Process user + system CPU time median | 1.00 s | 1.09 s |

Eight workers reduced the elapsed median by about 20% in this workload, while
process CPU time increased by about 9%. RSS increased slightly in these samples.
This comparison is between two configurations of the new conversion workflow;
it is not a comparison with the former root-preserving migrate command.

Limitations: only three short samples, warm/uncontrolled OS caches, very little
history and a small code set. Timing excludes fixture setup; RSS and process CPU
include it. Apparent file sizes are sampled every 20 ms and may miss a brief
peak; they are not allocated disk blocks. These results do not establish a speedup
or a memory bound for a production snapshot, large historical receipt scans,
path targets or LevelDB targets. Validate those on representative snapshots.

## After the history reader fixes

The [follow-up raw results](ovm-history-fixes-benchmark-2026-09-15.txt) record the
source digest after allowing ancient history above LastBlock and validating
equivalent hot/cold receipt encodings by their canonical commitments. The same
three alternating pairs ran without concurrent CI or benchmarks.

| Measurement | 2 workers | 8 workers |
| --- | ---: | ---: |
| Migration elapsed range | 0.919–0.966 s | 0.750–0.807 s |
| Migration elapsed median | 0.934 s | 0.788 s |
| Process maximum RSS median | 81.16 MiB | 82.44 MiB |
| Sampled scratch + target apparent disk peak median | 14.14 MiB | 10.25 MiB |
| Process user + system CPU time median | 1.07 s | 1.20 s |

Eight workers used about 16% less elapsed time and about 12% more process CPU
time than two workers in this run. Relative to the earlier separate run, elapsed
medians increased approximately 2% and 7%, respectively. The before/after runs
were not paired against old and new binaries, so those increases cannot be
attributed to the fixes. All earlier measurement limits still apply. In
particular, this hot-only, two-block workload does not measure the cost of
validating overlapping ancient receipts; it is not evidence of production
history-scan throughput.
