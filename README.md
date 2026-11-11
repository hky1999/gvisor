# External filestore checkpoint validation — 2026-11-11

Reproducibility material for [google/gvisor#14228](https://github.com/google/gvisor/pull/14228).
These research scripts are kept outside the feature PR. Formal regression tests
are in `runsc/container`, `pkg/sentry/pgalloc`, and the related runtime packages.

- Upstream: `a42ad63bdbc80581b567817b0f7fe9e878df6637`.
- Candidate: `f87bd8102be7dc51a1979040a55c91d08d91baf6`.
- Linux x86-64, XFS reflink enabled, systrap for performance; systrap and KVM for integration.
- No checkpoint prewrite or asynchronous park. Both performance variants use `--leave-running`.

## Reproduce the workload comparisons

Use a root test host with XFS supporting reflink and O_TMPFILE, Python 3 and gcc
with static libc. Build each revision's **complete** release with
`bazel build -c opt --nostamp --workspace_status_command=/bin/true //:release`.
Copy the entire `bazel-bin/release` fileset to a distinct installation directory
for each revision, keeping all its files together. The commands below assume
`/opt/upstream/runsc` and `/opt/candidate/runsc` are those installed entry points.

Each work and output directory must be new. Substitute your reflink filesystem
for `/mnt/xfs`. The tests create and delete their own OCI containers and check
both the resumed source's newer contents and the restored snapshot's old contents.

```sh
python3 harness/resume_benchmark.py --baseline /opt/upstream/runsc --candidate /opt/candidate/runsc --work-dir /mnt/xfs/fresh --output results/fresh --preflush-mode off --pairs 12 --mib 1024
python3 harness/resume_benchmark.py --baseline /opt/upstream/runsc --candidate /opt/candidate/runsc --work-dir /mnt/xfs/sparse --output results/sparse --preflush-mode off --pairs 12 --mib 4096 --sparse-mib 64
python3 harness/resume_benchmark.py --baseline /opt/upstream/runsc --candidate /opt/candidate/runsc --work-dir /mnt/xfs/dense --output results/dense --preflush-mode off --pairs 12 --mib 4096 --rewrite-mib 64
benchstat results/dense/baseline-dirty.txt results/dense/candidate-dirty.txt
python3 -m unittest discover -s harness -p '*test*.py'
```

`benchmark.py` contains shared helpers; `resume_benchmark.py` is the entry point.
The historical `dirty` filename suffix means preflush is disabled, not that all
pages are dirty. The sparse fixture is already synchronized; the dense fixture
synchronizes its 4 GiB base, then rewrites 64 MiB. This fixture preparation is
outside checkpoint timing and is not checkpoint prewrite.

## What the measurements mean

- ApplicationPause: the host-observed heartbeat gap across capture, with a nominal
  1 ms period and observer scheduling error; cross-checked with sentry pause logs.
- Checkpoint: CLI return. Candidate success includes synchronization of all restore
  inputs and containing/ancestor directories; upstream CLI return alone does not.
- DurableCheckpoint: checkpoint invocation through host fsync of every restore
  input and containing/ancestor directories, applied to both variants. It includes
  CLI time. It checks fsync completion, not power-loss recovery.
- Restore: CLI return, not completion of all background page loading.
- The source rewrites 64 MiB after resume; every sample verifies the full source
  and restored payload and rejects the opposite version, including sparse zeros.

`results/*/samples.jsonl` records actual dirty bytes, host extent layout, timing
breakdowns and verification counts. Raw benchstat files include confidence
intervals; `summary.json` additionally records sample standard deviations.
`p=0.000` in benchstat means p<0.001 after rounding.

The ordinary sparse fixture also benefits from allocator bookkeeping and does
not isolate SEEK_DATA/SEEK_HOLE's contribution. The dense fixture has no holes.
First fully dirty layers can pause longer because FICLONE can wait for writeback.
The data does not establish constant latency, a universal speedup, multi-instance
scaling, behavior on other filesystems, or the reviewer's loop-backed XFS timing.

## Formal validation

The exact host commands are in `validation/validate.sh`. Relevant unit and race
suites, explicit XFS allocator tests, and container tests on systrap/KVM are run.
The container matrix includes stopping, resumed writes, two private restores,
compressed checkpoints, read-only roots, and a real directory-fsync failure after
a stopping capture. ENOSPC coverage uses error injection, not a physically full disk.

Full sandbox tests have three route assertion failures reproduced on the exact
unmodified upstream: SingleInterface, MultipleInterfaces, and IPv6Disabled.
These are recorded separately, not reported as passing. No C++ source is changed;
source lint and clang-tidy are rerun after regenerating the compilation database.

Existing ordinary-save benchmarks cover **all** BenchmarkSaveTo subcases with
24 alternating pairs. The initial 1-iteration run is retained as preliminary;
five fixed iterations per sample are used for the follow-up because one
short-run microbenchmark was noisy. Benchmark stdout and diagnostic stderr are
kept separate. An adaptive 200 ms attempt was stopped because fixture setup
scaled disproportionately and mixed log output broke benchmark records. The three workload comparisons use 12
alternating pairs and are checked for p<0.01 on pause, CLI and durable time.

The fixed-iteration ordinary-save run observed a 2.33% regression in memfd/1%-written (p=0.005); the other five subcases had no statistically significant difference. Its cause has not been isolated. This result is retained rather than reported as zero overhead.

## Results

Medians in milliseconds, upstream → candidate.

| Workload | Application pause | CLI return | All restore inputs durable |
|---|---:|---:|---:|
| 4 GiB sparse, 64 MiB data, clean | 31.11 → 8.81 | 36.34 → 18.76 | 95.95 → 20.81 |
| Existing 4 GiB, 64 MiB overwritten | 1,583.40 → 70.24 | 1,588.71 → 79.31 | 6,046.66 → 81.24 |
| Fresh 1 GiB, fully dirty | 399.65 → 909.54 | 405.49 → 918.24 | 1,292.89 → 920.18 |

| External phase, ms | Sparse / clean | Dense + 64 MiB rewrite | Fresh / dirty |
|---|---:|---:|---:|
| FICLONE | 0.065 | 64.539 | 904.343 |
| State/pages fsync | 2.223 | 2.337 | 2.124 |
| Snapshot fsync | 0.030 | 0.029 | 0.030 |
| Total persistence after RPC | 3.522 | 3.492 | 3.533 |

Restore CLI (not full recovery): 4 GiB sparse, 64 MiB data, clean: 69.76 → 81.54 ms; Existing 4 GiB, 64 MiB overwritten: 70.12 → 81.19 ms; Fresh 1 GiB, fully dirty: 69.89 → 82.04 ms.
