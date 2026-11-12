# Filestore reflink seccomp precompilation: reproduction and validation

Published for [google/gvisor#14228](https://github.com/google/gvisor/pull/14228) in response to [Ayush's missing-variant report](https://github.com/google/gvisor/pull/14228#discussion_r4540405313).

## Commits and measurement boundaries

- Measured before: `cd7d4251b17357d8f1004dbb3d82bd4a086cc89c`.
- Measured after: [`dbbad2349bbd41b5d68a350fc5791817181f342c`](https://github.com/hky1999/gvisor/commit/dbbad2349bbd41b5d68a350fc5791817181f342c). Both measured commits use upstream `a42ad63bdbc80581b567817b0f7fe9e878df6637`.
- Published PR head after rebase: `faa7206bc21cebae2ecd8c96691fe849e97fd0cb`, based on `2f5253abfaa87b6d62d79a0da60942e773389934`.
- The only textual rebase conflict was the `runsc/boot/BUILD` dependency list; both `socket/netstack` and `state` dependencies were retained. All 4,651 source file hashes were verified before and after the post-rebase run. The source manifest and separate validation logs are included.
- Performance and size results below isolate the precompile fix on the previous base. They are **not new measurements of the later upstream changes or the rebased head**. The original measurement provenance records its then-local status; this README records publication.

## Reproduction

Apply `tests-only.patch` to the before commit and run the two opt test targets in `REPRODUCE.txt`: the enumeration fails with 576 missing counterparts; four enabled generated-filter lookups fail. Apply `production-only.patch` to fix them. Alternatively apply `fix.patch` to a clean before checkout or check out the measured-after commit directly. Do not apply both patch combinations.

The fix expands both flag settings, generating 1,152 configurations on the measured amd64 host. Tests cover all counterpart keys, serial/parallel generation determinism, and the actual generated BPF lookup/render path with systrap/KVM, DirectFS on/off, opt-in FICLONE permission and invalid FD rejection. Fastbuild intentionally omits precompilation; run the generated-filter test with `-c opt`.

`baseline.sh`, `fixed.sh`, `performance.py` and `harness/` reproduce the pre-rebase measurement. `post-rebase-validate-rebase.sh` records the subsequent build/test command; its initial source-transfer steps are specific to the recorded host and can be replaced by checking out the published head. Adjust checkout/cache/output paths. Always install the complete `//:release` fileset, not only runsc.

## Results

24 alternating pairs per variant on the same Xeon Platinum 8269CY amd64 host. Existing BenchmarkSandboxBoot runs systrap/KVM and overlay variants, both reflink flag settings, five boots per sample. The benchmark-only configuration hook is identical on both sides and is not in the feature patch.

| Reflink enabled, median ms | Before | After |
| --- | ---: | ---: |
| systrap active boot | 36.49 | 27.08 |
| systrap + overlay | 37.38 | 27.69 |
| KVM active boot | 75.25 | 67.28 |
| KVM + overlay | 75.88 | 69.06 |
| 64 MiB external restore CLI | 75.31 | 67.54 |

All five have p<0.001. Active boot excludes waiting for the start signal and is not CLI time. All 12 primary Boot/New/Run metrics with reflink disabled have no statistically significant change. The external harness uses a newly dirty 64 MiB layer, leave-running, no prewrite, and external mode on both sides. Timed restore disables debug logging to exclude the old fallback's cache-key dump. All 48 snapshot/source checks pass; all sources start new writes before checkpoint CLI return. Application pause, checkpoint CLI and persistence time do not change significantly. These results do not remove FICLONE's first-dirty writeback delay.

The complete release grows 4,317,760 bytes (+4.12 MiB, 2.29%). Sentry grows from 55,379,043 to 59,696,803 bytes (+7.80%); all other release files are byte-identical. Generated Go source grows from 20,597,828 to 41,286,271 bytes. These are file sizes, not RSS; cold-cache and other-architecture effects were not measured.

Full raw benchstat tables, samples, sample SDs, size/hash records and cache-path logs are in `evidence/`; `report.txt` explains limitations. Benchstat ± intervals are not sample SD; timing-component geomeans are not performance claims. Post-rebase logs are separate in `rebase-evidence/`: five full unit-test targets, complete release build, 54 external integration cases (24 reordered), ordinary checkpoint/resume, split-FS and failure-cleanup regression coverage all pass, with no skipped integration cases. Post-rebase lint and clang-tidy (367 sources) pass. No new post-rebase timing or power-loss claim is made.
