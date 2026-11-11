#!/bin/bash
set -euo pipefail
R=/root/filestore-publish-20261111
B=(/root/filestore-rebase-20261105/bazel-9.3.1 --output_user_root=/root/filestore-rebase-20261105/bazel-cache --host_jvm_args=-Xmx4g)
O=(-c opt --jobs=16 --local_resources=memory=32000 --repository_cache=/root/codex-filestore-20261023/repository-cache --disk_cache=/root/codex-filestore-20261023/disk-cache --nostamp --workspace_status_command=/bin/true)
mkdir -p /mnt/m0-xfs/filestore-publish-tests
chmod 777 /mnt/m0-xfs/filestore-publish-tests
cd "$R/source"
"${B[@]}" test "${O[@]}" --test_output=errors //pkg/sentry/pgalloc:pgalloc_test //pkg/sentry/control:control_test //runsc/boot:boot_test //runsc/boot/filter/config:config_test > "$R/unit.log" 2>&1
"${B[@]}" test "${O[@]}" --config=race --linkopt=-fuse-ld=bfd --host_linkopt=-fuse-ld=bfd --test_output=errors //pkg/sentry/pgalloc:pgalloc_test //pkg/sentry/control:control_test > "$R/race.log" 2>&1
TMPDIR=/mnt/m0-xfs/filestore-publish-tests bazel-bin/pkg/sentry/pgalloc/pgalloc_test_/pgalloc_test -test.run='TestFilestoreSnapshot|TestOrdinaryCheckpoint|TestDestroyAfterLoadFailsBeforeMmap' -test.v -test.count=10 > "$R/xfs-race.log" 2>&1
"${B[@]}" build "${O[@]}" //:release //pkg/sentry/pgalloc:pgalloc_test //runsc/sandbox:sandbox_test > "$R/candidate-build.log" 2>&1
cp -aL bazel-bin/release "$R/final-measured-release"
TMPDIR=/mnt/m0-xfs/filestore-publish-tests bazel-bin/pkg/sentry/pgalloc/pgalloc_test_/pgalloc_test -test.run='TestFilestoreSnapshot|TestOrdinaryCheckpoint|TestDestroyAfterLoadFailsBeforeMmap' -test.v -test.count=10 > "$R/xfs-unit.log" 2>&1
"${B[@]}" test "${O[@]}" --test_strategy=standalone --spawn_strategy=local --test_sharding_strategy=disabled --test_timeout=600 --test_env=TEST_TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_env=TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_filter='^(TestExternalFilestore|TestCheckpointRestore$|TestCheckpointResume$|TestSplitFSCheckpointRestore$|TestMultiBundleSplitFSCheckpointRestore$)' --test_arg=-test.v --test_arg=-test.count=3 --test_arg=--test_platforms=systrap,kvm --test_output=errors //runsc/container:container_test > "$R/integration.log" 2>&1
cp bazel-testlogs/runsc/container/container_test/test.log "$R/container-test.log"
cp bazel-testlogs/runsc/container/container_test/test.xml "$R/container-test.xml"
set +e
TMPDIR=/mnt/m0-xfs/filestore-publish-tests bazel-bin/runsc/sandbox/sandbox_test_/sandbox_test -test.v > "$R/candidate-sandbox.log" 2>&1
printf '%s\n' "$?" > "$R/candidate-sandbox.exit"
set -e
cd "$R/upstream"
"${B[@]}" build "${O[@]}" //:release //pkg/sentry/pgalloc:pgalloc_test //runsc/sandbox:sandbox_test > "$R/upstream-build.log" 2>&1
cp -aL bazel-bin/release "$R/upstream-release"
set +e
TMPDIR=/mnt/m0-xfs/filestore-publish-tests bazel-bin/runsc/sandbox/sandbox_test_/sandbox_test -test.v > "$R/upstream-sandbox.log" 2>&1
printf '%s\n' "$?" > "$R/upstream-sandbox.exit"
set -e
python3 "$R/run-performance.py" > "$R/performance.log" 2>&1
printf 'VALIDATION_AND_BENCHMARK_COMPLETE\n'
