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
Use --sync-artifacts to include fsync of every image/snapshot file and its
directories in DurableCheckpoint. The separate Checkpoint metric retains CLI
latency. --background-preflush-ms starts host fsync while the sandbox runs,
then waits the given lead time before checkpoint. This workload stops writing
at readiness, so that experiment measures a best-case opportunity to overlap
writeback, not behavior under continuous application writes.
--candidate-mode=deferred pins the original root filestore, saves external
metadata without an in-window clone, waits for Sentry exit, and clones/syncs
on a host worker. It records StoppedCheckpoint separately from durability.
This experiment requires --sync-artifacts and excludes --leave-running.

Example:
  sudo python3 benchmark.py --baseline /opt/before/runsc \
    --candidate /opt/after/runsc --work-dir /mnt/xfs/filestore-bench \
    --output /tmp/filestore-results --pairs 16 --sizes-mib 64 1024 4096
  benchstat /tmp/filestore-results/{baseline,candidate}.txt
"""

import argparse
import concurrent.futures
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import select
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
    parser.add_argument("--baseline-mode", choices=["full", "external"], default="full")
    parser.add_argument("--candidate-mode", choices=["full", "external", "deferred", "runsc-park"], default="external")
    parser.add_argument("--leave-running", action="store_true")
    parser.add_argument("--verify-clones", action="store_true")
    parser.add_argument("--split-filesystem", action="store_true")
    parser.add_argument("--preflush", action="store_true",
                        help="fsync host filestores before the measured checkpoint; report that cost separately")
    parser.add_argument("--background-preflush-ms", type=int,
                        help="start host filestore fsync on a worker, then checkpoint after this many milliseconds")
    parser.add_argument("--sync-artifacts", action="store_true",
                        help="fsync all restore artifacts and their directories; report total durable checkpoint latency")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run as root")
    if args.pairs < 1 or any(size < 0 for size in args.sizes_mib) or args.memory_mib < 0:
        parser.error("pairs must be positive; sizes and memory limits must be nonnegative")
    if args.background_preflush_ms is not None:
        if args.background_preflush_ms < 0 or args.preflush:
            parser.error("background preflush requires a nonnegative delay and cannot be combined with --preflush")
    if args.candidate_mode in ["deferred", "runsc-park"] and (args.leave_running or not args.sync_artifacts):
        parser.error("asynchronous capture requires --sync-artifacts and cannot use --leave-running")
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
                            if args.sync_artifacts:
                                for metric, key in [("ArtifactSync", "artifact_sync_ns"),
                                                    ("DurableCheckpoint", "durable_checkpoint_ns")]:
                                    bench.write(f"BenchmarkFilestore/{plat}/{mib}MiB/{metric} 1 {data[key]} ns/op\n")
                            if args.background_preflush_ms is not None:
                                bench.write(f"BenchmarkFilestore/{plat}/{mib}MiB/BackgroundPreflush 1 {data['background_preflush']['duration_ns']} ns/op\n")
                            if data["stopped_checkpoint_ns"] is not None:
                                bench.write(f"BenchmarkFilestore/{plat}/{mib}MiB/StoppedCheckpoint 1 {data['stopped_checkpoint_ns']} ns/op\n")
                        snapshot_phases = data["snapshot_phases"] or data["deferred_snapshot_phases"] or data["park_snapshot_phases"]
                        if snapshot_phases:
                            with (args.output / f"{variant}-phases.txt").open("a") as phases:
                                for key in snapshot_phases[0]:
                                    value = sum(x[key] for x in snapshot_phases)
                                    phases.write(f"BenchmarkFilestore/{plat}/{mib}MiB/{key} 1 {value} ns/op\n")
                                for key, value in data["manifest_phases"].items():
                                    phases.write(f"BenchmarkFilestore/{plat}/{mib}MiB/{key} 1 {value} ns/op\n")
                        print(json.dumps(data), flush=True)
                        shutil.rmtree(trial)


def open_filestores(pid):
    """Open independent descriptors, including unlinked filestores."""
    files = []
    try:
        for file in Path(f"/proc/{pid}/fd").iterdir():
            try:
                name = os.readlink(file)
            except FileNotFoundError:
                continue
            if "runsc-filestore-" in name:
                files.append(os.open(file, os.O_RDWR))
        if not files:
            raise RuntimeError("no host filestore descriptor found for preflush")
        return files
    except BaseException:
        for fd in files:
            os.close(fd)
        raise


def sync_filestores(files):
    """Own and close files; return the actual worker interval."""
    start = time.monotonic_ns()
    calls = []
    try:
        for fd in files:
            before = time.monotonic_ns()
            os.fsync(fd)
            calls.append(time.monotonic_ns() - before)
    finally:
        for fd in files:
            os.close(fd)
    end = time.monotonic_ns()
    return {"start_ns": start, "duration_ns": end - start, "fsync_calls_ns": calls}


def sync_artifacts(roots, parent):
    """Sync only this checkpoint's restore inputs, then their directories."""
    files = sorted(file for root in roots for file in root.rglob("*") if file.is_file())
    dirs = {parent}
    calls = []
    for file in files:
        directory = file.parent
        while directory != parent:
            dirs.add(directory)
            directory = directory.parent
    for root in roots:
        dirs.update(directory for directory in root.rglob("*") if directory.is_dir())
    dirs.update(roots)
    for file, kind in [(file, "file") for file in files] + [
            (directory, "directory") for directory in sorted(dirs, key=lambda d: (-len(d.parts), str(d)))]:
        flags = os.O_RDONLY | os.O_CLOEXEC
        if kind == "directory":
            flags |= os.O_DIRECTORY
        fd = os.open(file, flags)
        try:
            start = time.monotonic_ns()
            os.fsync(fd)
            elapsed = time.monotonic_ns() - start
        finally:
            os.close(fd)
        calls.append({"path": str(file.relative_to(parent)), "kind": kind, "duration_ns": elapsed})
    return calls


def snapshot_timings(logs):
    snapshots = []
    manifest = {}
    for file in sorted(logs.glob("*.txt")):
        data = file.read_text()
        for fields in re.findall(r"filestore snapshot timing: ([^\n]+)", data):
            snapshots.append({key: int(value) for key, value in re.findall(r"(\w+_ns)=(\d+)", fields)})
        for fields in re.findall(r"Filestore manifest timing: ([^\n]+)", data):
            for key, value in re.findall(r"(\w+_ns)=(\d+)", fields):
                manifest[key] = manifest.get(key, 0) + int(value)
    return snapshots, manifest


def commit_deferred(files, image, snapshot, trial):
    """Own stopped-sandbox filestores until independent artifacts are durable."""
    phases = []
    try:
        snapshot.mkdir()
        for index, source in enumerate(files):
            dest = os.open(snapshot / f"filestore-{index}", os.O_RDWR | os.O_CREAT | os.O_EXCL, 0o600)
            try:
                start = time.monotonic_ns()
                # Linux FICLONE, from linux/fs.h. This runs on the host.
                fcntl.ioctl(dest, 0x40049409, source)
                cloned = time.monotonic_ns()
                os.fsync(dest)
                synced = time.monotonic_ns()
                phases.append({"ficlone_ns": cloned - start, "fsync_ns": synced - cloned})
            finally:
                os.close(dest)
        start = time.monotonic_ns()
        calls = sync_artifacts([image, snapshot], trial)
        elapsed = time.monotonic_ns() - start
        return phases, calls, elapsed
    finally:
        for fd in files:
            os.close(fd)


def run_trial(args, trial, rootfs, plat, mib, variant):
    binary = getattr(args, variant)
    mode = args.candidate_mode if variant == "candidate" else args.baseline_mode
    external = mode != "full"
    deferred = mode == "deferred"
    runsc_park = mode == "runsc-park"
    bundle = trial / "bundle"
    bundle.mkdir()
    filestores = trial / "filestores"
    filestores.mkdir()
    job = trial / "park-job"
    image = job / "image" if runsc_park else trial / "image"
    if not runsc_park:
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
    if external and not deferred and not runsc_park:
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

    flush_executor = None
    flush_future = None
    commit_executor = None
    preserved_files = []
    source_pidfd = None
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
        if args.candidate_mode in ["deferred", "runsc-park"]:
            if state is None:
                state = json.loads(subprocess.check_output(common + ["state", ids["source"]], timeout=30))
            source_pidfd = os.pidfd_open(state['pid'])
            if deferred:
                preserved_files = open_filestores(state['pid'])
                if len(preserved_files) != 1:
                    raise RuntimeError("deferred benchmark supports exactly one root filestore")
        preflush_ns = 0
        if args.preflush or args.background_preflush_ms is not None:
            start = time.monotonic_ns()
            if state is None:
                state = json.loads(subprocess.check_output(common + ["state", ids["source"]], timeout=30))
            files = open_filestores(state['pid'])
            if args.preflush:
                sync_filestores(files)
                preflush_ns = time.monotonic_ns() - start
            else:
                flush_executor = concurrent.futures.ThreadPoolExecutor(max_workers=1)
                flush_future = flush_executor.submit(sync_filestores, files)
                time.sleep(args.background_preflush_ms / 1000)
        checkpoint = ["checkpoint", f"--image-path={image}"]
        if args.split_filesystem:
            checkpoint.append("--fs-checkpoint-paths=/")
        if args.leave_running:
            checkpoint.append("--leave-running")
        if external:
            checkpoint.append("--skip-filestore-pages")
            if not deferred:
                checkpoint.append(f"--filestore-snapshot-dir={trial / 'snapshot'}")
        if runsc_park:
            checkpoint = ["park", f"--job-dir={job}", "--completion=stopped"]
            if args.split_filesystem:
                checkpoint.append("--fs-checkpoint-paths=/")
        checkpoint.append("source")
        checkpoint_start = time.monotonic_ns()
        checkpoint_ns, _ = run(checkpoint)
        stopped_checkpoint_ns = None
        source_exit_wait_ns = 0
        if source_pidfd is not None:
            before = time.monotonic_ns()
            poller = select.poll()
            poller.register(source_pidfd, select.POLLIN)
            if not poller.poll(180000):
                raise RuntimeError("source Sentry did not exit after non-resuming checkpoint")
            source_exit_wait_ns = time.monotonic_ns() - before
            stopped_checkpoint_ns = time.monotonic_ns() - checkpoint_start
        park_status = None
        park_wait_ready_ns = 0
        if runsc_park:
            before = time.monotonic_ns()
            park_status = json.loads(subprocess.check_output(
                common + ["park-status", f"--job-dir={job}", "--wait=ready"], timeout=180))
            park_wait_ready_ns = time.monotonic_ns() - before
            if park_status["state"] != "ready" or not park_status.get("stopped_at"):
                raise RuntimeError("park did not reach both completion barriers")
        artifact_sync_start = time.monotonic_ns()
        artifact_sync_calls = []
        deferred_phases = []
        if deferred:
            commit_executor = concurrent.futures.ThreadPoolExecutor(max_workers=1)
            commit_future = commit_executor.submit(commit_deferred, preserved_files, image, trial / "snapshot", trial)
            preserved_files = []  # The commit worker now owns these descriptors.
            deferred_phases, artifact_sync_calls, artifact_sync_ns = commit_future.result(timeout=180)
        elif args.sync_artifacts:
            snapshot = job / "filestores" if runsc_park else trial / "snapshot"
            roots = [image, snapshot] if external else [image]
            artifact_sync_calls = sync_artifacts(roots, trial)
            artifact_sync_ns = time.monotonic_ns() - artifact_sync_start
        else:
            artifact_sync_ns = 0
        durable_checkpoint_ns = time.monotonic_ns() - checkpoint_start if args.sync_artifacts else None
        snapshots, manifest = snapshot_timings(logs)
        background_preflush = None
        if flush_future is not None:
            background_preflush = flush_future.result(timeout=180)
            background_preflush["lead_ns"] = checkpoint_start - background_preflush["start_ns"]
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
                if args.sync_artifacts:
                    roots = [second_image, trial / "second-snapshot"] if external else [second_image]
                    sync_artifacts(roots, trial)
                second_extra = [f"--filestore-adopt-dir={trial / 'second-snapshot'}"] if external else []
                run(["restore", "--detach", f"--bundle={bundle}",
                     f"--image-path={second_image}", "newer"], extra=second_extra)
                if run(["exec", "newer", "/workload", "unmarked"], check=False)[1] != 1:
                    raise RuntimeError("second checkpoint did not preserve the newer file state")
                run(["exec", "newer", "/workload", "verify", str(size)])
        image_bytes = sum(p.stat().st_size for p in image.rglob("*") if p.is_file())
        snapshot = job / "filestores" if runsc_park else trial / "snapshot"
        extra = [f"--filestore-adopt-dir={snapshot}"] if external else []
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
        return {"capture_mode": mode, "checkpoint_ns": checkpoint_ns, "restore_ns": restore_ns,
                "image_bytes": image_bytes, "verified_bytes": size,
                "preflush_ns": preflush_ns,
                "artifact_sync_ns": artifact_sync_ns,
                "durable_checkpoint_ns": durable_checkpoint_ns,
                "artifact_sync_calls": artifact_sync_calls,
                "snapshot_phases": snapshots, "manifest_phases": manifest,
                "deferred_snapshot_phases": deferred_phases,
                "park_snapshot_phases": [{"ficlone_ns": park_status.get("ficlone_ns", 0), "fsync_ns": park_status.get("snapshot_fsync_ns", 0)}] if park_status else [],
                "park_status": park_status, "park_wait_ready_ns": park_wait_ready_ns,
                "stopped_checkpoint_ns": stopped_checkpoint_ns,
                "source_exit_wait_ns": source_exit_wait_ns,
                "background_preflush": background_preflush,
                "effective_memory_limit_bytes": effective_memory_limit,
                "memory_limit_bytes": args.memory_mib << 20}
    finally:
        if flush_executor is not None:
            flush_executor.shutdown(wait=True)
        if commit_executor is not None:
            commit_executor.shutdown(wait=True)
        for fd in preserved_files:
            os.close(fd)
        if source_pidfd is not None:
            os.close(source_pidfd)
        for cid in ids:
            run(["delete", "--force", cid], check=False)


if __name__ == "__main__":
    main()
