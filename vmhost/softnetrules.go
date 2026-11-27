package vmhost

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Softnet's own policy, which Potluck's rules sit on top of (lib/proxy/vm.rs):
// a VM's IPv4 packet that no rule matches leaves when its destination is a
// global address, or is the gateway (this Mac on the VM's bridge, "@host");
// anything else is dropped. DHCP to the gateway passes before any rule. No
// IPv6 leaves a VM at all, so a Mac's IPv6 addresses need no rule.
//
// So Potluck blocks, besides the gateway and outbound mail:
//   - multicast (224.0.0.0/4), which Softnet counts as global and hands to the
//     bridge, where every program on this Mac that joined the group gets it;
//   - every IPv4 address this Mac holds on any interface, with that
//     interface's network and broadcast addresses: a Mac with a public
//     address (a rented or hosted Mac, or one plugged straight into a modem)
//     would otherwise be reached at it on every port, since a public address
//     is global. Private and tailnet addresses are already dropped; they are
//     blocked too, so the rule does not depend on Softnet's idea of global.
//
// The addresses are read when a box boots. One this Mac gains while a box
// runs is not blocked for that box until it boots again (docs/network.md).

// softnetArgs isolate a Tart guest with block, this Mac's block rules now.
// Softnet's rule syntax, which tart passes through as --block/--allow: "out
// proto tcp port 25" (target 0.0.0.0/0 when omitted), "1.2.3.4/32"; a rule
// with a port wins over one without, and block wins ties.
func softnetArgs(block string) []string {
	return []string{"--net-softnet", "--net-softnet-block=" + block, "--net-softnet-allow=" + SoftnetAllowRules()}
}

// SoftnetBlockRules is the block rules a box boots with on this Mac now,
// used both at boot and by the host's capability preflight, so the probe and
// the boot carry the same rules: the gateway, multicast, this Mac's own
// addresses, then outbound mail (tcp 25, 465, 587, to anywhere;
// zm-eakh.12.5.13), so a box cannot send spam from the lender's IP. It fails
// when this Mac's addresses cannot be read: a box must not boot without them.
func SoftnetBlockRules() (string, error) {
	as, err := HostAddresses()
	if err != nil {
		return "", err
	}
	return softnetBlockRulesFor(as), nil
}

// SoftnetAllowRules lets a box ask the gateway for DNS, the one service of
// this Mac it reaches.
func SoftnetAllowRules() string { return "out proto udp @host port 53,out proto tcp @host port 53" }

// softnetGatewayBlock leads the block rules (a broken rule set in
// scripts/softnet-sim drops it by prefix); softnetMailBlock ends them.
const softnetGatewayBlock = "@host,224.0.0.0/4"

// softnetBlockRulesFor is the block rules for a Mac holding as.
func softnetBlockRulesFor(as []netip.Prefix) string {
	rs := []string{softnetGatewayBlock}
	for _, a := range hostBlockAddrs(as) {
		rs = append(rs, a.String()+"/32")
	}
	return strings.Join(append(rs, softnetMailBlock), ",")
}

// hostBlockAddrs is every IPv4 address in as but loopback, with its
// network's own and broadcast addresses when it has them (a prefix of /30 or
// wider), sorted, each once.
func hostBlockAddrs(as []netip.Prefix) []netip.Addr {
	var out []netip.Addr
	for _, p := range as {
		a := p.Addr()
		if !a.Is4() || a.IsLoopback() {
			continue
		}
		out = append(out, a)
		if p.Bits() <= 30 {
			n := p.Masked().Addr().As4()
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], binary.BigEndian.Uint32(n[:])|(1<<(32-p.Bits())-1))
			out = append(out, netip.AddrFrom4(n), netip.AddrFrom4(b))
		}
	}
	slices.SortFunc(out, func(x, y netip.Addr) int { return x.Compare(y) })
	return slices.Compact(out)
}

// interfaceAddrs is the host's interface addresses (a test replaces it).
var interfaceAddrs = net.InterfaceAddrs

// HostAddresses is every address on this machine's interfaces, each with its
// interface's prefix length, IPv4 unmapped. The one list of this machine's
// own addresses: a box's block rules come from it, and so do the addresses
// `potluck host` shows a box cannot reach.
func HostAddresses() ([]netip.Prefix, error) {
	as, err := interfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("this machine's network addresses cannot be read, so a box could not be kept from them: %w", err)
	}
	var out []netip.Prefix
	for _, a := range as {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ones, bits := n.Mask.Size()
		if ip.Is4In6() {
			ip = ip.Unmap()
			if bits == 128 {
				ones -= 96
			}
		}
		if bits == 0 || ones < 0 { // a non-canonical mask: the address alone
			ones = ip.BitLen()
		}
		out = append(out, netip.PrefixFrom(ip, ones))
	}
	return out, nil
}

// softnetMailBlock is one block rule per mail port.
var softnetMailBlock = func() string {
	rs := make([]string, len(mailPorts))
	for i, p := range mailPorts {
		rs[i] = "out proto tcp port " + strconv.Itoa(p)
	}
	return strings.Join(rs, ",")
}()

// mailPorts are outbound mail's TCP ports (SMTP, SMTPS, submission): every
// layer blocks them for an isolated VM.
var mailPorts = []int{25, 465, 587}
