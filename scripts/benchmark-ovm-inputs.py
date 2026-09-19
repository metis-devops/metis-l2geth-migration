#!/usr/bin/env python3
"""Compare frozen and optimized OVM input readers in alternating fresh processes."""

import argparse
import hashlib
from pathlib import Path
import platform
import subprocess
import tempfile


def source_digest(root):
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*.go")) + [root / "go.mod", root / "go.sum"]:
        digest.update(str(path.relative_to(root)).encode() + b"\0" + path.read_bytes())
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--count", type=int, default=5)
    args = parser.parse_args()
    if args.count < 2:
        parser.error("count must be at least 2 for paired measurements")
    root = Path(__file__).resolve().parents[1]
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="l2state-input-bench-") as temp:
        binary = Path(temp) / "inputs.test"
        subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/migration"],
                       cwd=root, check=True)
        available = subprocess.check_output([str(binary), "-test.list=^Benchmark"], text=True).splitlines()
        required = {"BenchmarkOVMHotHistoryRead", "BenchmarkOVMWitnessDecode"}
        if not required.issubset(available):
            parser.error("input-reader benchmarks are missing")
        with args.out.open("x") as output:
            output.write(f"# {platform.platform()}\n# source-sha256={source_digest(root)}\n")
            output.write(f"# Fresh process per component/variant; {args.count} repetitions; alternating reference/optimized order.\n")
            output.write("# Witness: one bounded JSON record per op. Hot history: 60000 sequential 512-byte values per op; strict read-only LevelDB, cache-mb=16 handles=16.\n")
            output.write("# Setup excluded from benchmark timers; caches not flushed; only synthetic inputs.\n")
            for repetition in range(args.count):
                variants = ["reference", "optimized"]
                if repetition % 2:
                    variants.reverse()
                for component in ["address", "allowance", "history"]:
                    for variant in variants:
                        if component == "history":
                            name = "reference" if variant == "reference" else "single-get"
                            selector = f"^BenchmarkOVMHotHistoryRead$/^{name}$"
                        else:
                            name = "reference" if variant == "reference" else "single-pass"
                            selector = f"^BenchmarkOVMWitnessDecode$/^{component}$/^{name}$"
                        message = f"sample={repetition + 1} component={component} variant={variant}"
                        print(message, flush=True)
                        output.write("# " + message + "\n")
                        output.flush()
                        subprocess.run([str(binary), "-test.run=^$", f"-test.bench={selector}",
                                        "-test.benchtime=300ms", "-test.count=1"],
                                       cwd=root / "internal/migration", stdout=output,
                                       stderr=subprocess.STDOUT, check=True)


if __name__ == "__main__":
    main()
