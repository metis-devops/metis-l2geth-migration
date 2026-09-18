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
                retention_modes = [False, True] if args.with_retain_list else [False]
                if repetition % 2:
                    retention_modes.reverse()
                for retention in retention_modes:
                    name = f"BenchmarkOVM{operation}" + ("Alloc" if alloc else "") + ("Retain" if retention else "")
                    yield name, workers, alloc
    if args.with_ancient:
        yield "BenchmarkOVMAncientRead", None, False


def timed_command(binary, name, workers, component=None):
    selector = f"^{name}$"
    if component:
        backend, count, operation = component
        selector += f"/^backend={backend}$/^records={count}$/^op={operation}$"
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
    parser.add_argument("--with-retain-list", action="store_true",
                        help="pair absent/present manual retention lists on the same source-contract fixture")
    parser.add_argument("--with-verify", action="store_true", help="also measure standalone verification")
    parser.add_argument("--with-preimages", action="store_true",
                        help="also store each holder's address preimage in the source fixture")
    parser.add_argument("--with-ancient", action="store_true", help="also measure 60000 sequential ancient record reads")
    parser.add_argument("--holders", type=int, nargs="+", default=[10000],
                        help="synthetic state sizes, e.g. 10000 100000")
    parser.add_argument("--temp-dbs", choices=["disk", "memory"], nargs="+", default=["disk"],
                        help="alternate temporary storage modes in fresh processes")
    parser.add_argument("--with-components", action="store_true",
                        help="also compare disk/memory Pebble and geth memorydb traces")
    parser.add_argument("--baseline-root", type=Path,
                        help="alternate with an isolated baseline checkout containing the same benchmark harness")
    args = parser.parse_args()
    if args.count < 2:
        parser.error("count must be at least 2 for paired measurements")
    if any(size < 1000 for size in args.holders):
        parser.error("holders must be at least 1000")
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
        if args.with_components:
            required.add("BenchmarkTemporaryDB")
        for label, source in sources.items():
            if args.with_preimages:
                harness = source / "internal/migration/ovm_optimization_benchmark_test.go"
                if "L2STATE_BENCH_PREIMAGES" not in harness.read_text():
                    parser.error(f"{label} lacks the preimage fixture benchmark harness")
            if args.temp_dbs != ["disk"] or args.holders != [10000] or args.with_components:
                harness = source / "internal/migration/tempdb_benchmark_test.go"
                if not harness.exists() or "L2STATE_BENCH_TEMP_DB" not in harness.read_text():
                    parser.error(f"{label} lacks the temporary storage benchmark harness")
            binary = Path(temp) / f"{label}.test"
            subprocess.run(["go", "test", "-c", "-o", str(binary), "./internal/migration"], cwd=source, check=True)
            available = subprocess.check_output([str(binary), "-test.list=^Benchmark"], text=True).splitlines()
            if not required.issubset(available):
                parser.error(f"{label} is missing benchmark harness entries: {required - set(available)}")
            binaries[label] = binary
        with args.out.open("x") as output:
            output.write(f"# {platform.platform()} CPUs={os.cpu_count()}\n")
            for label, source in sources.items():
                output.write(f"# {label} source-sha256={source_digest(source)} cache-mb=128 handles=128\n")
            output.write(f"# Synthetic holders={args.holders}, two-block history, hash/Pebble; temp-dbs={args.temp_dbs}.\n")
            output.write(f"# Holder address preimages: {args.with_preimages}.\n")
            if args.with_alloc:
                output.write("# Alloc: first 1000 holders receive code, balance and one storage override.\n")
            if args.with_retain_list:
                output.write("# Retention fixture: first up-to-1000 existing holders have source code in every paired case; Retain benchmarks list those contracts.\n")
            if args.with_ancient:
                output.write("# Ancient microbenchmark: 20000 synthetic blocks, 3 tables, 4 files/table.\n")
            output.write("# Fresh processes; OS caches not flushed; setup excluded from ns/op.\n")
            output.write("# RSS includes setup (also initial migration for verify); operation heap/files are sampled every 20ms; file lengths exclude allocator capacity.\n")
            for repetition in range(args.count):
                labels = list(sources)
                if repetition % 2:
                    labels.reverse()
                modes = list(args.temp_dbs)
                sizes = list(args.holders)
                if repetition % 2:
                    modes.reverse()
                    sizes.reverse()
                for name, workers, alloc in configurations(args, repetition):
                    for holders in (sizes if workers is not None else [10000]):
                        for mode in (modes if workers is not None else ["disk"]):
                            for label in labels:
                                message = (f"sample={repetition + 1} variant={label} benchmark={name} "
                                           f"workers={workers} alloc={alloc} holders={holders} temp_db={mode}")
                                print(message, flush=True)
                                output.write(f"# {message}\n")
                                output.flush()
                                env = dict(os.environ, L2STATE_BENCH_HOLDERS=str(holders),
                                           L2STATE_BENCH_PREIMAGES="1" if args.with_preimages else "0",
                                           L2STATE_BENCH_TEMP_DB=mode,
                                           L2STATE_BENCH_RETAIN_FIXTURE="1" if args.with_retain_list else "0")
                                subprocess.run(timed_command(binaries[label], name, workers), env=env,
                                               cwd=sources[label] / "internal/migration", stdout=output,
                                               stderr=subprocess.STDOUT, check=True)
                if args.with_components:
                    backends = ["disk", "memory", "geth"]
                    if repetition % 2:
                        backends.reverse()
                    for count in [10000, 100000, 1000000]:
                        for operation in ["batch", "dedup", "get", "scan", "prefix"]:
                            for backend in backends:
                                for label in labels:
                                    message = (f"sample={repetition + 1} variant={label} component={operation} "
                                               f"records={count} backend={backend}")
                                    print(message, flush=True)
                                    output.write(f"# {message}\n")
                                    output.flush()
                                    subprocess.run(timed_command(binaries[label], "BenchmarkTemporaryDB", None,
                                                                 (backend, count, operation)),
                                                   cwd=sources[label] / "internal/migration", stdout=output,
                                                   stderr=subprocess.STDOUT, check=True)



if __name__ == "__main__":
    main()
