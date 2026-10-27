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

"""Measure save-resume, including application heartbeat gaps and durability.

Use full release directories for both runtime arguments. Preflush is a host
fsync while application heartbeats continue. A host control byte, sent only
after the sentry reports capture complete, makes the resumed source rewrite
64 MiB. Its heartbeat runs in a different process from this post-resume writer.
Both the source's newer contents and the snapshot's older contents are checked.
ApplicationPause is the host-observed heartbeat interval around the midpoint
of the sentry pause/resume log window, with a nominal 1 ms heartbeat period.
It includes sampling and observer scheduling error, not just internal freeze.
"""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import select
import shutil
import subprocess
import threading
import time

from benchmark import snapshot_timings, sync_artifacts
from cache_stats import file_cache_stats, filestore_fds


class Heartbeats:
    """Collect application stdout using the host monotonic clock."""

    def __init__(self):
        self.input_read, self.input_write = os.pipe2(os.O_CLOEXEC)
        self.output_read, self.output_write = os.pipe2(os.O_CLOEXEC)
        self.events = []
        self.error = None
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.collect, daemon=True)
        self.thread.start()

    def collect(self):
        pending = b""
        batch = 0
        try:
            while not self.stop.is_set():
                if not select.select([self.output_read], [], [], 0.05)[0]:
                    continue
                chunk = os.read(self.output_read, 65536)
                now, wall = time.monotonic_ns(), time.time_ns()
                if not chunk:
                    break
                pending += chunk
                while b"\n" in pending:
                    line, pending = pending.split(b"\n", 1)
                    fields = line.decode().split()
                    if not fields or fields[0] not in ["H", "R", "B", "W"]:
                        raise RuntimeError(f"unexpected heartbeat output: {line!r}")
                    self.events.append({"kind": fields[0], "value": int(fields[1]) if len(fields) > 1 else None,
                                        "monotonic_ns": now, "wall_ns": wall, "batch": batch})
                batch += 1
        except BaseException as error:
            self.error = error

    def handed_off(self):
        os.close(self.input_read)
        os.close(self.output_write)
        self.input_read = self.output_write = -1

    def wait(self, kind, count=1, timeout=30, resume_writer=None):
        deadline = time.monotonic() + timeout
        while sum(event["kind"] == kind for event in self.events) < count:
            if self.error:
                raise self.error
            if resume_writer is not None and resume_writer.error:
                raise RuntimeError("resume log watcher failed") from resume_writer.error
            if time.monotonic() >= deadline:
                raise RuntimeError(f"heartbeat deadline waiting for {kind}")
            time.sleep(0.001)

    def close(self):
        self.stop.set()
        self.thread.join(timeout=5)
        if self.thread.is_alive():
            raise RuntimeError("heartbeat reader did not exit")
        for file in [self.input_read, self.input_write, self.output_read, self.output_write]:
            if file >= 0:
                os.close(file)


class ResumeWriter:
    """Send the source write request after its state has been captured."""

    def __init__(self, log, heartbeats):
        self.log = log
        self.offset = log.stat().st_size
        self.heartbeats = heartbeats
        self.stop = threading.Event()
        self.sent_ns = None
        self.error = None
        self.thread = threading.Thread(target=self.watch, daemon=True)
        self.thread.start()

    def watch(self):
        try:
            with self.log.open("rb") as stream:
                stream.seek(self.offset)
                pending = b""
                needle = b"Overall save took ["
                while not self.stop.is_set():
                    pending += stream.read()
                    if needle in pending:
                        os.write(self.heartbeats.input_write, b"W")
                        self.sent_ns = time.monotonic_ns()
                        return
                    pending = pending[-len(needle):]
                    self.stop.wait(0.001)
        except BaseException as error:
            self.error = error

    def close(self):
        self.stop.set()
        self.thread.join(timeout=5)
        if self.thread.is_alive():
            raise RuntimeError("resume writer watcher did not exit")


def log_time(line, year):
    match = re.match(r"[A-Z](\d{4}) (\d\d:\d\d:\d\d\.\d{6})", line)
    if not match:
        raise RuntimeError("unrecognized sentry timestamp: " + line)
    date = datetime.datetime.strptime(str(year) + match[1] + " " + match[2], "%Y%m%d %H:%M:%S.%f")
    return int(date.replace(microsecond=0).timestamp()) * 1_000_000_000 + date.microsecond * 1000


def pause_measurement(log, events, checkpoint_start, checkpoint_end):
    lines = log.read_text().splitlines()
    starts = [line for line in lines if "Sandbox save started, pausing all tasks." in line]
    ends = [line for line in lines if "Tasks resumed after save." in line]
    if len(starts) != 1 or len(ends) != 1:
        raise RuntimeError("expected exactly one source pause/resume window")
    year = datetime.datetime.now().year
    start, end = log_time(starts[0], year), log_time(ends[0], year)
    midpoint = (start + end) // 2
    # R is emitted by the heartbeat process before it forks the post-resume
    # writer. It is also evidence that the application has resumed.
    beats = [event for event in events if event["kind"] in ["H", "R"]]
    pairs = [(a, b) for a, b in zip(beats, beats[1:]) if a["wall_ns"] <= midpoint <= b["wall_ns"]]
    if len(pairs) != 1:
        raise RuntimeError("heartbeat does not bracket the pause log midpoint")
    a, b = pairs[0]
    control = [b["monotonic_ns"] - a["monotonic_ns"] for a, b in zip(beats, beats[1:])
               if checkpoint_start - 200_000_000 <= a["monotonic_ns"] <= b["monotonic_ns"] < checkpoint_start]
    during = [b["monotonic_ns"] - a["monotonic_ns"] for a, b in zip(beats, beats[1:])
              if a["monotonic_ns"] <= checkpoint_end and b["monotonic_ns"] >= checkpoint_start]
    return {"application_pause_ns": b["monotonic_ns"] - a["monotonic_ns"],
            "pause_log_ns": end - start, "pause_log_start_wall_ns": start, "pause_log_end_wall_ns": end,
            "heartbeat_before": a, "heartbeat_after": b,
            "control_max_gap_ns": max(control, default=0), "checkpoint_max_gap_ns": max(during),
            "heartbeat_period_ns": 1_000_000, "pause_log_start": starts[0], "pause_log_end": ends[0]}


def run_trial(args, rootfs, trial, logs, variant):
    size, changed = args.mib << 20, (args.rewrite_mib or 0) << 20
    post_bytes = min(size, 64 << 20)
    external = variant == "candidate"
    binary = getattr(args, variant)
    bundle, image, filestores = [trial / name for name in ["bundle", "image", "filestores"]]
    for directory in [bundle, image, filestores, logs]:
        directory.mkdir(parents=True)
    spec = {"ociVersion": "1.0.2", "process": {"terminal": False, "user": {"uid": 0, "gid": 0},
            "args": ["/workload", "serve-heartbeat", str(size), str(post_bytes)], "env": [], "cwd": "/"},
            "root": {"path": str(rootfs), "readonly": False}, "hostname": "filestore-resume",
            "mounts": [{"destination": "/proc", "type": "proc", "source": "proc"}],
            "linux": {"namespaces": [{"type": name} for name in ["pid", "mount", "ipc", "uts", "network"]]}}
    (bundle / "config.json").write_text(json.dumps(spec))
    common = [str(binary), f"--root={args.work_dir / ('runtime-' + variant)}", "--platform=systrap",
              "--network=none", f"--overlay2=all:dir={filestores}", "--debug", f"--debug-log={logs}/"]
    if external:
        common.append("--filestore-reflink=enabled")
    prefix = hashlib.sha256(str(trial).encode()).hexdigest()[:16]
    ids = {name: f"resume-{prefix}-{name}" for name in ["source", "restored"]}
    source_files = []
    heartbeats = Heartbeats()
    restored_input_read, restored_input_write = os.pipe2(os.O_CLOEXEC)
    writer = None

    def run(command, extra=(), check=True, source_stdio=False, restored_stdio=False):
        argv = common + list(extra) + [ids.get(arg, str(arg)) for arg in command]
        with (logs / "commands.log").open("ab") as stream:
            stream.write((json.dumps(argv) + "\n").encode())
            stream.flush()
            start = time.monotonic_ns()
            input_file = heartbeats.input_read if source_stdio else restored_input_read if restored_stdio else subprocess.DEVNULL
            process = subprocess.Popen(argv, stdin=input_file,
                                       stdout=heartbeats.output_write if source_stdio else stream, stderr=stream)
            if source_stdio:
                heartbeats.handed_off()
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

    try:
        run(["run", "--detach", f"--bundle={bundle}", "source"], source_stdio=True)
        deadline = time.monotonic() + 120
        while run(["exec", "source", "/workload", "ready"], check=False)[1]:
            if time.monotonic() > deadline:
                raise RuntimeError("workload did not become ready")
            time.sleep(0.05)
        heartbeats.wait("H", count=10)
        state = json.loads(subprocess.check_output(common + ["state", ids["source"]], timeout=30))
        source_files = filestore_fds(state["pid"])
        preparation = None
        if args.rewrite_mib is not None:
            before = [file_cache_stats(file) for file in source_files]
            base_start = time.monotonic_ns()
            for file in source_files:
                os.fsync(file)
            base_ns = time.monotonic_ns() - base_start
            clean = [file_cache_stats(file) for file in source_files]
            if any(stat["nr_dirty"] or stat["nr_writeback"] for stat in clean):
                raise RuntimeError("base did not become clean")
            rewrite_ns, _ = run(["exec", "source", "/workload", "rewrite", str(size), str(changed), "prefix"])
            preparation = {"before": before, "after_base_flush": clean, "base_flush_ns": base_ns, "rewrite_ns": rewrite_ns}
        time.sleep(0.2)
        before_preflush = [file_cache_stats(file) for file in source_files]
        operation_start = time.monotonic_ns()
        preflush_start = time.monotonic_ns()
        if args.preflush:
            for file in source_files:
                os.fsync(file)
        preflush_end = time.monotonic_ns()
        source_cache = [file_cache_stats(file) for file in source_files]
        if args.preflush and any(stat["nr_dirty"] or stat["nr_writeback"] for stat in source_cache):
            raise RuntimeError("preflush did not become clean")
        boot_logs = list(logs.glob("*.boot.txt"))
        if len(boot_logs) != 1:
            raise RuntimeError("expected one source boot log")
        boot_log = boot_logs[0]
        writer = ResumeWriter(boot_log, heartbeats)
        checkpoint = ["checkpoint", f"--image-path={image}", "--leave-running"]
        if external:
            checkpoint += ["--skip-filestore-pages", f"--filestore-snapshot-dir={trial / 'snapshot'}"]
        checkpoint.append("source")
        checkpoint_start = time.monotonic_ns()
        checkpoint_ns, _ = run(checkpoint)
        checkpoint_end = time.monotonic_ns()
        roots = [image] + ([trial / "snapshot"] if external else [])
        sync_start = time.monotonic_ns()
        sync_calls = sync_artifacts(roots, trial)
        durable_end = time.monotonic_ns()
        heartbeats.wait("W", resume_writer=writer)
        heartbeats.wait("H", count=20)
        if writer.error:
            raise writer.error
        if writer.sent_ns is None:
            raise RuntimeError("resume write command was not sent")
        events = list(heartbeats.events)
        pauses = pause_measurement(boot_log, events, checkpoint_start, checkpoint_end)
        write_begin = next(event for event in events if event["kind"] == "B")
        write_done = next(event for event in events if event["kind"] == "W")
        if write_done["value"] != post_bytes:
            raise RuntimeError("post-resume write size mismatch")
        run(["exec", "source", "/workload", "verify-post", str(size), str(changed), str(post_bytes)])
        if run(["exec", "source", "/workload", "verify", str(size), str(changed), "prefix"], check=False)[1] != 1:
            raise RuntimeError("source should differ from the old snapshot")
        restore_ns, _ = run(["restore", "--detach", f"--bundle={bundle}", f"--image-path={image}", "restored"],
                            extra=[f"--filestore-adopt-dir={trial / 'snapshot'}"] if external else [], restored_stdio=True)
        run(["exec", "restored", "/workload", "verify", str(size), str(changed), "prefix"])
        if run(["exec", "restored", "/workload", "verify-post", str(size), str(changed), str(post_bytes)], check=False)[1] != 1:
            raise RuntimeError("snapshot should not include post-resume writes")
        snapshots, manifest = snapshot_timings(logs)
        phases = {key: sum(snapshot.get(key, 0) for snapshot in snapshots)
                  for key in {key for snapshot in snapshots for key in snapshot}}
        phases.update(manifest)
        beats = [event for event in events if event["kind"] == "H"]
        preflush_gaps = [b["monotonic_ns"] - a["monotonic_ns"] for a, b in zip(beats, beats[1:])
                        if a["monotonic_ns"] <= preflush_end and b["monotonic_ns"] >= preflush_start]
        return {"checkpoint_ns": checkpoint_ns, "durable_checkpoint_ns": durable_end - checkpoint_start,
                "artifact_sync_ns": durable_end - sync_start, "end_to_end_ns": durable_end - operation_start,
                "preflush_ns": preflush_end - preflush_start if args.preflush else 0,
                "preflush_max_heartbeat_gap_ns": max(preflush_gaps, default=0),
                "restore_ns": restore_ns, "pause": pauses, "source_cache": source_cache,
                "source_cache_before_preflush": before_preflush, "layer_preparation": preparation,
                "snapshot_phases": phases, "artifact_sync_calls": sync_calls,
                "verified_bytes": size, "verified_source_bytes": size, "post_resume_write_bytes": post_bytes,
                "resume_write_request_ns": writer.sent_ns, "post_write_start": write_begin, "post_write_done": write_done,
                "write_started_before_cli_return": write_begin["monotonic_ns"] < checkpoint_end,
                "write_started_before_durable_complete": write_begin["monotonic_ns"] < durable_end,
                "checkpoint_start_ns": checkpoint_start, "checkpoint_end_ns": checkpoint_end,
                "durable_end_ns": durable_end, "operation_start_ns": operation_start}
    finally:
        try:
            if writer:
                writer.close()
                (logs / "resume-writer.json").write_text(json.dumps({
                    "log": str(writer.log), "start_offset": writer.offset,
                    "sent_ns": writer.sent_ns, "error": repr(writer.error) if writer.error else None,
                }) + "\n")
            for name in ids:
                run(["delete", "--force", name], check=False)
        finally:
            try:
                heartbeats.close()
            finally:
                os.close(restored_input_read)
                os.close(restored_input_write)
                for file in source_files:
                    os.close(file)
                (logs / "heartbeats.jsonl").write_text("".join(json.dumps(event) + "\n" for event in heartbeats.events))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--mib", type=int, required=True)
    parser.add_argument("--rewrite-mib", type=int)
    parser.add_argument("--preflush-mode", choices=["off", "on", "both"], default="both")
    parser.add_argument("--pairs", type=int, default=16)
    args = parser.parse_args()
    if os.geteuid() or args.mib < 1 or args.pairs < 1 or (args.rewrite_mib is not None and not 0 <= args.rewrite_mib <= args.mib):
        parser.error("requires root, positive layer/pair count, and a rewrite fitting in the layer")
    args.work_dir.mkdir(parents=True, exist_ok=False)
    args.output.mkdir(parents=True, exist_ok=False)
    rootfs = args.work_dir / "rootfs"
    (rootfs / "proc").mkdir(parents=True)
    subprocess.run(["gcc", "-O2", "-static", str(Path(__file__).with_name("resume_workload.c")),
                    "-o", str(rootfs / "workload")], check=True)
    (args.output / "environment.json").write_text(json.dumps({key: str(value) if isinstance(value, Path) else value
                                                             for key, value in vars(args).items()}, indent=2) + "\n")
    with (args.output / "samples.jsonl").open("w") as samples:
        for pair in range(args.pairs):
            conditions = [("baseline", False), ("candidate", False), ("baseline", True), ("candidate", True)]
            conditions = conditions[pair % 4:] + conditions[:pair % 4]
            for variant, preflush in conditions:
                if args.preflush_mode == "off" and preflush or args.preflush_mode == "on" and not preflush:
                    continue
                args.preflush = preflush
                treatment = "preflush" if preflush else "dirty"
                name = f"{args.mib}-{pair}-{variant}-{treatment}"
                trial = args.work_dir / name
                data = run_trial(args, rootfs, trial, args.output / name, variant)
                data.update(variant=variant, pair=pair, mib=args.mib, rewrite_mib=args.rewrite_mib, preflush=args.preflush)
                samples.write(json.dumps(data) + "\n")
                samples.flush()
                metrics = {"ApplicationPause": data["pause"]["application_pause_ns"], "PauseLogWindow": data["pause"]["pause_log_ns"],
                           "Checkpoint": data["checkpoint_ns"], "DurableCheckpoint": data["durable_checkpoint_ns"],
                           "EndToEnd": data["end_to_end_ns"], "Preflush": data["preflush_ns"], "Restore": data["restore_ns"]}
                metrics.update(data["snapshot_phases"])
                with (args.output / f"{variant}-{treatment}.txt").open("a") as bench:
                    for metric, value in metrics.items():
                        bench.write(f"BenchmarkResume/{args.mib}MiB/{metric} 1 {value} ns/op\n")
                print(json.dumps(data), flush=True)
                shutil.rmtree(trial)


if __name__ == "__main__":
    main()
