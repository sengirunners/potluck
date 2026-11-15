package conformance

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance/refdriver"
)

func TestNetAddr(t *testing.T) {
	for _, ok := range []string{"10.0.0.1:22", "[fd00::1]:443", "example.com:80"} {
		if err := NetAddr(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"10.0.0.1", "fd00::1:443", ":80", "h:0", "h:70000", "h:x"} {
		if err := NetAddr(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// The net checks run inside a run (here the reference driver runs on this
// machine, so it reaches what this machine reaches): an open forbidden
// address fails, a closed one passes, an allowed one passes, an unreachable
// allowed one fails, and with none of the flags the group is a SKIP.
func TestNetChecks(t *testing.T) {
	path, opt := fixture(t, "run", "")
	opt.Only = []string{"net"}
	if out := Format(Run(context.Background(), path, opt)); !strings.Contains(out, "SKIP  net") {
		t.Fatalf("no flags:\n%s", out)
	}
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	go func() {
		for {
			c, err := open.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	shut, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := shut.Addr().String()
	shut.Close()
	lanOpen, lanClosed, lan := lanListeners(t)
	opt.NetAllow = []string{open.Addr().String(), closed}
	opt.NetForbid = []string{open.Addr().String()}
	var chatty string
	if lan {
		chatty = chattyListener(t, lanClosed)
		opt.NetForbid = append(opt.NetForbid, lanOpen, lanClosed, chatty)
	}
	_, port, _ := net.SplitHostPort(open.Addr().String())
	opt.NetResolve = []string{"localhost:" + port} // without getent the probe opens it
	res := Run(context.Background(), path, opt)
	want := map[string]bool{
		"the net probe can fail: a closed port reads closed": true,
		"the net probe sees a port open in the guest":        true,
		"a run resolves localhost (DNS)":                     true,
		"a run reaches " + open.Addr().String():              true,
		"a run reaches " + closed:                            false,
	}
	if lan {
		want["a run cannot reach "+lanOpen] = false
		want["a run cannot reach "+chatty] = false
	}
	seen := 0
	for _, r := range res {
		if w, ok := want[r.Name]; ok {
			seen++
			if r.Skipped || r.OK != w {
				t.Errorf("%s: ok=%v skipped=%v (%s), want ok=%v", r.Name, r.OK, r.Skipped, r.Detail, w)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d net checks:\n%s", seen, len(want), Format(res))
	}
	forbidSkips(t, "run", res, open.Addr().String(), lanClosed)
}

// forbidSkips checks that a loopback forbidden address, and (when there is
// one) a LAN address closed here too, are SKIPs that prove nothing.
func forbidSkips(t *testing.T, kind string, res []Result, loopback, lanClosed string) {
	t.Helper()
	out := Format(res)
	if !strings.Contains(out, "SKIP  a "+kind+" cannot reach "+loopback+": not proven: "+loopback+" is this machine's loopback address") {
		t.Errorf("a loopback forbidden address:\n%s", out)
	}
	if lanClosed != "" && !strings.Contains(out, "SKIP  a "+kind+" cannot reach "+lanClosed+": not proven: "+lanClosed+" does not open from this machine either") {
		t.Errorf("a closed forbidden address:\n%s", out)
	}
}

// lanListeners answers an address on this machine's LAN interface that
// accepts and one that is closed: a forbidden address the suite will try
// (it skips loopback ones). ok is false when the machine has no LAN address.
func lanListeners(t *testing.T) (open, closed string, ok bool) {
	t.Helper()
	ip, ok := refdriver.LANAddrCI(t, "the reached-forbidden-address part is not checked")
	if !ok {
		t.Log("no LAN address: the reached-forbidden-address part is not checked")
		return "", "", false
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for c, err := l.Accept(); err == nil; c, err = l.Accept() {
			c.Close()
		}
	}()
	shut, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	closed = shut.Addr().String()
	shut.Close()
	return l.Addr().String(), closed, true
}

// chattyListener answers an address on this machine's LAN interface whose
// service sends a byte every 0.3 s for 8 s after each accept and then closes:
// an nc reading it is never idle for the probe's first seconds, and every nc
// ends once it closes. A forbidden address like it that opens must count as reached, never
// as closed because the probe gave up on an nc that had connected. It never
// takes avoid, the closed address lanListeners just freed: port 0 can hand
// that port straight back, and the closed address would then open.
func chattyListener(t *testing.T, avoid string) string {
	t.Helper()
	ip := refdriver.RequireLANAddr(t, "no chatty forbidden address")
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	for err == nil && l.Addr().String() == avoid {
		taken := l // held while asking again, so the next answer differs
		l, err = net.Listen("tcp", net.JoinHostPort(ip, "0"))
		taken.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); l.Close() })
	go func() {
		for c, err := l.Accept(); err == nil; c, err = l.Accept() {
			go func(c net.Conn) {
				defer c.Close()
				tick := time.NewTicker(300 * time.Millisecond)
				defer tick.Stop()
				end := time.After(8 * time.Second)
				for {
					select {
					case <-done:
						return
					case <-end:
						return
					case <-tick.C:
						if _, err := c.Write([]byte("x")); err != nil {
							return
						}
					}
				}
			}(c)
		}
	}()
	return l.Addr().String()
}

// A probe committed before the net subcommand says so instead of passing.
func TestNetChecksNeedACurrentProbe(t *testing.T) {
	path, opt := fixture(t, "run", "")
	dir := t.TempDir()
	repo, err := refdriver.NewRepo(dir, "#!/bin/sh\nexit 64\n")
	if err != nil {
		t.Fatal(err)
	}
	opt.Commit, opt.Repo = repo.Commit, repo.Remote
	opt.Config = map[string]string{"remote": repo.Remote, "worktree": repo.Worktree}
	opt.Only, opt.NetForbid = []string{"net"}, []string{"127.0.0.1:9"}
	out := Format(Run(context.Background(), path, opt))
	if !strings.Contains(out, "predates the net checks") {
		t.Fatalf("an old probe:\n%s", out)
	}
	_ = os.RemoveAll(dir)
}

// listeners answers an address that accepts and one that is closed.
func listeners(t *testing.T) (open, closed string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	shut, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed = shut.Addr().String()
	shut.Close()
	return l.Addr().String(), closed
}

// A box's network is checked through exec, as a run's is through its
// committed probe (zm-eakh.12.5.12: the group used to skip every box). The
// reference box runs on this machine, so it reaches what this machine does.
func TestBoxNetChecks(t *testing.T) {
	path, opt := fixture(t, "box", "")
	opt.Only = []string{"net"}
	if out := Format(Run(context.Background(), path, opt)); !strings.Contains(out, "SKIP  net: no --net-allow") {
		t.Fatalf("no flags:\n%s", out)
	}
	open, closed := listeners(t)
	lanOpen, lanClosed, lan := lanListeners(t)
	opt.NetAllow = []string{open, closed}
	opt.NetForbid = []string{open}
	var chatty string
	if lan {
		chatty = chattyListener(t, lanClosed)
		opt.NetForbid = append(opt.NetForbid, lanOpen, lanClosed, chatty)
	}
	_, port, _ := net.SplitHostPort(open)
	opt.NetResolve = []string{"localhost:" + port}
	res := Run(context.Background(), path, opt)
	want := map[string]bool{
		"a box for the net checks":                           true,
		"a box resolves localhost (DNS)":                     true,
		"the net probe can fail: a closed port reads closed": true,
		"the net probe sees a port open in the guest":        true,
		"a box reaches " + open:                              true,
		"a box reaches " + closed:                            false,
	}
	if lan {
		want["a box cannot reach "+lanOpen] = false
		want["a box cannot reach "+chatty] = false
	}
	seen := 0
	for _, r := range res {
		if w, ok := want[r.Name]; ok {
			seen++
			if r.Skipped || r.OK != w {
				t.Errorf("%s: ok=%v skipped=%v (%s), want ok=%v", r.Name, r.OK, r.Skipped, r.Detail, w)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d of %d net checks:\n%s", seen, len(want), Format(res))
	}
	forbidSkips(t, "box", res, open, lanClosed)
	for _, r := range res {
		if strings.HasPrefix(r.Name, "nothing left") && !r.OK {
			t.Errorf("the net group left something: %s", r.Detail)
		}
	}
}

// Two boxes: one listens through an exec held open, the other scans its /24.
// An ip that names a loopback /24 makes this machine's listener the peer the
// scan must not reach, so a reference box (no isolation) fails the check.
func TestBoxNetPeers(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS's nc -w does not bound a connect to an unconfigured loopback address (75 s each); Linux's 127/8 refuses at once")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\ncase \"$1\" in -4) echo '1: lo    inet 127.0.0.5/24 scope global lo' ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path, opt := fixture(t, "box", "")
	_, free := listeners(t) // a port nothing holds
	_, port, _ := net.SplitHostPort(free)
	opt.Only, opt.NetPeers = []string{"net"}, true
	opt.NetPeerPort, _ = strconv.Atoi(port)
	out := Format(Run(context.Background(), path, opt))
	switch {
	case strings.Contains(out, "SKIP  net: the guest has no nc"):
		refdriver.NoNC(t, out)
	case !strings.Contains(out, "FAIL  two boxes cannot reach each other") || !strings.Contains(out, "a box reached another box's machine"):
		t.Fatalf("a box that reaches its peer:\n%s", out)
	}
}

func TestMailAddrs(t *testing.T) {
	got, err := MailAddrs("smtp.example")
	if err != nil || strings.Join(got, " ") != "smtp.example:25 smtp.example:465 smtp.example:587" {
		t.Errorf("a host: %v %v", got, err)
	}
	if got, err := MailAddrs("[2001:db8::1]:25"); err != nil || len(got) != 1 {
		t.Errorf("a host:port: %v %v", got, err)
	}
	for _, bad := range []string{"", "-x", "a b", "2001:db8::1", "h:0"} {
		if _, err := MailAddrs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Outbound mail (zm-eakh.12.5.13): a mail server that opens from inside the
// box fails (the reference box is this machine, which reaches it), and one
// that does not open from here either is a SKIP, never a pass. A name is
// resolved here and each address probed as a literal (a guest that cannot
// resolve it must not pass); one that does not resolve here is a SKIP.
func TestBoxMailCheck(t *testing.T) {
	path, opt := fixture(t, "box", "")
	open, closed := listeners(t)
	_, port, _ := net.SplitHostPort(open)
	named := "localhost:" + port
	opt.Only, opt.NetMail = []string{"net"}, []string{open, closed, named, "no-such-host.invalid:25"}
	res := Run(context.Background(), path, opt)
	var reached, unproven, byName, unresolved bool
	for _, r := range res {
		switch r.Name {
		case "a box cannot send mail: " + open + " (" + open + ") does not open":
			reached = !r.OK && strings.Contains(r.Detail, "it can send mail")
		case "a box cannot send mail: " + closed + " (" + closed + ") does not open":
			unproven = r.Skipped && strings.Contains(r.Detail, "not proven")
		case "a box cannot send mail: " + named + " (" + open + ") does not open":
			byName = !r.OK && strings.Contains(r.Detail, open+" opened")
		case "a box cannot send mail: no-such-host.invalid:25 does not open":
			unresolved = r.Skipped && strings.Contains(r.Detail, "does not resolve")
		}
	}
	if !reached || !unproven || !byName || !unresolved {
		t.Fatalf("reached %v, unproven %v, by name %v, unresolved %v:\n%s", reached, unproven, byName, unresolved, Format(res))
	}
}

// A box image without nc fails the net group with a plain message naming nc
// (never a SKIP: zm host share would then bind a box with its network
// unchecked). PATH here is /bin and /usr/bin without nc, which the
// reference box's exec inherits.
func TestBoxNetNeedsNC(t *testing.T) {
	path, opt := fixture(t, "box", "")
	bin := t.TempDir()
	for _, d := range []string{"/bin", "/usr/bin"} {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			switch n := e.Name(); n {
			case "nc", "ncat", "netcat":
			default:
				_ = os.Symlink(filepath.Join(d, n), filepath.Join(bin, n))
			}
		}
	}
	t.Setenv("PATH", bin)
	_, closed := listeners(t)
	opt.Only, opt.NetForbid = []string{"net"}, []string{closed}
	out := Format(Run(context.Background(), path, opt))
	if !strings.Contains(out, "FAIL  the box has nc (the network checks need it): this image has no nc") {
		t.Fatalf("a box without nc:\n%s", out)
	}
}

// The VM-to-VM check proves its listener is up before trusting a clean scan
// (zm-eakh.12.5.12): a listening box whose exec starts late (bug
// box-serve-late: 20 s) fails it instead of passing a scan of nothing. ip
// names no /24 here, so the scan itself finds nothing at once.
func TestBoxNetPeersNeedAListener(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path, opt := fixture(t, "box", "box-serve-late")
	_, free := listeners(t)
	_, port, _ := net.SplitHostPort(free)
	opt.Only, opt.NetPeers = []string{"net"}, true
	opt.NetPeerPort, _ = strconv.Atoi(port)
	out := Format(Run(context.Background(), path, opt))
	switch {
	case strings.Contains(out, "FAIL  the box has nc"):
		refdriver.NoNC(t, out)
	case !strings.Contains(out, "FAIL  two boxes cannot reach each other") || !strings.Contains(out, "nothing listened on port "+port):
		t.Fatalf("a listener that never came up:\n%s", out)
	}
}

// A clean scan counts only if the listener was still up when it ended: a
// listening box whose listener stops while its exec goes on (bug
// box-serve-stops: the scan's exec stops it) fails the check.
func TestBoxNetPeersListenerStops(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path, opt := fixture(t, "box", "box-serve-stops")
	_, free := listeners(t)
	_, port, _ := net.SplitHostPort(free)
	opt.Only, opt.NetPeers = []string{"net"}, true
	opt.NetPeerPort, _ = strconv.Atoi(port)
	out := Format(Run(context.Background(), path, opt))
	switch {
	case strings.Contains(out, "FAIL  the box has nc"):
		refdriver.NoNC(t, out)
	case !strings.Contains(out, "FAIL  two boxes cannot reach each other") || !strings.Contains(out, "stopped listening on port "+port+" before the scan ended"):
		t.Fatalf("a listener that stopped during the scan:\n%s", out)
	}
}

// A loaded guest's nc can take over a second to listen (zm-eakh.12.82): the
// probe's self and serve wait for their listener instead of reading "cannot
// listen" half a second in. An nc on PATH that sleeps 1 s before any -l
// stands in for it; ip names no /24, so each peers scan finds nothing.
func TestNetSlowListener(t *testing.T) {
	ncPath, err := exec.LookPath("nc")
	if err != nil {
		refdriver.NoNC(t, "a slow nc wraps the real one")
	}
	bin := t.TempDir()
	slow := "#!/bin/sh\nfor a in \"$@\"; do case $a in -h) ;; -*l*) sleep 1; break ;; esac; done\nexec " + ncPath + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "nc"), []byte(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for kind, peers := range map[string]string{"box": "boxes", "run": "runs"} {
		t.Run(kind, func(t *testing.T) {
			path, opt := fixture(t, kind, "")
			_, free := listeners(t)
			_, port, _ := net.SplitHostPort(free)
			_, closed := listeners(t)
			opt.Only, opt.NetForbid, opt.NetPeers = []string{"net"}, []string{closed}, true
			opt.NetPeerPort, _ = strconv.Atoi(port)
			out := Format(Run(context.Background(), path, opt))
			for _, want := range []string{"PASS  the net probe sees a port open in the guest", "PASS  " + fmt.Sprintf(peersCheck, peers)} {
				if !strings.Contains(out, want) {
					t.Fatalf("an nc slow to listen: no %q:\n%s", want, out)
				}
			}
		})
	}
}

// serveListen is the probe's own serve deadline: the peers checks wait it out.
func TestServeListenIsTheProbes(t *testing.T) {
	if want := fmt.Sprintf(`listening "$3" %d k`, int(serveListen/time.Second)); !strings.Contains(ProbeScript, want) {
		t.Fatalf("ProbeScript's serve has no %q: serveListen and the script disagree", want)
	}
}

// A run guest with bash and timeout but no nc passes the first control and
// then cannot listen: that FAILs with the plain no-nc message, as for boxes,
// never a SKIP that would pass the net group untested. PATH is /bin and
// /usr/bin without nc, which the reference run inherits.
func TestRunNetNeedsNC(t *testing.T) {
	path, opt := fixture(t, "run", "")
	bin := t.TempDir()
	for _, d := range []string{"/bin", "/usr/bin"} {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			switch n := e.Name(); n {
			case "nc", "ncat", "netcat":
			default:
				_ = os.Symlink(filepath.Join(d, n), filepath.Join(bin, n))
			}
		}
	}
	if _, err := os.Stat(filepath.Join(bin, "timeout")); err != nil {
		t.Skip("no timeout in /bin or /usr/bin: the first control cannot pass without nc here")
	}
	t.Setenv("PATH", bin)
	_, closed := listeners(t)
	opt.Only, opt.NetForbid = []string{"net"}, []string{closed}
	res := Run(context.Background(), path, opt)
	if out := Format(res); !strings.Contains(out, "FAIL  the net probe sees a port open in the guest: this image has no nc") || Failed(res) == 0 {
		t.Fatalf("a run without nc:\n%s", out)
	}
}

func TestResolveTargets(t *testing.T) {
	for _, c := range []struct {
		allow, resolve []string
		want           string
	}{
		{nil, nil, ""}, // no network claimed: nothing to resolve
		{[]string{"140.82.112.3:443", "[2606:4700::1111]:443", "[fe80::1%eth0]:443"}, nil, "github.com:443"},
		{[]string{"140.82.112.3:443", "example.com:80", "example.com:80"}, nil, "example.com:80"},
		{[]string{"example.com:80"}, []string{"pypi.org", "localhost:8080"}, "pypi.org:443 localhost:8080"},
		{nil, []string{"pypi.org"}, "pypi.org:443"},
	} {
		if got := strings.Join(Options{NetAllow: c.allow, NetResolve: c.resolve}.resolveTargets(), " "); got != c.want {
			t.Errorf("allow %v resolve %v: %q, want %q", c.allow, c.resolve, got, c.want)
		}
	}
	for _, ok := range []string{"github.com", "localhost", "a.b-c.example:53", "github.com."} {
		if err := ResolveName(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1.1.1.1", "1.1.1.1:53", "[::1]:53", "[fe80::1%eth0]:53", "-x", "a b", "a;b", "h:0", "x/y"} {
		if err := ResolveName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A guest that reaches addresses but resolves no name (zm-eakh.12.24.7: a
// gVisor box whose resolv.conf named Docker's 127.0.0.11) fails the net
// group, not the first job. The reference box is this machine; a getent on
// its PATH that resolves nothing stands in for the broken resolver. A name
// this machine cannot resolve is a SKIP.
func TestBoxNetResolveFails(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "getent"), []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path, opt := fixture(t, "box", "")
	open, _ := listeners(t)
	_, port, _ := net.SplitHostPort(open)
	opt.Only, opt.NetAllow = []string{"net"}, []string{open}
	opt.NetResolve = []string{"localhost:" + port, "no-such-host.invalid"}
	res := Run(context.Background(), path, opt)
	var reached, broken, unproven bool
	for _, r := range res {
		switch r.Name {
		case "a box reaches " + open:
			reached = r.OK
		case "a box resolves localhost (DNS)":
			broken = !r.OK && !r.Skipped && strings.Contains(r.Detail, "localhost does not resolve inside the box")
		case "a box resolves no-such-host.invalid (DNS)":
			unproven = r.Skipped && strings.Contains(r.Detail, "does not resolve on this machine")
		}
	}
	if !reached || !broken || !unproven || Failed(res) == 0 {
		t.Fatalf("reached %v, broken resolver failed %v, unresolvable here skipped %v:\n%s", reached, broken, unproven, Format(res))
	}
}

// Without getent the probe can only try a connection to name:port, so a port
// that does not open says so (exit 7), never "does not resolve". PATH is
// /bin and /usr/bin without getent, which the reference box inherits.
func TestBoxNetResolveWithoutGetent(t *testing.T) {
	bin := t.TempDir()
	for _, d := range []string{"/bin", "/usr/bin"} {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			if n := e.Name(); n != "getent" {
				_ = os.Symlink(filepath.Join(d, n), filepath.Join(bin, n))
			}
		}
	}
	if _, err := os.Stat(filepath.Join(bin, "nc")); err != nil {
		refdriver.NoNC(t, "no nc in /bin or /usr/bin")
	}
	t.Setenv("PATH", bin)
	path, opt := fixture(t, "box", "")
	open, closed := listeners(t)
	_, oport, _ := net.SplitHostPort(open)
	_, cport, _ := net.SplitHostPort(closed)
	opt.Only, opt.NetResolve = []string{"net"}, []string{"localhost:" + oport}
	if out := Format(Run(context.Background(), path, opt)); !strings.Contains(out, "PASS  a box resolves localhost (DNS)") {
		t.Fatalf("an open name:port without getent:\n%s", out)
	}
	opt.NetResolve = []string{"localhost:" + cport}
	if out := Format(Run(context.Background(), path, opt)); !strings.Contains(out, "FAIL  a box resolves localhost (DNS): localhost:"+cport+" did not open") ||
		!strings.Contains(out, "no getent") || strings.Contains(out, "does not resolve inside") {
		t.Fatalf("a closed name:port without getent:\n%s", out)
	}
}

// A driver that lists net-off (it honours acquire's net, zm-eakh.12.5.19)
// gets one more check: a box acquired with net off reaches nothing of
// --net-allow. The reference box runs on this machine and isolates nothing,
// so it fails on the open address and passes on the closed one; one that
// does not list the capability gets no such check.
func TestBoxNetOffCheck(t *testing.T) {
	open, closed := listeners(t)
	for _, honours := range []bool{true, false} {
		path, opt := fixture(t, "box", "")
		if honours {
			opt.Config["net_off"] = "1"
		}
		opt.Only, opt.NetAllow = []string{"net"}, []string{open, closed}
		res := Run(context.Background(), path, opt)
		want := map[string]bool{
			"a box made with net off for the net checks":                    true,
			"a box made with net off: its probe sees a port open inside it": true,
			"a box made with net off cannot reach " + open:                  false,
			"a box made with net off cannot reach " + closed:                true,
		}
		seen := 0
		for _, r := range res {
			if w, ok := want[r.Name]; ok {
				seen++
				if r.Skipped || r.OK != w {
					t.Errorf("net-off %v: %s: ok=%v skipped=%v (%s), want ok=%v", honours, r.Name, r.OK, r.Skipped, r.Detail, w)
				}
			}
		}
		if honours && seen != len(want) {
			t.Errorf("saw %d of %d net-off checks:\n%s", seen, len(want), Format(res))
		}
		if !honours && seen != 0 {
			t.Errorf("a driver without net-off got the net-off checks:\n%s", Format(res))
		}
	}
	// Without --net-allow there is nothing to try: a SKIP, never a pass.
	path, opt := fixture(t, "box", "")
	opt.Config["net_off"] = "1"
	opt.Only, opt.NetForbid = []string{"net"}, []string{closed}
	if out := Format(Run(context.Background(), path, opt)); !strings.Contains(out, "SKIP  a box made with net off reaches nothing") {
		t.Errorf("net-off with no --net-allow:\n%s", out)
	}
}
