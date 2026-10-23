# Linux 5.10 qualification of anonymous filestore checkpoints

This evidence supplements gVisor PR #11 at `31b8e2a868913a95dbddb6d8c446716379bd884a`, on top of PR #10 (`e6e6a6cf5e9f0141272f210198e9cf8e3f3ae3ce`). It was collected on 2026-10-23. The runtime source and published `.5` candidate bytes were unchanged throughout these tests. This evidence branch adds fixtures and results only; it is not a new runtime build.

## Exact artifacts

- Complete candidate: [release-20260817.0-akernel.5](https://github.com/hky1999/gvisor/releases/tag/release-20260817.0-akernel.5), installed with sandboxd's checksum-pinned `third_party/install-gvisor.sh`, including the matching shim and all four adjacent helpers.
- amd64 archive SHA-512: `837ed08f9c3a0346a5d61c83ee0f68a86e0e4b0ffa9ee7888b82be5bf8c0488b1ba19e9ebaa966b5a4847a69507db65bcc2943351a5b0ae70fc3cdbd0151bed8`.
- Installed runsc SHA-256: `f6416bd37596fb6e468d336765c98374d1d84096d4e29fcf657e84a1fa0e9853`; version `release-20260817.0-12-g31b8e2a86891`.
- Final sandboxd test checkout: [`bf623fe36bf3fd363d8f0c1f63f61d4429f14234`](https://github.com/hky1999/sandboxd/commit/bf623fe36bf3fd363d8f0c1f63f61d4429f14234), in [sandboxd PR #78](https://github.com/inclusionAI/sandboxd/pull/78).
- The daemon was built statically with Go 1.25.5 from `3fba0372531aa5c8d5dab68a1180c649ea4a3657`, SHA-256 `301358dbfc745686d41124c3f28c02ba43db7ad479540c886cb29faecfa96bd6`. The two subsequent commits change tests only. The final test script was copied from the final checkout into the test image before the unified run.

## Environment and scope

The live cluster survey found kernel `5.10.134-19.8.al8.x86_64` with **cgroup-v2** on all 11 sampled workers. To test cgroup-v1 compatibility, an isolated nested-KVM VM booted the production host's exact kernel and initramfs with `systemd.unified_cgroup_hierarchy=0 systemd.legacy_systemd_cgroup_controller=true`. The VM had 6 vCPUs, 8 GiB RAM, Ubuntu 24.04 userspace, genuine nested `/dev/kvm`, and a separate XFS disk with `reflink=1`. Its `user.max_user_namespaces=65536` matched the deployed host. The VM exposed genuine cgroup-v1 controllers, including co-mounted `net_cls,net_prio`. See [captured environment](results/environment-v1.json).

The agreed old-kernel gate is the complete sandboxd **runsc** regression: systrap, KVM, and reflink. Other runtimes retain the earlier [complete candidate qualification](https://github.com/hky1999/gvisor/actions/runs/39383260302), which passed all eight runtime cases and AKernel standalone: all-in-one build, ten SDK examples, eight integration tests, and thirty E2E tests. That qualification tested these exact gVisor bytes on amd64. ARM64 had native archive build/layout/ELF validation, not a functional runtime qualification. Linux 5.10 lacks `mount_setattr`, so Firecracker virtio-fs was not claimed as passing on this old kernel.

## Full sandboxd runsc regression

`run_guest_regression.sh` runs the original `test/e2e/run.sh` from the final sandboxd checkout. It enables Redis SNAT/DNAT traffic and the additional cgroup-disabled paths used by the systrap and reflink profiles. No functional assertions were skipped. The final unified run exited 0.

| Profile | Managed cgroup-v1 | Additional cgroup-disabled path | Result |
| --- | --- | --- | --- |
| systrap | Full suite | Passed | PASS |
| KVM | Full suite | Not part of this profile | PASS |
| reflink (systrap) | Full suite | Passed | PASS |

Coverage includes lifecycle/env/exec, direct DNS, TAP ACL updates, DNAT, ten consecutive leave-running checkpoints and final-state restore, writable host-mount C/R (append FD, read offset and memory), writable-layer quota, partial host-task OOM with an independent live guard, immediate OOM, cached cgroup reuse with changed CPU/memory limits, daemon crash/recovery and OOM watcher reattachment, and Redis SNAT/DNAT benchmarks. The reflink profile additionally asserts immutable artifacts, independent clones, checkpoint after restore and continued execution after caller artifact removal.

Raw logs: [systrap](results/runsc-systrap.log), [KVM](results/runsc-kvm.log), [reflink](results/runsc-reflink.log), [unified driver](results/guest-regression-final.log). The intentional OOM `Killed` messages and read-only write rejection are expected assertions, not failures.

The full cgroup-v1 run reproduced the previously reported missing `pids.max` during daemon recovery. Sandboxd's cgroup-v1 deletion loaded both aliases of a co-mounted controller; the second alias failed with ENOENT after the first was deleted, leaving a partial memory-controller lease behind. PR #78 now deletes each physical hierarchy once, skips already absent paths on retry, and preserves containerd's live-membership checks. Three unit regressions cover aliases, distinct hierarchies and invalid paths. The full runsc suites above passed after this fix. Local formatting, vet and all 41 tested Go packages also passed; the [local log](results/sandboxd-final-checks.log) is included.

KVM's functional fixtures use 256 MiB rather than 128 MiB on this cgroup-v1 kernel, with the cached-limit change raised to 512 MiB. KVM memslot metadata/helper startup charges otherwise caused an early OOM before the intended test action. Hard-limit assertions still check the configured values; the injected OOM, independent guard, cache-reuse and recovery assertions remain enabled. Systrap budgets are unchanged. The unrelated mountpoint unit test uses a fresh child directory because `/tmp` itself can be mounted on CI runners.

## PR #10 anonymous tmpfs + PR #11 parent-mountns

`combo_probe.py` runs a root writable overlay plus a private anonymous tmpfs filestore mounted at `/data` through the runsc annotations. `/data` reports tmpfs and the host bind source stays empty. The payload contains 32 MiB of random bytes, with 1 MiB overwritten between generations. The static Go worker retains 8 MiB of random RAM and an unlinked open FD in the tmpfs, at offset 6.

For each combination, the negative control omits `--filestore-parent-mountns` and reproduces checkpoint EXDEV. With the flag enabled, five leave-running captures succeed, each manifest contains both `/` and `/data`, and **all five generations are restored and checked**. Verification compares the complete payload SHA-256, RAM hash, PID, marker, FD contents/offset, and running counter. Two restores are independently writable in root and tmpfs; saved sparse-file digests remain unchanged. Checkpoint after restore succeeds, and live restored sandboxes continue after the named artifacts are removed. Successful probes remove their sandboxes, artifacts, state and null-netns mount.

| Host kernel mode | Platform | Restored generations | Result |
| --- | --- | --- | --- |
| Same production kernel in cgroup-v1 VM | systrap | 5/5 | PASS |
| Same production kernel in cgroup-v1 VM | KVM | 5/5 | PASS |
| Native production kernel, cgroup-v2 | systrap | 5/5 | PASS |
| Native production kernel, cgroup-v2 | KVM | 5/5 | PASS |

These direct runsc probes use `--ignore-cgroups`; managed-cgroup behavior is covered by the complete sandboxd suites above. Native cgroup-v2 was used for the two combination probes, not another complete three-profile sandboxd suite.

FIEMAP accounting over the ten saved filestore files (two per generation) yields the same totals in all four probes: **167,813,120 bytes** (160.039 MiB) summed mapped extents versus **37,773,312 bytes** (36.023 MiB) in their physical interval union, a **77.49%** reduction. Shared extents carry the FIEMAP shared flag. See [machine-readable summary](results/summary.json) and the four `combo-*.json` files for per-generation hashes, manifests and physical extents. Only temporary directory prefixes were removed from the captured file names.

This is **filestore data-block accounting**, excluding RAM serialization, filesystem metadata, live filestores and restored clones. RAM is still serialized in full. The synthetic payload and timings are regression evidence, not a general capacity or performance benchmark.

## Reproduction inputs

Use the exact candidate archive and final sandboxd checkout above. Build sandboxd and its E2E helper binaries with Go 1.25.5. Prepare a privileged VM/host with the specified kernel, Docker, nested KVM, XFS reflink storage and the kernel modules required by sandboxd's original suite. `run_guest_regression.sh` expects those binaries in `/usr/local/bin`, a root-readable `/root/qualification/share/sandboxd-source.tar.gz` (containing `sandboxd/`), and a Docker-exported Redis fixture in `redis-image.tar.gz` from `docker.io/library/redis@sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf`. It builds a local test-image layer from that checkout and daemon; its imported base is an Ubuntu 24.04 userspace with the original E2E dependencies, complete candidate bundle and helper binaries. The VM root must omit Docker's `/.dockerenv` marker so systemd initializes normal guest networking.

For the combination probe, prepare an `assets/` directory containing `rootfs.tar.gz` (a root-owned BusyBox rootfs with sh/cat/stat/sha256sum/dd) and the statically compiled worker:

```sh
CGO_ENABLED=0 go build -o assets/tmpfs-worker tmpfs_worker.go
python3 combo_probe.py --assets assets --output combo.json \
  --platform systrap --cgroup v1 --runsc /usr/local/bin/runsc \
  --work-root /home/akernel
```

Repeat with `--platform kvm`, and on the native cgroup-v2 host with `--cgroup v2` and an owned XFS work directory. The probe intentionally rejects any kernel release other than the qualified one. Production daemon binaries, configuration and service Pods were not replaced.
