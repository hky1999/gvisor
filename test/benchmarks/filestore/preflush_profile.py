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

"""Trace host waits during the continuous-write benchmark, without runtime edits.

These instrumented trials diagnose waits; they are not performance estimates.
The tracefs instance is private. Trace data stays in tmpfs until capture ends.
The original benchmark still verifies source and restored contents completely.
"""

import gzip
import hashlib
import json
import os
from pathlib import Path
import select
import shutil
import subprocess
import sys
import tempfile
import threading
import time

import live_write_benchmark as bench

SESSION = None
CLONE_ONLY = False


class Profile:
    def __init__(self, args, trial, logs):
        self.args, self.trial, self.logs = args, trial, logs
        self.pid, self.files, self.monitor = None, [], None
        self.stop_event = threading.Event()
        self.threads, self.errors, self.debug = [], [], []
        self.samples = []
        self.started = False
        self.fsync_active = False
        self.instance = self.memory = self.graph = None
        self.pipe = -1

    def start(self, monitor):
        self.monitor = monitor
        self.started = True
        tracefs = Path('/sys/kernel/tracing')
        tag = 'codex-filestore-' + hashlib.sha256(str(self.trial).encode()).hexdigest()[:12]
        self.instance = tracefs / 'instances' / tag
        self.instance.mkdir()
        self.memory = Path(tempfile.mkdtemp(prefix=tag + '-', dir='/dev/shm'))
        self.graph = (self.memory / 'graph.txt').open('wb')
        functions = {line.split()[0] for line in (tracefs / 'available_filter_functions').read_text().splitlines()}
        selected = [name for name in functions if any(name == key or name.startswith(key + '.') for key in [
            'xfs_file_fsync', 'xfs_file_remap_range', 'xfs_reflink_remap_prep', 'xfs_reflink_remap_blocks',
            'file_write_and_wait_range', 'filemap_write_and_wait_range', '__filemap_fdatawrite_range',
            'filemap_fdatawait_range', '__folio_lock', 'folio_wait_writeback', 'balance_dirty_pages',
            'xfs_log_force_seq', 'wbt_wait', 'rq_qos_wait', 'blk_mq_get_tag', '__blk_mq_alloc_requests'])]
        self.write('tracing_on', '0')
        self.write('buffer_size_kb', '1024')
        self.write('trace_clock', 'mono')
        self.write('set_ftrace_filter', '\n'.join(sorted(selected)))
        self.write('current_tracer', 'function_graph')
        for option in ['funcgraph-abstime', 'funcgraph-proc', 'funcgraph-tail', 'function-fork', 'event-fork']:
            self.write('trace_options', option)
        tids = [thread.name for pid in [self.pid, os.getpid()] for thread in Path(f'/proc/{pid}/task').iterdir()]
        self.write('set_ftrace_pid', ' '.join(tids))
        if not CLONE_ONLY:
            self.write('set_event_pid', ' '.join(tids))
        events = ['writeback/balance_dirty_pages', 'writeback/writeback_wait', 'xfs/xfs_file_fsync',
                  'xfs/xfs_reflink_remap_range', 'xfs/xfs_reflink_remap_blocks']
        if CLONE_ONLY:
            events = []
            self.write('events/syscalls/sys_enter_ioctl/trigger', 'traceon:1 if cmd == 1074041865')
            self.write('events/syscalls/sys_enter_ioctl/filter', 'cmd == 1074041865')
            events.append('syscalls/sys_enter_ioctl')
            for name in ['wbt_step', 'wbt_timer', 'wbt_lat', 'wbt_stat']:
                self.write(f'events/wbt/{name}/filter', 'name == "vdb"')
                events.append('wbt/' + name)
        for event in events:
            file = self.instance / 'events' / event / 'enable'
            if file.exists():
                file.write_text('1')
        (self.logs / 'profile-source.json').write_text(json.dumps({
            'source_pid': self.pid, 'driver_pid': os.getpid(), 'functions': sorted(selected),
            'tracefs_instance': str(self.instance), 'clock': 'CLOCK_MONOTONIC',
            'clone_only_kernel_trigger': CLONE_ONLY,
            'instrumented_for_diagnosis_only': True,
            'filestores': [bench.file_cache_stats(file) for file in self.files]}, indent=2) + '\n')
        self.pipe = os.open(self.instance / 'trace_pipe', os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)
        if not CLONE_ONLY:
            self.write('tracing_on', '1')
        for target in ([self.read_trace] if CLONE_ONLY else [self.read_trace, self.sample]):
            thread = threading.Thread(target=self.guarded, args=(target,), daemon=True)
            self.threads.append(thread)
            thread.start()

    def write(self, name, text):
        (self.instance / name).write_text(text)

    def guarded(self, function):
        try:
            function()
        except BaseException as error:
            self.errors.append(repr(error))

    def read_trace(self):
        while True:
            ready = select.select([self.pipe], [], [], .02)[0]
            if ready:
                try:
                    data = os.read(self.pipe, 1 << 20)
                except BlockingIOError:
                    data = b''
                if data:
                    self.graph.write(data)
                    continue
            if self.stop_event.is_set():
                return

    def sample(self):
        last_debug_sequence = None
        last_cache = 0
        while not self.stop_event.is_set():
            now = time.monotonic_ns()
            record = {'monotonic_ns': now, 'wall_ns': time.time_ns(), 'waits': []}
            for pid in [self.pid, os.getpid()]:
                for task in Path(f'/proc/{pid}/task').iterdir():
                    try:
                        stat = (task / 'stat').read_text()
                        state = stat[stat.rfind(')') + 2:].split()[0]
                        if state == 'D':
                            record['waits'].append({'pid': pid, 'tid': int(task.name),
                                                    'wchan': (task / 'wchan').read_text(),
                                                    'stack': (task / 'stack').read_text()})
                    except (FileNotFoundError, ProcessLookupError):
                        pass
            latest = next((event for event in reversed(self.monitor.events) if event['kind'] in ['B', 'E']), None)
            if latest and latest['kind'] == 'B' and now - latest['monotonic_ns'] > 20_000_000:
                record['pending_write'] = latest
                if self.fsync_active and latest['value'] != last_debug_sequence and len(self.debug) < 3:
                    last_debug_sequence = latest['value']
                    tag = hashlib.sha256(str(self.trial).encode()).hexdigest()[:16]
                    output = (self.logs / f'go-stacks-{latest["value"]}.log').open('wb')
                    process = subprocess.Popen([str(self.args.runsc), f'--root={self.args.work_dir / "runtime"}',
                                                'debug', '--stacks', f'live-{tag}-source'],
                                               stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.STDOUT)
                    output.close()
                    self.debug.append(process)
                    record['stack_request_ns'] = time.monotonic_ns()
            if now - last_cache >= 10_000_000:
                record['cache'] = [bench.file_cache_stats(file) for file in self.files]
                record['diskstats'] = Path('/proc/diskstats').read_text()
                last_cache = now
            if record['waits'] or 'cache' in record or 'pending_write' in record:
                self.samples.append(record)
            self.stop_event.wait(.003)

    def stop(self):
        if self.stop_event.is_set():
            return
        exists = self.instance is not None and self.instance.exists()
        if exists:
            self.write('tracing_on', '0')
        self.stop_event.set()
        for thread in self.threads:
            thread.join(timeout=10)
            if thread.is_alive():
                self.errors.append('profile thread did not exit')
        if self.pipe >= 0:
            os.close(self.pipe)
            self.pipe = -1
        if self.graph is not None:
            self.graph.close()
        if exists:
            stats = {file.parent.name: file.read_text() for file in (self.instance / 'per_cpu').glob('*/stats')}
            (self.logs / 'trace-buffer-stats.json').write_text(json.dumps(stats, indent=2) + '\n')
            self.write('current_tracer', 'nop')
            if CLONE_ONLY:
                self.write('events/syscalls/sys_enter_ioctl/trigger', '!traceon')
            self.instance.rmdir()

    def finish(self):
        self.stop()
        if not self.started:
            return
        for process in self.debug:
            try:
                code = process.wait(timeout=10)
                if code:
                    self.errors.append(f'stacks CLI exit {code}')
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                self.errors.append('stacks CLI timeout')
        if self.memory is not None:
            if (self.memory / 'graph.txt').exists():
                with (self.memory / 'graph.txt').open('rb') as source, gzip.open(self.logs / 'kernel-graph.txt.gz', 'wb') as output:
                    shutil.copyfileobj(source, output)
            shutil.rmtree(self.memory)
        with gzip.open(self.logs / 'host-samples.jsonl.gz', 'wt') as output:
            for sample in self.samples:
                output.write(json.dumps(sample) + '\n')
        (self.logs / 'profile-cleanup.json').write_text(json.dumps({'errors': self.errors}) + '\n')
        if self.errors:
            raise RuntimeError('profile errors: ' + repr(self.errors))


original_fds = bench.filestore_fds
original_feeder = bench.Feeder
original_sync = bench.sync_artifacts
original_trial = bench.run_trial
original_fsync = os.fsync


def filestore_fds(pid):
    files = original_fds(pid)
    SESSION.pid, SESSION.files = pid, files
    return files


class ProfileFeeder(original_feeder):
    def __init__(self, monitor, rate):
        SESSION.start(monitor)
        super().__init__(monitor, rate)


def fsync(file):
    active = SESSION and SESSION.started and file in SESSION.files
    if active:
        SESSION.fsync_active = True
        SESSION.samples.append({'event': 'source_fsync_start', 'monotonic_ns': time.monotonic_ns()})
    try:
        return original_fsync(file)
    finally:
        if active:
            SESSION.samples.append({'event': 'source_fsync_end', 'monotonic_ns': time.monotonic_ns()})
            SESSION.fsync_active = False


def sync_artifacts(*args):
    result = original_sync(*args)
    SESSION.stop()
    return result


def run_trial(args, rootfs, trial, logs, preflush):
    global SESSION
    SESSION = Profile(args, trial, logs)
    try:
        return original_trial(args, rootfs, trial, logs, preflush)
    finally:
        failed = sys.exc_info()[0] is not None
        try:
            SESSION.finish()
        except BaseException:
            if not failed:
                raise
        finally:
            SESSION = None


if __name__ == '__main__':
    if '--clone-only' in sys.argv:
        CLONE_ONLY = True
        sys.argv.remove('--clone-only')
    bench.filestore_fds = filestore_fds
    bench.Feeder = ProfileFeeder
    bench.sync_artifacts = sync_artifacts
    bench.run_trial = run_trial
    os.fsync = fsync
    bench.main()
