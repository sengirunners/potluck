package vmhost

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The cloud-hypervisor layer's network (docs/cloudhv.md, "Network"). Each VM
// has one tap device on the host, a point-to-point /30 with the host at one
// end and the guest at the other. Nothing bridges two VMs. The host routes
// and masquerades what the fence lets through, and the fence is one nftables
// table, potluck_cloudhv, hooked before routing, where no other program's
// accept rule comes first. From a VM's tap it drops:
//
//   - every IPv6 packet;
//   - a source address that is not the VM's own link's (no spoofing);
//   - any address of this machine, and broadcast;
//   - the private, shared, loopback, link-local (cloud metadata), benchmark
//     and multicast ranges (the list in docs/network.md), which covers the
//     LAN, tailnets and every other VM's link;
//   - outbound mail (tcp 25, 465, 587, 2525).
//
// Nothing is forwarded to a VM but replies to what it started, and the
// kernel's record of what a link's last VM started is deleted before the
// link's next VM boots, so one VM's replies never reach another.
//
// Making a tap and loading a table need root, and the driver does not run as
// root. So those two steps are this file's helper: the driver re-executed as
// `potluck-driver-tart --cloudhv-net up|down ...` through `sudo -n`, which
// the host's owner allows for exactly that command (docs/cloudhv.md). It
// takes a tap name of the layer's own form, a user id and an address inside
// 10/8, and does nothing else. Boot refuses a VM whose helper call failed:
// without the fence the VM must not start.

var cloudhvTapName = regexp.MustCompile(`^pch[0-9a-f]{6}$`)

// cloudhvTapOf names the tap of the link whose guest address is guest: the
// address's low 24 bits, so the name and the address cannot be mixed.
func cloudhvTapOf(guest uint32) string { return fmt.Sprintf("pch%06x", guest&0xffffff) }

// cloudhvTapLetGo is how long a down waits for a tap to be let go of.
const cloudhvTapLetGo = 10 * time.Second

// cloudhvTapBusy says whether a process holds the tap open: a tap has a
// carrier only while one does.
func cloudhvTapBusy(tap string) bool {
	b, err := os.ReadFile("/sys/class/net/" + tap + "/carrier")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// cloudhvNetUp makes the VM's tap and loads the fence; cloudhvNetDown
// removes the tap. Tests replace them. held is the VM's network lock
// (cloudhvNetLock): the helper is given it and holds it until it ends.
var (
	cloudhvNetUp = func(v cloudhvVM, held *os.File) error {
		return cloudhvHelper(held, "up", v.Tap, strconv.Itoa(os.Getuid()), v.HostIP)
	}
	cloudhvNetDown = func(v cloudhvVM) {
		if cloudhvTapExists(v.Tap) {
			_ = cloudhvHelper(nil, "down", v.Tap)
		}
	}
)

// cloudhvNetLock takes the VM's network lock, waiting up to wait for it. The
// helper that makes the tap runs as root, so the driver cannot end it: a
// cancelled boot's helper can still be making the tap after the boot's own
// process is gone. The lock says when it is done. It is a lock on an open
// file, which the helper inherits as its standard input, so it is held until
// the last process of the helper has ended, whoever else died first. Whoever
// removes the tap takes the lock first, and so never looks for a tap that is
// about to appear. locked false: the wait ran out, or the file could not be
// opened (f is then nil).
func cloudhvNetLock(dir string, wait time.Duration) (f *os.File, locked bool) {
	f, err := os.OpenFile(filepath.Join(dir, "net.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	for end := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return f, true
		}
		if !time.Now().Before(end) {
			return f, false
		}
	}
}

// cloudhvNetWait is how long a boot or a stop waits for a helper still at
// work. Tests shorten it.
var cloudhvNetWait = 15 * time.Second

// cloudhvHelperCmd is the helper's command: the driver itself when it is
// already root, through sudo when not, with the network lock as its standard
// input.
func cloudhvHelperCmd(self string, root bool, held *os.File, args ...string) *exec.Cmd {
	a := append([]string{flagCloudHVNet}, args...)
	cmd := exec.Command(self, a...)
	if !root {
		cmd = exec.Command("sudo", append([]string{"-n", self}, a...)...)
	}
	if held != nil {
		cmd.Stdin = held
	}
	return cmd
}

func cloudhvHelper(held *os.File, args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := cloudhvHelperCmd(self, os.Geteuid() == 0, held, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "a password is required") || strings.Contains(msg, "not allowed") {
			msg = "sudo refused. Allow this one command for this user (a line in /etc/sudoers.d, see docs/cloudhv.md): " +
				self + " " + flagCloudHVNet + " *"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// cloudhvForbidden is docs/network.md's list: what no guest may reach.
var cloudhvForbidden = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "168.63.129.16", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4"}

// cloudhvFence is the nftables table, loaded whole in one transaction each
// time a VM boots, so a fence someone flushed is back before the next guest
// runs. It names no binding's range: every tap of the layer's is held to the
// same rules, and a packet's source must be an address routed to the tap it
// came in on, which is that VM's own link and nothing else.
func cloudhvFence() string {
	return `table inet potluck_cloudhv
delete table inet potluck_cloudhv
table inet potluck_cloudhv {
	set forbidden {
		type ipv4_addr; flags interval
		elements = { ` + strings.Join(cloudhvForbidden, ", ") + ` }
	}
	chain from_guest {
		type filter hook prerouting priority raw; policy accept;
		iifname "pch*" jump guest_rules
	}
	chain guest_rules {
		meta nfproto ipv6 drop
		fib saddr . iif oif missing drop
		fib daddr type { local, broadcast } drop
		ip daddr @forbidden drop
		tcp dport { 25, 465, 587, 2525 } drop
	}
	chain to_guest {
		type filter hook forward priority filter; policy accept;
		oifname "pch*" ct state established,related accept
		oifname "pch*" drop
	}
	chain guest_nat {
		type nat hook postrouting priority srcnat; policy accept;
		iifname "pch*" oifname != "pch*" masquerade
	}
}
`
}

// cloudhvNetArgs reads the helper's arguments: up <tap> <uid> <host address>,
// or down <tap>. The tap is one of the layer's own names, and the host's
// address the first of a /30 inside 10.0.0.0/8, so the helper can be allowed
// in sudoers for any arguments.
func cloudhvNetArgs(args []string) (op, tap string, uid int, host net.IP, err error) {
	usage := fmt.Errorf("usage: %s up <tap> <uid> <host address> | down <tap>", flagCloudHVNet)
	if len(args) < 2 || !cloudhvTapName.MatchString(args[1]) {
		return "", "", 0, nil, usage
	}
	switch {
	case args[0] == "down" && len(args) == 2:
		return "down", args[1], 0, nil, nil
	case args[0] == "up" && len(args) == 4:
	default:
		return "", "", 0, nil, usage
	}
	uid, err = strconv.Atoi(args[2])
	if err != nil || uid < 0 {
		return "", "", 0, nil, errors.New("bad user id")
	}
	host = net.ParseIP(args[3]).To4()
	if host == nil || host[0] != 10 || host[3]&3 != 1 {
		return "", "", 0, nil, errors.New("bad address: the host's end must be the first address of a /30 inside 10.0.0.0/8")
	}
	if args[1] != cloudhvTapOf(binary.BigEndian.Uint32(host)+1) {
		return "", "", 0, nil, errors.New("bad tap: a link's tap is named for its guest address")
	}
	return "up", args[1], uid, host, nil
}

// cloudhvNetHelper is the --cloudhv-net process (root). It prints nothing on
// success and one plain line on failure.
func cloudhvNetHelper(args []string) int {
	fail := func(f string, a ...any) int {
		fmt.Fprintf(os.Stderr, f+"\n", a...)
		return 1
	}
	op, tap, uid, host, err := cloudhvNetArgs(args)
	if err != nil {
		return fail("%v", err)
	}
	if os.Geteuid() != 0 {
		return fail("the network helper must run as root")
	}
	run := func(name string, a ...string) error {
		out, err := toolCmd(name, a...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(a, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// A tap a hypervisor holds open is a running VM's link. It is never
	// taken over or removed, whoever asks: two bindings given one range
	// would otherwise cut each other's VMs off.
	// A hypervisor that was just killed lets go of its tap a moment after
	// its process is seen gone, so a down waits for that.
	for end := time.Now().Add(cloudhvTapLetGo); op == "down" && cloudhvTapBusy(tap) && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
	if cloudhvTapBusy(tap) {
		return fail("%s is the link of a running VM. Two bindings on one host need two ranges (ch_net)", tap)
	}
	if op == "down" {
		if cloudhvTapExists(tap) {
			if err := run("ip", "link", "del", tap); err != nil && cloudhvTapExists(tap) {
				return fail("%v", err)
			}
		}
		return 0
	}
	guest := make(net.IP, 4)
	copy(guest, host)
	guest[3]++
	// The link must be this layer's alone: an address this host already
	// routes somewhere (its LAN, a VPN) would be taken from there.
	routes, err := toolCmd("ip", "-4", "route", "show", "match", guest.String()).Output()
	if err != nil {
		return fail("ip route: %v", err)
	}
	if clash := cloudhvRouteClash(string(routes), tap); clash != "" {
		return fail("%s is already routed on this host (%s): give the binding another ch_net", guest, clash)
	}
	if _, ok := cloudhvTool("conntrack"); !ok {
		return fail("conntrack is not installed (the conntrack package): it clears what a link's last VM left before the next VM uses it")
	}
	// The fence first: a tap never exists without it.
	nft := toolCmd("nft", "-f", "-")
	nft.Stdin = strings.NewReader(cloudhvFence())
	if out, err := nft.CombinedOutput(); err != nil {
		return fail("nft: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if !cloudhvTapExists(tap) {
		if err := run("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", strconv.Itoa(uid)); err != nil {
			return fail("%v", err)
		}
	}
	// A link's last VM may have left connections in the kernel's table,
	// which would pass their far end's packets on to this VM.
	for _, dir := range []string{"-s", "-d"} {
		out, err := toolCmd("conntrack", "-D", dir, guest.String()).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "flow entries have been deleted") { // it exits 1 when there were none
			return fail("conntrack: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+tap+"/disable_ipv6", []byte("1\n"), 0o644)
	// Answer ARP on the tap for the tap's own address only: by default a
	// host answers for every address it has, and a guest could list them.
	if err := os.WriteFile("/proc/sys/net/ipv4/conf/"+tap+"/arp_ignore", []byte("1\n"), 0o644); err != nil {
		return fail("%v", err)
	}
	for _, a := range [][]string{
		{"addr", "replace", host.String() + "/30", "dev", tap},
		{"link", "set", "dev", tap, "up"},
	} {
		if err := run("ip", a...); err != nil {
			return fail("%v", err)
		}
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fail("could not turn on forwarding: %v", err)
	}
	// Docker sets the forward chain's policy to drop; DOCKER-USER is where
	// it asks others to put their accepts. The fence above has already
	// dropped what a VM may not reach.
	if _, ok := cloudhvTool("iptables"); ok && toolCmd("iptables", "-w", "-n", "-L", "DOCKER-USER").Run() == nil {
		for _, r := range [][]string{
			{"DOCKER-USER", "-i", "pch+", "-j", "ACCEPT"},
			{"DOCKER-USER", "-o", "pch+", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		} {
			if toolCmd("iptables", append([]string{"-w", "-C"}, r...)...).Run() != nil {
				if err := run("iptables", append([]string{"-w", "-I"}, r...)...); err != nil {
					return fail("%v", err)
				}
			}
		}
	}
	return 0
}

// cloudhvRouteClash reads `ip route show match <address>`: the first route,
// other than the default one and the tap's own, that already covers the
// address. Empty: none.
func cloudhvRouteClash(routes, tap string) string {
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] == "default" {
			continue
		}
		own := false
		for i := 0; i+1 < len(f); i++ {
			own = own || (f[i] == "dev" && f[i+1] == tap)
		}
		if !own {
			return strings.Join(f, " ")
		}
	}
	return ""
}

// cloudhvTool finds a system tool where root's tools live. The helper runs
// as root for another user, so it never looks a tool up in PATH, and its
// tools get an environment of its own making.
func cloudhvTool(name string) (string, bool) {
	for _, d := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		if st, err := os.Stat(d + "/" + name); err == nil && st.Mode().IsRegular() {
			return d + "/" + name, true
		}
	}
	return "", false
}

func toolCmd(name string, a ...string) *exec.Cmd {
	path, ok := cloudhvTool(name)
	if !ok {
		path = "/usr/sbin/" + name // not there: the command fails saying so
	}
	cmd := exec.Command(path, a...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	return cmd
}
