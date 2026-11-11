#!/bin/bash
set -euo pipefail
R=/root/filestore-mount-identity-20261111
cd /root/filestore-publish-20261111/source
B=(/root/filestore-rebase-20261105/bazel-9.3.1 --output_user_root=/root/filestore-rebase-20261105/bazel-cache --host_jvm_args=-Xmx4g)
O=(-c opt --jobs=16 --local_resources=memory=32000 --repository_cache=/root/codex-filestore-20261023/repository-cache --disk_cache=/root/codex-filestore-20261023/disk-cache --nostamp --workspace_status_command=/bin/true)
"${B[@]}" test "${O[@]}" --test_strategy=standalone --spawn_strategy=local --test_sharding_strategy=disabled --test_timeout=900 --test_env=TEST_TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_env=TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_filter='^(TestFilestoreSnapshot|TestExternalFilestore|TestCheckpointRestore$|TestCheckpointResume$|TestSplitFSCheckpointRestore$|TestMultiBundleSplitFSCheckpointRestore$)' --test_arg=-test.v --test_arg=-test.count=3 --test_arg=--test_platforms=systrap,kvm --test_output=errors //runsc/container:container_test > "$R/integration.log" 2>&1
cp bazel-testlogs/runsc/container/container_test/test.log "$R/integration-test.log"
cp bazel-testlogs/runsc/container/container_test/test.xml "$R/integration-test.xml"
"${B[@]}" test "${O[@]}" --test_output=errors //runsc/specutils:specutils_test //runsc/boot:boot_test //pkg/sentry/pgalloc:pgalloc_test //pkg/sentry/control:control_test //runsc/boot/filter/config:config_test > "$R/unit.log" 2>&1
"${B[@]}" test "${O[@]}" --config=race --linkopt=-fuse-ld=bfd --host_linkopt=-fuse-ld=bfd --test_strategy=standalone --spawn_strategy=local --test_sharding_strategy=disabled --test_timeout=900 --test_env=TEST_TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_env=TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_filter='^(TestFilestoreSnapshot|TestExternalFilestore)' --test_arg=-test.v --test_arg=--test_platforms=systrap --test_output=errors //runsc/container:container_test > "$R/race.log" 2>&1
cp bazel-testlogs/runsc/container/container_test/test.log "$R/race-test.log"
"${B[@]}" build "${O[@]}" //:release //runsc/container:boot_benchmark_test > "$R/build.log" 2>&1
cp -aL bazel-bin/release "$R/fixed-release"
cp bazel-bin/runsc/container/boot_benchmark_test_/boot_benchmark_test "$R/fixed-boot.test"
echo VALIDATION_COMPLETE
