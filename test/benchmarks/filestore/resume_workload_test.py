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

"""Check post-resume masks and the heartbeat/control protocol natively."""

import os
from pathlib import Path
import select
import subprocess
import tempfile
import time
import unittest


class ResumeWorkloadTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="filestore-resume-native-")
        cls.binary = Path(cls.workspace.name) / "workload"
        subprocess.run(["gcc", "-std=c11", "-D_POSIX_C_SOURCE=200809L", "-O2", "-Wall", "-Wextra", "-Werror",
                        str(Path(__file__).with_name("resume_workload.c")), "-o", str(cls.binary)], check=True)

    @classmethod
    def tearDownClass(cls):
        cls.workspace.cleanup()

    def test_postwrite_bytes_and_verifier(self):
        total = 8 << 20
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "payload"
            env = {**os.environ, "FILESTORE_BENCH_PAYLOAD": str(path)}

            def run(*args):
                return subprocess.run([str(self.binary), *map(str, args)], env=env,
                                      stdout=subprocess.PIPE, stderr=subprocess.PIPE).returncode

            for before in [0, 4096, 1 << 20]:
                for after in [4096, 2 << 20, total]:
                    with self.subTest(before=before, after=after):
                        self.assertEqual(run("write", total), 0)
                        self.assertEqual(run("rewrite", total, before, "prefix"), 0)
                        original = path.read_bytes()
                        self.assertEqual(run("postwrite", total, after), 0)
                        observed = path.read_bytes()
                        self.assertEqual(observed[:after], bytes(value ^ 0x55 for value in original[:after]))
                        self.assertEqual(observed[after:], original[after:])
                        self.assertEqual(run("verify-post", total, before, after), 0)
                        self.assertNotEqual(run("verify", total, before, "prefix"), 0)

    def test_heartbeat_and_postwrite_command(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "payload"
            env = {**os.environ, "FILESTORE_BENCH_PAYLOAD": str(path),
                   "FILESTORE_BENCH_READY": str(Path(directory) / "ready")}
            process = subprocess.Popen([str(self.binary), "serve-heartbeat", str(1 << 20), "4096"],
                                       env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                data = b""
                sent = False
                deadline = time.monotonic() + 10
                while b"W 4096\n" not in data:
                    self.assertLess(time.monotonic(), deadline)
                    if not select.select([process.stdout], [], [], 0.1)[0]:
                        continue
                    chunk = os.read(process.stdout.fileno(), 65536)
                    self.assertTrue(chunk)
                    data += chunk
                    if not sent and b"H " in data:
                        process.stdin.write(b"W")
                        process.stdin.flush()
                        sent = True
                self.assertIn(b"R\n", data)
                self.assertIn(b"B\n", data)
                self.assertEqual(subprocess.run([str(self.binary), "verify-post", str(1 << 20), "0", "4096"],
                                                env=env, capture_output=True).returncode, 0)
            finally:
                process.terminate()
                process.communicate(timeout=10)


if __name__ == "__main__":
    unittest.main()
