#!/bin/bash
# Verify a pre-faulted read-only MAP_SHARED alias across first writes after restore.
# Run as root in a private mount namespace with a reflink-capable XFS scratch dir.
set -euo pipefail
runsc=$(realpath "$1")
scratch=$(mktemp -d "$2/alias.XXXXXXXX")
runtime=$(mktemp -d /tmp/runsc-alias.XXXXXXXX)
src=$(dirname "$0")
common=("$runsc" "--root=$runtime" --platform=systrap --network=none --ignore-cgroups
        --filestore-parent-mountns=true)
cleanup() {
    "${common[@]}" delete --force reader >/dev/null 2>&1 || true
    "${common[@]}" delete --force source >/dev/null 2>&1 || true
    if mountpoint -q "$runtime/null-netns"; then umount "$runtime/null-netns"; fi
    rm -rf "$runtime" "$scratch"
}
trap cleanup EXIT
mkdir -p "$scratch"/{live-a,live-b,image,bundle/rootfs}
cp "$(command -v busybox)" "$scratch/bundle/rootfs/busybox"
gcc -O2 -static "$src/workload.c" -o "$scratch/bundle/rootfs/workload"
cat > "$scratch/bundle/config.json" <<'JSON'
{
  "ociVersion": "1.0.2",
  "root": {"path": "rootfs"},
  "process": {"args": ["/busybox", "sleep", "600"], "cwd": "/", "user": {"uid": 0, "gid": 0}},
  "mounts": [{"destination": "/proc", "type": "proc", "source": "proc"}],
  "linux": {"namespaces": [{"type": "pid"}, {"type": "mount"}, {"type": "network"}, {"type": "ipc"}, {"type": "uts"}]}
}
JSON
a=("${common[@]}" "--overlay2=root:dir=$scratch/live-a,size=128M")
b=("${common[@]}" "--overlay2=root:dir=$scratch/live-b,size=128M"
   "--filestore-adopt-dir=$scratch/g0" --filestore-lazy-restore=lazy)
"${a[@]}" run --detach --bundle="$scratch/bundle" source </dev/null
"${a[@]}" exec source /workload seed
"${a[@]}" checkpoint --compression=none --skip-filestore-pages \
    --image-path="$scratch/image" --filestore-snapshot-dir="$scratch/g0" source
"${a[@]}" delete --force source
"${b[@]}" restore --detach --bundle="$scratch/bundle" --image-path="$scratch/image" reader </dev/null
"${b[@]}" exec reader /workload aliases
