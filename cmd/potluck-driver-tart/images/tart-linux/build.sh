#!/bin/sh
# Builds Potluck's small Linux image for the Tart layer (imagepins/images.pin, the `tart linux ... name=small-linux` line):
# what `potluck try` lends on a Mac, so a first sandbox does not wait for the 27 GB macOS image. Run it on an
# Apple-silicon Mac with Tart; it boots one VM. GitHub's hosted runners have no nested virtualization, so no
# workflow builds it.
#
#   build.sh <registry>/<repository>:<tag> [tart push flags, such as --insecure]
#
# The base is Cirrus Labs' Debian image (github.com/cirruslabs/linux-image-templates; MIT), which has the Tart guest
# agent, sh, bash, awk, ps, nc, ip and ping but only perl-base and no git. This adds perl (the in-box exec helper,
# vmhost/exec.go, needs Time::HiRes and File::Temp) and git, and lowers the image's floor from Tart's Linux default
# (cpuCountMin 4, memorySizeMin 4 GiB, which no size fits twice in half of an 8-core Mac) to 1 vCPU and 2 GiB, the
# smallest size Potluck lends. Bump the base by its digest: ghcr.io/cirruslabs/debian:latest.
#
# The base's user admin has passwordless sudo, its password is admin, and sshd takes logins. The Tart layer refuses
# root in a Linux guest (its root would hold NET_ADMIN and NET_RAW in the VM; vmhost/oci_root.go), and the guest
# agent, not SSH, is how Potluck drives it, so this purges sudo and openssh-server and locks admin's and root's
# passwords. The
# base's cloud-init reads no data source, so a clone's boot does not put them back.
#
# Pushing needs registry credentials in TART_REGISTRY_USERNAME and TART_REGISTRY_PASSWORD (for ghcr.io, a token with
# write:packages). It prints the pushed image by digest: write that on images.pin's tart linux line.
set -eu

BASE=ghcr.io/cirruslabs/debian@sha256:e3dae8c6c653070318b620502abe84df9a90372a2bbd6026e0138b23045c411d
MIN_CPU=1
MIN_MEMORY=2147483648 # bytes: 2 GiB

[ $# -ge 1 ] || { echo "usage: build.sh <registry>/<repository>:<tag> [tart push flags]" >&2; exit 2; }
dest=$1
shift
case "${dest##*/}" in *@* | *:*:* | "") ok=no ;; *:?*) ok=yes ;; *) ok=no ;; esac
[ "$ok" = yes ] || { echo "build.sh: push to <registry>/<repository>:<tag>, not $dest" >&2; exit 2; }
repo=${dest%:*}
tag=${dest##*:}

vm=potluck-tart-linux-build-$$
tart_home=${TART_HOME:-$HOME/.tart}
config=$tart_home/vms/$vm/config.json
cleanup() {
	tart stop "$vm" >/dev/null 2>&1 || true
	tart delete "$vm" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

tart clone "$BASE" "$vm"
# The build needs the network (apt); Tart's default NAT gives it.
tart run --no-graphics --no-audio --no-clipboard "$vm" >/dev/null 2>&1 &
i=0
until tart exec "$vm" true >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 120 ] || { echo "build.sh: the guest agent did not answer in 4 minutes" >&2; exit 1; }
	sleep 2
done

# sudo goes last: this runs under it.
tart exec "$vm" sudo env DEBIAN_FRONTEND=noninteractive SUDO_FORCE_REMOVE=yes sh -eu -c '
apt-get update -q
apt-get install -y -q --no-install-recommends perl git
apt-get purge -y -q openssh-server
usermod -L admin
usermod -L root
for u in root admin; do
	case "$(passwd -S "$u" | cut -d" " -f2)" in L) ;; *) echo "build.sh: $u can log in with a password" >&2; exit 1 ;; esac
done
rm -f /etc/sudoers.d/90-cloud-init-users
apt-get purge -y -q sudo
apt-get autoremove -y -q --purge
apt-get clean
rm -rf /var/lib/apt/lists/*
fstrim -a || true'

# What a box needs (images.pin's header), and what it must not have, checked in the built guest.
tart exec "$vm" sh -eu -c '
for t in sh bash awk sleep cat kill ps perl git nc ip ping; do
	command -v "$t" >/dev/null || { echo "build.sh: the image has no $t" >&2; exit 1; }
done
perl -MTime::HiRes -MFile::Temp -MPOSIX -MIO::Select -MIO::Socket::INET -MIO::Socket::UNIX -MFcntl -e 1
[ "$(id -u)" != 0 ] || { echo "build.sh: the guest agent runs commands as root" >&2; exit 1; }
! command -v sudo >/dev/null || { echo "build.sh: sudo is still installed" >&2; exit 1; }
! nc -z -w 2 127.0.0.1 22 </dev/null || { echo "build.sh: something still listens on port 22" >&2; exit 1; }
[ "$(grep -h "^datasource_list" /etc/cloud/cloud.cfg.d/*.cfg 2>/dev/null)" = "datasource_list: [ None ]" ] ||
	{ echo "build.sh: cloud-init reads a data source: a clone could put users or sudo back" >&2; exit 1; }'

tart stop "$vm"
perl -0pi -e "s/\"cpuCountMin\"\\s*:\\s*\\d+/\"cpuCountMin\" : $MIN_CPU/; s/\"memorySizeMin\"\\s*:\\s*\\d+/\"memorySizeMin\" : $MIN_MEMORY/" "$config"
perl -0ne "exit !(/\"cpuCountMin\"\\s*:\\s*$MIN_CPU\\b/ && /\"memorySizeMin\"\\s*:\\s*$MIN_MEMORY\\b/)" "$config" ||
	{ echo "build.sh: could not lower the floor in $config" >&2; exit 1; }

# --populate-cache leaves the pushed image in Tart's cache, whose tag entry links to its digest.
tart push --populate-cache "$@" "$vm" "$dest"
digest=$(readlink "$tart_home/cache/OCIs/$(printf %s "$repo" | sed 's/:/%3A/')/$tag" || true)
digest=${digest##*/}
case "$digest" in sha256:*) echo "$repo@$digest" ;; *) echo "build.sh: pushed, but no digest in Tart's cache for $dest" >&2; exit 1 ;; esac
