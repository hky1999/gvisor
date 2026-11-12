#!/bin/bash
set -euo pipefail
R=/root/filestore-reflink-precompile-20261112
S=/root/filestore-publish-20261111/source
cd "$S"
tar -xzf "$R/rebase-source.tar.gz"
python3 - <<'PY'
import pathlib,json,hashlib
r=pathlib.Path('/root/filestore-reflink-precompile-20261112')
for f in json.loads((r/'deleted-files.json').read_text()):
 p=pathlib.Path(f)
 assert not p.is_absolute() and '..' not in p.parts
 p.unlink(missing_ok=True)
m=json.loads((r/'source-manifest.json').read_text())
assert all(hashlib.sha256(pathlib.Path(f).read_bytes()).hexdigest()==h for f,h in m.items())
print('REBASED_SOURCE_VERIFIED',len(m),flush=True)
PY
B=(/root/filestore-rebase-20261105/bazel-9.3.1 --output_user_root=/root/filestore-rebase-20261105/bazel-cache --host_jvm_args=-Xmx4g)
O=(-c opt --jobs=16 --local_resources=memory=32000 --repository_cache=/root/codex-filestore-20261023/repository-cache --disk_cache=/root/codex-filestore-20261023/disk-cache --nostamp --workspace_status_command=/bin/true)
"${B[@]}" test "${O[@]}" --test_timeout=900 --test_arg=-test.v --test_output=errors //runsc/boot/filter/config:config_test //runsc/boot/filter:filter_test //runsc/config:config_test //pkg/sentry/pgalloc:pgalloc_test //pkg/sentry/control:control_test > "$R/rebase-unit.log" 2>&1
for target in runsc/boot/filter/config/config_test runsc/boot/filter/filter_test runsc/config/config_test pkg/sentry/pgalloc/pgalloc_test pkg/sentry/control/control_test; do
 cp "bazel-testlogs/$target/test.log" "$R/rebase-${target//\//-}.log"
done
"${B[@]}" build "${O[@]}" //:release > "$R/rebase-build.log" 2>&1
cp -aL bazel-bin/release "$R/rebased-release"
"${B[@]}" test "${O[@]}" --test_strategy=standalone --spawn_strategy=local --test_sharding_strategy=disabled --test_timeout=900 --test_env=TEST_TMPDIR=/mnt/m0-xfs/filestore-publish-tests --test_env=TMPDIR=/mnt/m0-xfs/filestore-publish-tests '--test_filter=^(TestFilestoreSnapshot|TestExternalFilestore|TestCheckpointRestore$|TestCheckpointResume$|TestSplitFSCheckpointRestore$|TestMultiBundleSplitFSCheckpointRestore$)' --test_arg=-test.v --test_arg=-test.count=3 --test_arg=--test_platforms=systrap,kvm --test_output=errors //runsc/container:container_test > "$R/rebase-integration.log" 2>&1
cp bazel-testlogs/runsc/container/container_test/test.log "$R/rebase-integration-test.log"
echo REBASE_VALIDATION_PASSED
