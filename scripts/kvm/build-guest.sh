#!/usr/bin/env bash
# Build the read-only guest root filesystem and stage the guest kernel.
# Run as root on the benchmark host after stage-assets.sh.
set -euo pipefail
stage=${KVMEXP_STAGE:-/var/tmp/iris-kvm}
tar=ubuntu-base-24.04.4-base-amd64.tar.gz
tar_sha=c1e67ef7b17a6300e136118bd1dc04725009cb376c1aad10abcf8cd453628d58
cd "$stage"
mkdir -p assets images
if [[ ! -f assets/$tar ]]; then
  curl -fsSL -o "assets/$tar.part" "https://cdimage.ubuntu.com/ubuntu-base/releases/24.04/release/$tar"
  mv "assets/$tar.part" "assets/$tar"
fi
echo "$tar_sha  assets/$tar" | sha256sum -c --quiet

root=$stage/guest-root
rm -rf "$root"
mkdir "$root"
tar -xzf "assets/$tar" -C "$root"
# The compute fixture is a dynamically linked C++ binary; ubuntu-base ships
# the same noble libstdc++ as the host, but verify instead of assuming.
for lib in libstdc++.so.6 libgcc_s.so.1 libm.so.6 libc.so.6; do
  [[ -e $root/usr/lib/x86_64-linux-gnu/$lib ]] || { echo "guest lacks $lib" >&2; exit 1; }
done
for tool in bash mount seq date head tail tr sleep sync cat; do
  chroot "$root" /usr/bin/env PATH=/usr/sbin:/usr/bin:/sbin:/bin which "$tool" > /dev/null \
    || { echo "guest lacks $tool" >&2; exit 1; }
done
mkdir -p "$root/opt/kvmexp"
install -m 0755 assets/judger-bench "$root/opt/kvmexp/judger-bench"
install -m 0755 assets/libjudger.so "$root/opt/kvmexp/libjudger.so"
cp -a workloads "$root/opt/kvmexp/workloads"
install -m 0755 scripts/guest-init.sh "$root/opt/kvmexp/init"

rm -f images/rootfs.img
mkfs.ext4 -q -F -L kvmexp -d "$root" images/rootfs.img 1G
install -m 0600 "/boot/vmlinuz-$(uname -r)" images/vmlinuz
uname -r > images/kernel-release
(cd images && sha256sum rootfs.img vmlinuz > SHA256SUMS)
cat images/SHA256SUMS
