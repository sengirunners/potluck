#!/bin/sh
# box-cache-fill.sh: fill the owner's cache volume for boxes (potluck-cache; cmd/potluck-driver-tart/README.md,
# "A guarded uplink"). Run it on the lending machine as the user who runs the container CLI; run it again to add
# versions. Boxes made after it get the volume read-only at /opt/potluck-cache. Go modules are not this script's:
# the host fills goproxy/ itself, verified (potluck host shelf fill --go-sum, or boxes' wants; docs/shelf.md),
# with the newest toolchain this script added.
#
#   scripts/box-cache-fill.sh [--cli docker] [--network NAME] [--image IMAGE] [--go VERSION]...
#
#   --go VERSION   a Go toolchain (1.27.4), checked against go.dev's SHA-256, laid out as GitHub's tool cache is
#                  (tools/go/<version>/<x64|arm64>), so actions/setup-go finds it and downloads nothing
#   --network NAME the network the fill downloads on (default: the CLI's own). On a machine with a download cap,
#                  name the capped one (potluck-box) so the fill stays under it too; the fill then resolves names
#                  through 1.1.1.1 and 8.8.8.8, as boxes do (the network's filter drops the host's own resolver)
#   --image IMAGE  an image with sh, curl, tar, sha256sum and git (default buildpack-deps:bookworm-scm); run as root
#
# The fill runs in a plain container of yours with the volume writable: it is your own trusted work, not a box.
set -eu
cli=docker network= image=buildpack-deps:bookworm-scm gos=
while [ $# -gt 0 ]; do
  case "$1" in
    --cli) cli=$2 ;;
    --network) network=$2 ;;
    --image) image=$2 ;;
    --go) gos="$gos $2" ;;
    *) sed -n '2,17p' "$0" >&2; exit 2 ;;
  esac
  shift 2
done
[ -n "$gos" ] || { sed -n '2,17p' "$0" >&2; exit 2; }
"$cli" volume inspect potluck-cache >/dev/null 2>&1 || "$cli" volume create potluck-cache >/dev/null
exec "$cli" run --rm --user 0:0 ${network:+--network "$network" --dns 1.1.1.1 --dns 8.8.8.8} --mount type=volume,source=potluck-cache,target=/cache \
  --env "GOS=$gos" --entrypoint sh "$image" -euc '
umask 022
case $(uname -m) in x86_64) goarch=amd64 arch=x64 ;; aarch64|arm64) goarch=arm64 arch=arm64 ;; *) echo "no Go build for $(uname -m)" >&2; exit 1 ;; esac
mkdir -p /cache/tools/go
for v in $GOS; do
  d=/cache/tools/go/$v/$arch
  [ -e "$d.complete" ] && { echo "go $v: cached"; continue; }
  # Extracted on the volume under a name no box reads (.fill-*), renamed into place, and marked complete last:
  # a box started meanwhile sees no <arch>.complete and downloads its own.
  f=go$v.linux-$goarch.tar.gz; t=/cache/tools/go/.fill-$v-$arch
  rm -rf "$t" "$d.complete" "$d"; mkdir -p "$t" "${d%/*}"
  curl --proto =https -fsSL -o "$t/$f" "https://dl.google.com/go/$f"
  echo "$(curl --proto =https -fsSL "https://dl.google.com/go/$f.sha256")  $t/$f" | sha256sum -c - >/dev/null
  tar -xzf "$t/$f" -C "$t"
  chmod -R a+rX "$t/go"; mv "$t/go" "$d"; : >"$d.complete"
  rm -rf "$t"; echo "go $v: added"
done
chmod -R a+rX /cache/tools
du -sh /cache/tools'
