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

"""Compare whole-file fsync, bounded range writeback, and an elapsed-time control.

Range writeback is preparatory, not a durability barrier. The checkpoint's
stable clone and all-artifact fsync remain unchanged in every treatment.
"""

import argparse
import bisect
import ctypes
import json
import os
from pathlib import Path
import shutil
import subprocess
import time

import live_write_benchmark as bench

SOURCE_FILES = []
ACTIVE = False
METHOD = None
CALLS = []
original_fds = bench.filestore_fds
original_feeder = bench.Feeder
original_fsync = os.fsync
libc = ctypes.CDLL(None, use_errno=True)
libc.sync_file_range.argtypes = [ctypes.c_int, ctypes.c_longlong, ctypes.c_longlong, ctypes.c_uint]
libc.sync_file_range.restype = ctypes.c_int


def range_writeback(file, chunk):
    for offset in range(0, os.fstat(file).st_size, chunk):
        started = time.monotonic_ns()
        # WAIT_BEFORE | WRITE | WAIT_AFTER bounds outstanding writeback to
        # one range. File metadata is persisted by the final checkpoint.
        if libc.sync_file_range(file, offset, chunk, 7):
            error = ctypes.get_errno()
            raise OSError(error, 'sync_file_range: ' + os.strerror(error))
        CALLS.append({'offset': offset, 'length': chunk, 'start_ns': started,
                      'end_ns': time.monotonic_ns()})


def filestore_fds(pid):
    global SOURCE_FILES
    SOURCE_FILES = original_fds(pid)
    return SOURCE_FILES


class Feeder(original_feeder):
    def __init__(self, monitor, rate):
        global ACTIVE
        super().__init__(monitor, rate)
        ACTIVE = True


def fsync(file):
    if not ACTIVE or file not in SOURCE_FILES or METHOD == 'fsync':
        return original_fsync(file)
    if METHOD == 'wait':
        time.sleep(.9)
        return None
    return range_writeback(file, int(METHOD.removeprefix('range')) * bench.MIB)


def extremes(logs, data):
    events = [json.loads(line) for line in (logs / 'source-events.jsonl').read_text().splitlines()]
    credits = json.loads((logs / 'credits.json').read_text())[1:]
    ends = [event for event in events if event['kind'] == 'E']
    times = [event['monotonic_ns'] for event in ends]
    result = {}
    for phase, stats in data['writer_statistics'].items():
        if stats is None:
            continue
        begin, end = stats['begin_ns'], stats['end_ns']
        completed = [event for event in ends if begin <= event['monotonic_ns'] < end]
        pending = [bisect.bisect_right(credits, stamp) - bisect.bisect_right(times, stamp) for stamp in
                   [begin, end - 1] + credits[bisect.bisect_left(credits, begin):bisect.bisect_left(credits, end)]]
        result[phase] = {'completed_pwrite_max_ns': max((event['duration_ns'] for event in completed), default=None),
                         'completed_pwrite_over_10ms': sum(event['duration_ns'] > 10_000_000 for event in completed),
                         'pending_peak': max(pending)}
    return result


def main():
    global ACTIVE, METHOD, CALLS, SOURCE_FILES
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runsc', required=True, type=Path)
    parser.add_argument('--work-dir', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--mib', required=True, type=int)
    parser.add_argument('--hot-mib', default=1024, type=int)
    parser.add_argument('--rate-mib', required=True, type=int)
    parser.add_argument('--initial-dirty-mib', type=int)
    parser.add_argument('--rounds', default=8, type=int)
    parser.add_argument('--methods', nargs='+', choices=['fsync', 'range16', 'range64', 'wait'], required=True)
    args = parser.parse_args()
    if os.geteuid() or not 0 < args.hot_mib <= args.mib or args.rate_mib < 0 or args.rounds < 1:
        parser.error('requires root, valid sizes, nonnegative rate, and positive rounds')
    if args.initial_dirty_mib is not None and not 0 <= args.initial_dirty_mib <= args.hot_mib:
        parser.error('initial dirty prefix must fit in the hotset')
    args.work_dir.mkdir(exist_ok=False)
    args.output.mkdir(exist_ok=False)
    rootfs = args.work_dir / 'rootfs'
    (rootfs / 'proc').mkdir(parents=True)
    subprocess.run(['gcc', '-O2', '-static', str(Path(__file__).with_name('live_write_workload.c')),
                    '-o', str(rootfs / 'workload')], check=True)
    (args.output / 'environment.json').write_text(json.dumps({key: str(value) if isinstance(value, Path) else value
                                                            for key, value in vars(args).items()}, indent=2) + '\n')
    bench.filestore_fds, bench.Feeder, os.fsync = filestore_fds, Feeder, fsync
    with (args.output / 'samples.jsonl').open('w') as samples:
        for round_number in range(args.rounds):
            offset = round_number % len(args.methods)
            order = args.methods[offset:] + args.methods[:offset]
            for METHOD in order:
                ACTIVE, CALLS, SOURCE_FILES = False, [], []
                name = f'{round_number}-{METHOD}'
                trial, logs = args.work_dir / name, args.output / name
                try:
                    data = bench.run_trial(args, rootfs, trial, logs, True)
                finally:
                    ACTIVE = False
                    (logs / 'prewrite-calls.json').write_text(json.dumps(CALLS) + '\n')
                    SOURCE_FILES = []
                data.update(pair=round_number, method=METHOD, mib=args.mib, hot_mib=args.hot_mib,
                            initial_dirty_mib=args.initial_dirty_mib, rate_mib=args.rate_mib,
                            prewrite_calls=len(CALLS), writer_extremes=extremes(logs, data))
                samples.write(json.dumps(data) + '\n')
                samples.flush()
                (args.output / 'status.json').write_text(json.dumps({'phase': 'running', 'last': name}) + '\n')
                metrics = {'ApplicationPause': (data['pause']['application_pause_ns'], 'ns/op'),
                           'Checkpoint': (data['checkpoint_ns'], 'ns/op'),
                           'DurableCheckpoint': (data['durable_checkpoint_ns'], 'ns/op'),
                           'EndToEnd': (data['end_to_end_ns'], 'ns/op'),
                           'PrewriteWindow': (data['preflush_ns'], 'ns/op'),
                           'FICLONE': (data['snapshot_phases']['ficlone_ns'], 'ns/op'),
                           'RemainingDirty': (sum(stat['dirty_bytes'] for stat in data['after_preflush']['stats']), 'bytes/op')}
                stats, tail = data['writer_statistics']['preflush'], data['writer_extremes']['preflush']
                for label, key, unit in [('Throughput', 'throughput_Bps', 'B/s'), ('CreditP99', 'completed_credit_p99_ns', 'ns/op')]:
                    if stats[key] is not None:
                        metrics[label] = (stats[key], unit)
                if tail['completed_pwrite_max_ns'] is not None:
                    metrics['LongestPwrite'] = (tail['completed_pwrite_max_ns'], 'ns/op')
                metrics['PeakPending'] = (tail['pending_peak'], 'requests/op')
                with (args.output / (METHOD + '.txt')).open('a') as output:
                    for metric, (value, unit) in metrics.items():
                        output.write(f'BenchmarkPrewrite/{args.mib}MiB/{args.rate_mib}MiBps/{metric} 1 {value} {unit}\n')
                print(json.dumps({'trial': name, 'pause_ms': data['pause']['application_pause_ns'] / 1e6,
                                  'prewrite_ms': data['preflush_ns'] / 1e6,
                                  'longest_pwrite_ms': (tail['completed_pwrite_max_ns'] or 0) / 1e6}), flush=True)
                shutil.rmtree(trial)
    (args.output / 'status.json').write_text(json.dumps({'phase': 'complete'}) + '\n')


if __name__ == '__main__':
    main()
