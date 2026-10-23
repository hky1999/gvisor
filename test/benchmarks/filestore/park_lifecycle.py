#!/usr/bin/env python3
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Black-box park lifecycle checks against an entire installed runsc release.

Requires root, a reflink filesystem, and gcc with static libc. Checks capture
failure before ownership acknowledgement, completion gates, source deletion,
worker death/cleanup, moved checkpoints, and independent restored layers.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import select
import shutil
import signal
import subprocess
import tempfile
import time


class Runtime:
    def __init__(self, args, name, size, extra=False):
        self.dir = args.work_dir / name
        self.dir.mkdir()
        self.logs = args.output / name
        self.logs.mkdir()
        self.bundle = self.dir / "bundle"
        self.bundle.mkdir()
        rootfs = args.work_dir / "rootfs"
        self.common = [str(args.runsc), f"--root={args.work_dir / 'runtime'}",
                       "--platform=systrap", "--network=none",
                       f"--overlay2=all:dir={self.dir / 'stores'}",
                       "--debug", f"--debug-log={self.logs}/"]
        (self.dir / "stores").mkdir()
        spec = {"ociVersion": "1.0.2", "process": {"terminal": False,
                "user": {"uid": 0, "gid": 0}, "args": ["/workload", "serve", str(size)],
                "env": [], "cwd": "/"}, "root": {"path": str(rootfs), "readonly": False},
                "mounts": [{"destination": "/proc", "type": "proc", "source": "proc"}],
                "linux": {"namespaces": [{"type": item} for item in ["pid", "mount", "ipc", "uts", "network"]]}}
        if extra:
            lower = self.dir / "lower"
            lower.mkdir()
            spec["mounts"].append({"destination": "/extra", "type": "bind", "source": str(lower), "options": ["rbind", "rw"]})
        (self.bundle / "config.json").write_text(json.dumps(spec))
        self.ids = []
        self.source = self.id("source")
        self.size = size

    def id(self, label):
        value = "parkcheck-" + hashlib.sha256(str(self.dir).encode()).hexdigest()[:12] + "-" + label
        self.ids.append(value)
        return value

    def run(self, *command, extra=(), success=True):
        # Detached sandbox processes can retain standard output. A pipe would
        # make communicate() wait for sandbox exit after runsc has returned.
        with tempfile.TemporaryFile() as stdout, tempfile.TemporaryFile() as stderr:
            result = subprocess.run(self.common + list(extra) + list(command),
                                    stdout=stdout, stderr=stderr, timeout=180)
            stdout.seek(0)
            stderr.seek(0)
            result.stdout = stdout.read()
            result.stderr = stderr.read()
        with (self.logs / "commands.log").open("ab") as log:
            log.write((json.dumps(list(extra) + list(command)) + "\n").encode())
            log.write(result.stdout + result.stderr)
        if success and result.returncode:
            raise RuntimeError(f"command failed: {command}: {result.stderr.decode(errors='replace')}")
        return result

    def start(self):
        self.run("run", "--detach", f"--bundle={self.bundle}", self.source)
        deadline = time.monotonic() + 120
        while self.run("exec", self.source, "/workload", "ready", success=False).returncode:
            if time.monotonic() > deadline:
                raise RuntimeError("workload did not become ready")
            time.sleep(0.05)

    def status(self, job, wait="none", success=True):
        result = self.run("park-status", f"--job-dir={job}", f"--wait={wait}", success=success)
        return json.loads(result.stdout), result.returncode

    def park(self, job, completion="stopped"):
        result = self.run("park", f"--job-dir={job}", f"--completion={completion}", self.source)
        return json.loads(result.stdout)

    def restore(self, job, label, consume=False, success=True):
        cid = self.id(label)
        extra = [f"--filestore-adopt-dir={job / 'filestores'}"]
        if consume:
            extra.append("--filestore-adopt-mode=consume")
        result = self.run("restore", "--detach", f"--bundle={self.bundle}",
                          f"--image-path={job / 'image'}", cid, extra=extra, success=success)
        return cid, result.returncode

    def close(self):
        for cid in self.ids:
            self.run("delete", "--force", cid, success=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runsc", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--large-mib", type=int, default=1024)
    args = parser.parse_args()
    if os.geteuid() or args.large_mib < 1:
        parser.error("requires root and a positive large workload size")
    args.work_dir.mkdir(exist_ok=False)
    args.output.mkdir(parents=True, exist_ok=True)
    rootfs = args.work_dir / "rootfs"
    rootfs.mkdir()
    (rootfs / "proc").mkdir()
    (rootfs / "extra").mkdir()
    subprocess.run(["gcc", "-O2", "-static", str(Path(__file__).with_name("workload.c")),
                    "-o", str(rootfs / "workload")], check=True)
    outcomes = []

    def passed(name, **details):
        outcomes.append({"name": name, "passed": True, **details})
        (args.output / "results.json").write_text(json.dumps(outcomes, indent=2) + "\n")
        print(name, "passed", flush=True)

    rt = Runtime(args, "moved-multiple-filestores", 1 << 20, extra=True)
    try:
        rt.start()
        rt.run("exec", rt.source, "/workload", "write", str(2 << 20), "/extra/payload")
        job = rt.dir / "job"
        completed = rt.park(job, "ready")
        assert completed["state"] == "ready" and completed["stopped_at"] != completed["durable_at"]
        manifest = json.loads((job / "filestores/filestores.json").read_text())
        assert {entry["resource_path"] for entry in manifest["filestores"]} == {"/", "/extra"}
        moved = rt.dir / "moved"
        job.rename(moved)
        state, _ = rt.status(moved)
        assert state["image_path"] == str(moved / "image")
        first, _ = rt.restore(moved, "first")
        rt.run("exec", first, "/workload", "verify", str(1 << 20))
        rt.run("exec", first, "/workload", "verify", str(2 << 20), "/extra/payload")
        rt.run("exec", first, "/workload", "mark")
        second, _ = rt.restore(moved, "second")
        rt.run("exec", second, "/workload", "unmarked")
        rt.run("exec", second, "/workload", "verify", str(2 << 20), "/extra/payload")
        # Named consuming artifacts are rejected before acknowledgement; the
        # failed transfer must resume the source, not kill or truncate it.
        consumed, _ = rt.restore(moved, "consumed", consume=True)
        rejected = rt.run("park", f"--job-dir={rt.dir / 'rejected'}", consumed, success=False)
        assert rejected.returncode != 0
        state, _ = rt.status(rt.dir / "rejected")
        assert state["state"] == "failed" and not state.get("stopped_at") and state["cleanup_complete"]
        rt.run("exec", consumed, "/workload", "verify", str(1 << 20))
        passed("moved_multi_filestore_restore_and_transfer_abort", filestores=2)
    finally:
        rt.close()

    rt = Runtime(args, "unsupported-and-paused", 1 << 20)
    try:
        rt.start()
        assert rt.run("park", f"--job-dir={rt.dir / 'bad'}", "--leave-running", rt.source, success=False).returncode
        rt.run("pause", rt.source)
        assert rt.run("park", f"--job-dir={rt.dir / 'paused'}", rt.source, success=False).returncode
        rt.run("resume", rt.source)
        rt.run("exec", rt.source, "/workload", "verify", str(rt.size))
        passed("unsupported_options_preserve_source")
    finally:
        rt.close()

    rt = Runtime(args, "worker-loss", args.large_mib << 20)
    worker = None
    try:
        rt.start()
        job = rt.dir / "job"
        stopped = rt.park(job)
        assert stopped.get("stopped_at") and stopped["state"] != "ready", "commit finished before fault injection; increase --large-mib"
        worker = os.pidfd_open(stopped["worker_pid"])
        signal.pidfd_send_signal(worker, signal.SIGSTOP)
        # The source container can be deleted; the detached worker owns the FD.
        rt.run("delete", "--force", rt.source)
        _, code = rt.restore(job, "pending", success=False)
        assert code != 0
        state, _ = rt.status(job)
        assert state.get("stopped_at") and state["state"] != "ready"
        timed = rt.run("park-status", f"--job-dir={job}", "--wait=ready", "--timeout=20ms", success=False)
        assert timed.returncode
        signal.pidfd_send_signal(worker, signal.SIGKILL)
        poller = select.poll()
        poller.register(worker, select.POLLIN)
        assert poller.poll(30000), "worker did not exit after fault injection"
        state, code = rt.status(job, "ready", success=False)
        assert code != 0 and state["state"] == "failed" and state["cleanup_complete"]
        assert state.get("stopped_at") and not state.get("durable_at")
        assert not (job / "image").exists() and not (job / "filestores").exists()
        passed("pending_restore_timeout_worker_death_and_cleanup")
    finally:
        if worker is not None:
            try:
                signal.pidfd_send_signal(worker, signal.SIGKILL)
            except ProcessLookupError:
                pass
            os.close(worker)
        rt.close()

    rt = Runtime(args, "detached-source-deletion", args.large_mib << 20)
    try:
        rt.start()
        job = rt.dir / "job"
        stopped = rt.park(job)
        assert stopped.get("stopped_at")
        rt.run("delete", "--force", rt.source)
        ready, _ = rt.status(job, "ready")
        assert ready["state"] == "ready" and ready["stopped_at"] != ready["durable_at"]
        restored, _ = rt.restore(job, "restored")
        rt.run("exec", restored, "/workload", "verify", str(rt.size))
        passed("detached_commit_survives_source_deletion", verified_bytes=rt.size)
    finally:
        rt.close()


if __name__ == "__main__":
    main()
