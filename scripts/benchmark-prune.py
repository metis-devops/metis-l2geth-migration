#!/usr/bin/env python3
"""Run paired prune benchmarks in fresh processes; only generated databases are used."""

import argparse
import hashlib
import os
from pathlib import Path
import platform
import subprocess
import tempfile


WORKLOADS = (
    "light", "dense-storage", "storage-1024", "storage-1025",
    "giant-storage", "shared-code", "history-90pct", "history-99pct",
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--count", type=int, default=5)
    parser.add_argument("--workers", type=int, nargs="+", default=[2, 4, 8, 16])
    parser.add_argument("--workloads", choices=WORKLOADS, nargs="+", default=list(WORKLOADS))
    args = parser.parse_args()
    if args.count < 1 or any(w < 2 or w > 16 for w in args.workers):
        parser.error("count must be positive and workers must be 2..16")
    root = Path(__file__).resolve().parents[1]
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*.go")) + [root / "go.mod", root / "go.sum"]:
        digest.update(str(path.relative_to(root)).encode() + b"\0" + path.read_bytes())
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="l2state-prune-bench-") as temp:
        binary = Path(temp) / ("prune.test.exe" if os.name == "nt" else "prune.test")
        subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/migration"], cwd=root, check=True)
        with args.out.open("w") as output:
            output.write(f"# {platform.platform()} CPUs={os.cpu_count()}\n")
            output.write(f"# source-sha256={digest.hexdigest()} cache-mb=128 handles=128\n")
            output.write("# Fresh process per workload/mode/repetition; caches are not flushed.\n")
            modes = [None] + args.workers
            for repetition in range(args.count):
                for workload in args.workloads:
                    # Rotate execution order to reduce a consistent warm-cache bias.
                    offset = repetition % len(modes)
                    for workers in modes[offset:] + modes[:offset]:
                        mode = "serial" if workers is None else f"workers={workers}"
                        matcher = (
                            f"^BenchmarkPruneSerial$/^{workload}$/"
                            if workers is None else
                            f"^BenchmarkPruneOptimized$/^workers={workers}$/^{workload}$/"
                        )
                        print(f"sample {repetition + 1}/{args.count}: {workload}/{mode}", flush=True)
                        output.write(f"# sample={repetition + 1} workload={workload} mode={mode}\n")
                        output.flush()
                        subprocess.run(
                            [str(binary), "-test.run=^$", f"-test.bench={matcher}",
                             "-test.benchtime=1x", "-test.count=1", "-test.timeout=10m"],
                            cwd=root / "internal/migration", stdout=output,
                            stderr=subprocess.STDOUT, check=True,
                        )


if __name__ == "__main__":
    main()
