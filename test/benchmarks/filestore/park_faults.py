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

"""Exercise concurrent park, pre-ACK worker loss, and real ENOSPC cleanup.

Requires root, static gcc, losetup, mount, and mkfs.xfs. Disk-full injection is
restricted to newly created private 4 GiB loop filesystems. Never fills the
caller's existing filesystem. Install/use the entire runsc release directory.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import errno
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import threading
import time

from park_lifecycle import Runtime


class Volume:
    def __init__(self, directory):
        self.directory = directory
        self.loop = None
        self.mounted = False

    def __enter__(self):
        self.directory.mkdir()
        self.mount = self.directory / "mnt"
        self.mount.mkdir()
        self.image = self.directory / "volume.img"
        with self.image.open("xb") as image:
            image.truncate(4 << 30)
        try:
            self.loop = subprocess.check_output(
                ["losetup", "--find", "--show", str(self.image)], text=True).strip()
            subprocess.run(["mkfs.xfs", "-f", "-m", "reflink=1", self.loop],
                           check=True, stdout=subprocess.DEVNULL)
            subprocess.run(["mount", self.loop, str(self.mount)], check=True)
            self.mounted = True
            return self
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def fill(self):
        # Verify the private loop mount before allocating any filler blocks.
        source = subprocess.check_output(
            ["findmnt", "-n", "-o", "SOURCE", "--target", str(self.mount)], text=True).strip()
        assert source == self.loop, (source, self.loop)
        filler = self.mount / "filler"
        offset = 0
        with filler.open("xb") as file:
            for size in [1 << 30, 64 << 20, 1 << 20, 4096]:
                while True:
                    try:
                        os.posix_fallocate(file.fileno(), offset, size)
                        offset += size
                    except OSError as err:
                        if err.errno != errno.ENOSPC:
                            raise
                        break
            os.fsync(file.fileno())
            try:
                os.posix_fallocate(file.fileno(), offset, 4096)
            except OSError as err:
                assert err.errno == errno.ENOSPC
            else:
                raise AssertionError("private filesystem did not reach ENOSPC")
        return offset

    def __exit__(self, *unused):
        if self.mounted:
            subprocess.run(["umount", str(self.mount)], check=True)
        if self.loop:
            subprocess.run(["losetup", "--detach", self.loop], check=True)
        self.image.unlink(missing_ok=True)


def wait_for(predicate, message, timeout=30):
    deadline = time.monotonic() + timeout
    while not predicate():
        if time.monotonic() > deadline:
            raise AssertionError(message)
        time.sleep(0.01)


def worker_exit(pidfd):
    poller = select.poll()
    poller.register(pidfd, select.POLLIN)
    assert poller.poll(30000), "worker did not exit"


def fd_targets(directory):
    result = {}
    for file in directory.iterdir():
        try:
            result[file.name] = os.readlink(file)
        except FileNotFoundError:
            pass
    return result


def wait_cleanup(runtime, job):
    # Failure can be published while the owner is still removing outputs.
    # Observe the separate cleanup completion barrier before asserting it.
    state = {}

    def complete():
        nonlocal state
        state, _ = runtime.status(job, success=False)
        return state["state"] == "failed" and state["cleanup_complete"]

    wait_for(complete, "failure cleanup did not complete")
    return state


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runsc", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--cases", nargs="+", choices=["concurrent", "early-loss", "enospc"],
                        default=["concurrent", "early-loss", "enospc"])
    args = parser.parse_args()
    if os.geteuid():
        parser.error("requires root")
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

    if "concurrent" in args.cases:
        for same_job in [False, True]:
            rt = Runtime(args, "same-job" if same_job else "same-source", 1 << 20)
            try:
                rt.start()
                barrier = threading.Barrier(8)
                jobs = [rt.dir / ("job" if same_job else f"job-{i}") for i in range(8)]

                def launch(job):
                    barrier.wait()
                    return rt.run("park", f"--job-dir={job}", "--completion=none",
                                  "--save-restore-exec-argv=/workload delay 3", rt.source, success=False)

                with ThreadPoolExecutor(max_workers=8) as pool:
                    results = list(pool.map(launch, jobs))
                assert any(result.returncode == 0 for result in results)
                ready = []
                for job in sorted(set(jobs)):
                    if not job.exists():
                        continue
                    state, code = rt.status(job, "ready", success=False)
                    if not code:
                        ready.append(job)
                    else:
                        state = wait_cleanup(rt, job)
                        assert state["state"] == "failed" and state["cleanup_complete"], state
                assert len(ready) == 1, ready
                restored, _ = rt.restore(ready[0], "restored")
                rt.run("exec", restored, "/workload", "verify", str(rt.size))
                passed("concurrent_same_job" if same_job else "concurrent_same_source", requests=8)
            finally:
                rt.close()

        instances = [Runtime(args, f"independent-{i}", 1 << 20) for i in range(2)]
        try:
            for rt in instances:
                rt.start()
            with ThreadPoolExecutor(max_workers=2) as pool:
                states = list(pool.map(lambda rt: rt.park(rt.dir / "job", "ready"), instances))
            assert all(state["state"] == "ready" for state in states)
            for rt in instances:
                restored, _ = rt.restore(rt.dir / "job", "restored")
                rt.run("exec", restored, "/workload", "verify", str(rt.size))
            passed("concurrent_independent_sources", sources=2)
        finally:
            for rt in instances:
                rt.close()

    if "early-loss" in args.cases:
        rt = Runtime(args, "early-worker-loss", 1 << 20)
        try:
            rt.start()
            source_pid = json.loads(rt.run("state", rt.source).stdout)["pid"]
            source_fds = Path(f"/proc/{source_pid}/fd")
            before = fd_targets(source_fds)
            before_fds = len(before)
            job = rt.dir / "job"
            rt.run("park", f"--job-dir={job}", "--completion=none",
                   "--save-restore-exec-argv=/workload save-hook", rt.source)
            state = {}

            def capturing():
                nonlocal state
                state, _ = rt.status(job)
                return state["state"] == "capturing"

            wait_for(capturing, "worker did not enter capture")
            # Two copies prove the save RPC converted the donated image FD;
            # merely seeing host-side 'capturing' can precede the RPC itself.
            image_name = str(job / "image/checkpoint.img")
            wait_for(lambda: sum(target == image_name for target in fd_targets(source_fds).values()) >= 2,
                     "source did not enter the save RPC")
            time.sleep(0.1)
            during = fd_targets(source_fds)
            transient_sockets = {target for target in during.values()
                                 if target.startswith("socket:") and target not in before.values()}
            worker = os.pidfd_open(state["worker_pid"])
            try:
                signal.pidfd_send_signal(worker, signal.SIGKILL)
                worker_exit(worker)
            finally:
                os.close(worker)
            state, code = rt.status(job, "ready", success=False)
            state = wait_cleanup(rt, job)
            assert code and state["state"] == "failed" and state["cleanup_complete"], state
            assert not state.get("stopped_at"), state
            rt.run("exec", rt.source, "/workload", "verify", str(rt.size))
            wait_for(lambda: rt.run("exec", rt.source, "/workload", "hook-resumed", success=False).returncode == 0,
                     "resume hook was not run after rejected transfer")
            def transfer_closed():
                targets = fd_targets(source_fds).values()
                return not transient_sockets.intersection(targets) and not any(str(job) in target for target in targets)

            wait_for(transfer_closed, "source retained transfer sockets or checkpoint descriptors")
            after = fd_targets(source_fds)
            after_fds = len(after)
            (rt.logs / "fd-audit.json").write_text(json.dumps({"before": before, "during": during, "after": after}, indent=2) + "\n")
            full = rt.dir / "full"
            full.mkdir()
            rt.run("checkpoint", f"--image-path={full}", rt.source)
            restored = rt.id("full-restore")
            rt.run("restore", "--detach", f"--bundle={rt.bundle}", f"--image-path={full}", restored)
            rt.run("exec", restored, "/workload", "verify", str(rt.size))
            passed("worker_loss_before_ack_preserves_source_and_next_checkpoint",
                   source_fds_before=before_fds, source_fds_after=after_fds)
        finally:
            rt.close()

    if "enospc" in args.cases:
        with Volume(args.work_dir / "enospc-setup-volume") as volume:
            rt = Runtime(args, "enospc-before-capture", 1 << 20)
            stores = volume.mount / "stores"
            stores.mkdir()
            rt.common = [f"--overlay2=all:dir={stores}" if arg.startswith("--overlay2=") else arg
                         for arg in rt.common]
            try:
                rt.start()
                allocated = volume.fill()
                job = volume.mount / "job"
                result = rt.run("park", f"--job-dir={job}", "--completion=ready", rt.source, success=False)
                assert result.returncode and not job.exists()
                rt.run("exec", rt.source, "/workload", "verify", str(rt.size))
                passed("enospc_before_capture_preserves_source", filler_bytes=allocated)
            finally:
                rt.close()
        for kill_worker in [False, True]:
            name = "enospc-orphan" if kill_worker else "enospc-live"
            with Volume(args.work_dir / (name + "-volume")) as volume:
                rt = Runtime(args, name, 1 << 30)
                stores = volume.mount / "stores"
                stores.mkdir()
                rt.common = [f"--overlay2=all:dir={stores}" if arg.startswith("--overlay2=") else arg
                             for arg in rt.common]
                worker = None
                try:
                    rt.start()
                    job = volume.mount / "job"
                    stopped = rt.park(job)
                    assert stopped.get("stopped_at") and stopped["state"] != "ready", stopped
                    worker = os.pidfd_open(stopped["worker_pid"])
                    artifact = job / "filestores/filestore-0"
                    wait_for(artifact.exists, "worker did not begin filestore clone")
                    signal.pidfd_send_signal(worker, signal.SIGSTOP)
                    # FICLONE can finish in the kernel while the worker is
                    # stopped. Hold a materialized artifact before filling
                    # the volume, so closing the source cannot free its data.
                    wait_for(lambda: artifact.stat().st_size >= rt.size,
                             "clone did not materialize before fault injection")
                    rt.run("delete", "--force", rt.source)
                    allocated = volume.fill()
                    signal.pidfd_send_signal(worker, signal.SIGKILL if kill_worker else signal.SIGCONT)
                    if kill_worker:
                        worker_exit(worker)
                    state, code = rt.status(job, "ready", success=False)
                    state = wait_cleanup(rt, job)
                    (rt.logs / "failure-status.json").write_text(json.dumps(state, indent=2) + "\n")
                    assert code and state["state"] == "failed" and state["cleanup_complete"], state
                    assert state.get("stopped_at") and not state.get("durable_at"), state
                    assert not (job / "image").exists() and not (job / "filestores").exists()
                    assert (job / ".runsc-park.failed").exists(), "durable failure marker missing"
                    worker_exit(worker)
                    passed(name, filler_bytes=allocated, failure=state)
                finally:
                    if worker is not None:
                        try:
                            signal.pidfd_send_signal(worker, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                        worker_exit(worker)
                        os.close(worker)
                    rt.close()


if __name__ == "__main__":
    main()
