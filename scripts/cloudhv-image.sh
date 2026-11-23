#!/bin/sh
# Build a box image for the cloud-hypervisor VM layer (docs/cloudhv.md):
# Ubuntu 24.04's cloud image with potluck-guest installed and started at
# boot, Docker installed and not started, and everything that would configure
# the network or wait for it turned off (potluck-guest sets the guest's
# address itself).
#
#   scripts/cloudhv-image.sh <out.raw> <potluck-guest> <firmware> [disk size, default 20G] [agent]
#
# Needs cloud-hypervisor, qemu-img, genisoimage, curl and KVM, and no root. The
# image is built by booting it once with no network card, so nothing is
# installed from inside it: what the image has is what Ubuntu's cloud image
# has (sh, git, python3, curl, perl, sudo, iptables), potluck-guest, and
# Docker's static programs (the release pinned below, downloaded here and
# checked against its checksum). Every command runs as one ordinary account,
# box. The image gives box no sudo and starts no Docker: potluck-guest does
# both, in a box whose binding has root=on and docker=on.
#
# With agent as the fifth argument the image also has what a coding agent
# runs: the packages, the tools and the pinned versions of the agent template
# for Docker boxes, all read from that template's file
# (cmd/potluck-driver-tart/images/agent/Dockerfile). That needs Docker here
# too, and so an account that may use it (agent_tools, below).
set -eu
[ $# -ge 3 ] || { echo "usage: $0 <out.raw> <potluck-guest binary> <firmware (CLOUDHV.fd)> [size] [agent]" >&2; exit 2; }
out=$1 guest=$2 fw=$3 size=${4:-20G} template=${5:-}
case $size in [0-9]*) ;; *) echo "the fourth argument is the disk's size (20G), not $size" >&2; exit 2 ;; esac
[ -z "$template" ] || [ "$template" = agent ] || { echo "the fifth argument is agent or nothing, not $template" >&2; exit 2; }
[ -z "$template" ] || docker info >/dev/null 2>&1 || { echo "agent needs Docker here, running, and an account that may use it" >&2; exit 1; }
ch=${CLOUD_HYPERVISOR:-cloud-hypervisor}
base=https://cloud-images.ubuntu.com/noble/current
work=$(mktemp -d "${TMPDIR:-/tmp}/potluck-cloudhv.XXXXXX")
id=${work##*.} # this build's own name for what it makes in Docker
built=      # the Docker image agent_tools makes, to remove with its container
trap 'trap "" HUP INT TERM # a second Ctrl-C does not stop the cleaning up
  [ -z "$built" ] || { docker rm -f "potluck-cloudhv-image-$id"; docker rmi -f "$built"; } >/dev/null 2>&1 || true
  rm -rf "$work" "$out.part"' EXIT
trap 'exit 1' HUP INT TERM # so that a build that is cancelled cleans up too

cache=${XDG_CACHE_HOME:-$HOME/.cache}/potluck-cloudhv
mkdir -p "$cache"

# fetch <file> <url> <sha256> leaves the download in the cache under file,
# checked against the checksum.
fetch() {
  [ -n "$3" ] || { echo "there is no checksum to check $1 against" >&2; exit 1; }
  [ -f "$cache/$1" ] || { curl --proto =https -fsSL --retry 2 -o "$cache/$1.part" "$2" && mv "$cache/$1.part" "$cache/$1"; } || {
    echo "$2 could not be downloaded" >&2; exit 1; }
  [ "$(sha256sum "$cache/$1" | awk '{ print $1 }')" = "$3" ] || { echo "the cached $1 does not match its checksum; delete $cache/$1 and run again" >&2; exit 1; }
}

# ubuntu <file> fetches one file of Ubuntu's current cloud image, checked
# against Ubuntu's published checksum of it.
sums=$(curl --proto =https -fsSL "$base/SHA256SUMS")
ubuntu() {
  fetch "$1" "$base/$1" "$(printf '%s\n' "$sums" | awk -v f="*$1" '$2 == f { print $1 }')"
}
ubuntu noble-server-cloudimg-amd64.img
img=$cache/noble-server-cloudimg-amd64.img

# Docker's static release: dockerd, containerd, runc, the docker client and
# docker-init. To move to another release, change both lines.
docker_version=29.9.3
docker_sha256=3fb5d1f13014c18cc54945bc177c58a9056a482dada031e17d12cf2e6922c973
fetch "docker-$docker_version.tgz" "https://download.docker.com/linux/static/stable/x86_64/docker-$docker_version.tgz" "$docker_sha256"
docker=$cache/docker-$docker_version.tgz

qemu-img convert -O raw "$img" "$work/disk.raw"
qemu-img resize -q -f raw "$work/disk.raw" "$size"

mkdir "$work/seed"
cp "$guest" "$work/seed/potluck-guest"
cp "$docker" "$work/seed/docker.tgz"

# agent_tools puts what the agent template adds on the seed disk, for the
# build boot to install with no network: the Ubuntu packages as .deb files,
# and everything else as one archive of /usr/local. Both are made in a
# container of Ubuntu's own root archive of the same cloud image, so apt
# there chooses the packages against what that archive has, which the image
# has too, with a little more. The container has the network; the image
# never does.
agent_tools() {
  df=$(dirname "$0")/../cmd/potluck-driver-tart/images/agent/Dockerfile
  pin() {
    v=$(sed -n "s/^ARG $1=//p" "$df" | tr -d '"')
    [ -n "$v" ] || { echo "$df sets no $1" >&2; exit 1; }
    printf '%s' "$v"
  }
  node=$(pin NODE_VERSION)
  gh=$(pin GH_VERSION)
  go=$(pin GO_VERSION)
  claude=$(pin CLAUDE_CODE_VERSION)
  codex=$(pin CODEX_VERSION)
  packages=$(pin AGENT_PACKAGES)
  tools=$(pin AGENT_TOOLS)
  fetch "node-v$node-linux-x64.tar.xz" "https://nodejs.org/dist/v$node/node-v$node-linux-x64.tar.xz" "$(pin NODE_SHA256_AMD64)"
  fetch "gh_${gh}_linux_amd64.tar.gz" "https://github.com/cli/cli/releases/download/v$gh/gh_${gh}_linux_amd64.tar.gz" "$(pin GH_SHA256_AMD64)"
  fetch "go$go.linux-amd64.tar.gz" "https://go.dev/dl/go$go.linux-amd64.tar.gz" "$(pin GO_SHA256_AMD64)"
  ubuntu noble-server-cloudimg-amd64-root.tar.xz
  built=potluck-cloudhv-image:$id
  docker import "$cache/noble-server-cloudimg-amd64-root.tar.xz" "$built" >/dev/null
  # The container is given a directory of its own to write its two files into, not the seed's: npm runs the
  # packages' own install scripts in it. Go's programs are linked into /usr/local/bin because that, and not Go's
  # own directory, is on the PATH of a box's commands. --init is what lets a Ctrl-C end the container.
  mkdir "$work/out"
  docker run --rm --init --name "potluck-cloudhv-image-$id" -v "$cache:/cache:ro" -v "$work/out:/out" -e DEBIAN_FRONTEND=noninteractive "$built" sh -euc "
    apt-get update -q
    apt-get install -y -q --download-only --no-install-recommends $packages
    (cd /var/cache/apt/archives && tar -cf /out/debs.tar *.deb)
    tar -xJf /cache/node-v$node-linux-x64.tar.xz -C /usr/local --strip-components=1 --no-same-owner
    tar -xzf /cache/gh_${gh}_linux_amd64.tar.gz -C /tmp
    install -m 0755 /tmp/gh_${gh}_linux_amd64/bin/gh /usr/local/bin/gh
    tar -xzf /cache/go$go.linux-amd64.tar.gz -C /usr/local --no-same-owner
    ln -s ../go/bin/go ../go/bin/gofmt /usr/local/bin/
    corepack enable
    npm install -g --no-fund --no-audit @anthropic-ai/claude-code@$claude @openai/codex@$codex
    tar -cf /out/local.tar -C /usr/local .
    chmod a+r /out/local.tar /out/debs.tar
  " >"$work/agent.log" 2>&1 || { echo "the agent tools could not be fetched:" >&2; tail -30 "$work/agent.log" >&2; exit 1; }
  mv "$work/out/debs.tar" "$work/out/local.tar" "$work/seed/"
  # Run in the build boot, from the seed disk. apt installs the packages, so a set that does not fit the image
  # fails the build, and nothing the image has is removed for them. The template turns the agent programs'
  # updater off in its commands' environment; a box's commands get a fixed one from potluck-guest, so here
  # Claude Code's own settings file for the whole machine does it. The programs are tried as box in a home that
  # is then removed, so that what they write at a first run (an id, a counter) is not in every box of the image.
  cat > "$work/seed/template.sh" <<TEMPLATE
#!/bin/sh
set -eu
export DEBIAN_FRONTEND=noninteractive
mkdir /tmp/debs
tar -xf /mnt/seed/debs.tar -C /tmp/debs
apt-get install -y -q --no-remove --no-install-recommends /tmp/debs/*.deb
apt-get check -q
rm -rf /tmp/debs
tar -xf /mnt/seed/local.tar -C /usr/local
mkdir -p /etc/claude-code
echo '{"env":{"DISABLE_AUTOUPDATER":"1"}}' > /etc/claude-code/managed-settings.json
for t in git $tools; do
  command -v "\$t" >/dev/null || { echo "the image lacks \$t"; exit 1; }
done
h=\$(mktemp -d)
chown box "\$h"
su box -c "export HOME=\$h && cd && python3 -m venv --help >/dev/null && claude --version && codex --version && node --version && go version && gh --version"
rm -rf "\$h"
printf '\npotluck-image: the agent tools are installed\n'
TEMPLATE
  seed="$seed debs.tar local.tar template.sh"
}
seed="user-data meta-data network-config potluck-guest potluck-guest.service docker.tgz docker.service"
[ -z "$template" ] || agent_tools
# Not enabled: potluck-guest starts it in a box whose binding has docker=on.
cat > "$work/seed/docker.service" <<'EOF'
[Unit]
Description=Docker (started by potluck-guest where the box's binding has docker=on)
After=local-fs.target basic.target

[Service]
Type=notify
ExecStart=/usr/local/bin/dockerd --group docker
Restart=on-failure
RestartSec=2
Delegate=yes
KillMode=process
OOMScoreAdjust=-500
LimitNOFILE=1048576
LimitNPROC=infinity
LimitCORE=infinity
TasksMax=infinity
EOF
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
  - set -e
  - mkdir -p /mnt/seed
  - mount -o ro /dev/disk/by-label/cidata /mnt/seed
  - install -m 0755 /mnt/seed/potluck-guest /usr/local/bin/potluck-guest
  - install -m 0644 /mnt/seed/potluck-guest.service /etc/systemd/system/potluck-guest.service
  - tar -xzf /mnt/seed/docker.tgz -C /usr/local/bin --strip-components=1 --no-same-owner
  - install -m 0644 /mnt/seed/docker.service /etc/systemd/system/docker.service
  - groupadd --system docker
  - useradd --create-home --shell /bin/bash --uid 1000 --groups docker box
  - '[ ! -f /mnt/seed/template.sh ] || sh /mnt/seed/template.sh > /dev/ttyS0 2>&1'
  - umount /mnt/seed
  - printf '127.0.1.1 box\n' >> /etc/hosts
  - systemctl enable potluck-guest.service
  - rm -f /etc/netplan/*.yaml
  - systemctl mask systemd-networkd-wait-online.service systemd-networkd.service systemd-networkd.socket systemd-resolved.service
  - systemctl disable snapd.service snapd.socket snapd.seeded.service unattended-upgrades.service apt-daily.timer apt-daily-upgrade.timer motd-news.timer || true
  - rm -f /etc/resolv.conf
  - printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf
  - touch /etc/cloud/cloud-init.disabled
  - truncate -s 0 /etc/machine-id
  - sync
  - 'printf "\npotluck-image: the image is set up\n" > /dev/ttyS0'
power_state: {mode: poweroff, timeout: 60, condition: true}
EOF
(cd "$work/seed" && genisoimage -quiet -output ../seed.iso -volid cidata -joliet -rock $seed)

# A build boot that did not set the image up leaves no image. Its console is kept beside where the image would
# be, and what cloud-init and the commands said on it is shown, with what the hypervisor itself said.
boot_failed() {
  echo "$1" >&2
  [ ! -s "$work/ch.log" ] || { echo "cloud-hypervisor said:" >&2; tail -5 "$work/ch.log" >&2; }
  [ -s "$work/console.log" ] || exit 1
  mkdir -p "$(dirname "$out")"
  cp "$work/console.log" "$out.console.log"
  echo "the boot's console is $out.console.log, and says:" >&2
  tr -d '\r' < "$work/console.log" | grep -a -v "$(printf '\033')" | grep -a 'cloud-init\[\|^[^[<]' | grep -a -v '^ssh-\|^ecdsa-\|\]: [|+]' | tail -20 >&2
  exit 1
}
echo "booting the image once to set it up (a few minutes)" >&2
timeout --foreground 900 "$ch" --kernel "$fw" \
  --disk "path=$work/disk.raw,image_type=raw" "path=$work/seed.iso,readonly=on,image_type=raw" \
  --cpus boot=2 --memory size=2G --serial "file=$work/console.log" --console off >"$work/ch.log" 2>&1 ||
  boot_failed "the build boot failed, or took more than fifteen minutes"
# The boot's commands stop at the first that fails (set -e, above), and the last of them says this, on a line
# of its own; the agent tools' script says its own line when it is through.
{ grep -q "^potluck-image: the image is set up" "$work/console.log" &&
  { [ -z "$template" ] || grep -q "^potluck-image: the agent tools are installed" "$work/console.log"; }; } ||
  boot_failed "the build boot did not finish setting the image up"
mkdir -p "$(dirname "$out")"
rm -f "$out.console.log"
cp --sparse=always "$work/disk.raw" "$out.part"
mv "$out.part" "$out"
chmod 0444 "$out"
sum=$(sha256sum "$out" | awk '{ print $1 }')
echo "$sum" > "$out.sha256"
echo "built $out" >&2
echo "pin it in the binding as: $(basename "$out" .raw)@sha256:$sum" >&2
