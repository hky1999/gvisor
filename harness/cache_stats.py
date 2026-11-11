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

"""Observe one filestore's cache without syncing or faulting in its pages."""

import ctypes
import os
import platform
import time


class CacheRange(ctypes.Structure):
    _fields_ = [("off", ctypes.c_uint64), ("len", ctypes.c_uint64)]


class CacheStat(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint64) for name in
                ["nr_cache", "nr_dirty", "nr_writeback", "nr_evicted", "nr_recently_evicted"]]


def file_cache_stats(file):
    if platform.machine() not in ["x86_64", "aarch64"]:
        raise RuntimeError("cachestat syscall number is specified only for x86_64/aarch64")
    libc = ctypes.CDLL(None, use_errno=True)
    libc.syscall.restype = ctypes.c_long
    region = CacheRange(0, 0)
    stats = CacheStat()
    started = time.monotonic_ns()
    result = libc.syscall(ctypes.c_long(451), ctypes.c_int(file), ctypes.byref(region),
                          ctypes.byref(stats), ctypes.c_uint(0))
    ended = time.monotonic_ns()
    if result:
        error = ctypes.get_errno()
        raise OSError(error, "cachestat: " + os.strerror(error))
    info = os.fstat(file)
    page_size = os.sysconf("SC_PAGE_SIZE")
    return {**{name: getattr(stats, name) for name, _ in stats._fields_},
            "page_size": page_size, "size_bytes": info.st_size,
            "allocated_bytes": info.st_blocks * 512,
            "dirty_bytes": stats.nr_dirty * page_size,
            "writeback_bytes": stats.nr_writeback * page_size,
            "cached_bytes": stats.nr_cache * page_size,
            "sample_start_ns": started, "sample_end_ns": ended,
            "device": info.st_dev, "inode": info.st_ino}


def filestore_fds(pid):
    files = []
    seen = set()
    try:
        for entry in sorted(os.listdir(f"/proc/{pid}/fd")):
            path = f"/proc/{pid}/fd/{entry}"
            try:
                name = os.readlink(path)
            except FileNotFoundError:
                continue
            if "runsc-filestore-" in name:
                file = os.open(path, os.O_RDWR | os.O_CLOEXEC)
                info = os.fstat(file)
                identity = (info.st_dev, info.st_ino)
                if identity in seen:
                    os.close(file)
                else:
                    seen.add(identity)
                    files.append(file)
        if not files:
            raise RuntimeError("no source filestore descriptor found")
        return files
    except BaseException:
        for file in files:
            os.close(file)
        raise
