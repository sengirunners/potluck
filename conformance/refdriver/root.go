package refdriver

// Root and Docker in a box (SPEC 20 "Boxes", Root and Docker;
// zm-eakh.12.5.26): this driver's boxes are host directories, so it lists
// root and docker and puts a stand-in sudo and docker first on each exec's
// PATH, with a Bug switch for each way a driver can get them wrong. They
// stand in for what conformance asks; they grant nothing.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// rootServed: this driver's boxes list root (bug no-root: they have none;
// root-unlisted: sudo works, unlisted).
func (d *drv) rootServed() bool { return d.cfg["kind"] == "box" && !d.is("no-root", "root-unlisted") }

// guestVM: config guest_vm=on makes this driver's boxes stand for whole VMs
// of their own (capability root-vm), as the cloud-hypervisor layer's with
// root=on: each has a link and a fence of its own (guestStandIns). Config
// guest_link is the address of this machine that the boxes' listeners answer
// on; without it a box's link has no address.
func (d *drv) guestVM() bool { return d.cfg["guest_vm"] == "on" }

// guestFenceBugs are the ways a whole-VM stand-in's fence can fail, by bug:
// what the stand-in nc lets out that a fence on the host would drop.
var guestFenceBugs = map[string]string{
	"root-readdress-past-fence": "src",        // a packet from an address that is not the box's own
	"root-onlink-past-fence":    "onlink",     // a target root routed straight onto its link
	"root-neighbour-reached":    "neigh",      // a target root named a neighbour on its link
	"root-fence-in-guest":       "unfiltered", // anything, once root emptied the guest's own filter
	"root-cuts-neighbour-off":   "cut",        // nothing: a box whose address another took loses its network
}

// guestStandIns are a whole-VM box's link and fence. The box is this
// machine, whose own link is not a guest's to change, so an ip takes root's
// changes into a file of the box's (its HOME's .zmref-link) and an nc reads
// it and does what a fence on the host would: a connection from an address
// that is not the box's own, or to a target root put on the box's link, does
// not open. Each bug lets one of those out, so a check fails here only when
// its probe really made the change the bug is about. Each box has its own
// address (198.51.100.N); another box's address reaches this machine at
// guest_link, where every box's listener answers. An nft and iptables record
// that root emptied the guest's filter and touch nothing, and a sysctl gives
// each box a boot id and a kernel setting of its own.
func (d *drv) guestStandIns() map[string]string {
	refuse := map[bool]string{true: `echo "RTNETLINK answers: Operation not permitted" >&2; exit 2; `}[d.is("root-vm-cannot-readdress")]
	head := `#!/bin/sh
d=${0%/*}; st=$HOME/.zmref-link
own() {
	[ -s "$HOME/.zmref-addr" ] || {
		n=2; mkdir -p "$d/links"
		while ! mkdir "$d/links/$n" 2>/dev/null; do n=$((n + 1)); done
		(set -C; echo "198.51.100.$n" >"$HOME/.zmref-addr") 2>/dev/null
	}
	cat "$HOME/.zmref-addr"
}
`
	shown := map[bool]string{true: `echo "7: zmref0    inet $(own)/24 scope global zmref0"`, false: ":"}[d.cfg["guest_link"] != ""]
	ip := head + `drop() { [ -f "$st" ] || return 0; grep -v "$1" "$st" >"$st.new"; mv "$st.new" "$st"; }
case "$*" in
"-4 route show default") echo "default via 198.51.100.1 dev zmref0" ;;
"-4 -o addr show dev zmref0 scope global") ` + shown + ` ;;
"-o link show dev zmref0") echo "7: zmref0: <BROADCAST,UP> mtu 1500 state UP\\    link/ether 02:00:00:00:00:07 brd ff:ff:ff:ff:ff:ff" ;;
"addr replace "*) ` + refuse + `a=${3%/*}; drop "^addr $a\$"; echo "addr $a" >>"$st"; [ "$a" = "$(own)" ] || echo "$a" >>"$d/taken" ;;
"addr del "*) a=${3%/*}; drop "^addr $a\$" ;;
"route replace "*)
	` + refuse + `h=${3%/*}; shift 3; via=-; src=-
	while [ $# -gt 1 ]; do case "$1" in via) via=$2 ;; src) src=$2 ;; esac; shift; done
	drop "^route $h "; echo "route $h $via $src" >>"$st" ;;
"route del "*) drop "^route ${3%/*} " ;;
"neigh replace "*) ` + refuse + `drop "^neigh $3 "; echo "neigh $3 $5" >>"$st" ;;
"neigh del "*) drop "^neigh $3 " ;;
*) PATH=/usr/sbin:/usr/bin:/sbin:/bin exec ip "$@" ;;
esac
`
	nc := head + `next() { PATH=${PATH#"$d":}; exec nc "$@"; }
outside() { PATH=/usr/bin:/bin:/usr/sbin:/sbin; exec nc "$@"; }
for a; do case "$a" in -l | -k | -lk | -kl) next "$@" ;; esac; done
[ $# -ge 2 ] || next "$@"
h=; p=; for a; do h=$p; p=$a; done
# how the packet leaves, by what root made of the box's link
how=plain
r=$(grep "^route $h " "$st" 2>/dev/null | tail -n 1)
if [ -n "$r" ]; then
	via=$(echo "$r" | cut -d" " -f3); src=$(echo "$r" | cut -d" " -f4)
	if [ "$src" != - ] && [ "$src" != "$(own)" ]; then how=src
	elif [ "$via" = - ]; then how=onlink; if grep -q "^neigh $h " "$st" 2>/dev/null; then how=neigh; fi
	fi
fi
real=$h; case "$h" in 198.51.100.*) real=` + shQuote(d.cfg["guest_link"]) + ` ;; esac
n=$#; i=1; while [ "$i" -le $((n - 2)) ]; do set -- "$@" "$1"; shift; i=$((i + 1)); done; shift 2; set -- "$@" "$real" "$p"
bugs=" ` + guestFenceBugs[d.bug] + ` "
if grep -q "^filter off" "$st" 2>/dev/null; then case "$bugs" in *" unfiltered "*) outside "$@" ;; esac; fi
case "$bugs" in *" cut "*) if grep -qx "$(own)" "$d/taken" 2>/dev/null; then exit 1; fi ;; esac
case "$bugs" in *" $how "*) outside "$@" ;; esac
[ "$how" = plain ] || exit 1
next "$@"
`
	// The box's kernel: a boot id of its own, and a kernel setting kept in
	// a file of its own (its HOME's .zmref-kset), which root's sysctl -w
	// writes. root-vm-shared-kernel: one file of settings for every box, as
	// containers on one kernel have, each with a boot id of its own;
	// root-vm-kernel-read-only: the same, and root cannot write it;
	// root-vm-host-kernel: every box reads this machine's boot id.
	id := `printf '%s' "$HOME" | cksum | awk '{ print "zmref-" $1 }'`
	set := `$HOME/.zmref-kset`
	switch {
	case d.is("root-vm-host-kernel"):
		id = "cat /proc/sys/kernel/random/boot_id"
	case d.is("root-vm-shared-kernel"), d.is("root-vm-kernel-read-only"):
		set = `$d/kset`
	}
	write := map[bool]string{true: `echo "sysctl: permission denied on key" >&2; exit 255`, false: `echo "${2#*=}" >"$f"`}[d.is("root-vm-kernel-read-only")]
	sysctl := `#!/bin/sh
d=${0%/*}; f="` + set + `"
case "$*" in
"-n kernel.random.boot_id") ` + id + ` ;;
"-n kernel.printk_ratelimit_burst") cat "$f" 2>/dev/null || echo 10 ;;
"-qw kernel.printk_ratelimit_burst="*) ` + write + ` ;;
*) PATH=/usr/sbin:/usr/bin:/sbin:/bin exec sysctl "$@" ;;
esac
`
	return map[string]string{"ip": ip, "nc": nc, "sysctl": sysctl}
}

// shQuote is s as one word of a shell script.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// rootDescribe adds capabilities root, root-vm and docker, as the bug says.
func (d *drv) rootDescribe(caps []string) []string {
	switch {
	case !d.rootServed():
		return caps
	case d.is("docker-without-root"):
		return append(caps, "docker")
	case d.is("root-vm-without-root"):
		return append(caps, "root-vm")
	}
	caps = append(caps, "root")
	if d.guestVM() {
		caps = append(caps, "root-vm")
	}
	if d.is("no-docker") {
		return caps
	}
	return append(caps, "docker")
}

// rootPath is the stand-ins' directory, written once per driver state.
func (d *drv) rootPath() (string, error) {
	dir := filepath.Join(d.state, "rootbin")
	bnd := map[string]string{"root-net-admin": "00000000a00415fb", "root-net-raw": "00000000a00425fb", "root-mknod": "00000000a80405fb"}[d.bug]
	if bnd == "" {
		bnd = "00000000a00405fb" // Docker's default set less NET_RAW and MKNOD, as zm-run-tart's root box
	}
	refuse := d.is("root-no-sudo", "no-root")
	// root-past-fence: root's commands run without the guest firewall
	// (Firewall, on the test's PATH), as root in a VM whose only fence is
	// inside the guest could take it down.
	outside := "PATH=" + dir + ":/usr/bin:/bin:/usr/sbin:/sbin; export PATH\n"
	past := map[bool]string{true: outside}[d.is("root-past-fence")]
	sudo := `#!/bin/sh
[ "$1" = -n ] && shift
` + past + map[bool]string{true: `echo "sudo: a password is required" >&2; exit 1
`}[refuse] + `case "$1" in
id) echo 0 ;;
grep) printf 'CapBnd:\t%s\n' ` + bnd + ` ;;
chown) touch "$3.root-owned" ;; # root's now (secrets.go): this machine's user cannot chown
rm) for a; do last=$a; done; rm -f "$last.root-owned"; exec "$@" ;;
*) exec "$@" ;;
esac
`
	docker := `#!/bin/sh
case "$1" in
info) ` + map[bool]string{true: `echo "Cannot connect to the Docker daemon" >&2; exit 1`, false: "exit 0"}[d.is("docker-down")] + ` ;;
import) cat >/dev/null; echo sha256:0 ;;
run) ` + map[bool]string{true: `echo "docker: Error response from daemon" >&2; exit 125`, false: `echo "tini version 0.19.0"`}[d.is("docker-run-fails")] + ` ;;
rmi) exit 0 ;;
*) exit 2 ;;
esac
`
	init := "#!/bin/sh\necho tini version 0.19.0\n"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	stand := map[string]string{"sudo": sudo, "docker": docker, "docker-init": init, "stat": d.statStandIn(), "cat": d.catStandIn()}
	// The box's kernel (uname -s): Linux, whatever this machine runs, as the
	// guest the stand-in bounding set describes; config guest_os=darwin, a
	// macOS guest, as a Tart box with root=on (zm-eakh.12.54.14).
	kern := map[bool]string{true: "Darwin", false: "Linux"}[d.cfg["guest_os"] == "darwin"]
	stand["uname"] = "#!/bin/sh\nif [ \"$1\" = -s ]; then echo " + kern + "; exit 0; fi\nPATH=/usr/bin:/bin exec uname \"$@\"\n"
	// What empties a packet filter never reaches this machine's own: the
	// probe's unfilter runs these, and a test may run as root.
	record := "#!/bin/sh\nif [ \"$*\" = \"flush ruleset\" ]; then echo \"filter off\" >>\"$HOME/.zmref-link\"; fi\n"
	stand["nft"], stand["iptables"], stand["ip6tables"] = record, "#!/bin/sh\n", "#!/bin/sh\n"
	guest := d.guestStandIns()
	for n, s := range guest {
		if d.guestVM() {
			stand[n] = s
		} else {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
	if d.is("box-secrets-wrong-owner") { // commands run as a user other than the one owning the box's secrets
		stand["id"] = "#!/bin/sh\nif [ \"$1\" = -u ]; then echo $(( $(/usr/bin/id -u) + 1 )); exit 0; fi\nexec /usr/bin/id \"$@\"\n"
	} else {
		_ = os.Remove(filepath.Join(dir, "id"))
	}
	for n, s := range stand {
		p := filepath.Join(dir, n) // a temp file renamed in: another exec may be running it
		tmp := p + ".tmp-" + strconv.Itoa(os.Getpid())
		if err := os.WriteFile(tmp, []byte(s), 0o700); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, p); err != nil {
			return "", err
		}
	}
	return dir, nil
}
