#!/bin/bash
set -euo pipefail
cd /root/filestore-publish-20261111/source
R=/root/filestore-reflink-precompile-20261112
B=(/root/filestore-rebase-20261105/bazel-9.3.1 --output_user_root=/root/filestore-rebase-20261105/bazel-cache --host_jvm_args=-Xmx4g)
O=(-c opt --jobs=16 --local_resources=memory=32000 --repository_cache=/root/codex-filestore-20261023/repository-cache --disk_cache=/root/codex-filestore-20261023/disk-cache --nostamp --workspace_status_command=/bin/true)
"${B[@]}" test "${O[@]}" --test_timeout=900 --test_arg=-test.v --test_output=errors //runsc/boot/filter/config:config_test //runsc/boot/filter:filter_test > "$R/fixed-unit.log" 2>&1
cp bazel-testlogs/runsc/boot/filter/config/config_test/test.log "$R/config-test.log"
cp bazel-testlogs/runsc/boot/filter/filter_test/test.log "$R/filter-test.log"
"${B[@]}" build "${O[@]}" //:release //runsc/container:boot_benchmark_test > "$R/fixed-build.log" 2>&1
cp -aL bazel-bin/release "$R/fixed-release"
cp bazel-bin/runsc/container/boot_benchmark_test_/boot_benchmark_test "$R/fixed-boot.test"
cp bazel-bin/runsc/boot/filter/filter_precompiled.go "$R/fixed-filter_precompiled.go"
"${B[@]}" test "${O[@]}" --test_strategy=standalone --spawn_strategy=local --test_sharding_strategy=disabled --test_timeout=900 --test_env=TEST_TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_env=TMPDIR=/mnt/m0-xfs/filestore-publish-tests '--test_filter=^(TestFilestoreSnapshot|TestExternalFilestore|TestCheckpointRestore$|TestCheckpointResume$|TestSplitFSCheckpointRestore$|TestMultiBundleSplitFSCheckpointRestore$)' --test_arg=-test.v --test_arg=-test.count=3 --test_arg=--test_platforms=systrap,kvm --test_output=errors //runsc/container:container_test > "$R/integration.log" 2>&1
cp bazel-testlogs/runsc/container/container_test/test.log "$R/integration-test.log"
python3 "$R/performance.py" > "$R/performance.log" 2>&1
echo FIXED_COMPLETE
