package hostprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Potluck's filtered network for boxes that serve GitHub runners (zm-eakh.12.24.6).
// An oci box has no network by default, which is right for a job (its code
// comes in as a bundle) and useless for a runner, which must reach GitHub.
// Docker cannot filter a network's egress itself, so the filter is iptables
// rules on the network's bridge, kept by a systemd oneshot unit: doctor prints
// the one root command that writes, enables and starts it, and share uses the
// network for runner boxes once it exists as Potluck makes it and the unit is
// active (the driver checks the unit again before every box on it). The unit
// runs only sh, iptables, ip6tables, sysctl and docker, never Potluck (the
// binary is often the user's own file, which root must not run at boot).
//
// The names: a machine set up now gets the docker network potluck-box on the
// bridge br-potluckbox, kept by potluck-box-net.service. A machine set up
// under zm has zm-box, br-zmbox and zm-box-net.service, with live boxes on
// them: detectBoxNet keeps reading those names (same subnet, same filter, so
// the two cannot both be up) and never makes the new ones beside them, so
// that lender keeps working until its owner rolls the names (docs/network.md).
//
// Why each piece:
//   - The drops are in the raw table's PREROUTING, the first hook a packet
//     from the bridge meets, where nothing ACCEPTs ahead of them: a filter
//     chain's rule can be outrun by another daemon's jump inserted above it
//     (tailscaled puts ts-forward and ts-input at position 1 of FORWARD and
//     INPUT whenever it starts, and ts-forward ends "-o tailscale0 -j ACCEPT";
//     tailscale util/linuxfw/iptables_runner.go), and Docker re-asserts its
//     DOCKER-USER jump only when dockerd starts. "-m addrtype --dst-type
//     LOCAL" stands for every address of this machine, BROADCAST for its
//     subnets' broadcast addresses.
//   - iptables rules are kernel state, gone at reboot, so the unit re-applies
//     them: WantedBy=docker.service starts it with Docker at every boot,
//     After= orders it once Docker is up, PartOf= re-runs it when Docker
//     restarts. Each rule is checked (-C) and inserted only when missing, so a
//     re-run never opens a gap or doubles one; -w waits for the xtables lock
//     (other units take it at boot). The network is made last, so a unit that
//     failed part way leaves no new network behind, and zm-run-tart starts no
//     box on it unless the unit is active. What removes the rules while the
//     unit stays active goes unseen, since zm cannot read them without root:
//     a firewalld reload on firewalld's iptables backend, an "nft flush
//     ruleset", an "iptables -t raw -F". Restart the unit after one.
//   - The network is IPv4-only (no --ipv6). Docker then disables IPv6 on each
//     container's interface on it (moby osl/interface_linux.go: "IPv6 must be
//     enabled on the interface if and only if the network is IPv6-enabled"),
//     but the bridge keeps a kernel link-local address and gVisor brings its
//     own network stack: so where the host has IPv6 at all (/proc/sys/net/ipv6),
//     the bridge's IPv6 is disabled and ip6tables drops every IPv6 packet from
//     it, both required.
//   - icc off: no box reaches another on the bridge. The IPv4 drops: outbound
//     mail (tcp 25, 465, 587, 2525) anywhere, and this machine, private,
//     CGNAT (a tailnet), link-local (metadata), Azure's wireserver, the IETF
//     and benchmarking blocks, multicast and reserved destinations.
//
// Conformance still decides: share binds a runner box on the network only
// when its net checks pass (GitHub reached; this machine's addresses, its
// gateway and a tailnet address forbidden; outbound mail refused against a
// server that opens from here).
const (
	BoxNet       = "potluck-box"
	BoxNetBridge = "br-potluckbox"
	BoxNetSubnet = "10.199.0.0/24"
	BoxNetUnit   = "potluck-box-net.service"
	// LegacyBoxNet is the network zm made, still read where it exists.
	LegacyBoxNet = "zm-box"
	// DefaultNetMail is the outside mail server conformance tries from a box
	// on Potluck's network when the owner names none: a submission server most
	// networks reach on 587, so the mail check proves something.
	DefaultNetMail = "smtp.gmail.com"
	// RunnerNetAllow is what a runner box must reach: GitHub.
	RunnerNetAllow = "github.com:443"
)

// boxNetNames are the three names a filtered network goes by on a machine:
// the docker network, its bridge and the unit that keeps its filter.
type boxNetNames struct{ Net, Bridge, Unit string }

var (
	// currentBoxNet is what a machine set up now gets.
	currentBoxNet = boxNetNames{BoxNet, BoxNetBridge, BoxNetUnit}
	// legacyBoxNet is what zm made: read where it exists, never made anew.
	legacyBoxNet = boxNetNames{LegacyBoxNet, "br-zmbox", "zm-box-net.service"}
)

// IsBoxNet says whether a docker network name is Potluck's own filtered
// network, under its current or its old name.
func IsBoxNet(name string) bool { return name == BoxNet || name == LegacyBoxNet }

// boxNetOf is the names layer l's network goes by: the old ones where that is
// what the machine has (Layer.NetworkLegacy), else the current.
func boxNetOf(l Layer) boxNetNames {
	if l.NetworkLegacy {
		return legacyBoxNet
	}
	return currentBoxNet
}

// drops are the IPv4 filter, rules of the raw table's PREROUTING.
func (n boxNetNames) drops() []string {
	b := "-i " + n.Bridge
	rs := []string{
		b + " -p tcp -m multiport --dports 25,465,587,2525 -j DROP",
		b + " -m addrtype --dst-type LOCAL -j DROP",
		b + " -m addrtype --dst-type BROADCAST -j DROP",
	}
	for _, d := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "168.63.129.16/32", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4"} {
		rs = append(rs, b+" -d "+d+" -j DROP")
	}
	return rs
}

// create is how the network is made; the unit file and doctor's check both
// read it.
func (n boxNetNames) create() map[string]string {
	return map[string]string{"com.docker.network.bridge.name": n.Bridge, "com.docker.network.bridge.enable_icc": "false"}
}

// BoxNetUnitFile is the systemd unit that installs the filter and makes the
// network (/etc/systemd/system/potluck-box-net.service). systemd finds each
// command on its own search path.
func BoxNetUnitFile() []string { return currentBoxNet.unit(0) }

// unit is the unit with boxes' downloads capped at mbit Mbit/s (0: not
// capped; --net-mbit, zm-eakh.12.43). The cap is for a machine whose owner
// guards its uplink: a job's setup (a toolchain, a restored cache) downloads
// hundreds of MB at line rate. It is a token bucket on the bridge, whose
// transmit side is every packet routed to a box, so all boxes on the network
// share it, whatever they run and whatever their runtime (the bridge is
// outside gVisor's and Kata's guests), and a fair queue under it, so one
// box's download does not starve another's. Uploads are not capped. Made
// after the network (the bridge must exist); "replace" makes a re-run (Docker
// restarting) set the same two again.
func (n boxNetNames) unit(mbit int) []string {
	lines := []string{
		"[Unit]",
		"Description=Potluck box network " + n.Net + ": egress to the internet only (potluck host doctor)",
		"After=docker.service",
		"Requires=docker.service",
		"PartOf=docker.service",
		"[Service]",
		"Type=oneshot",
		"RemainAfterExit=yes",
	}
	// Check, then insert only what is missing: a re-run (PartOf= restarts the
	// unit with Docker) never removes a rule, so the filter has no gap, and
	// never doubles one. One address per rule, so -C checks exactly it.
	for _, r := range n.drops() {
		lines = append(lines, `ExecStart=sh -c "iptables -w -t raw -C PREROUTING `+r+` || iptables -w -t raw -I PREROUTING 1 `+r+`"`)
	}
	v6 := "-i " + n.Bridge + " -j DROP"
	create := n.create()
	opts := make([]string, 0, len(create))
	for k, v := range create {
		opts = append(opts, "-o "+k+"="+v)
	}
	sort.Strings(opts)
	lines = append(lines,
		`ExecStart=sh -c "test ! -d /proc/sys/net/ipv6 || ip6tables -w -t raw -C PREROUTING `+v6+` || ip6tables -w -t raw -I PREROUTING 1 `+v6+`"`,
		`ExecStart=sh -c "docker network inspect `+n.Net+` >/dev/null 2>&1 || docker network create --driver bridge --subnet `+BoxNetSubnet+" "+strings.Join(opts, " ")+" "+n.Net+`"`,
		`ExecStart=sh -c "test ! -d /proc/sys/net/ipv6 || sysctl -q -w net.ipv6.conf.`+n.Bridge+`.disable_ipv6=1"`)
	if mbit > 0 {
		// tbf's burst must hold what the rate sends in one timer tick (4 ms at HZ=250).
		lines = append(lines,
			fmt.Sprintf("ExecStart=tc qdisc replace dev %s root handle 1: tbf rate %dmbit burst %dkb latency 50ms", n.Bridge, mbit, max(256, mbit)),
			"ExecStart=tc qdisc replace dev "+n.Bridge+" parent 1:1 handle 10: fq_codel")
	}
	return append(lines, "[Install]", "WantedBy=docker.service")
}

// boxNetCapRe reads the cap from a unit file of either name: the bridge is
// whatever the unit's own tc line names.
var boxNetCapRe = regexp.MustCompile(`(?m)^ExecStart=tc qdisc replace dev \S+ root handle 1: tbf rate (\d+)mbit `)

// boxNetCapOf is the cap a unit file's text sets, in Mbit/s (0: none).
func boxNetCapOf(unit string) int {
	if m := boxNetCapRe.FindStringSubmatch(unit); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// BoxNetSteps are the root steps that install, enable and (re)start the
// current unit. No unit line holds a $, backtick, backslash, single quote or
// %, so each sits in double quotes (its own " escaped) inside RootCommand's
// single-quoted sh -c.
func BoxNetSteps() []string { return currentBoxNet.steps(0) }

// BoxNetCapSteps are the root steps that set the cap on boxes' downloads to
// mbit Mbit/s (0: none), on a network already made or not: BoxNetSteps with
// the capped unit.
func BoxNetCapSteps(mbit int) []string { return currentBoxNet.capSteps(mbit) }

// BoxNetCapStepsFor is BoxNetCapSteps under the names layer l's network goes
// by: the old ones (zm-box-net.service) where that is what the machine has
// (Layer.NetworkLegacy). It is what --net-mbit alone runs on that layer.
func BoxNetCapStepsFor(l Layer, mbit int) []string { return boxNetOf(l).capSteps(mbit) }

// capSteps is BoxNetCapSteps for these names. Removing a cap also deletes the
// live one, which a unit without it would leave until the next boot.
func (n boxNetNames) capSteps(mbit int) []string {
	if mbit > 0 {
		return n.steps(mbit)
	}
	return append(n.steps(0), "(tc qdisc del dev "+n.Bridge+" root 2>/dev/null || true)")
}

func (n boxNetNames) steps(mbit int) []string {
	u := n.unit(mbit)
	q := make([]string, 0, len(u))
	for _, l := range u {
		q = append(q, `"`+strings.ReplaceAll(l, `"`, `\"`)+`"`)
	}
	return []string{
		`printf "%s\n" ` + strings.Join(q, " ") + " >/etc/systemd/system/" + n.Unit,
		"systemctl daemon-reload",
		"systemctl enable " + n.Unit,
		"systemctl restart " + n.Unit,
	}
}

// dockerNet is what doctor reads of a network (docker network inspect).
type dockerNet struct {
	Driver                        string
	EnableIPv4                    *bool
	EnableIPv6, Internal, Ingress bool
	ConfigOnly                    bool
	ConfigFrom                    struct{ Network string }
	Options                       map[string]string
	IPAM                          struct {
		Driver  string
		Options map[string]string
		Config  []struct {
			Subnet, Gateway, IPRange string
			AuxiliaryAddresses       map[string]string `json:"AuxiliaryAddresses"`
		}
	}
}

// differs names how a network differs from what is made under these names, ""
// when it does not: every driver option and the address plan, so a
// same-named network with another gateway mode, masquerading off or trusted
// host interfaces is not taken for Potluck's.
func (names boxNetNames) differs(raw string) string {
	var n dockerNet
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		return "unreadable inspect output"
	}
	var d []string
	if n.Driver != "bridge" {
		d = append(d, "driver "+n.Driver)
	}
	if n.EnableIPv4 != nil && !*n.EnableIPv4 {
		d = append(d, "IPv4 off")
	}
	if n.EnableIPv6 {
		d = append(d, "IPv6 on")
	}
	if n.Internal || n.Ingress || n.ConfigOnly || n.ConfigFrom.Network != "" {
		d = append(d, "internal, ingress or config-only")
	}
	if c := n.IPAM.Config; (n.IPAM.Driver != "" && n.IPAM.Driver != "default") || len(n.IPAM.Options) > 0 || len(c) != 1 ||
		c[0].Subnet != BoxNetSubnet || (c[0].Gateway != "" && c[0].Gateway != "10.199.0.1") || c[0].IPRange != "" || len(c[0].AuxiliaryAddresses) > 0 {
		d = append(d, "addresses other than subnet "+BoxNetSubnet)
	}
	// Docker may record the address families it was asked for as options.
	tolerated := map[string]string{"com.docker.network.enable_ipv4": "true", "com.docker.network.enable_ipv6": "false"}
	create := names.create()
	for k, v := range n.Options {
		if create[k] != v && tolerated[k] != v {
			d = append(d, fmt.Sprintf("option %s=%s", k, v))
		}
	}
	for k, v := range create {
		if n.Options[k] != v {
			d = append(d, fmt.Sprintf("option %s not %s", k, v))
		}
	}
	sort.Strings(d)
	return strings.Join(d, ", ")
}

// unitFilePath is where the names' unit lives.
func (n boxNetNames) unitFilePath() string { return "/etc/systemd/system/" + n.Unit }

// found is what detectBoxNet reads of one set of names, without root.
type found struct {
	names   boxNetNames
	inspect string // docker network inspect, when the network exists
	exists  bool
	unit    string // the unit file's text, when it is there
	hasUnit bool
}

// present says the machine was set up under these names, fully or in part.
func (f found) present() bool { return f.exists || f.hasUnit }

func readBoxNet(ctx context.Context, run Runner, names boxNetNames) found {
	f := found{names: names}
	if res, err := run.Run(ctx, "docker", "network", "inspect", "--format", "{{json .}}", names.Net); err == nil && res.Exit == 0 {
		f.exists, f.inspect = true, strings.TrimSpace(res.Stdout)
	}
	if r, err := run.Run(ctx, "cat", names.unitFilePath()); err == nil && r.Exit == 0 {
		f.hasUnit, f.unit = true, r.Stdout
	}
	return f
}

// detectBoxNet sets the oci layer's Network when Potluck's filtered network
// exists as Potluck makes it and its unit is active (both readable without
// root), else NetworkProblem and, where root can fix it, NetworkSteps. It
// never makes the layer unavailable: without the network, boxes serve jobs
// only. A machine that has only the old names (zm-box, zm-box-net.service)
// is read under them (Layer.NetworkLegacy) and its steps keep them: the two
// share one subnet, so the new ones are never made beside live old ones.
func detectBoxNet(ctx context.Context, run Runner, f Facts, l *Layer) {
	const jobsOnly = "until it is, boxes serve jobs only, not GitHub Actions (a runner must reach GitHub), and sandboxes have no internet"
	if l.CLI != "docker" {
		l.NetworkProblem = "Potluck makes its filtered network for docker only; " + jobsOnly + ": or make one for " + l.CLI +
			" (sengirunners/potluck cmd/potluck-driver-tart/README.md) and pass --oci-network NAME --net-mail HOST"
		return
	}
	// The names whose network is live win: a new unit file beside a live old
	// network (a setup that stopped part way) must not make a second network
	// on the old one's subnet. Else whichever has a unit file, the new first.
	fd := readBoxNet(ctx, run, currentBoxNet)
	if !fd.exists {
		if old := readBoxNet(ctx, run, legacyBoxNet); old.exists || (!fd.hasUnit && old.hasUnit) {
			fd, l.NetworkLegacy = old, true
		}
	}
	names := fd.names
	if fd.exists {
		if why := names.differs(fd.inspect); why != "" {
			l.NetworkProblem = "a docker network " + names.Net + " exists but is not as Potluck makes it (" + why + "): remove it (docker network rm " +
				names.Net + ") and run doctor again, or pass --oci-network " + names.Net + " --net-mail HOST if you filtered it yourself"
			return
		}
	}
	if _, ok := f.LookPath("systemctl"); !ok {
		l.NetworkProblem = "Potluck's filtered network needs systemd to keep its filter across reboots, and there is none; " + jobsOnly +
			": or make one (sengirunners/potluck cmd/potluck-driver-tart/README.md) and pass --oci-network NAME --net-mail HOST"
		return
	}
	state := ""
	if r, err := run.Run(ctx, "systemctl", "is-active", names.Unit); err == nil {
		state = strings.TrimSpace(r.Stdout)
	}
	// The unit file's cap, whatever the unit's state: the steps that remake
	// the network (the unit inactive, or the network removed) must keep it,
	// and a cap this kernel cannot set (the unit failed) is removed with
	// --net-mbit 0.
	if fd.hasUnit {
		l.NetworkMbit = boxNetCapOf(fd.unit)
	}
	switch {
	case state == "active" && fd.exists:
		l.Network = names.Net
		return
	case state == "failed":
		why := ""
		if r, err := run.Run(ctx, "journalctl", "-u", names.Unit, "-n", "20", "--no-pager", "-o", "cat"); err == nil && r.Exit == 0 {
			if ls := strings.Split(strings.TrimSpace(r.Stdout), "\n"); ls[len(ls)-1] != "" {
				why = ": " + ls[len(ls)-1]
			}
		}
		if why == "" {
			why = " (journalctl -u " + names.Unit + " says why)"
		}
		l.NetworkProblem = names.Unit + " failed" + why + "; fix that (a subnet " + BoxNetSubnet + " already in use, docker or iptables not found by systemd), then: sudo systemctl restart " +
			names.Unit + "; " + jobsOnly
		return
	case state == "activating" || state == "deactivating" || state == "reloading":
		l.NetworkProblem = names.Unit + " is " + state + ": run doctor again in a moment; " + jobsOnly
		return
	}
	l.NetworkProblem = "Potluck's filtered network for GitHub runners and sandboxes (docker network " + names.Net + ", its filter kept by " + names.Unit +
		") is not set up; " + jobsOnly
	l.NetworkSteps = names.steps(l.NetworkMbit)
}
