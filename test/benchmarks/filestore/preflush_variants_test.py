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

"""Check native range writeback and failure propagation."""

import ctypes
import errno
import hashlib
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock

import preflush_variants as variants


class RangeWritebackTest(unittest.TestCase):
    def test_writeback_keeps_all_content_and_eof(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'payload'
            content = bytes(range(256)) * (37 * 4096)
            path.write_bytes(content)
            variants.CALLS = []
            with path.open('r+b') as file:
                variants.range_writeback(file.fileno(), 16 << 20)
            self.assertEqual(path.stat().st_size, len(content))
            self.assertEqual(hashlib.sha256(path.read_bytes()).digest(), hashlib.sha256(content).digest())

    def test_io_error_is_not_silently_accepted(self):
        def full(*args):
            ctypes.set_errno(errno.ENOSPC)
            return -1
        with tempfile.TemporaryFile() as file:
            file.write(b'x' * 4096)
            file.flush()
            with mock.patch.object(variants, 'libc', types.SimpleNamespace(sync_file_range=full)):
                with self.assertRaises(OSError) as failure:
                    variants.range_writeback(file.fileno(), 16 << 20)
            self.assertEqual(failure.exception.errno, errno.ENOSPC)


if __name__ == '__main__':
    unittest.main()
