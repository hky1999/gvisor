#!/usr/bin/env python3
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Compare full C/R with external filestores on the same runtime source base.

Requires root, a reflink filesystem, and gcc with static libc. Both runsc
arguments must refer to complete release directories, with sidecar binaries.
The script creates isolated OCI bundles and runtime state under --work-dir.
Every restore checks the complete nonzero payload. Results are saved as JSON
and Go benchmark text for benchstat. Alternate sample order to reduce drift.

Example:
  sudo python3 benchmark.py --baseline /opt/before/runsc \
    --candidate /opt/after/runsc --work-dir /mnt/xfs/filestore-bench \
    --output /tmp/filestore-results --pairs 16 --sizes-mib 64 1024 4096
  benchstat /tmp/filestore-results/{baseline,candidate}.txt
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--pairs", type=int, default=16)
    parser.add_argument("--sizes-mib", type=int, nargs="+", default=[64, 1024, 4096])
    parser.add_argument("--platforms", nargs="+", default=["systrap"])
    parser.add_argument("--memory-mib", type=int, default=0)
    parser.add_argument("--candidate-mode", choices=["full", "external"], default="external")
    parser.add_argument("--leave-running", action="store_true")
    parser.add_argument("--verify-clones", action="store_true")
    parser.add_argument("--split-filesystem", action="store_true")
    parser.add_argument("--preflush", action="store_true",
                        help="fsync host filestores before the measured checkpoint; report that cost separately")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run as root")
    if args.pairs < 1 or any(size < 0 for size in args.sizes_mib) or args.memory_mib < 0:
        parser.error("pairs must be positive; sizes and memory limits must be nonnegative")
    args.work_dir.mkdir(parents=True, exist_ok=False)
    args.output.mkdir(parents=True, exist_ok=True)
    rootfs = args.work_dir / "rootfs"
    rootfs.mkdir()
    (rootfs / "proc").mkdir()
    subprocess.run(
        ["gcc", "-O2", "-static", str(Path(__file__).with_name("workload.c")),
         "-o", str(rootfs / "workload")], check=True)
    (args.output / "environment.json").write_text(json.dumps({
        "uname": platform.uname()._asdict(),
        "args": {k: str(v) if isinstance(v, Path) else v for k, v in vars(args).items()},
    }, indent=2) + "\n")
    with (args.output / "samples.jsonl").open("a") as samples:
        for plat in args.platforms:
            for mib in args.sizes_mib:
                for pair in range(args.pairs):
                    order = ["baseline", "candidate"] if pair % 2 == 0 else ["candidate", "baseline"]
                    for variant in order:
                        trial = args.work_dir / f"{plat}-{mib}-{pair}-{variant}"
                        trial.mkdir()
                        data = run_trial(args, trial, rootfs, plat, mib, variant)
                        data.update(platform=plat, mib=mib, pair=pair, variant=variant)
                        samples.write(json.dumps(data) + "\n")
                        samples.flush()
                        with (args.output / f"{variant}.txt").open("a") as bench:
                            for metric, value, unit in [
                                ("Checkpoint", data["checkpoint_ns"], "ns/op"),
                                ("Restore", data["restore_ns"], "ns/op"),
                                ("Image", data["image_bytes"], "bytes/op"),
                            ]:
                                bench.write(f"BenchmarkFilestore/{plat}/{mib}MiB/{metric} 1 {value} {unit}\n")
                            if args.preflush:
                                bench.write(f"BenchmarkFilestore/{plat}/{mib}MiB/Preflush 1 {data['preflush_ns']} ns/op\n")
                        print(json.dumps(data), flush=True)
                        shutil.rmtree(trial)


def run_trial(args, trial, rootfs, plat, mib, variant):
    binary = getattr(args, variant)
    external = variant == "candidate" and args.candidate_mode == "external"
    bundle = trial / "bundle"
    bundle.mkdir()
    filestores = trial / "filestores"
    filestores.mkdir()
    image = trial / "image"
    image.mkdir()
    logs = args.output / trial.name
    logs.mkdir()
    size = mib << 20
    spec = {
        "ociVersion": "1.0.2",
        "process": {"terminal": False, "user": {"uid": 0, "gid": 0},
                    "args": ["/workload", "serve", str(size)], "env": [], "cwd": "/"},
        "root": {"path": str(rootfs), "readonly": False},
        "hostname": "filestore-benchmark",
        "mounts": [{"destination": "/proc", "type": "proc", "source": "proc"}],
        "linux": {"namespaces": [{"type": n} for n in ["pid", "mount", "ipc", "uts", "network"]]},
    }
    if args.memory_mib:
        spec["linux"]["resources"] = {"memory": {"limit": args.memory_mib << 20}}
    (bundle / "config.json").write_text(json.dumps(spec))
    # The runtime root can retain its FD parking mount after container deletion.
    # Keep one root per build outside the disposable trial directories.
    common = [str(binary), f"--root={args.work_dir / ('runtime-' + variant)}", f"--platform={plat}",
              "--network=none", f"--overlay2=all:dir={filestores}", "--debug",
              f"--debug-log={logs}/"]
    if external:
        common.append("--filestore-reflink=enabled")
    prefix = hashlib.sha256(str(trial.resolve()).encode()).hexdigest()[:16]
    ids = {label: f"fsbench-{prefix}-{label}"
           for label in ["source", "restored", "clone", "newer"]}

    def run(command, extra=(), check=True):
        with (logs / "commands.log").open("ab") as log:
            start = time.monotonic_ns()
            command = [ids.get(arg, arg) for arg in command]
            result = subprocess.run(common + list(extra) + command, stdout=log,
                                    stderr=log, timeout=180, check=check)
            return time.monotonic_ns() - start, result.returncode

    try:
        run(["run", "--detach", f"--bundle={bundle}", "source"])
        deadline = time.monotonic() + 120
        while run(["exec", "source", "/workload", "ready"], check=False)[1]:
            if time.monotonic() > deadline:
                raise RuntimeError(f"workload did not become ready: {logs}")
            time.sleep(0.05)
        # The workload issues fsync before readiness. Host filestore writeback
        # required by FICLONE remains inside the checkpoint measurement.
        state = None
        effective_memory_limit = None
        if args.memory_mib:
            state = json.loads(subprocess.check_output(common + ["state", ids["source"]], timeout=30))
            cgroups = Path(f"/proc/{state['pid']}/cgroup").read_text().splitlines()
            controller = next((line.split(":", 2) for line in cgroups
                               if line.startswith("0::") or "memory" in line.split(":", 2)[1].split(",")), None)
            if controller is None:
                raise RuntimeError("cannot find the sandbox memory cgroup")
            base = Path("/sys/fs/cgroup") if not controller[1] else Path("/sys/fs/cgroup/memory")
            group = base / controller[2].lstrip("/")
            limits = []
            while group.is_relative_to(base):
                limit_file = group / ("memory.max" if not controller[1] else "memory.limit_in_bytes")
                if limit_file.exists():
                    value = limit_file.read_text().strip()
                    if value != "max":
                        limits.append(int(value))
                if group == base:
                    break
                group = group.parent
            effective_memory_limit = min(limits) if limits else None
            if effective_memory_limit != args.memory_mib << 20:
                raise RuntimeError(f"effective memory limit is {effective_memory_limit}, wanted {args.memory_mib << 20}")
        preflush_ns = 0
        if args.preflush:
            start = time.monotonic_ns()
            if state is None:
                state = json.loads(subprocess.check_output(common + ["state", ids["source"]], timeout=30))
            flushed = 0
            for file in Path(f"/proc/{state['pid']}/fd").iterdir():
                try:
                    name = os.readlink(file)
                except FileNotFoundError:
                    continue
                if "runsc-filestore-" not in name:
                    continue
                fd = os.open(file, os.O_RDWR)
                try:
                    os.fsync(fd)
                    flushed += 1
                finally:
                    os.close(fd)
            if not flushed:
                raise RuntimeError("no host filestore descriptor found for preflush")
            preflush_ns = time.monotonic_ns() - start
        checkpoint = ["checkpoint", f"--image-path={image}"]
        if args.split_filesystem:
            checkpoint.append("--fs-checkpoint-paths=/")
        if args.leave_running:
            checkpoint.append("--leave-running")
        if external:
            checkpoint.extend(["--skip-filestore-pages", f"--filestore-snapshot-dir={trial / 'snapshot'}"])
        checkpoint.append("source")
        checkpoint_ns, _ = run(checkpoint)
        if args.leave_running:
            run(["exec", "source", "/workload", "mark"])
            if args.verify_clones:
                second_image = trial / "second-image"
                second_image.mkdir()
                second_checkpoint = [
                    arg.replace(f"--image-path={image}", f"--image-path={second_image}")
                    .replace(f"--filestore-snapshot-dir={trial / 'snapshot'}",
                             f"--filestore-snapshot-dir={trial / 'second-snapshot'}")
                    for arg in checkpoint
                ]
                run(second_checkpoint)
                second_extra = [f"--filestore-adopt-dir={trial / 'second-snapshot'}"] if external else []
                run(["restore", "--detach", f"--bundle={bundle}",
                     f"--image-path={second_image}", "newer"], extra=second_extra)
                if run(["exec", "newer", "/workload", "unmarked"], check=False)[1] != 1:
                    raise RuntimeError("second checkpoint did not preserve the newer file state")
                run(["exec", "newer", "/workload", "verify", str(size)])
        image_bytes = sum(p.stat().st_size for p in image.rglob("*") if p.is_file())
        extra = [f"--filestore-adopt-dir={trial / 'snapshot'}"] if external else []
        restore_ns, _ = run(["restore", "--detach", f"--bundle={bundle}",
                             f"--image-path={image}", "restored"], extra=extra)
        run(["exec", "restored", "/workload", "verify", str(size)])
        run(["exec", "restored", "/workload", "unmarked"])
        if args.verify_clones:
            run(["exec", "restored", "/workload", "mark"])
            run(["restore", "--detach", f"--bundle={bundle}",
                 f"--image-path={image}", "clone"], extra=extra)
            run(["exec", "clone", "/workload", "unmarked"])
            run(["exec", "clone", "/workload", "verify", str(size)])
        return {"checkpoint_ns": checkpoint_ns, "restore_ns": restore_ns,
                "image_bytes": image_bytes, "verified_bytes": size,
                "preflush_ns": preflush_ns,
                "effective_memory_limit_bytes": effective_memory_limit,
                "memory_limit_bytes": args.memory_mib << 20}
    finally:
        for cid in ids:
            run(["delete", "--force", cid], check=False)


if __name__ == "__main__":
    main()
