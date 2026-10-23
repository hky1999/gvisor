#!/usr/bin/env bash
set -Eeuo pipefail
export PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
out=/root/qualification
mkdir -p "$out"
share="$out/share"
tar -xzf "$share/sandboxd-source.tar.gz" -C /root
# Match the deployed node prerequisite; the distribution kernel defaults to 0.
sysctl -w user.max_user_namespaces=65536
python3 - <<'PY' > /root/qualification/environment.json
import hashlib,json,os,pathlib,subprocess
assert os.uname().release=='5.10.134-19.8.al8.x86_64'
assert not pathlib.Path('/sys/fs/cgroup/cgroup.controllers').exists()
print(json.dumps({'kernel':os.uname().release,'cgroup':'v1','proc_cgroup':pathlib.Path('/proc/self/cgroup').read_text(),'cmdline':pathlib.Path('/proc/cmdline').read_text(),'xfs_info':subprocess.check_output(['xfs_info','/home/akernel'],text=True),'runsc_version':subprocess.check_output(['runsc','--version'],text=True),'runsc_sha256':hashlib.sha256(pathlib.Path('/usr/local/bin/runsc').read_bytes()).hexdigest(),'sandboxd_sha256':hashlib.sha256(pathlib.Path('/usr/local/bin/sandboxd').read_bytes()).hexdigest(),'kvm_device':pathlib.Path('/dev/kvm').exists()},indent=2))
PY
printf 'Verified production-version kernel, cgroup-v1 and XFS\n'
systemctl start docker
docker info --format '{{.CgroupVersion}} {{.KernelVersion}}' | tee "$out/docker-environment.txt"
test "$(docker info --format '{{.CgroupVersion}}')" = 1
if ! docker image inspect codex-sandboxd-qualification:20261023 >/dev/null 2>&1; then
    tar --numeric-owner --one-file-system --exclude='./proc' --exclude='./sys' --exclude='./dev' --exclude='./run' --exclude='./tmp' --exclude='./home' --exclude='./root' --exclude='./var/lib/docker' --exclude='./var/lib/containerd' --exclude='./var/log' -C / -cf - . |
        docker import --change 'ENTRYPOINT ["/usr/local/bin/sandboxd-e2e-run"]' - codex-sandboxd-qualification:20261023
fi
# Stage the tested daemon and the exact checkout's fixture script together.
mkdir -p "$out/image"
cp /usr/local/bin/sandboxd "$out/image/"
cp /root/sandboxd/test/e2e/e2e-run.sh "$out/image/sandboxd-e2e-run"
cat > "$out/image/Dockerfile" <<'DOCKER'
FROM codex-sandboxd-qualification:20261023
COPY sandboxd /usr/local/bin/sandboxd
COPY sandboxd-e2e-run /usr/local/bin/sandboxd-e2e-run
RUN mkdir -p /tmp /root /run /proc /sys /dev /home && chmod 1777 /tmp
DOCKER
docker build --network=none -t codex-sandboxd-qualification:20261023 "$out/image"
docker load -i "$share/redis-image.tar.gz" | tee "$out/redis-load.log"
redis_image_id="$(sed -n 's/^Loaded image ID: //p' "$out/redis-load.log" | tail -1)"
test -n "$redis_image_id"
docker tag "$redis_image_id" codex-redis-qualification:20261023
cd /root/sandboxd
mkdir -p output
for name in sandboxd sbox runc-shim sandbox-logger firecracker-agent oom-hog network-policy-client checkpoint-restore; do
    ln -sf "/usr/local/bin/$name" "output/$name"
done
printf 'Test fixtures and complete candidate bundle prepared\n'
modprobe erofs
modprobe tun
modprobe br_netfilter
modprobe xt_physdev
modprobe nf_conntrack_netlink
modprobe ip_set_hash_ip
status=0
for profile in systrap kvm reflink; do
    platform="$profile"
    mode=""
    disabled=0
    if [ "$profile" = reflink ]; then platform=systrap; mode=reflink; disabled=1; fi
    if [ "$profile" = systrap ]; then disabled=1; fi
    printf 'Starting full runsc %s regression\n' "$profile"
    if RUN_UNIT_TESTS=0 E2E_RUNTIME=runsc E2E_RUNSC_PLATFORM="$platform" E2E_RUNSC_CHECKPOINT_MODE="$mode" \
        E2E_SKIP_BUILD=1 E2E_NETWORK_SOAK=1 E2E_RUN_CGROUP_DISABLED="$disabled" \
        E2E_HOME_FIXTURE_PARENT=/home/akernel E2E_REDIS_IMAGE=codex-redis-qualification:20261023 \
        SANDBOXD_E2E_IMAGE=codex-sandboxd-qualification:20261023 \
        SANDBOXD_E2E_CONTAINER="codex-runsc-$profile" \
        bash test/e2e/run.sh > "$out/runsc-$profile.log" 2>&1; then
        printf 'PASS runsc %s\n' "$profile" | tee "$out/runsc-$profile.status"
    else
        printf 'FAIL runsc %s\n' "$profile" | tee "$out/runsc-$profile.status"
        status=1
    fi
done
printf 'Full runsc regression complete: status=%s\n' "$status"
exit "$status"
