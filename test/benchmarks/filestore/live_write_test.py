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

"""Independent record/header, write-credit, oracle, and latency-window checks."""

import os
from pathlib import Path
import signal
import struct
import subprocess
import tempfile
import time
import unittest

from live_write_benchmark import Feeder, MIB, Monitor, capture_oracle, write_statistics


class LiveWriteNativeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="live-write-native-")
        cls.binary = Path(cls.workspace.name) / "workload"
        subprocess.run(["gcc", "-O2", "-std=c11", "-D_POSIX_C_SOURCE=200809L", "-Wall", "-Wextra", "-Werror",
                        str(Path(__file__).with_name("live_write_workload.c")), "-o", str(cls.binary)], check=True)

    @classmethod
    def tearDownClass(cls):
        cls.workspace.cleanup()

    def test_initial_versions_and_corruption_detection(self):
        with tempfile.TemporaryDirectory() as directory:
            payload = Path(directory) / "payload"
            env = {**os.environ, "FILESTORE_BENCH_PAYLOAD": str(payload)}

            def run(*args):
                return subprocess.run([str(self.binary), *map(str, args)], env=env, capture_output=True).returncode

            self.assertEqual(run("init", 8 * MIB), 0)
            self.assertEqual(run("dirty", 8 * MIB, 2 * MIB), 0)
            self.assertEqual(run("verify", 8 * MIB, 4 * MIB, 2 * MIB, 0), 0)
            content = payload.read_bytes()
            for block in range(8):
                header = struct.unpack_from("<QQQQ", content, block * MIB)
                self.assertEqual(header[1:], (block, int(block < 2), MIB))
            self.assertNotEqual(run("verify", 8 * MIB, 4 * MIB, 2 * MIB, 1), 0)
            with payload.open("r+b") as file:
                file.seek(7 * MIB + 255)
                file.write(bytes([content[7 * MIB + 255] ^ 0xff]))
            self.assertEqual(run("verify", 8 * MIB, 4 * MIB, 2 * MIB, 0), 1)

    def test_ring_generations_and_credit_barrier(self):
        with tempfile.TemporaryDirectory() as directory:
            payload = Path(directory) / "payload"
            env = {**os.environ, "FILESTORE_BENCH_PAYLOAD": str(payload),
                   "FILESTORE_BENCH_READY": str(Path(directory) / "ready")}
            monitor = Monitor()
            process = subprocess.Popen([str(self.binary), "serve", str(4 * MIB), str(2 * MIB)], env=env,
                                       stdin=monitor.input_read, stdout=monitor.output_write,
                                       stderr=subprocess.PIPE, start_new_session=True)
            monitor.handed_off()
            try:
                monitor.wait_kind("H", 2)
                monitor.write(b"wwwwwb")
                monitor.wait_kind("Z")
                self.assertEqual(monitor.completed, 5)
                self.assertEqual(next(event["value"] for event in monitor.events if event["kind"] == "Z"), 5)
                content = payload.read_bytes()
                headers = [struct.unpack_from("<QQQQ", content, block * MIB)[2] for block in range(4)]
                self.assertEqual(headers, [6, 5, 0, 0])
                verified = subprocess.run([str(self.binary), "verify", str(4 * MIB), str(2 * MIB), "0", "5"],
                                          env=env, capture_output=True)
                self.assertEqual(verified.returncode, 0, verified.stderr)
                monitor.write(b"wwb")
                monitor.wait_kind("Z", 2)
                self.assertEqual(monitor.completed, 7)
                self.assertNotEqual(payload.read_bytes()[:2 * MIB], content[:2 * MIB])
                self.assertEqual(payload.read_bytes()[2 * MIB:], content[2 * MIB:])
                monitor.write(b"q")
                self.assertEqual(process.wait(timeout=5), 0)
            finally:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                process.communicate(timeout=5)
                monitor.close()


class MeasurementTest(unittest.TestCase):
    def test_rate_demand_does_not_queue_restorable_pipe_credits(self):
        monitor = Monitor()
        feeder = Feeder(monitor, 2048)
        try:
            deadline = time.monotonic() + 5
            while feeder.issued < 20:
                if time.monotonic() > deadline:
                    self.fail("demand feeder did not run")
                time.sleep(.001)
            self.assertEqual(len(feeder.dispatched_ns), 2)
            self.assertGreaterEqual(feeder.issued, 20)
            os.set_blocking(monitor.input_read, False)
            self.assertEqual(os.read(monitor.input_read, 65536), b"w")
            with self.assertRaises(BlockingIOError):
                os.read(monitor.input_read, 65536)
        finally:
            feeder.finish(drain=False)
            monitor.close()

    def test_frozen_oracle_uses_drained_prefix(self):
        events = [{"kind": "E", "value": 30}, {"kind": "B", "value": 31}, {"kind": "E", "value": 31}]
        pause = {"pause_log_start_wall_ns": 1000, "pause_log_end_wall_ns": 1600}
        drains = [{"wall_ns": 1250, "monotonic_ns": 5, "event_count": 2}]
        result = capture_oracle(events, drains, pause)
        self.assertEqual(result["completed_sequence"], 30)
        self.assertEqual(result["started_sequence"], 31)
        with self.assertRaisesRegex(RuntimeError, "empty"):
            capture_oracle(events, [], pause)
        with self.assertRaisesRegex(RuntimeError, "in flight"):
            capture_oracle([{"kind": "E", "value": 30}, {"kind": "B", "value": 32}], drains, pause)

    def test_latency_keeps_operations_finishing_after_window(self):
        events = [{"kind": "B", "value": 1, "monotonic_ns": 10},
                  {"kind": "B", "value": 2, "monotonic_ns": 18},
                  {"kind": "E", "value": 1, "monotonic_ns": 30, "duration_ns": 20},
                  {"kind": "E", "value": 2, "monotonic_ns": 35, "duration_ns": 17}]
        result = write_statistics(events, [None, 5, 15], 0, 20)
        self.assertEqual(result["completed_ops"], 0)
        self.assertEqual(result["throughput_Bps"], 0)
        self.assertEqual(result["syscall_ops"], 2)
        self.assertEqual(result["pwrite_p99_ns"], 20)
        self.assertEqual(result["request_ops"], 2)
        self.assertEqual(result["credit_p99_ns"], 25)
        self.assertEqual(result["pending_at_end"], 2)
        self.assertEqual(result["syscall_cohort_finishing_after_end"], 2)
        self.assertIsNone(result["completed_credit_p99_ns"])


if __name__ == "__main__":
    unittest.main()
