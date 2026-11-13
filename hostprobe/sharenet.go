package hostprobe

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HostAddrs answers this machine's own addresses, which a box must reach on
// no port: every interface address but loopback and IPv6 link-local (its
// zone names a host interface, which means nothing inside a guest; the
// suite's --net-peers covers link-local neighbours). A tailnet address is an
// interface address too.
func HostAddrs() []string {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range as {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.To4() == nil && n.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, n.IP.String())
	}
	return out
}

// tailnetDNS is Tailscale's resolver, a tailnet address behind tailscale0:
// a box reaching it got through this machine's forward path (where
// tailscaled's ts-forward accepts anything bound for tailscale0).
const tailnetDNS = "100.100.100.100:53"

// forwardTargets are addresses past this machine that a box on a network
// must not reach, each on a port that opens from here (so a pass means
// blocked, not closed): the default gateway on 53, 80 or 443, and
// tailnetDNS. A host-address check covers only the INPUT path; these cover
// FORWARD. Tests swap it.
var forwardTargets = func() []string {
	opens := func(a string) bool {
		c, err := net.DialTimeout("tcp", a, time.Second)
		if err == nil {
			c.Close()
		}
		return err == nil
	}
	var out []string
	if gw := defaultGateway(); gw != "" {
		for _, p := range []string{"53", "80", "443"} {
			if a := net.JoinHostPort(gw, p); opens(a) {
				out = append(out, a)
				break
			}
		}
	}
	if opens(tailnetDNS) {
		out = append(out, tailnetDNS)
	}
	return out
}

// forbidForward is o's --net-forbid with forwardTargets added, and, when
// none opens from here, a note on res that the forward path went unchecked.
// A dry run probes nothing.
func forbidForward(o ShareOptions, res *ShareResult) []string {
	if o.DryRun {
		return o.NetForbid
	}
	fwd := forwardTargets()
	if len(fwd) == 0 {
		res.Notes = append(res.Notes, forwardUnchecked)
	}
	return append(slices.Clone(o.NetForbid), fwd...)
}

// forwardUnchecked says no forward-path target opened from this machine.
const forwardUnchecked = "the forward path past this machine is not checked: neither its default gateway (on 53, 80 or 443) nor a tailnet address (" +
	tailnetDNS + ") opens from here; add --net-forbid host:port for a LAN host that does"

// defaultGateway is the IPv4 default route's gateway, "" when none: Linux's
// from /proc/net/route (destination 00000000, the gateway little-endian
// hex), macOS's from `route -n get default`.
func defaultGateway() string {
	if b, err := os.ReadFile("/proc/net/route"); err == nil {
		return gatewayFromRoutes(string(b))
	}
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	return gatewayFromRouteGet(string(out))
}

// gatewayFromRouteGet reads macOS `route -n get default`'s "gateway:" line:
// an IPv4 address, "" for anything else (an interface name on a
// point-to-point default, as a VPN sets).
func gatewayFromRouteGet(s string) string {
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || k != "gateway" {
			continue
		}
		if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil && ip.To4() != nil && !ip.IsUnspecified() {
			return ip.String()
		}
		return ""
	}
	return ""
}

func gatewayFromRoutes(s string) string {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String()
	}
	return ""
}

// mailPassed starts a passing --net-mail check in conformance's output.
const mailPassed = "PASS  a box cannot send mail: "

// netArgs are the network targets each size's conformance run gets, as a
// run's are given by hand: the owner's --net-allow and --net-forbid, the
// peers check, and each of HostAddrs on a port share listens on for the
// length of the runs (so a pass means blocked, not closed). A dry run names
// the port <port> and listens on nothing. stop ends the listener.
func netArgs(o ShareOptions) (args []string, stop func(), err error) {
	stop = func() {}
	port := "<port>"
	if len(o.HostAddrs) > 0 && !o.DryRun {
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			return nil, stop, err
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		stop = func() { l.Close() }
		port = strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	}
	for _, a := range o.NetAllow {
		args = append(args, "--net-allow", a)
	}
	for _, a := range o.NetForbid {
		args = append(args, "--net-forbid", a)
	}
	for _, m := range o.NetMail {
		args = append(args, "--net-mail", m)
	}
	for _, h := range o.HostAddrs {
		args = append(args, "--net-forbid", net.JoinHostPort(h, port))
	}
	return append(args, "--net-peers"), stop, nil
}

// checkAddrs refuses a network target conformance would refuse.
func checkAddrs(as ...[]string) error {
	for _, l := range as {
		for _, a := range l {
			h, p, err := net.SplitHostPort(a)
			if n, perr := strconv.Atoi(p); err != nil || perr != nil || h == "" || n < 1 || n > 65535 {
				return fmt.Errorf("network target %q: want host:port ([v6]:port), port 1..65535", a)
			}
		}
	}
	return nil
}
