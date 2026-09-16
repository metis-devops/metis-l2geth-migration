#!/usr/bin/env python3
"""Measure OVM migration and optional verification in alternating fresh processes."""

import argparse
import hashlib
import os
from pathlib import Path
import platform
import subprocess
import tempfile


def source_digest(root):
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*.go")) + [root / "go.mod", root / "go.sum"]:
        digest.update(str(path.relative_to(root)).encode() + b"\0" + path.read_bytes())
    return digest.hexdigest()


def configurations(args, repetition):
    for workers in ([2, 8] if repetition % 2 == 0 else [8, 2]):
        modes = [False, True] if args.with_alloc else [False]
        if repetition % 2:
            modes.reverse()
        for alloc in modes:
            operations = ["Migration", "Verification"] if args.with_verify else ["Migration"]
            if repetition % 2:
                operations.reverse()
            for operation in operations:
                name = f"BenchmarkOVM{operation}" + ("Alloc" if alloc else "")
                yield name, workers, alloc
    if args.with_ancient:
        yield "BenchmarkOVMAncientRead", None, False


def timed_command(binary, name, workers):
    selector = f"^{name}$"
    if workers is not None:
        selector += f"/^workers={workers}$"
    command = [str(binary), "-test.run=^$", f"-test.bench={selector}",
               "-test.benchtime=1x", "-test.count=1", "-test.timeout=10m"]
    if platform.system() == "Darwin":
        return ["/usr/bin/time", "-l"] + command
    if Path("/usr/bin/time").exists():
        return ["/usr/bin/time", "-v"] + command
    return command


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--count", type=int, default=3)
    parser.add_argument("--with-alloc", action="store_true",
                        help="pair ordinary conversion with 1000 GenesisAlloc account overrides")
    parser.add_argument("--with-verify", action="store_true", help="also measure standalone verification")
    parser.add_argument("--with-ancient", action="store_true", help="also measure 60000 sequential ancient record reads")
    parser.add_argument("--baseline-root", type=Path,
                        help="alternate with an isolated baseline checkout containing the same benchmark harness")
    args = parser.parse_args()
    if args.count < 2:
        parser.error("count must be at least 2 for paired measurements")
    root = Path(__file__).resolve().parents[1]
    sources = {"current": root}
    if args.baseline_root:
        baseline = args.baseline_root.resolve()
        if baseline == root:
            parser.error("baseline must be an isolated checkout")
        sources = {"baseline": baseline, **sources}
    args.out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="l2state-ovm-bench-") as temp:
        binaries = {}
        required = {name for name, _, _ in configurations(args, 0)}
        for label, source in sources.items():
            binary = Path(temp) / f"{label}.test"
            subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/migration"], cwd=source, check=True)
            available = subprocess.check_output([str(binary), "-test.list=^BenchmarkOVM"], text=True).splitlines()
            if not required.issubset(available):
                parser.error(f"{label} is missing benchmark harness entries: {required - set(available)}")
            binaries[label] = binary
        with args.out.open("x") as output:
            output.write(f"# {platform.platform()} CPUs={os.cpu_count()}\n")
            for label, source in sources.items():
                output.write(f"# {label} source-sha256={source_digest(source)} cache-mb=128 handles=128\n")
            output.write("# Synthetic 10000 holder state, two-block history, hash/Pebble.\n")
            if args.with_alloc:
                output.write("# Alloc: first 1000 holders receive code, balance and one storage override.\n")
            if args.with_ancient:
                output.write("# Ancient microbenchmark: 20000 synthetic blocks, 3 tables, 4 files/table.\n")
            output.write("# Fresh processes; OS caches not flushed; setup excluded from ns/op.\n")
            output.write("# RSS includes setup (also initial migration for verify); sampled disk metric is migration only.\n")
            for repetition in range(args.count):
                labels = list(sources)
                if repetition % 2:
                    labels.reverse()
                for name, workers, alloc in configurations(args, repetition):
                    for label in labels:
                        message = f"sample={repetition + 1} variant={label} benchmark={name} workers={workers} alloc={alloc}"
                        print(message, flush=True)
                        output.write(f"# {message}\n")
                        output.flush()
                        subprocess.run(timed_command(binaries[label], name, workers),
                                       cwd=sources[label] / "internal/migration", stdout=output,
                                       stderr=subprocess.STDOUT, check=True)


if __name__ == "__main__":
    main()
