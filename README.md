# External filestore mount identity: reproduction and validation

Published with google/gvisor#14228 on November 12, 2026.

- Before: `f87bd8102be7dc51a1979040a55c91d08d91baf6`.
- After: `cd7d4251b17357d8f1004dbb3d82bd4a086cc89c` (single squashed PR commit).
- Both are based on `a42ad63bdbc80581b567817b0f7fe9e878df6637`.
- All 4,652 source file-content hashes in `source-manifest.json` match the candidate commit. For the `.bazelversion` symlink, the manifest hashes the target contents; both the committed link target and its contents were checked. The measurements were run on this exact working tree before its commit was amended; amending changed commit metadata, not tested files.

Apply `reproducer.patch` to the before commit in a separate checkout to reproduce the corruption. The 18 reordered-mount runs fail by reading `checkpoint-2` instead of `checkpoint-0`. In another clean checkout of the before commit, apply `local-fix.patch`, or check out the candidate directly. See `REPRODUCE.txt` for commands, filesystem requirements and the broader regression filter. Do not apply both patches on top of each other.

The fixed suite passes 54 external snapshot cases on real XFS across systrap/KVM with three repetitions, including 24 reordered cases, source relocation, symlink destinations, source writes and two independent restores. Reordered instances are checkpointed again and restored with the original mount order. Ordinary checkpoint/resume and split-FS regressions pass. The affected container suite also passes under the race detector on systrap. Invalid layouts and old positional artifacts are rejected. See `evidence/verification-summary.json` and the test logs.

Complete opt release builds, source lint, clang-tidy (367 sources), and the nine benchmark harness tests passed. Related unit-test targets passed; four unchanged targets reused Bazel's cached results. ENOSPC cleanup is injection-tested, not a physical disk-fill test. No power-loss test was performed.

## Performance

Both sides use external filestore snapshots, `--leave-running`, a newly dirty 64 MiB layer and no prewrite. These are 24 alternating pairs, all 48 verified against both the old snapshot and the source's newer contents. They measure this mapping fix, not a new upstream-versus-external comparison.

| Median, ms | Before | After |
| --- | ---: | ---: |
| Application pause | 63.91 | 64.23 |
| CLI return | 72.88 | 73.07 |
| All restore inputs durable | 74.98 | 74.95 |
| Restore CLI | 79.73 | 79.29 |

None of those differences is statistically significant. The existing `BenchmarkSandboxBoot` runs all systrap/KVM and overlay variants at 24 alternating pairs, with five iterations per sample; none of its 12 primary time metrics differs significantly. Raw benchstat includes derived p50/p90 metrics. `boot-statistics.json` and `performance-summary.json` provide sample standard deviations; benchstat's ± values are confidence intervals. Geomeans across timing components are not performance claims.

`performance.py` records the build paths and matching release/sidecar selection. Its harness differs from the [previous published harness](https://github.com/hky1999/gvisor/tree/e9881565fa90bfff4e64f4c853e7dad60c2361db) by enabling external mode for both variants. Install the whole `//:release` fileset for each variant. Helper scripts use the recorded host's absolute paths; adapt those paths to your own checkouts, Bazel cache and reflink filesystem.

```sh
python3 -m unittest discover -s harness -p '*_test.py'
python3 harness/resume_benchmark.py \
  --baseline /path/to/before-release/runsc \
  --candidate /path/to/after-release/runsc \
  --work-dir /mnt/xfs/new-mount-identity-work \
  --output /path/to/new-results --mib 64 --preflush-mode off --pairs 24
benchstat /path/to/new-results/baseline-dirty.txt /path/to/new-results/candidate-dirty.txt
```

The focused timing results do not replace the older sparse/dense/fresh upstream comparisons or explain the previously reported ordinary SaveTo microbenchmark result. The filename hash identifies a mount destination, not file contents. Old positional snapshots are intentionally not accepted; no protection against manually swapping artifacts is claimed.

Raw logs of intermediate test-fixture and benchmark setup mistakes are retained under names containing `initial` or in the initial `performance.log`/`performance-resumed.log`. They are diagnostic history, not successful runs or benchmark samples. Final functional results are in `integration-test.log` and `race-test.log`; final benchmark completion is in `performance-final.log`. Large logs are gzip-compressed.
