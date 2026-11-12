#!/bin/bash
set -euo pipefail
cd /root/filestore-publish-20261111/source
R=/root/filestore-reflink-precompile-20261112
B=(/root/filestore-rebase-20261105/bazel-9.3.1 --output_user_root=/root/filestore-rebase-20261105/bazel-cache --host_jvm_args=-Xmx4g)
O=(-c opt --jobs=16 --local_resources=memory=32000 --repository_cache=/root/codex-filestore-20261023/repository-cache --disk_cache=/root/codex-filestore-20261023/disk-cache --nostamp --workspace_status_command=/bin/true)
set +e
"${B[@]}" test "${O[@]}" --test_output=all //runsc/boot/filter:filter_test > "$R/baseline-generated-repro.log" 2>&1
code=$?
set -e
echo "$code" > "$R/baseline-generated-repro.exit"
if [ "$code" != 3 ]; then tail -50 "$R/baseline-generated-repro.log"; exit 1; fi
"${B[@]}" build "${O[@]}" //:release //runsc/container:boot_benchmark_test > "$R/baseline-build.log" 2>&1
cp -aL bazel-bin/release "$R/baseline-release"
cp bazel-bin/runsc/container/boot_benchmark_test_/boot_benchmark_test "$R/baseline-boot.test"
cp bazel-bin/runsc/boot/filter/filter_precompiled.go "$R/baseline-filter_precompiled.go"
echo BASELINE_READY
