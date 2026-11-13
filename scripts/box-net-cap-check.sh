#!/bin/sh
# box-net-cap-check.sh: a manual check that the download cap on Potluck's box network holds (potluck host
# --net-mbit; cmd/potluck-driver-tart/README.md, "A guarded uplink"). Run it on the lending machine. It downloads
# URL in a container on the network, as a box would, and prints the container's rate beside what the machine's
# uplink interface received meanwhile. It changes nothing.
#
#   scripts/box-net-cap-check.sh [IMAGE]     (an image with curl 7.62+; default curlimages/curl)
#   env: CLI=docker RUNTIME=runsc NETWORK=potluck-box BRIDGE=br-potluckbox (a machine still on the old names: zm-box, br-zmbox) URL=<a file of 50 MB or more>
set -eu
cli=${CLI:-docker} net=${NETWORK:-potluck-box} br=${BRIDGE:-br-potluckbox} image=${1:-curlimages/curl}
url=${URL:-https://dl.google.com/go/go1.27.3.linux-amd64.tar.gz}
cap=$(tc qdisc show dev "$br" | sed -n 's/.* tbf .*rate \([0-9]*\)Mbit.*/\1/p')
echo "cap on $br: ${cap:-none} Mbit/s"
up=$(ip -4 route show default | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -1)
rx() { cat "/sys/class/net/$up/statistics/rx_bytes"; }
b0=$(rx); t0=$(date +%s)
# DNS over HTTPS: under gVisor or Kata the CLI's own resolver on a named network does not answer (oci_dns).
speed=$("$cli" run --rm ${RUNTIME:+--runtime "$RUNTIME"} --network "$net" --entrypoint curl "$image" \
  --doh-url https://1.1.1.1/dns-query -fsSL -o /dev/null -w '%{speed_download}' "$url")
b1=$(rx); t1=$(date +%s); dt=$((t1 - t0)); [ "$dt" -gt 0 ] || dt=1
box=$(awk -v s="$speed" 'BEGIN { printf "%.1f", s * 8 / 1e6 }')
host=$(awk -v b="$((b1 - b0))" -v t="$dt" 'BEGIN { printf "%.1f", b * 8 / t / 1e6 }')
echo "the container downloaded at $box Mbit/s; $up received $host Mbit/s over those $dt s (all traffic, image pull included)"
[ -z "$cap" ] || awk -v b="$box" -v c="$cap" 'BEGIN { if (b > c * 1.05) { print "FAIL: over the cap"; exit 1 } print "PASS: under the cap" }'
