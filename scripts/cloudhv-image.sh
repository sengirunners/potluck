#!/bin/sh
# Build a box image for the cloud-hypervisor VM layer (docs/cloudhv.md):
# Ubuntu 24.04's cloud image with potluck-guest installed and started at
# boot, and everything that would configure the network or wait for it
# turned off (potluck-guest sets the guest's address itself).
#
#   scripts/cloudhv-image.sh <out.raw> <potluck-guest> <firmware> [disk size, default 20G]
#
# Needs cloud-hypervisor, qemu-img, genisoimage, curl and KVM. No root. The
# image is built by booting it once with no network card, so the build
# installs nothing from the internet: what the image has is what Ubuntu's
# cloud image has (sh, git, python3, curl, perl) plus potluck-guest, and one
# ordinary account, box, with no sudo, which every command runs as.
set -eu
[ $# -ge 3 ] || { echo "usage: $0 <out.raw> <potluck-guest binary> <firmware (CLOUDHV.fd)> [size]" >&2; exit 2; }
out=$1 guest=$2 fw=$3 size=${4:-20G}
ch=${CLOUD_HYPERVISOR:-cloud-hypervisor}
base=https://cloud-images.ubuntu.com/noble/current
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cache=${XDG_CACHE_HOME:-$HOME/.cache}/potluck-cloudhv
mkdir -p "$cache"
img=$cache/noble-server-cloudimg-amd64.img
[ -f "$img" ] || { curl -fsSL --retry 2 -o "$img.part" "$base/noble-server-cloudimg-amd64.img" && mv "$img.part" "$img"; }
want=$(curl -fsSL "$base/SHA256SUMS" | awk '$2 == "*noble-server-cloudimg-amd64.img" { print $1 }')
have=$(sha256sum "$img" | awk '{ print $1 }')
[ -n "$want" ] && [ "$want" = "$have" ] || { echo "the cached Ubuntu image does not match Ubuntu's checksum; delete $img and run again" >&2; exit 1; }

qemu-img convert -O raw "$img" "$work/disk.raw"
qemu-img resize -q -f raw "$work/disk.raw" "$size"

mkdir "$work/seed"
cp "$guest" "$work/seed/potluck-guest"
cat > "$work/seed/potluck-guest.service" <<'EOF'
[Unit]
Description=Potluck guest agent (answers the host on vsock)
DefaultDependencies=no
After=systemd-modules-load.service local-fs.target
Before=basic.target shutdown.target
Conflicts=shutdown.target

[Service]
ExecStartPre=-/sbin/modprobe vmw_vsock_virtio_transport
ExecStart=/usr/local/bin/potluck-guest
Restart=always
RestartSec=1
OOMScoreAdjust=-900

[Install]
WantedBy=sysinit.target
EOF
printf 'instance-id: potluck-cloudhv-build\nlocal-hostname: box\n' > "$work/seed/meta-data"
printf 'version: 2\nethernets: {}\n' > "$work/seed/network-config"
cat > "$work/seed/user-data" <<'EOF'
#cloud-config
users: []
growpart: {mode: auto, devices: ['/']}
resize_rootfs: true
runcmd:
  - mkdir -p /mnt/seed
  - mount -o ro /dev/disk/by-label/cidata /mnt/seed
  - install -m 0755 /mnt/seed/potluck-guest /usr/local/bin/potluck-guest
  - install -m 0644 /mnt/seed/potluck-guest.service /etc/systemd/system/potluck-guest.service
  - umount /mnt/seed
  - useradd --create-home --shell /bin/bash --uid 1000 box
  - systemctl enable potluck-guest.service
  - rm -f /etc/netplan/*.yaml
  - systemctl mask systemd-networkd-wait-online.service systemd-networkd.service systemd-networkd.socket systemd-resolved.service
  - systemctl disable snapd.service snapd.socket snapd.seeded.service unattended-upgrades.service apt-daily.timer apt-daily-upgrade.timer motd-news.timer || true
  - rm -f /etc/resolv.conf
  - printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf
  - touch /etc/cloud/cloud-init.disabled
  - truncate -s 0 /etc/machine-id
  - sync
power_state: {mode: poweroff, timeout: 60, condition: true}
EOF
(cd "$work/seed" && genisoimage -quiet -output ../seed.iso -volid cidata -joliet -rock user-data meta-data network-config potluck-guest potluck-guest.service)

echo "booting the image once to set it up (about a minute)" >&2
timeout 900 "$ch" --kernel "$fw" \
  --disk "path=$work/disk.raw,image_type=raw" "path=$work/seed.iso,readonly=on,image_type=raw" \
  --cpus boot=2 --memory size=2G --serial "file=$work/console.log" --console off >"$work/ch.log" 2>&1 || {
    echo "the build boot failed or timed out; console:" >&2; tail -30 "$work/console.log" >&2 || true; exit 1; }
mkdir -p "$(dirname "$out")"
cp --sparse=always "$work/disk.raw" "$out.part"
mv "$out.part" "$out"
chmod 0444 "$out"
sum=$(sha256sum "$out" | awk '{ print $1 }')
echo "$sum" > "$out.sha256"
echo "built $out" >&2
echo "pin it in the binding as: $(basename "$out" .raw)@sha256:$sum" >&2
