#!/bin/sh
# potluck-box-guard NETWORK BRIDGE TABLE [--once]: per-port anti-spoofing on a
# Potluck box bridge (hostprobe/boxguard.go). Root runs it from the network's
# guard unit: with --once (ExecStartPre) it loads the tables and the set and
# exits, so the unit is active only once both are in place; without, it keeps
# them. It runs only sh, nft, nsenter, ip, awk, docker and coreutils, never
# Potluck or zm.
#
# Every frame a box's port sends must carry that box's own MAC and IPv4 (an
# ARP frame: its Ethernet source and its sender MAC and IPv4 too), no frame is
# forwarded from one box port to another, and every other ethertype (IPv6, VLAN
# tags) is dropped. Link-local frames (01:80:c2:00:00:0x) skip the bridge's
# prerouting: the bridge learns no address from them (no_linklocal_learn),
# those its input hook sees meet the same rules there, and nothing reaches the
# host's own IP or ARP stack on any port of the bridge (the kernel hands such
# frames up on the port's device, not on the bridge). The allowed (port, MAC, IPv4) set
# comes from Docker's own record of the network (the address it gave each
# box), never from the box. A box not yet in the set sends nothing: the guard
# fails closed.
set -eu
net=${1:?network} br=${2:?bridge} tbl=${3:?nft table} mode=${4:-}
for b in nft nsenter ip awk docker timeout; do
	command -v "$b" >/dev/null || { echo "potluck-box-guard: $b not found" >&2; exit 1; }
done

# load (re)applies the tables in one transaction: the chains are rewritten,
# the sets kept, so a box already admitted keeps its traffic.
load() {
	ip link set "$br" type bridge no_linklocal_learn 1
	nft -f - <<EOF
add table bridge $tbl
add set bridge $tbl boxes { type ifname . ether_addr . ipv4_addr; }
add chain bridge $tbl from_box { type filter hook prerouting priority -300; policy accept; }
add chain bridge $tbl to_host { type filter hook input priority -300; policy accept; }
add chain bridge $tbl between_boxes { type filter hook forward priority -300; policy accept; }
flush chain bridge $tbl from_box
flush chain bridge $tbl to_host
flush chain bridge $tbl between_boxes
add rule bridge $tbl from_box meta ibrname != "$br" accept
add rule bridge $tbl from_box ether type ip iifname . ether saddr . ip saddr @boxes accept
add rule bridge $tbl from_box ether type arp arp htype 1 arp ptype ip arp hlen 6 arp plen 4 iifname . ether saddr . arp saddr ip @boxes iifname . arp saddr ether . arp saddr ip @boxes accept
add rule bridge $tbl from_box counter drop
add rule bridge $tbl to_host meta ibrname != "$br" accept
add rule bridge $tbl to_host ether type ip iifname . ether saddr . ip saddr @boxes accept
add rule bridge $tbl to_host ether type arp arp htype 1 arp ptype ip arp hlen 6 arp plen 4 iifname . ether saddr . arp saddr ip @boxes iifname . arp saddr ether . arp saddr ip @boxes accept
add rule bridge $tbl to_host counter drop
add rule bridge $tbl between_boxes meta ibrname "$br" counter drop
add table inet $tbl
add set inet $tbl ports { type ifname; }
add chain inet $tbl on_port { type filter hook prerouting priority -400; policy accept; }
flush chain inet $tbl on_port
add rule inet $tbl on_port iifname @ports counter drop
add table arp $tbl
add set arp $tbl ports { type ifname; }
add chain arp $tbl on_port { type filter hook input priority -400; policy accept; }
flush chain arp $tbl on_port
add rule arp $tbl on_port iifname @ports counter drop
EOF
}

# element ID prints the set element "port . mac . ipv4" of container ID on
# the network, or nothing: the box's interface with the MAC Docker gave it,
# and that interface's host-side peer, which must be a port of the bridge.
# It fails (2) when Docker does not answer for ID, which is not the same as
# ID having no port here.
element() {
	info=$(timeout 10 docker inspect -f '{{with index .NetworkSettings.Networks "'"$net"'"}}{{.MacAddress}} {{.IPAddress}}{{end}} {{.NetworkSettings.SandboxKey}}' "$1" 2>/dev/null) || return 2
	echo "$info" | {
		read -r mac ip key || exit 0
		[ -n "$mac" ] && [ -n "$ip" ] && [ -n "$key" ] || exit 0
		peer=$(nsenter --net="$key" ip -o link 2>/dev/null | awk -v m="$mac" 'index($0, "link/ether " m " ") {split($2, a, "@if"); sub(":", "", a[2]); print a[2]; exit}')
		[ -n "$peer" ] || exit 0
		port=$(ip -o link 2>/dev/null | awk -F': ' -v i="$peer" '$1 == i {split($2, a, "@"); print a[1]; exit}')
		[ -n "$port" ] && [ "$(basename "$(readlink "/sys/class/net/$port/master")")" = "$br" ] || exit 0
		printf '%s . %s . %s\n' "$port" "$mac" "$ip"
	} | grep -E '^[A-Za-z0-9_.-]{1,15} \. ([0-9a-f]{2}:){5}[0-9a-f]{2} \. ([0-9]{1,3}\.){3}[0-9]{1,3}$' || :
}

# ports prints every port of the bridge, admitted or not: the host-stack
# drops cover a box the guard has not (yet) admitted too.
ports() {
	ip -o link show master "$br" 2>/dev/null | awk -F': ' '{split($2, a, "@"); print a[1]}' | grep -E '^[A-Za-z0-9_.-]{1,15}$' || :
}

# write sets the sets from file $1 in one transaction: with "replace", the
# boxes become exactly those (a box that left loses its entry); with "add",
# they are added to what is there (a round where Docker did not answer for
# some box removes nobody). The ports set is always every port of the bridge.
write() {
	{
		[ "$2" = replace ] && echo "flush set bridge $tbl boxes"
		echo "flush set inet $tbl ports"
		echo "flush set arp $tbl ports"
		[ -s "$1" ] && printf 'add element bridge %s boxes { %s }\n' "$tbl" "$(paste -sd, "$1")"
		p=$(ports | sort -u | paste -sd, -)
		if [ -n "$p" ]; then
			printf 'add element inet %s ports { %s }\n' "$tbl" "$p"
			printf 'add element arp %s ports { %s }\n' "$tbl" "$p"
		fi
		:
	} | nft -f -
}

# resync rebuilds the sets from Docker. When Docker does not answer for the
# network, the sets stay as they are; when it does not answer for a box, the
# boxes it found are added and none removed this round.
resync() {
	ids=$(timeout 20 docker network inspect "$net" --format '{{range $k, $v := .Containers}}{{$k}} {{end}}') || return 0
	: >"$tmp"
	how=replace
	for id in $ids; do
		element "$id" >>"$tmp" || how=add
	done
	write "$tmp" "$how"
}

# admit adds one box at once (a box just connected), with the bridge's
# ports; resync otherwise.
admit() {
	e=$(element "$1") && [ -n "$e" ] || { resync; return; }
	printf '%s\n' "$e" >"$tmp"
	write "$tmp" add || resync
}

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
trap 'exit 143' INT TERM
load
resync
[ "$mode" = --once ] && exit 0
while :; do
	since=$(date +%s)
	# The tables again (a flushed ruleset comes back within one round), and
	# the sets from Docker: a box leaving, and anything missed, at least
	# every 15 s; a box joining at once.
	load
	resync
	timeout 15 docker events --since "$since" --filter type=network --filter network="$net" \
		--filter event=connect --filter event=disconnect --format '{{.Action}} {{index .Actor.Attributes "container"}}' |
		while read -r action id; do
			case $action in
			connect) admit "$id" ;;
			*) resync ;;
			esac
		done || :
done
