#!/usr/bin/env python3
"""Measure OVM migration with 2 and 8 workers in alternating fresh processes."""

import argparse
import hashlib
import os
from pathlib import Path
import platform
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--count", type=int, default=3)
    args = parser.parse_args()
    if args.count < 2:
        parser.error("count must be at least 2 for paired measurements")
    root = Path(__file__).resolve().parents[1]
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*.go")) + [root / "go.mod", root / "go.sum"]:
        digest.update(str(path.relative_to(root)).encode() + b"\0" + path.read_bytes())
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="l2state-ovm-bench-") as temp:
        binary = Path(temp) / "migration.test"
        subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/migration"], cwd=root, check=True)
        with args.out.open("x") as output:
            output.write(f"# {platform.platform()} CPUs={os.cpu_count()}\n")
            output.write(f"# source-sha256={digest.hexdigest()} cache-mb=128 handles=128\n")
            output.write("# Synthetic 10000 holder state, two-block history, hash/Pebble.\n")
            output.write("# Fresh process; OS caches not flushed; setup excluded from ns/op.\n")
            output.write("# RSS includes fixture setup; disk peak sampled at 20 ms includes scratch and target.\n")
            for repetition in range(args.count):
                for workers in ([2, 8] if repetition % 2 == 0 else [8, 2]):
                    print(f"sample {repetition + 1}/{args.count}: workers={workers}", flush=True)
                    output.write(f"# sample={repetition + 1} workers={workers}\n")
                    output.flush()
                    command = [str(binary), "-test.run=^$",
                               f"-test.bench=^BenchmarkOVMMigration$/^workers={workers}$",
                               "-test.benchtime=1x", "-test.count=1", "-test.timeout=10m"]
                    if platform.system() == "Darwin":
                        command = ["/usr/bin/time", "-l"] + command
                    elif Path("/usr/bin/time").exists():
                        command = ["/usr/bin/time", "-v"] + command
                    subprocess.run(command, cwd=root / "internal/migration", stdout=output,
                                   stderr=subprocess.STDOUT, check=True)


if __name__ == "__main__":
    main()
