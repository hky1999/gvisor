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

"""Shared persistence and runtime timing helpers for resume_benchmark.py."""

import os
from pathlib import Path
import re
import time


def sync_artifacts(roots, parent):
    """Sync only this checkpoint's restore inputs, then their directories."""
    files = sorted(file for root in roots for file in root.rglob("*") if file.is_file())
    dirs = {parent, *parent.parents}
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
        calls.append({"path": os.path.relpath(file, parent), "kind": kind, "duration_ns": elapsed})
    return calls


def snapshot_timings(logs):
    snapshots = []
    manifest = {}
    for file in sorted(logs.glob("*.txt")):
        data = file.read_text()
        for fields in re.findall(r"filestore snapshot timing: ([^\n]+)", data):
            snapshots.append({key: int(value) for key, value in re.findall(r"(\w+_ns)=(\d+)", fields)})
        for value in re.findall(r"Filestore \d+ FICLONE: (\d+) ns", data):
            snapshots.append({"ficlone_ns": int(value)})
        for name, value in re.findall(r'Checkpoint file fsync: name="([^"\n]+)" duration_ns=(\d+)', data):
            key = "snapshot_fsync_ns" if Path(name).name.startswith("filestore-") else "state_pages_fsync_ns"
            manifest[key] = manifest.get(key, 0) + int(value)
        for value in re.findall(r"Checkpoint post-resume persistence: (\d+) ns", data):
            manifest["post_resume_persistence_ns"] = int(value)
        for fields in re.findall(r"Filestore manifest timing: ([^\n]+)", data):
            for key, value in re.findall(r"(\w+_ns)=(\d+)", fields):
                manifest[key] = manifest.get(key, 0) + int(value)
    return snapshots, manifest
