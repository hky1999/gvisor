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

"""Native byte-difference checks independent of the workload's verifier."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class RewriteWorkloadTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workspace = tempfile.TemporaryDirectory(prefix="filestore-rewrite-native-")
        cls.binary = Path(cls.workspace.name) / "workload"
        subprocess.run(["gcc", "-std=c11", "-D_POSIX_C_SOURCE=200809L", "-O2", "-Wall", "-Wextra", "-Werror",
                        str(Path(__file__).with_name("rewrite_workload.c")), "-o", str(cls.binary)], check=True)

    @classmethod
    def tearDownClass(cls):
        cls.workspace.cleanup()

    def invoke(self, path, *args):
        return subprocess.run([str(self.binary), *map(str, args)],
                              env={**os.environ, "FILESTORE_BENCH_PAYLOAD": str(path)},
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE)

    def test_exact_changed_pages_and_verifier(self):
        page_size = os.sysconf("SC_PAGE_SIZE")
        total = 8 << 20
        for pattern in ["prefix", "spread"]:
            for changed in [0, page_size, 1 << 20, total]:
                with self.subTest(pattern=pattern, changed=changed), tempfile.TemporaryDirectory() as directory:
                    path = Path(directory) / "payload"
                    self.assertEqual(self.invoke(path, "write", total).returncode, 0)
                    before = path.read_bytes()
                    self.assertEqual(self.invoke(path, "rewrite", total, changed, pattern).returncode, 0)
                    after = path.read_bytes()
                    self.assertEqual(len(after), total)
                    expected_pages = ({i * (total // page_size) // (changed // page_size)
                                       for i in range(changed // page_size)} if pattern == "spread"
                                      else set(range(changed // page_size)))
                    observed_pages = set()
                    for page in range(total // page_size):
                        start = page * page_size
                        a = before[start:start + page_size]
                        b = after[start:start + page_size]
                        if a != b:
                            observed_pages.add(page)
                            self.assertEqual(b, bytes(value ^ 0xff for value in a))
                    self.assertEqual(observed_pages, expected_pages)
                    self.assertEqual(self.invoke(path, "verify", total, changed, pattern).returncode, 0)
                    self.assertEqual(self.invoke(path, "verify", total).returncode == 0, changed == 0)

    def test_invalid_counts_do_not_modify_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "payload"
            self.assertEqual(self.invoke(path, "write", 8192).returncode, 0)
            before = path.read_bytes()
            for count in [-1, 1, 12288, "invalid"]:
                self.assertNotEqual(self.invoke(path, "rewrite", 8192, count, "prefix").returncode, 0)
                self.assertEqual(path.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
