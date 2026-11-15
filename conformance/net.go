package conformance

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Probe exits for the net checks (ProbeScript's net subcommand).
const (
	exitReached     = 5  // a forbidden address opened
	exitPeerReached = 6  // another run's machine accepted
	exitUnreachable = 7  // an allowed address did not open
	exitUnresolved  = 8  // a name did not resolve
	exitUsage       = 64 // a probe older than the net subcommand
	exitNoTools     = 65 // the guest lacks nc or bash, ip, ping
	exitUnreliable  = 66 // the guest's nc cannot listen, or cannot tell open from closed
)

// MailAddrs expands a --net-mail value: host:port as it is, a bare host on
// each mail port (25, 465 and 587).
func MailAddrs(v string) ([]string, error) {
	if NetAddr(v) == nil {
		return []string{v}, nil
	}
	if v == "" || strings.ContainsAny(v, " :/[]") || strings.HasPrefix(v, "-") {
		return nil, fmt.Errorf("%q: want a host, or host:port", v)
	}
	return []string{net.JoinHostPort(v, "25"), net.JoinHostPort(v, "465"), net.JoinHostPort(v, "587")}, nil
}

// NetAddr checks a host:port the net checks take ("[v6]:port" for IPv6).
func NetAddr(a string) error {
	h, p, err := net.SplitHostPort(a)
	if err != nil {
		return fmt.Errorf("%q: %v", a, err)
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q: port must be 1..65535", a)
	}
	if h == "" {
		return fmt.Errorf("%q: no host", a)
	}
	return nil
}

// ResolveName checks a --net-resolve value: a DNS name, or name:port (the
// port the probe connects to where the guest has no getent; default 443).
func ResolveName(v string) error {
	h := v
	if NetAddr(v) == nil {
		h, _, _ = net.SplitHostPort(v)
	}
	if !dnsName.MatchString(h) || isIPLiteral(h) {
		return fmt.Errorf("%q: want a DNS name, or name:port", v)
	}
	return nil
}

// isIPLiteral is an IP address, an IPv6 zone included ("fe80::1%eth0").
func isIPLiteral(h string) bool { _, err := netip.ParseAddr(h); return err == nil }

var dnsName = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9_.-]{0,251}[A-Za-z0-9.])?$`)

// defaultResolve is the name a target with a network must resolve when
// nothing else names one: a runner binding fetches from it, and every box
// that reaches the internet should.
const defaultResolve = "github.com:443"

// resolveTargets are the names (name:port) the net checks resolve inside the
// guest (zm-eakh.12.24.7): --net-resolve; else, when --net-allow says the
// guest has a network, the names among its hosts, or defaultResolve when
// they are all literals. A broken resolver then fails the host's check, not
// the first job: an allowed IP literal opens without one.
func (o Options) resolveTargets() []string {
	var out []string
	add := func(a string) {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	for _, v := range o.NetResolve {
		if NetAddr(v) != nil {
			v = net.JoinHostPort(v, "443")
		}
		add(v)
	}
	if len(out) > 0 || len(o.NetAllow) == 0 {
		return out
	}
	for _, a := range o.NetAllow {
		if h, _, err := net.SplitHostPort(a); err == nil && !isIPLiteral(h) {
			add(a)
		}
	}
	if len(out) == 0 {
		add(defaultResolve)
	}
	return out
}

// net checks a run's or a box's network from inside it: it reaches what it
// should and nothing it should not, and two at once cannot reach each other.
// A run carries the committed probe; a box gets the same probe through exec.
func (r *run) net() {
	const name = "net"
	if r.kind != "run" && r.kind != "box" {
		r.skip(name, "run and box targets only")
		return
	}
	if len(r.opt.NetAllow)+len(r.opt.NetForbid)+len(r.opt.NetMail)+len(r.opt.NetResolve) == 0 && !r.opt.NetPeers {
		r.skip(name, "no --net-allow, --net-forbid, --net-mail, --net-resolve or --net-peers")
		return
	}
	in := func(a ...string) (int, error) { return r.runExit(append([]string{"net"}, a...)...) }
	if r.kind == "box" {
		// A driver that honours net makes this box as a trusted host's
		// sandbox is made (net on: zm-eakh.12.5.19), so the checks cover it.
		h, err := r.acquireBoxNet(r.netOn())
		if !r.add("a box for the net checks", err) {
			return
		}
		defer r.endHeld(h)
		// A box image must have nc: a box without it would bind with its
		// network unchecked, so this fails rather than skips.
		const hasNC = "the box has nc (the network checks need it)"
		res, err := r.exec(h.Handle, []string{"sh", "-c", "command -v nc >/dev/null 2>&1"}, "", 0)
		if err == nil && *res.Exit != 0 {
			err = errNoNC
		}
		if !r.add(hasNC, err) {
			return
		}
		in = func(a ...string) (int, error) { return r.boxNet(h.Handle, a...) }
	}
	if !r.netControls(in) {
		return
	}
	probe := func(check string, ok int, fail func(int) error, args ...string) {
		code, err := in(args...)
		switch {
		case err != nil:
			r.add(check, err)
		case code == ok:
			r.add(check, nil)
		case code == exitNoTools:
			r.skip(check, "the guest has no nc (or bash and timeout), ip or ping")
		case code == exitUsage:
			r.add(check, fmt.Errorf("the committed probe predates the net checks: commit --print-probe again"))
		default:
			r.add(check, fail(code))
		}
	}
	for _, a := range r.opt.NetAllow {
		probe("a "+r.kind+" reaches "+a, 0, func(c int) error {
			return fmt.Errorf("the probe exited %d: %s did not open from inside the %s", c, a, r.kind)
		}, "allow", a)
	}
	// DNS (zm-eakh.12.24.7): a name this machine resolves must resolve inside
	// too. One that does not resolve here proves nothing: a SKIP.
	for _, a := range r.opt.resolveTargets() {
		host, port, _ := net.SplitHostPort(a)
		check := "a " + r.kind + " resolves " + host + " (DNS)"
		if ips, err := net.DefaultResolver.LookupHost(r.ctx, host); err != nil || len(ips) == 0 {
			r.skip(check, "not proven: "+host+" does not resolve on this machine")
			continue
		}
		probe(check, 0, func(c int) error {
			switch c {
			case exitUnresolved:
				return fmt.Errorf("%s does not resolve inside the %s (it resolves here): its resolver is unreachable or broken, so a job reaches addresses but no names (check its /etc/resolv.conf)", host, r.kind)
			case exitUnreachable: // no getent: the probe could only try a connection
				return fmt.Errorf("%s did not open from inside the %s, and it has no getent to tell a broken resolver from a closed port: give --net-resolve a name:port that opens, or add getent to the image", a, r.kind)
			}
			return fmt.Errorf("the probe exited %d", c)
		}, "resolve", host, port)
	}
	// A forbidden address proves something only when it means the same place
	// inside the guest as here and opens from here. Its name is resolved here
	// and each address probed as a literal (a name the guest cannot resolve,
	// or resolves to another family, would otherwise pass without trying).
	// This machine's loopback, unspecified, link-local and multicast addresses
	// mean the guest itself inside a VM, and a closed port passes however open
	// the guest's network is: each is a SKIP, as for mail.
	for _, a := range r.opt.NetForbid {
		host, port, _ := net.SplitHostPort(a)
		ips := []string{host}
		if !isIPLiteral(host) {
			var err error
			if ips, err = net.DefaultResolver.LookupHost(r.ctx, host); err != nil || len(ips) == 0 {
				r.skip("a "+r.kind+" cannot reach "+a, "not proven: "+host+" does not resolve on this machine")
				continue
			}
		}
		for _, ip := range ips {
			to := net.JoinHostPort(ip, port)
			check := "a " + r.kind + " cannot reach " + a
			if to != a {
				check += " (" + to + ")"
			}
			if why := localOnly(ip); why != "" {
				r.skip(check, "not proven: "+to+" is this machine's "+why+" address, which means something else inside a guest: forbid this machine's LAN address instead")
				continue
			}
			if err := opensHere(r.ctx, to); err != nil {
				r.skip(check, "not proven: "+to+" does not open from this machine either ("+err.Error()+")")
				continue
			}
			probe(check, 0, func(c int) error {
				if c == exitReached {
					return fmt.Errorf("%s opened from inside the %s", to, r.kind)
				}
				return fmt.Errorf("the probe exited %d", c)
			}, "forbid", to)
		}
	}
	// Outbound mail (zm-eakh.12.5.13): an outside mail server must not open
	// from inside, and must from here, or a closed port would pass. A name
	// is resolved here and each address probed as a literal: a guest that
	// cannot resolve it would otherwise pass without trying.
	for _, a := range r.opt.NetMail {
		host, port, _ := net.SplitHostPort(a)
		ips, err := net.DefaultResolver.LookupHost(r.ctx, host)
		if err != nil || len(ips) == 0 {
			r.skip("a "+r.kind+" cannot send mail: "+a+" does not open", "not proven: "+host+" does not resolve on this machine")
			continue
		}
		for _, ip := range ips {
			to := net.JoinHostPort(ip, port)
			check := "a " + r.kind + " cannot send mail: " + a + " (" + to + ") does not open"
			c, err := net.DialTimeout("tcp", to, 5*time.Second)
			if err != nil {
				r.skip(check, "not proven: "+to+" does not open from this machine either")
				continue
			}
			c.Close()
			probe(check, 0, func(c int) error {
				if c == exitReached {
					return fmt.Errorf("%s opened from inside the %s: it can send mail", to, r.kind)
				}
				return fmt.Errorf("the probe exited %d", c)
			}, "forbid", to)
		}
	}
	if r.opt.NetPeers && r.kind == "box" {
		r.peersBox(in)
	} else if r.opt.NetPeers {
		r.peers()
	}
	if r.netOn() != "" {
		r.netOff()
	}
}

// localOnly names the class of an address that means this machine here but
// the guest itself (or nothing) inside a VM: "" for any other.
func localOnly(ip string) string {
	addr, err := netip.ParseAddr(ip)
	switch {
	case err != nil:
		return ""
	case addr.IsLoopback():
		return "loopback"
	case addr.IsUnspecified():
		return "unspecified"
	case addr.IsLinkLocalUnicast():
		return "link-local"
	case addr.IsMulticast():
		return "multicast"
	}
	return ""
}

// opensHere dials a from this machine: a forbidden address proves something
// only when it opens here.
func opensHere(ctx context.Context, a string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", a)
	if err != nil {
		return err
	}
	return c.Close()
}

// netOffCap is a box driver's capability that it honours acquire's net
// (SPEC 20 "Boxes", Network): off makes the box with no network at all.
const netOffCap = "net-off"

// netOn is the net a box under the net checks is acquired with: on for a
// driver listing net-off, else unsaid (as zm sends such a driver nothing).
func (r *run) netOn() string {
	if r.kind == "box" && r.desc.Capabilities != nil && slices.Contains(*r.desc.Capabilities, netOffCap) {
		return "on"
	}
	return ""
}

// acquireBoxNet acquires a box for the net checks with net ("" unsaid).
func (r *run) acquireBoxNet(net string) (held, error) {
	if net == "" {
		return r.acquireAny(r.longTTL())
	}
	h, ttl := held{key: r.key()}, r.longTTL()
	h.sent = args("request_id", h.key, "actor", actor, "ttl_s", ttl, "net", net)
	before := time.Now()
	a, e, err := r.acquire(h.sent, r.cfg)
	h.acquired = a
	if err = must(e, err); err != nil {
		return h, err
	}
	return h, r.acquireShape(a, ttl, before)
}

// netOff checks a box acquired with net off reaches nothing a networked box
// reaches (--net-allow), while its probe still sees a port open inside it,
// so a pass means no network rather than a probe that cannot connect.
func (r *run) netOff() {
	const name = "a box made with net off"
	if len(r.opt.NetAllow) == 0 {
		r.skip(name+" reaches nothing", "no --net-allow: nothing a networked box must reach to try")
		return
	}
	h, err := r.acquireBoxNet("off")
	if err == nil && h.Net != "" && h.Net != "off" {
		err = fmt.Errorf("acquire answered net %q, want off", h.Net)
	}
	if !r.add(name+" for the net checks", err) {
		return
	}
	defer r.endHeld(h)
	code, err := r.boxNet(h.Handle, "self", strconv.Itoa(r.peerPort()+2))
	if err == nil && code != 0 {
		err = fmt.Errorf("the probe exited %d: it cannot see a port open inside the box, so its other answers prove nothing", code)
	}
	if !r.add(name+": its probe sees a port open inside it", err) {
		return
	}
	for _, a := range r.opt.NetAllow {
		check := name + " cannot reach " + a
		code, err := r.boxNet(h.Handle, "forbid", a)
		switch {
		case err != nil:
			r.add(check, err)
		case code == exitReached:
			r.add(check, fmt.Errorf("%s opened from inside a box acquired with net off", a))
		case code != 0:
			r.add(check, fmt.Errorf("the probe exited %d", code))
		default:
			r.add(check, nil)
		}
	}
}

// boxNet runs the probe's net subcommand in box h through exec: the box
// needs no commit, the probe goes in as sh -c's script.
func (r *run) boxNet(h string, a ...string) (int, error) {
	res, err := r.exec(h, probeArgv(append([]string{"net"}, a...)...), "", execTimeoutMax)
	switch {
	case err != nil:
		return 0, err
	case *res.Exit == exitExpired:
		return 0, fmt.Errorf("the probe %v did not end within %d s", a, execTimeoutMax)
	}
	return *res.Exit, nil
}

// probeArgv is the probe with its arguments as one exec's literal argv.
func probeArgv(a ...string) []string {
	return append([]string{"sh", "-c", ProbeScript, "zm-probe"}, a...)
}

var errNoNC = fmt.Errorf("this image has no nc; the network checks need it: install netcat-openbsd or pick an image that has it")

// netControls proves a net check's answer means something before trusting
// one: the script runs and its exit comes back (a closed port reads closed,
// exit 7, not the 0 a driver faking exits gives), and this guest's tools see
// an open port as open. False ends the group.
func (r *run) netControls(in func(...string) (int, error)) bool {
	const fails, sees = "the net probe can fail: a closed port reads closed", "the net probe sees a port open in the guest"
	code, err := in("allow", "127.0.0.1:1")
	switch {
	case err != nil:
		r.add(fails, err)
		return false
	case code == exitNoTools:
		r.skip("net", "the guest has no nc (or bash and timeout)")
		return false
	case code == exitUsage:
		r.add(fails, fmt.Errorf("the committed probe predates the net checks: commit --print-probe again"))
		return false
	case code != exitUnreachable:
		r.add(fails, fmt.Errorf("the probe exited %d, want %d: the driver is not running the script, or not passing its exit", code, exitUnreachable))
		return false
	}
	r.add(fails, nil)
	code, err = in("self", strconv.Itoa(r.peerPort()+1))
	switch {
	case err == nil && code == 0:
		r.add(sees, nil)
		return true
	case err == nil && code == exitNoTools: // bash and timeout passed the first control, but listening needs nc
		err = errNoNC // a FAIL: a SKIP would pass the whole group untested
	case err == nil && code == exitUnreliable:
		err = fmt.Errorf("this guest's nc cannot listen, or cannot tell an open port from a closed one")
	case err == nil:
		err = fmt.Errorf("the probe exited %d", code)
	}
	r.add(sees, err)
	return false
}

func (r *run) peerPort() int {
	if r.opt.NetPeerPort != 0 {
		return r.opt.NetPeerPort
	}
	return 47123
}

const (
	// serveTimeout is the listening box's exec: it outlasts listenWait, the
	// scan (one exec, at most execTimeoutMax) and the checks around it.
	serveTimeout = 120
	// listenWait is how long the listening box's exec may take to listen.
	listenWait = 10 * time.Second
	// serveListen is how long the probe's net serve gives its own listener to
	// answer before it exits 66 (the 4 in ProbeScript's serve). Nothing outside
	// a run sees that verdict but its exit, so the peers checks wait it out.
	serveListen = 4 * time.Second
)

const peersCheck = "two %s cannot reach each other (same IPv4 /24; IPv6 link-local neighbours that answer ping)"

// peers holds one run listening and checks another cannot reach it. What it
// scans is what the name says: a pass is no proof about wider networks.
func (r *run) peers() {
	check := fmt.Sprintf(peersCheck, "runs")
	port := r.peerPort()
	ps := strconv.Itoa(port)
	a, e, err := r.acquire(r.runArgs(r.key(), r.longTTL(), "net", "serve", ps), r.cfg)
	if err = must(e, err); err != nil {
		r.add(check, fmt.Errorf("the listening run: %v", err))
		return
	}
	defer func() {
		if _, err := r.release(a.Handle); err == nil {
			delete(r.live, a.Handle)
		}
	}()
	// serve checks its own listener answers, and exits 66 if it cannot within
	// serveListen: a run still going after that (and 2 s for the run to start)
	// is listening. The scan must not run before then: it would pass with
	// nothing listening yet.
	r.sleep(serveListen + 2*time.Second)
	st, err := r.status(a.Handle)
	switch {
	case err != nil:
		r.add(check, fmt.Errorf("the listening run: %v", err))
		return
	case st.State != "running" && st.Exit != nil && *st.Exit == exitNoTools:
		r.skip(check, "the guest has no nc to listen with")
		return
	case st.State != "running" && st.Exit != nil && *st.Exit == exitUnreliable:
		r.add(check, fmt.Errorf("the listening run could not listen on port %d", port))
		return
	case st.State != "running":
		r.add(check, fmt.Errorf("the listening run is %s, not running", st.State))
		return
	}
	code, err := r.runExit("net", "peers", ps)
	switch {
	case err != nil:
		r.add(check, err)
	case code == 0:
		r.add(check, nil)
	case code == exitNoTools:
		r.skip(check, "the guest has no ip, or no nc (or bash and timeout)")
	case code == exitUsage:
		r.add(check, fmt.Errorf("the committed probe predates the net checks: commit --print-probe again"))
	case code == exitPeerReached:
		r.add(check, fmt.Errorf("a run reached another run's machine on port %d", port))
	default:
		r.add(check, fmt.Errorf("the probe exited %d", code))
	}
}

// peersBox is peers for boxes: a second box listens through an exec held open
// on its own goroutine while the net group's box (in) scans for it.
func (r *run) peersBox(in func(...string) (int, error)) {
	check := fmt.Sprintf(peersCheck, "boxes")
	ps := strconv.Itoa(r.peerPort())
	l, err := r.acquireAny(r.longTTL())
	if err != nil {
		r.add(check, fmt.Errorf("the listening box: %v", err))
		return
	}
	type served struct {
		res execResult
		err error
	}
	done := make(chan served, 1)
	go func() {
		res, err := r.exec(l.Handle, probeArgv("net", "serve", ps), "", serveTimeout)
		done <- served{res, err}
	}()
	defer func() {
		r.endHeld(l) // a box's end ends its exec
		select {
		case <-done:
		case <-time.After(r.limit(verbExec) + serveTimeout*time.Second):
		}
	}()
	// ended reports a listener that is gone (serve exits 66 if it cannot
	// listen), adding why.
	ended := func() bool {
		select {
		case s := <-done:
			done <- s
			switch {
			case s.err != nil:
				r.add(check, fmt.Errorf("the listening box: %v", s.err))
			case *s.res.Exit == exitNoTools:
				r.add(check, fmt.Errorf("the listening box has no nc to listen with"))
			case *s.res.Exit == exitUnreliable:
				r.add(check, fmt.Errorf("the listening box could not listen on port %s", ps))
			default:
				r.add(check, fmt.Errorf("the listening box's probe ended with exit %d", *s.res.Exit))
			}
			return true
		default:
			return false
		}
	}
	// up: the listener answers inside its own box. An exec still going is no
	// proof: a slow start, or one ended before the scan is done, would pass
	// the scan with nothing listening.
	local := "127.0.0.1:" + ps
	up := func() bool { code, err := r.boxNet(l.Handle, "allow", local); return err == nil && code == 0 }
	// serve first checks its own listener (for up to serveListen; a listener
	// of the one-connection forms takes one connection); a check of ours
	// inside that window can take that connection, and serve exits 66 as if
	// it could not listen. As peers waits for a run's.
	r.sleep(serveListen + time.Second)
	for deadline := time.Now().Add(listenWait); !up(); r.sleep(500 * time.Millisecond) {
		if ended() {
			return
		}
		if !time.Now().Before(deadline) || r.ctx.Err() != nil {
			r.add(check, fmt.Errorf("nothing listened on port %s in the listening box within %s", ps, listenWait))
			return
		}
	}
	code, err := in("peers", ps)
	if err == nil && code == 0 && !up() {
		if !ended() {
			r.add(check, fmt.Errorf("the listening box stopped listening on port %s before the scan ended: the scan proved nothing", ps))
		}
		return
	}
	switch {
	case err != nil:
		r.add(check, err)
	case code == 0:
		r.add(check, nil)
	case code == exitNoTools:
		r.skip(check, "the guest has no ip, or no nc (or bash and timeout)")
	case code == exitPeerReached:
		r.add(check, fmt.Errorf("a box reached another box's machine on port %s", ps))
	default:
		r.add(check, fmt.Errorf("the probe exited %d", code))
	}
}
