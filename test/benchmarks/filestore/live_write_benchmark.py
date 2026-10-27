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

"""Measure preflush under continuous, rate-controlled versioned overwrites.

The application consumes one external stdin credit per 1 MiB operation. Its
sequence and complete block contents are checked after restore. An empty pipe
observation inside the freeze window identifies all completed source records.
Restored stdin starts empty: at most one already-consumed operation can finish.
A barrier, without a write credit, then reports the restored sequence. All file
bytes must match that application sequence, and no later source generations.
"""

import argparse
import fcntl
import hashlib
import json
import math
import os
from pathlib import Path
import re
import select
import shutil
import subprocess
import sys
import threading
import time

from benchmark import sync_artifacts
from cache_stats import file_cache_stats, filestore_fds
from resume_benchmark import pause_measurement

MIB = 1 << 20


class Monitor:
    """Timestamp application records and explicit empty-pipe observations."""

    def __init__(self, allow_existing_sequence=False):
        self.input_read, self.input_write = os.pipe2(os.O_CLOEXEC)
        self.output_read, self.output_write = os.pipe2(os.O_CLOEXEC)
        try:
            self.pipe_size = fcntl.fcntl(self.input_write, fcntl.F_SETPIPE_SZ, 1 << 20)
        except OSError:
            self.pipe_size = fcntl.fcntl(self.input_write, fcntl.F_GETPIPE_SZ)
        os.set_blocking(self.output_read, False)
        os.set_blocking(self.input_write, False)
        self.events, self.drains = [], []
        self.completed = 0
        self.progress = threading.Condition()
        self.allow_existing_sequence = allow_existing_sequence
        self.error = None
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.collect, daemon=True)
        self.thread.start()

    def collect(self):
        pending = b""
        last_drain = 0
        try:
            while not self.stop.is_set():
                select.select([self.output_read], [], [], 0.001)
                while True:
                    try:
                        chunk = os.read(self.output_read, 65536)
                    except BlockingIOError:
                        now = time.monotonic_ns()
                        if not pending and now - last_drain >= 1_000_000:
                            self.drains.append({"monotonic_ns": now, "wall_ns": time.time_ns(),
                                                "event_count": len(self.events)})
                            last_drain = now
                        break
                    if not chunk:
                        return
                    now, wall = time.monotonic_ns(), time.time_ns()
                    pending += chunk
                    while b"\n" in pending:
                        line, pending = pending.split(b"\n", 1)
                        parts = line.decode("ascii").split()
                        if not parts or parts[0] not in ["H", "B", "E", "Z"]:
                            raise RuntimeError(f"invalid application record: {line!r}")
                        kind = parts[0]
                        fields = list(map(int, parts[1:]))
                        expected = {"H": 2, "B": 1, "E": 3, "Z": 1}[kind]
                        if len(fields) != expected:
                            raise RuntimeError("incorrect record length")
                        event = {"kind": kind, "value": fields[0], "monotonic_ns": now, "wall_ns": wall}
                        if kind == "H":
                            event["guest_ns"] = fields[1]
                        elif kind == "E":
                            first_restored = self.allow_existing_sequence and self.completed == 0
                            if not first_restored and fields[0] != self.completed + 1:
                                raise RuntimeError("missing or duplicated write completion")
                            event.update(guest_start_ns=fields[1], guest_end_ns=fields[2],
                                         duration_ns=fields[2] - fields[1])
                            if event["duration_ns"] < 0:
                                raise RuntimeError("guest clock moved backwards")
                        self.events.append(event)
                        if kind == "E":
                            with self.progress:
                                self.completed = fields[0]
                                self.progress.notify_all()
        except BaseException as error:
            self.error = error

    def handed_off(self):
        os.close(self.input_read)
        os.close(self.output_write)
        self.input_read = self.output_write = -1

    def check(self):
        if self.error:
            raise self.error

    def wait_completed(self, count, timeout=120):
        deadline = time.monotonic() + timeout
        while self.completed < count:
            self.check()
            if time.monotonic() > deadline:
                raise RuntimeError(f"write completion timeout: {self.completed}/{count}")
            time.sleep(0.001)

    def wait_kind(self, kind, count=1, timeout=30):
        deadline = time.monotonic() + timeout
        while sum(event["kind"] == kind for event in self.events) < count:
            self.check()
            if time.monotonic() > deadline:
                raise RuntimeError("application record timeout: " + kind)
            time.sleep(0.001)

    def write(self, data):
        view = memoryview(data)
        while view:
            self.check()
            try:
                written = os.write(self.input_write, view)
            except BlockingIOError:
                select.select([], [self.input_write], [], 0.05)
                continue
            view = view[written:]

    def close(self):
        self.stop.set()
        self.thread.join(timeout=5)
        if self.thread.is_alive():
            raise RuntimeError("monitor did not exit")
        for file in [self.input_read, self.input_write, self.output_read, self.output_write]:
            if file >= 0:
                os.close(file)


class Feeder:
    """Queue open-loop demand on the host, with one dispatched credit at a time.

    Stdin pipe contents can be checkpointed. Keeping the uncompleted dispatch
    count at one bounds replay after restore, without stopping demand or writes
    before capture. Completion notifications avoid a polling throughput cap.
    """

    def __init__(self, monitor, rate):
        self.monitor, self.rate = monitor, rate
        self.issued_ns = [None]
        self.dispatched_ns = [None]
        self.error = None
        self.stop = threading.Event()
        self.drain = False
        self.started_ns = time.monotonic_ns()
        self.thread = threading.Thread(target=self.feed, daemon=True)
        self.thread.start()

    @property
    def issued(self):
        return len(self.issued_ns) - 1

    def feed(self):
        try:
            while True:
                self.monitor.check()
                now = time.monotonic_ns()
                if not self.stop.is_set():
                    target = int((now - self.started_ns) * self.rate / 1e9)
                    self.issued_ns.extend(self.started_ns + (index * 1_000_000_000) // self.rate
                                          for index in range(self.issued + 1, target + 1))
                dispatched = len(self.dispatched_ns) - 1
                if self.stop.is_set() and (not self.drain or self.monitor.completed == self.issued):
                    return
                if dispatched < self.issued and self.monitor.completed == dispatched:
                    try:
                        written = os.write(self.monitor.input_write, b"w")
                    except BlockingIOError:
                        written = 0
                    if written:
                        self.dispatched_ns.append(now)
                with self.monitor.progress:
                    if self.monitor.completed < len(self.dispatched_ns) - 1 or len(self.dispatched_ns) - 1 == self.issued:
                        self.monitor.progress.wait(.001)
        except BaseException as error:
            self.error = error

    def finish(self, drain=True):
        self.drain = drain
        self.stop.set()
        with self.monitor.progress:
            self.monitor.progress.notify_all()
        self.thread.join(timeout=120 if drain else 5)
        if self.thread.is_alive():
            raise RuntimeError("rate feeder did not exit")
        if self.error:
            raise self.error


def percentile(values, fraction):
    if not values:
        return None
    values = sorted(values)
    return values[max(0, math.ceil(len(values) * fraction) - 1)]


def write_statistics(events, issued_ns, begin, end, dispatched_ns=None):
    if end <= begin:
        return None
    starts = {event["value"]: event for event in events if event["kind"] == "B"}
    completions = [event for event in events if event["kind"] == "E"]
    completed = [event for event in completions if begin <= event["monotonic_ns"] < end]
    syscalls = [event["duration_ns"] for event in completions
                if begin <= starts[event["value"]]["monotonic_ns"] < end]
    requests = [event["monotonic_ns"] - issued_ns[event["value"]] for event in completions
                if event["value"] < len(issued_ns) and begin <= issued_ns[event["value"]] < end]
    observed_requests = [event["monotonic_ns"] - issued_ns[event["value"]] for event in completed
                         if event["value"] < len(issued_ns)]
    offered = [stamp for stamp in issued_ns[1:] if begin <= stamp < end]
    dispatched = [stamp for stamp in (dispatched_ns or [])[1:] if begin <= stamp < end]
    pending = sum(stamp < end for stamp in issued_ns[1:]) - sum(event["monotonic_ns"] < end for event in completions)
    crossing = sum(begin <= starts[event["value"]]["monotonic_ns"] < end <= event["monotonic_ns"] for event in completions)
    return {"begin_ns": begin, "end_ns": end, "completed_ops": len(completed),
            "throughput_Bps": len(completed) * MIB * 1e9 / (end - begin),
            "offered_ops": len(offered), "offered_Bps": len(offered) * MIB * 1e9 / (end - begin),
            "dispatched_ops": len(dispatched), "dispatched_Bps": len(dispatched) * MIB * 1e9 / (end - begin),
            "pending_at_end": pending, "syscall_cohort_finishing_after_end": crossing,
            "completed_pwrite_p99_ns": percentile([event["duration_ns"] for event in completed], .99),
            "completed_credit_p99_ns": percentile(observed_requests, .99),
            "syscall_ops": len(syscalls), "pwrite_p50_ns": percentile(syscalls, .5),
            "pwrite_p99_ns": percentile(syscalls, .99), "request_ops": len(requests),
            "credit_p50_ns": percentile(requests, .5), "credit_p99_ns": percentile(requests, .99)}


def capture_oracle(events, drains, pause):
    low, high = pause["pause_log_start_wall_ns"], pause["pause_log_end_wall_ns"]
    margin = (high - low) // 3
    candidates = [drain for drain in drains if low + margin <= drain["wall_ns"] <= high - margin]
    if not candidates:
        raise RuntimeError("no empty stdout-pipe observation inside freeze")
    drain = candidates[-1]
    prefix = events[:drain["event_count"]]
    completed = max((event["value"] for event in prefix if event["kind"] == "E"), default=0)
    started = max((event["value"] for event in prefix if event["kind"] == "B"), default=0)
    if started not in [completed, completed + 1]:
        raise RuntimeError("more than one source write in flight")
    return {"completed_sequence": completed, "started_sequence": started, "empty_pipe": drain}


def safe_log_text(file):
    return file.read_bytes().decode("utf-8", errors="replace")


def snapshot_phases(logs):
    result = {}
    for file in logs.glob("*.txt"):
        text = safe_log_text(file)
        for line in text.splitlines():
            if "filestore snapshot timing:" in line or "Filestore manifest timing:" in line:
                for key, value in re.findall(r"(\w+_ns)=(\d+)", line):
                    result[key] = result.get(key, 0) + int(value)
    return result


def run_trial(args, rootfs, trial, logs, preflush):
    size, hot, dirty = args.mib * MIB, args.hot_mib * MIB, (args.initial_dirty_mib or 0) * MIB
    bundle, image, filestores = [trial / name for name in ["bundle", "image", "filestores"]]
    for directory in [bundle, image, filestores, logs]:
        directory.mkdir(parents=True)
    spec = {"ociVersion": "1.0.2", "process": {"terminal": False, "user": {"uid": 0, "gid": 0},
            "args": ["/workload", "serve", str(size), str(hot)], "env": [], "cwd": "/"},
            "root": {"path": str(rootfs), "readonly": False}, "hostname": "live-write-benchmark",
            "mounts": [{"destination": "/proc", "type": "proc", "source": "proc"}],
            "linux": {"namespaces": [{"type": name} for name in ["pid", "mount", "ipc", "uts", "network"]]}}
    (bundle / "config.json").write_text(json.dumps(spec))
    common = [str(args.runsc), f"--root={args.work_dir / 'runtime'}", "--platform=systrap", "--network=none",
              f"--overlay2=all:dir={filestores}", "--filestore-reflink=enabled", "--debug", f"--debug-log={logs}/"]
    prefix = hashlib.sha256(str(trial).encode()).hexdigest()[:16]
    ids = {name: f"live-{prefix}-{name}" for name in ["source", "restored"]}
    source, restored = Monitor(), Monitor(allow_existing_sequence=True)
    feeder = None
    source_files = []
    sync_thread = None
    sync_done = threading.Event()
    sync_result = {}

    def run(command, check=True, extra=(), monitor=None):
        argv = common + list(extra) + [ids.get(arg, str(arg)) for arg in command]
        with (logs / "commands.log").open("ab") as stream:
            stream.write((json.dumps(argv) + "\n").encode())
            stream.flush()
            start = time.monotonic_ns()
            process = subprocess.Popen(argv, stdin=monitor.input_read if monitor else subprocess.DEVNULL,
                                       stdout=monitor.output_write if monitor else stream, stderr=stream)
            if monitor:
                monitor.handed_off()
            try:
                pidfd = os.pidfd_open(process.pid)
                try:
                    if not select.select([pidfd], [], [], 180)[0]:
                        raise subprocess.TimeoutExpired(argv, 180)
                finally:
                    os.close(pidfd)
                code = process.wait()
                elapsed = time.monotonic_ns() - start
            finally:
                if process.poll() is None:
                    process.kill()
                process.wait()
            if check and code:
                raise subprocess.CalledProcessError(code, argv)
            return elapsed, code

    def sample_cache():
        before, started = source.completed, time.monotonic_ns()
        stats = [file_cache_stats(file) for file in source_files]
        return {"stats": stats, "completed_before": before, "completed_after": source.completed,
                "query_start_ns": started, "query_end_ns": time.monotonic_ns()}

    try:
        run(["run", "--detach", f"--bundle={bundle}", "source"], monitor=source)
        deadline = time.monotonic() + 120
        while run(["exec", "source", "/workload", "ready"], check=False)[1]:
            if time.monotonic() > deadline:
                raise RuntimeError("workload initialization timeout")
            time.sleep(.05)
        source.wait_kind("H", 10)
        state = json.loads(subprocess.check_output(common + ["state", ids["source"]], timeout=30))
        source_files = filestore_fds(state["pid"])
        if len(source_files) != 1:
            raise RuntimeError("expected one private filestore")
        preparation = None
        if args.initial_dirty_mib is not None:
            start = time.monotonic_ns()
            for file in source_files:
                os.fsync(file)
            clean = [file_cache_stats(file) for file in source_files]
            if any(stat["nr_dirty"] or stat["nr_writeback"] for stat in clean):
                raise RuntimeError("base is not clean")
            preparation = {"base_flush_ns": time.monotonic_ns() - start, "clean": clean}
            preparation["dirty_ns"], _ = run(["exec", "source", "/workload", "dirty", str(size), str(dirty)])
        feeder = Feeder(source, args.rate_mib)
        time.sleep(.75)
        before_cache = sample_cache()
        operation_start = time.monotonic_ns()
        pre_start = time.monotonic_ns()
        if preflush:
            def sync_source():
                try:
                    for file in source_files:
                        os.fsync(file)
                except BaseException as error:
                    sync_result["error"] = repr(error)
                finally:
                    sync_result["end_ns"] = time.monotonic_ns()
                    sync_done.set()
            sync_thread = threading.Thread(target=sync_source, daemon=True)
            sync_thread.start()
            if not sync_done.wait(30):
                raise RuntimeError("preflush did not complete within 30 seconds under sustained writes")
            if "error" in sync_result:
                raise RuntimeError(sync_result["error"])
        pre_end = sync_result.get("end_ns", time.monotonic_ns())
        after_cache = sample_cache()
        checkpoint_start = time.monotonic_ns()
        checkpoint_ns, _ = run(["checkpoint", f"--image-path={image}", "--leave-running", "--skip-filestore-pages",
                                f"--filestore-snapshot-dir={trial / 'snapshot'}", "source"])
        checkpoint_end = time.monotonic_ns()
        sync_calls = sync_artifacts([image, trial / "snapshot"], trial)
        durable_end = time.monotonic_ns()
        time.sleep(.5)
        feeder.finish()
        source.wait_completed(feeder.issued)
        performance_end = time.monotonic_ns()
        events, drains = list(source.events), list(source.drains)
        boot = [file for file in logs.glob("*.boot.txt") if "Sandbox save started, pausing all tasks." in safe_log_text(file)]
        if len(boot) != 1:
            raise RuntimeError("expected one source save log")
        pause_log = logs / "pause-only.log"
        pause_log.write_text("\n".join(line for line in safe_log_text(boot[0]).splitlines()
                                      if "Sandbox save started, pausing all tasks." in line or "Tasks resumed after save." in line) + "\n")
        pause = pause_measurement(pause_log, events, checkpoint_start, checkpoint_end)
        oracle = capture_oracle(events, drains, pause)
        (logs / "capture-diagnostics.json").write_text(json.dumps({
            "pause": pause, "oracle": oracle, "before_preflush": before_cache,
            "after_preflush": after_cache, "preflush_start_ns": pre_start,
            "preflush_end_ns": pre_end, "checkpoint_start_ns": checkpoint_start,
            "checkpoint_end_ns": checkpoint_end, "durable_end_ns": durable_end,
        }, indent=2) + "\n")
        frozen = oracle["completed_sequence"]
        before, after = pause["heartbeat_before"], pause["heartbeat_after"]
        beats = [event for event in events if event["kind"] == "H"]
        # Skip the first resumed record: its guest timestamp may have been
        # sampled just before the save interrupted its stdout write.
        before_clock = [event["monotonic_ns"] - event["guest_ns"] for event in beats
                        if pause["pause_log_start_wall_ns"] - 100_000_000 <= event["wall_ns"] < pause["pause_log_start_wall_ns"] - 5_000_000]
        after_clock = [event["monotonic_ns"] - event["guest_ns"] for event in beats
                       if pause["pause_log_end_wall_ns"] + 5_000_000 <= event["wall_ns"] < pause["pause_log_end_wall_ns"] + 100_000_000]
        if not before_clock or not after_clock:
            raise RuntimeError("missing guest clock calibration heartbeats")
        guest_clock_error = percentile(after_clock, .5) - percentile(before_clock, .5)
        if abs(guest_clock_error) > 10_000_000:
            raise RuntimeError("guest monotonic clock did not track host elapsed time")
        post_start = after["monotonic_ns"]
        def statistics_between(begin, end):
            return write_statistics(events, feeder.issued_ns, begin, end, feeder.dispatched_ns)
        statistics = {"control": statistics_between(operation_start - 500_000_000, operation_start),
                      "preflush": statistics_between(pre_start, pre_end) if preflush else None,
                      "checkpoint": statistics_between(checkpoint_start, checkpoint_end),
                      "post_resume": statistics_between(post_start, post_start + 500_000_000)}
        # A separate, unpaced verification sweep changes every hotset block.
        burst = args.hot_mib + 2
        source.write(b"w" * burst)
        latest = feeder.issued + burst
        source.wait_completed(latest)
        source.write(b"b")
        source.wait_kind("Z")
        run(["exec", "source", "/workload", "verify", str(size), str(hot), str(dirty), str(latest)])
        if run(["exec", "source", "/workload", "verify", str(size), str(hot), str(dirty), str(frozen)], check=False)[1] != 1:
            raise RuntimeError("continued source writes did not change the hotset")
        restore_ns, _ = run(["restore", "--detach", f"--bundle={bundle}", f"--image-path={image}", "restored"],
                            extra=[f"--filestore-adopt-dir={trial / 'snapshot'}"], monitor=restored)
        restored.write(b"b")
        restored.wait_kind("Z")
        restored_sequence = next(event["value"] for event in restored.events if event["kind"] == "Z")
        if sum(event["kind"] == "E" for event in restored.events) > 1:
            raise RuntimeError("restored writer completed more than one captured operation")
        if restored_sequence not in [frozen, frozen + 1]:
            raise RuntimeError(f"restored application sequence {restored_sequence} differs from frozen {frozen}")
        run(["exec", "restored", "/workload", "verify", str(size), str(hot), str(dirty), str(restored_sequence)])
        if run(["exec", "restored", "/workload", "verify", str(size), str(hot), str(dirty), str(latest)], check=False)[1] != 1:
            raise RuntimeError("snapshot contains the source's later generations")
        (logs / "credits.json").write_text(json.dumps(feeder.issued_ns) + "\n")
        return {"checkpoint_ns": checkpoint_ns, "durable_checkpoint_ns": durable_end - checkpoint_start,
                "end_to_end_ns": durable_end - operation_start, "preflush_ns": pre_end - pre_start if preflush else 0,
                "preflush_start_ns": pre_start, "preflush_end_ns": pre_end, "operation_start_ns": operation_start,
                "checkpoint_start_ns": checkpoint_start, "checkpoint_end_ns": checkpoint_end, "durable_end_ns": durable_end,
                "performance_end_ns": performance_end, "before_preflush": before_cache, "after_preflush": after_cache,
                "layer_preparation": preparation, "pause": pause, "guest_clock_error_ns": guest_clock_error,
                "oracle": oracle, "restored_sequence": restored_sequence, "source_latest_sequence": latest,
                "issued_before_verification_burst": feeder.issued, "source_completed_at_performance_end": feeder.issued,
                "writer_statistics": statistics, "snapshot_phases": snapshot_phases(logs), "restore_ns": restore_ns,
                "verified_source_bytes": size, "verified_snapshot_bytes": size, "verification_burst_ops": burst,
                "artifact_sync_calls": sync_calls, "stdin_pipe_size": source.pipe_size}
    finally:
        failed = sys.exc_info()[0] is not None
        cleanup_errors = []
        try:
            if feeder:
                try:
                    feeder.finish(drain=False)
                except BaseException as error:
                    cleanup_errors.append(repr(error))
            if sync_thread and sync_thread.is_alive():
                try:
                    run(["pause", "source"], check=False)
                except BaseException as error:
                    cleanup_errors.append(repr(error))
            for name in ids:
                try:
                    run(["delete", "--force", name], check=False)
                except BaseException as error:
                    cleanup_errors.append(repr(error))
            if sync_thread:
                sync_thread.join(timeout=30)
                if sync_thread.is_alive():
                    cleanup_errors.append("fsync worker did not exit after stopping source")
        finally:
            for label, monitor in [("source", source), ("restored", restored)]:
                try:
                    monitor.close()
                except BaseException as error:
                    cleanup_errors.append(repr(error))
                (logs / (label + "-events.jsonl")).write_text("".join(json.dumps(event) + "\n" for event in monitor.events))
                (logs / (label + "-drains.jsonl")).write_text("".join(json.dumps(event) + "\n" for event in monitor.drains))
            for file in source_files:
                os.close(file)
            if feeder:
                (logs / "credits.json").write_text(json.dumps(feeder.issued_ns) + "\n")
                (logs / "dispatched.json").write_text(json.dumps(feeder.dispatched_ns) + "\n")
            (logs / "cleanup.json").write_text(json.dumps({"errors": cleanup_errors, "preflush": sync_result}) + "\n")
        if cleanup_errors and not failed:
            raise RuntimeError("trial cleanup failed: " + repr(cleanup_errors))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runsc", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--mib", type=int, required=True)
    parser.add_argument("--hot-mib", type=int, required=True)
    parser.add_argument("--initial-dirty-mib", type=int)
    parser.add_argument("--rate-mib", type=int, required=True)
    parser.add_argument("--pairs", type=int, default=16)
    args = parser.parse_args()
    if os.geteuid() or not 0 < args.hot_mib <= args.mib or args.rate_mib < 0 or args.pairs < 1:
        parser.error("requires root, valid layer/hotset sizes, rate >= 0, and positive pair count")
    if args.initial_dirty_mib is not None and not 0 <= args.initial_dirty_mib <= args.hot_mib:
        parser.error("initial dirty range must fit in the hotset")
    args.work_dir.mkdir(parents=True, exist_ok=False)
    args.output.mkdir(parents=True, exist_ok=False)
    rootfs = args.work_dir / "rootfs"
    (rootfs / "proc").mkdir(parents=True)
    subprocess.run(["gcc", "-O2", "-static", str(Path(__file__).with_name("live_write_workload.c")),
                    "-o", str(rootfs / "workload")], check=True)
    (args.output / "environment.json").write_text(json.dumps({key: str(value) if isinstance(value, Path) else value
                                                             for key, value in vars(args).items()}, indent=2) + "\n")
    with (args.output / "samples.jsonl").open("w") as samples:
        for pair in range(args.pairs):
            for preflush in ([False, True] if pair % 2 == 0 else [True, False]):
                mode = "preflush" if preflush else "dirty"
                name = f"{pair}-{mode}"
                trial = args.work_dir / name
                data = run_trial(args, rootfs, trial, args.output / name, preflush)
                data.update(pair=pair, preflush=preflush, mib=args.mib, hot_mib=args.hot_mib,
                            initial_dirty_mib=args.initial_dirty_mib, rate_mib=args.rate_mib)
                samples.write(json.dumps(data) + "\n")
                samples.flush()
                metrics = {"ApplicationPause": (data["pause"]["application_pause_ns"], "ns/op"),
                           "Checkpoint": (data["checkpoint_ns"], "ns/op"), "DurableCheckpoint": (data["durable_checkpoint_ns"], "ns/op"),
                           "EndToEnd": (data["end_to_end_ns"], "ns/op"), "Preflush": (data["preflush_ns"], "ns/op"),
                           "RemainingDirty": (sum(stat["dirty_bytes"] for stat in data["after_preflush"]["stats"]), "bytes/op")}
                metrics.update({name: (value, "ns/op") for name, value in data["snapshot_phases"].items()})
                for phase, stats in data["writer_statistics"].items():
                    if stats is None:
                        continue
                    for name, unit in [("throughput_Bps", "B/s"), ("pwrite_p99_ns", "ns/op"), ("credit_p99_ns", "ns/op")]:
                        if stats[name] is not None:
                            metrics[phase + "_" + name] = (stats[name], unit)
                with (args.output / (mode + ".txt")).open("a") as bench:
                    for metric, (value, unit) in metrics.items():
                        bench.write(f"BenchmarkLiveWrite/{args.mib}MiB/{args.rate_mib}MiBps/{metric} 1 {value} {unit}\n")
                print(json.dumps(data), flush=True)
                shutil.rmtree(trial)


if __name__ == "__main__":
    main()
