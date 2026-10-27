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

"""Check that pause measurement selects the freeze gap, not later I/O stalls."""

import datetime
from pathlib import Path
import tempfile
import time
import unittest

from resume_benchmark import pause_measurement


class PauseMeasurementTest(unittest.TestCase):
    def test_selects_pause_gap_instead_of_larger_later_gap(self):
        wall = int(time.time()) * 1_000_000_000
        monotonic = 1_000_000_000

        def line(offset, message):
            date = datetime.datetime.fromtimestamp(wall // 1_000_000_000)
            stamp = date.strftime("I%m%d %H:%M:%S") + f".{offset // 1000:06d}"
            return stamp + "       1 state.go:1] " + message

        events = [{"kind": "H", "value": index, "wall_ns": wall + offset,
                   "monotonic_ns": monotonic + offset, "batch": index}
                  for index, offset in enumerate([98_000_000, 99_000_000, 301_000_000, 302_000_000, 900_000_000])]
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "boot.txt"
            log.write_text(line(100_000_000, "Sandbox save started, pausing all tasks.") + "\n" +
                           line(300_000_000, "Tasks resumed after save.") + "\n")
            result = pause_measurement(log, events, monotonic + 100_000_000, monotonic + 350_000_000)
            self.assertEqual(result["application_pause_ns"], 202_000_000)
            self.assertEqual(result["pause_log_ns"], 200_000_000)
            self.assertEqual(result["control_max_gap_ns"], 1_000_000)
            self.assertEqual(result["heartbeat_before"]["value"], 1)
            self.assertEqual(result["heartbeat_after"]["value"], 2)
            self.assertGreater(result["checkpoint_max_gap_ns"], result["application_pause_ns"])
            resume = {"kind": "R", "value": None, "wall_ns": wall + 300_000_000,
                      "monotonic_ns": monotonic + 300_000_000, "batch": 2}
            result = pause_measurement(log, events[:2] + [resume] + events[2:],
                                       monotonic + 100_000_000, monotonic + 350_000_000)
            self.assertEqual(result["application_pause_ns"], 201_000_000)
            self.assertEqual(result["heartbeat_after"]["kind"], "R")

    def test_missing_pause_window_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "boot.txt"
            log.write_text("unrelated output\n")
            with self.assertRaisesRegex(RuntimeError, "exactly one"):
                pause_measurement(log, [], 0, 1)


if __name__ == "__main__":
    unittest.main()
