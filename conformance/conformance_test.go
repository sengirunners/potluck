package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance/refdriver"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == boxClientArg { // the client in a box (group box-authority)
		os.Exit(boxClientMain())
	}
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	dockerUp = 3 * time.Second // root.go: the stand-in docker answers at once, or never
	os.Exit(withTempHome(m))
}

// withTempHome gives drivers and git a throwaway HOME: never the real one.
func withTempHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "zmrc-home")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(home)
	os.Setenv("HOME", home)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return m.Run()
}

// fileDB is the database probe for refdriver: a database is a file named in the url.
func fileDB(_ context.Context, raw string) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(u.Query().Get("file"))
	return err == nil, nil
}

// fixture installs refdriver with bug and answers its path and options.
func fixture(t *testing.T, kind, bug string) (string, Options) {
	t.Helper()
	dir, err := os.MkdirTemp("", "zmrc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	state := filepath.Join(dir, "state")
	path, err := refdriver.Install(dir, state, bug)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	// ReclaimIn must outlast an acquire and a status on a loaded CI runner; the
	// driver reads the window to the second, so it may close up to 1 s early.
	opt := Options{Kind: kind, ShortTTL: 2, Grace: 2 * time.Second, RunWait: 10 * time.Second,
		ReclaimConfig: map[string]string{"window": "{window}"}, ReclaimIn: 6 * time.Second, StateDirs: []string{state}}
	if kind == "db" {
		opt.DBExists = fileDB
		opt.Config = map[string]string{"server": "127.0.0.1:5432/postgres", "create_in": "127.0.0.1:5432/postgres"}
		opt.OutsideConfig = map[string]string{"server": "127.0.0.1:{port}/postgres"}
		return path, opt
	}
	repo, err := refdriver.NewRepo(dir, ProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	opt.Ref, opt.Commit, opt.Script, opt.Worktree, opt.Repo = "main", repo.Commit, "probe.sh", repo.Worktree, repo.Remote
	opt.Config = map[string]string{"remote": repo.Remote, "worktree": repo.Worktree}
	if kind == "box" { // a box takes a ref and commit only for its checkout
		opt.Script, opt.Worktree = "", ""
		// The boxes see the host's /tmp: a symlinked directory (as macOS
		// /tmp is) stands in for the shared roots, and the refdriver's
		// host_tmp bug leaves its copies there.
		tmp := filepath.Join(dir, "tmp")
		if err := os.Mkdir(tmp+"-real", 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tmp+"-real", tmp); err != nil {
			t.Fatal(err)
		}
		opt.LeftoverRoots = []string{tmp}
		opt.Config = map[string]string{"remote": repo.Remote, "host_tmp": tmp}
	}
	return path, opt
}

// proven holds which checks a good driver passed and which a broken one
// failed: TestBrokenDrivers ends by requiring the first to be inside the second.
var proven = struct {
	sync.Mutex
	passed, failed map[string]bool
}{passed: passedSet, failed: failedSet}

func record(res []Result, into map[string]bool) {
	proven.Lock()
	defer proven.Unlock()
	for _, r := range res {
		if (into["ok"] && r.OK && !r.Skipped) || (!into["ok"] && !r.OK) {
			into[r.Name] = true
		}
	}
}

var (
	passedSet = map[string]bool{"ok": true}
	failedSet = map[string]bool{}
)

func TestGoodDrivers(t *testing.T) {
	for _, c := range []struct {
		name, kind, bug string
		edit            func(*Options)
		wantSkip        string
		wantRan         []string // checks that must pass, not skip
	}{
		{name: "run", kind: "run"},
		{name: "db", kind: "db"},
		{name: "run without renew", kind: "run", bug: "no-renew", wantSkip: "the lifetime cap across renewals"},
		{name: "db without renew", kind: "db", bug: "no-renew", wantSkip: "the lifetime cap across renewals"},
		{name: "run without spend", kind: "run", bug: "no-spend", wantSkip: "spend in cost.unit"},
		{name: "remote run", kind: "run", edit: func(o *Options) { o.Remote = true }, wantSkip: "release kills the run's process group"},
		{name: "run without a worktree", kind: "run", edit: func(o *Options) { o.Worktree = "" }, wantSkip: "a dirty working tree"},
		{name: "db without a probe", kind: "db", edit: func(o *Options) { o.DBExists = nil }, wantSkip: "release drops the database"},
		{name: "db without outside config", kind: "db", edit: func(o *Options) { o.OutsideConfig = nil }, wantSkip: "a db host not in create_in"},
		{name: "db outside config without a port", kind: "db", edit: func(o *Options) { o.OutsideConfig = map[string]string{"server": "192.0.2.1:5432/postgres"} }, wantSkip: "a db host not in create_in"},
		{name: "db with strict names", kind: "db", bug: "strict-names"},
		{name: "run without --repo", kind: "run", edit: func(o *Options) { o.Repo = "" }, wantSkip: "a run's HEAD is the commit sent"},
		{name: "run without reclaim config", kind: "run", edit: func(o *Options) { o.ReclaimConfig = nil }, wantSkip: "a reclaim ends the resource"},
		{name: "box", kind: "box"},
		{name: "box database boundary", kind: "box", edit: func(o *Options) { o.Config["database"] = "1"; o.Only = []string{"database"} }, wantRan: []string{databaseRefusal}},
		{name: "box boot", kind: "box", edit: func(o *Options) { o.Only = []string{"boot"} }},
		{name: "run boot", kind: "run", edit: func(o *Options) { o.Only = []string{"boot"} }, wantSkip: "boot: box targets only"},
		{name: "box without a ref", kind: "box", edit: func(o *Options) { o.Ref, o.Commit = "", "" }, wantSkip: "a box's checkout is the commit sent, clean"},
		{name: "box without renew", kind: "box", bug: "no-renew", wantSkip: "the lifetime cap across renewals"},
		{name: "box without secrets", kind: "box", bug: "box-no-secrets", wantSkip: "exec secrets: a 0600 file"},
		{name: "box without box secrets", kind: "box", bug: "no-box-secrets", wantSkip: bsVisible},
		{name: "box reaching its authority", kind: "box", edit: withAuthority, wantRan: baChecks},
		{name: "box with no authority given", kind: "box", edit: func(o *Options) { o.Only = []string{"box-authority"} }, wantSkip: baReach},
		{name: "box without box secrets, an authority given", kind: "box", bug: "no-box-secrets", edit: withAuthority, wantSkip: baReach},
		{name: "box secrets without a state directory", kind: "box", edit: func(o *Options) { o.StateDirs = []string{}; o.Only = []string{"box-secrets"} }, wantSkip: bsNoState},
		{name: "box without background processes", kind: "box", bug: "box-no-spawn", wantSkip: "background processes"},
		{name: "box without terminals", kind: "box", bug: "box-no-pty", wantSkip: "terminals"},
		{name: "box without kept disks", kind: "box", bug: "no-persist", wantSkip: "kept disks"},
		{name: "box without root", kind: "box", bug: "no-root", wantSkip: "root and docker in the box"},
		{name: "box without volumes", kind: "box", bug: "no-volumes", wantSkip: "named volumes"},
		{name: "box without warm boxes", kind: "box", wantSkip: "warm boxes"},
		{name: "box without acquire progress", kind: "box", bug: "no-acquire-progress", edit: func(o *Options) { o.Only = []string{"acquire-progress"} }, wantSkip: "acquire progress and cancel"},
		{name: "box with a slow acquire", kind: "box", edit: slowAcquire, wantRan: []string{pgInFlight, pgCancel, pgAfter}},
		{name: "run with a slow acquire", kind: "run", edit: slowAcquire, wantRan: []string{pgInFlight, pgCancel, pgAfter}},
		{name: "db with a slow acquire", kind: "db", edit: slowAcquire, wantRan: []string{pgInFlight, pgCancel, pgAfter}},
		{name: "box without sealed volumes", kind: "box", edit: func(o *Options) { o.Config["seal"] = "0"; o.Only = []string{"volumes"} }, wantSkip: "sealed volumes"},
		{name: "box with warm boxes", kind: "box", edit: func(o *Options) { o.Config["warm"] = "1"; o.Only = []string{"warm", "lifecycle"} }},
		{name: "box with a shelf", kind: "box", edit: func(o *Options) { o.Config["shelf"] = "1"; o.Only = []string{"shelf", "lifecycle"} }},
		{name: "box with warm boxes and a shelf", kind: "box", edit: func(o *Options) {
			o.Config["warm"], o.Config["shelf"] = "1", "1"
			o.Only = []string{"warm", "shelf"}
		}},
		{name: "box without a shelf", kind: "box", wantSkip: "the package shelf"},
		{name: "box with root, no docker", kind: "box", bug: "no-docker", wantSkip: "docker in the box"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path, opt := fixture(t, c.kind, c.bug)
			if c.edit != nil {
				c.edit(&opt)
			}
			res := Run(context.Background(), path, opt)
			record(res, passedSet)
			out := Format(res)
			if n := Failed(res); n > 0 {
				t.Fatalf("%d failed:\n%s", n, out)
			}
			if c.wantSkip != "" && !strings.Contains(out, "SKIP  "+c.wantSkip) {
				t.Fatalf("want SKIP %q in:\n%s", c.wantSkip, out)
			}
			for _, name := range c.wantRan {
				if !strings.Contains(out, "PASS  "+name) {
					t.Fatalf("want PASS %q in:\n%s", name, out)
				}
			}
			if strings.Contains(out, "sekrit") {
				t.Fatalf("a secret in the results:\n%s", out)
			}
		})
	}
}

// Strict fails a run whose network or expiry checks skipped or did not run,
// and passes one where they all ran; without it nothing is added. The
// reference box is this machine, so a guest firewall (refdriver.Firewall)
// keeps it from the forbidden listener on this machine's LAN address, and an
// ip naming no /24 gives the peers scan nothing to reach. A forbidden address
// that means the guest itself inside a VM (loopback, unspecified, localhost,
// link-local) is a SKIP, which fails strict.
func TestStrict(t *testing.T) {
	open, _ := listeners(t)
	_, free := listeners(t)
	forbid, lanClosed, ok := lanListeners(t)
	if !ok {
		t.Skip("this machine has no non-loopback, non-link-local IPv4 address on an up interface: no forbidden address the suite tries")
	}
	_, port, _ := net.SplitHostPort(open)
	_, fport, _ := net.SplitHostPort(forbid)
	_, pport, _ := net.SplitHostPort(free)
	bin := refdriver.FirewallCI(t, t.TempDir(), forbid)
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	peerPort, _ := strconv.Atoi(pport)
	withNet := func(forbid string) func(*Options) {
		return func(o *Options) {
			o.NetAllow, o.NetForbid, o.NetResolve = []string{open}, []string{forbid}, []string{"localhost:" + port}
			o.NetPeers, o.NetPeerPort = true, peerPort
		}
	}
	notProven := "FAIL  " + StrictCheck + `: not proven: "a box cannot reach `
	for _, c := range []struct {
		name string
		edit func(*Options)
		want []string // lines that must be in the output
	}{
		{"all ran", withNet(forbid), []string{"PASS  " + StrictCheck, "PASS  " + boxGone, "PASS  a box cannot reach " + forbid}},
		{"net skipped, no --net-forbid or --net-peers", nil, []string{"FAIL  " + StrictCheck + `: not proven: "net" skipped (no --net-allow`,
			"no --net-forbid, so nothing showed the box cannot reach", "no --net-peers"}},
		{"a forbidden address closed here", withNet(lanClosed), []string{"SKIP  a box cannot reach " + lanClosed, "does not open from this machine either", notProven}},
		{"a loopback forbidden address", withNet("127.0.0.1:" + fport), []string{"SKIP  a box cannot reach 127.0.0.1:" + fport + ": not proven: 127.0.0.1:" + fport + " is this machine's loopback address", notProven}},
		{"an unspecified forbidden address", withNet("0.0.0.0:" + fport), []string{"is this machine's unspecified address", notProven}},
		{"localhost", withNet("localhost:" + fport), []string{"SKIP  a box cannot reach localhost:" + fport + " (", "is this machine's loopback address", notProven}},
		{"a zoned link-local forbidden address", withNet("[fe80::1%lo0]:" + fport), []string{"is this machine's link-local address", notProven}},
		{"not strict", func(o *Options) { withNet("127.0.0.1:" + fport)(o); o.Strict = false }, []string{"SKIP  a short ttl ends the resource with no release", "is this machine's loopback address"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, opt := fixture(t, "box", "")
			opt.Only, opt.Strict = []string{"net", "expiry"}, true
			if c.edit != nil {
				c.edit(&opt)
			}
			res := Run(context.Background(), path, opt)
			out := Format(res)
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("want %q in:\n%s", w, out)
				}
			}
			for _, r := range res { // a skip fails nothing but strict
				if !r.OK && r.Name != StrictCheck {
					t.Errorf("FAIL %s: %s", r.Name, r.Detail)
				}
			}
			if !opt.Strict && (strings.Contains(out, "strict") || strings.Contains(out, boxGone)) {
				t.Errorf("a strict check without Strict:\n%s", out)
			}
			if c.name == "all ran" {
				if Failed(res) > 0 || Skipped(res) > 0 {
					t.Errorf("failed or skipped:\n%s", out)
				}
				var mine []Result // the net checks' names carry this run's ports: no broken driver could match them
				for _, r := range res {
					if r.Name == StrictCheck || r.Name == boxGone {
						mine = append(mine, r)
					}
				}
				record(mine, passedSet)
			}
			for _, r := range res {
				if r.Name == boxGone && r.Group != "expiry" || r.Name == "nothing left behind" && r.Group != "" {
					t.Errorf("%q in group %q", r.Name, r.Group)
				}
			}
		})
	}
}

// A macOS box with root (Tart's root=on, zm-eakh.12.54.14) has no Linux
// bounding set: the root group skips those checks and proves instead that
// root's own shell cannot reach what the net checks forbid. The reference box
// is this machine, so a guest firewall (refdriver.Firewall) blocks the
// forbidden listener on its LAN address; bug root-past-fence gives root a
// shell outside it, and no forbidden address that proves something fails.
func TestRootMacGuest(t *testing.T) {
	forbid, _, ok := lanListeners(t)
	if !ok {
		t.Skip("this machine has no non-loopback, non-link-local IPv4 address on an up interface")
	}
	bin := refdriver.FirewallCI(t, t.TempDir(), forbid)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	const fence = "root in a macOS box cannot reach what the net checks forbid"
	for _, c := range []struct {
		name, bug string
		forbid    []string
		want      []string
		pass      bool
	}{
		{"fenced", "", []string{forbid}, []string{"PASS  " + fence, "PASS  sudo works in the box", "SKIP  root in the box holds no NET_ADMIN", "SKIP  root in the box holds no MKNOD"}, true},
		{"root past the fence", "root-past-fence", []string{forbid}, []string{"FAIL  " + fence, forbid + " opened from root's shell"}, false},
		{"nothing forbidden", "", nil, []string{"FAIL  " + fence, "nothing showed root in the box is still fenced"}, false},
		{"loopback only", "", []string{"127.0.0.1:9"}, []string{"FAIL  " + fence, "nothing showed root"}, false},
		{"no sudo", "root-no-sudo", []string{forbid}, []string{"FAIL  sudo works in the box", "FAIL  " + fence, "under sudo exited"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, opt := fixture(t, "box", c.bug)
			opt.Only, opt.NetForbid = []string{"root"}, c.forbid
			opt.Config["guest_os"] = "darwin"
			res := Run(context.Background(), path, opt)
			out := Format(res)
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("want %q in:\n%s", w, out)
				}
			}
			if c.pass {
				if Failed(res) > 0 {
					t.Errorf("failed:\n%s", out)
				}
				record(res, passedSet)
			} else {
				record(res, failedSet)
			}
		})
	}
}

// brokenDrivers: one bad driver, the group it runs and the line it must print.
var brokenDrivers = []struct {
	kind, bug, group, want string
	edit                   func(*Options)
}{
	{"box", "database-ignored", "database", "FAIL  " + databaseRefusal, nil},
	// describe and the envelope
	{"run", "describe-no-class", "handles", "FAIL  describe", nil},
	{"run", "describe-no-credentials", "handles", "FAIL  describe", nil},
	{"run", "describe-no-cost-unit", "handles", "FAIL  describe", nil},
	{"run", "describe-no-reports-spend", "handles", "FAIL  describe", nil},
	{"run", "describe-no-list", "handles", "FAIL  describe", nil},
	{"run", "unavailable", "handles", "FAIL  the target is available", nil},
	{"run", "stdout-noise", "handles", "FAIL  describe", nil},
	{"run", "unknown-verb-ok", "handles", "FAIL  an unknown verb", nil},
	{"run", "exit-1-on-refusal", "handles", "FAIL  an unknown verb", nil},
	{"run", "bad-code", "handles", "FAIL  an unknown verb", nil},
	// handles
	{"run", "malformed-not-found", "handles", "FAIL  a malformed handle", nil},
	{"run", "foreign-not-found", "handles", "FAIL  another binding's handle", nil},
	{"run", "unknown-bad-request", "handles", "FAIL  a handle never issued", nil},
	{"db", "release-unknown-ok", "handles", "FAIL  a handle never issued", nil},
	// timing
	{"run", "slow-status", "lifecycle", "FAIL  every verb answers", func(o *Options) { o.VerbLimit = 3 * time.Second }},
	{"run", "acquire-blocks", "lifecycle", "FAIL  every verb answers", func(o *Options) { o.AcquireLimit = 3 * time.Second }},
	{"run", "acquire-blocks", "lifecycle", "FAIL  acquire answers handle", func(o *Options) { o.AcquireLimit = 3 * time.Second }},
	// a run's lifecycle
	{"run", "no-commit-acquire", "lifecycle", "FAIL  acquire echoes commit", nil},
	{"run", "no-commit-status", "lifecycle", "FAIL  status answers running", nil},
	{"run", "handle-shape", "lifecycle", "FAIL  acquire answers handle", nil},
	{"run", "no-expires-at", "lifecycle", "FAIL  acquire answers handle", nil},
	{"run", "list-missing", "lifecycle", "FAIL  list shows the handle", nil},
	{"run", "list-no-key", "lifecycle", "FAIL  list shows the handle", nil},
	{"run", "new-resource", "lifecycle", "FAIL  the same request_id gives", nil},
	{"run", "new-after-release", "lifecycle", "FAIL  the same request_id after the end", nil},
	{"run", "release-exit-running", "lifecycle", "FAIL  release of a running run", nil},
	{"run", "status-released-exit", "lifecycle", "FAIL  status after release", nil},
	{"run", "release-twice-error", "lifecycle", "FAIL  release twice", nil},
	{"run", "release-twice-true", "lifecycle", "FAIL  release twice", nil},
	{"run", "release-noop", "lifecycle", "FAIL  release kills", nil},
	{"run", "kill-leader-only", "lifecycle", "a child of the run is still going", nil},
	// a run that ends on its own
	{"run", "ended-wrong-exit", "ended", "FAIL  status after the script exits", nil},
	{"run", "release-no-exit", "ended", "FAIL  release of a run already ended", nil},
	// the code a run gets
	{"run", "wrong-head", "code", "FAIL  a run's HEAD is the commit sent", nil},
	{"run", "ended-wrong-exit", "code", "FAIL  the probe can fail", nil},
	{"run", "ended-wrong-exit", "net", "FAIL  the net probe can fail", func(o *Options) { o.NetForbid = []string{"127.0.0.1:9"} }},
	{"run", "dirty", "code", "FAIL  a dirty working tree does not leak in", nil},
	{"run", "run-worktree", "code", "FAIL  a run's HEAD is the commit sent", nil},
	// renew
	{"run", "renew-unlisted", "renew", "FAIL  renew answers not_implemented", nil},
	{"run", "renew-no-cap", "renew", "FAIL  renew past the cap", nil},
	{"run", "renew-cap-per-call", "renew", "FAIL  the cap counts", nil},
	{"run", "renew-ignored", "renew", "FAIL  a renewed resource outlives", nil},
	{"db", "renew-ignored", "renew", "FAIL  a renewed resource outlives", nil},
	// expiry
	{"run", "no-deadline", "expiry", "FAIL  a short ttl ends", nil},
	{"run", "lazy-deadline", "expiry", "FAIL  a short ttl ends", nil},
	{"run", "early-kill", "expiry", "FAIL  a short ttl ends", func(o *Options) { o.ShortTTL = 6 }},
	{"run", "expired-exit-0", "expiry", "FAIL  status after the deadline", nil},
	{"run", "expired-as-ended", "expiry", "FAIL  status after the deadline", nil},
	{"db", "no-deadline", "expiry", "FAIL  a short ttl ends", nil},
	{"db", "lazy-deadline", "expiry", "FAIL  a short ttl ends", nil},
	// reclaim
	{"run", "no-reclaim", "reclaim", "FAIL  a reclaim ends the resource", nil},
	{"run", "lazy-reclaim", "reclaim", "FAIL  a reclaim ends the resource", nil},
	{"run", "reclaim-as-lost", "reclaim", "FAIL  status after a reclaim", nil},
	{"run", "reclaim-exit", "reclaim", "FAIL  status after a reclaim", nil},
	{"db", "no-reclaim", "reclaim", "FAIL  a reclaim ends the resource", nil},
	{"db", "lazy-reclaim", "reclaim", "FAIL  a reclaim ends the resource", nil},
	{"run", "list-stale", "reclaim", "FAIL  list shows it reclaimed", nil},
	{"run", "release-reclaimed-true", "reclaim", "FAIL  release after a reclaim", nil},
	{"run", "reclaim-available", "reclaim", "FAIL  describe while the owner has it", nil},
	{"run", "reclaim-acquire-ok", "reclaim", "FAIL  acquire while the owner has it", nil},
	{"run", "acquire-refused", "reclaim", "FAIL  acquire while the owner lets it", nil},
	{"run", "die-at-once", "reclaim", "FAIL  alive before the owner takes it back", nil},
	// spend
	{"run", "spend-falls", "lifecycle", "FAIL  spend in cost.unit", nil},
	{"run", "spend-unit", "lifecycle", "FAIL  spend in cost.unit", nil},
	{"run", "spend-gone", "lifecycle", "FAIL  spend in cost.unit", nil},
	{"run", "spend-undeclared", "lifecycle", "FAIL  spend in cost.unit", nil},
	// refusals
	{"run", "no-request-id-ok", "refuse", "FAIL  acquire without request_id", nil},
	{"run", "ttl-no-cap", "refuse", "FAIL  a ttl_s past", nil},
	{"run", "shell-string", "refuse", "FAIL  a shell-string script", nil},
	{"db", "ttl-no-cap", "refuse", "FAIL  a ttl_s past", nil},
	{"db", "create-in-ok", "refuse", "FAIL  a db host not in create_in", nil},
	{"db", "create-in-connects", "refuse", "FAIL  a db host not in create_in", nil},
	{"db", "leak", "refuse", "FAIL  nothing left behind", nil},
	// a database's lifecycle
	{"db", "no-url", "lifecycle", "FAIL  acquire answers handle", nil},
	{"db", "no-conflict", "lifecycle", "FAIL  an existing name", nil},
	{"db", "new-resource", "lifecycle", "FAIL  the same request_id gives", nil},
	{"db", "release-noop", "lifecycle", "FAIL  release drops the database", nil},
	{"db", "url-on-stderr", "lifecycle", "FAIL  no url on stderr", nil},
	{"db", "url-in-status", "lifecycle", "FAIL  url only in acquire answers", nil},
	{"db", "no-name-ok", "refuse", "FAIL  acquire without a name", nil},
	{"db", "list-missing", "lifecycle", "FAIL  list shows the handle", nil},
	{"db", "db-fake", "lifecycle", "FAIL  the database is there", nil},
	{"db", "db-prefix-handle", "lifecycle", "FAIL  the handle names the caller's database", nil},
	{"db", "status-wrong-state", "lifecycle", "FAIL  status answers ready", nil},
	{"db", "release-false-first", "lifecycle", "FAIL  release of a ready database", nil},
	// what a driver can get wrong once in each group
	{"run", "malformed-crash", "handles", "FAIL  a malformed request", nil},
	{"run", "list-no-array", "handles", "FAIL  list answers a handles array", nil},
	{"run", "fake-run", "lifecycle", "FAIL  the run is going", func(o *Options) { o.RunWait = time.Second }},
	{"run", "acquire-refused", "lifecycle", "FAIL  acquire answers handle", nil},
	{"run", "acquire-refused", "ended", "FAIL  acquire a run that exits 7", nil},
	{"run", "acquire-refused", "renew", "FAIL  acquire with a short ttl", nil},
	{"run", "list-stale", "ended", "FAIL  list shows it ended", nil},
	{"run", "list-stale", "expiry", "FAIL  list shows it expired", nil},
	{"run", "release-ended-once", "ended", "FAIL  release twice of a run already ended", nil},
	{"run", "release-expired-true", "expiry", "FAIL  release after expiry", nil},
	{"run", "die-at-once", "expiry", "FAIL  alive before its deadline", nil},
	{"run", "renew-same", "renew", "FAIL  renew answers a later expires_at", nil},
	{"run", "renew-no-ttl-ok", "renew", "FAIL  renew without ttl_s", nil},
	{"run", "renew-ignored", "renew", "FAIL  status answers the renewed expires_at", nil},
	// a box
	{"box", "port-any-port", "ports", "FAIL  " + portArguments, nil},
	{"box", "port-no-page-cap", "ports", "FAIL  " + portArguments, nil},
	{"box", "port-half-duplex", "ports", "FAIL  " + portDuplex, nil},
	{"box", "port-cross-stream", "ports", "FAIL  " + portForeign, nil},
	{"box", "port-close-ignored", "ports", "FAIL  " + portClosed, nil},
	{"box", "port-after-release", "ports", "FAIL  " + portLease, nil},

	{"box", "box-no-exec", "handles", "FAIL  describe", nil},
	{"box", "box-credentialed", "handles", "FAIL  describe", nil},
	{"box", "box-no-size", "handles", "FAIL  describe", nil},
	{"box", "handle-shape", "lifecycle", "FAIL  acquire answers handle", nil},
	{"box", "box-no-size-acquire", "lifecycle", "FAIL  acquire answers its size and image_digest", nil},
	{"box", "handle-shape", "boot", "FAIL  acquire answers handle", nil},
	{"box", "box-no-size-acquire", "boot", "FAIL  acquire answers its size and image_digest", nil},
	{"box", "box-exec-fake", "boot", "FAIL  exec runs the command", nil},
	{"box", "release-false-first", "boot", "FAIL  release of a ready box", nil},
	{"box", "box-exec-fake", "lifecycle", "FAIL  exec runs the command", nil},
	{"box", "box-exec-fake", "net", "FAIL  the net probe can fail", func(o *Options) { o.NetForbid = []string{"127.0.0.1:9"} }},
	{"box", "box-no-stdin", "lifecycle", "FAIL  exec passes stdin", nil},
	{"box", "box-exec-fresh", "lifecycle", "FAIL  the box keeps its files across exec", nil},
	{"box", "box-no-timeout", "lifecycle", "FAIL  exec past timeout_s", nil},
	{"box", "box-kill-leader-only", "lifecycle", "FAIL  exec past timeout_s", nil},
	{"box", "box-no-cap", "lifecycle", "FAIL  exec keeps the first 1 MiB", nil},
	{"box", "box-cap-tail", "lifecycle", "FAIL  exec keeps the first 1 MiB", nil},
	{"box", "box-stderr-uncapped", "lifecycle", "FAIL  exec keeps the first 1 MiB", nil},
	{"box", "release-false-first", "lifecycle", "FAIL  release of a ready box", nil},
	{"box", "box-exec-after-release", "lifecycle", "FAIL  exec after release answers conflict", nil},
	{"box", "box-exec-after-release", "expiry", "FAIL  exec after the deadline answers conflict", nil},
	{"box", "box-reuse", "lifecycle", "FAIL  a new box does not see the last box's files", nil},
	{"box", "box-dir-kept", "lifecycle", "FAIL  a new box does not see the last box's files", nil}, // each box its own HOME, the last one's kept
	{"box", "tmp-kept", "lifecycle", "FAIL  a new box does not see the last box's files", nil},     // HOME and TMPDIR wiped, /tmp kept (zm-eakh.12.65.17)
	{"box", "wrong-head", "code", "FAIL  a box's checkout is the commit sent", nil},
	{"box", "box-dirty-checkout", "code", "FAIL  a box's checkout is the commit sent, clean", nil},
	{"box", "box-any-image", "refuse", "FAIL  an image not in the binding's images", nil},
	{"box", "box-shell-string", "refuse", "FAIL  exec refuses a shell string", nil},
	{"box", "box-empty-argv", "refuse", "FAIL  exec refuses a shell string", nil},
	{"box", "box-timeout-no-cap", "refuse", "FAIL  exec refuses a shell string", nil},
	{"box", "no-deadline", "expiry", "FAIL  status after the deadline", nil},
	{"box", "lazy-deadline", "expiry", "FAIL  " + boxGone + ": state \"ready\"", strict}, // ends a box only when status asks
	// strict: a run with the network checks not run fails it
	{"box", "", "expiry", "FAIL  " + StrictCheck + ": not proven: group net did not run", func(o *Options) { o.Strict = true }},
	{"box", "no-reclaim", "reclaim", "FAIL  status after a reclaim", nil},
	// renter secrets on exec
	{"box", "secrets-ignored", "lifecycle", "FAIL  an exec with secrets on a driver without the secrets capability", nil},
	{"box", "secrets-env", "lifecycle", "FAIL  exec secrets: a 0600 file", nil},
	{"box", "secrets-argv", "lifecycle", "FAIL  exec secrets: a 0600 file", nil},
	{"box", "secrets-left", "lifecycle", "FAIL  exec secrets: a 0600 file", nil},
	{"box", "secrets-loose", "lifecycle", "FAIL  exec secrets: a 0600 file", nil},
	{"box", "secrets-stderr", "lifecycle", "FAIL  no secret on stderr or in any answer", nil},
	{"box", "secrets-any-name", "lifecycle", "FAIL  exec refuses bad secret names", nil},
	{"box", "secrets-small-cap", "lifecycle", "FAIL  exec refuses bad secret names", nil},
	{"box", "secrets-name-echo", "lifecycle", "FAIL  no secret on stderr or in any answer", nil},
	{"box", "secrets-left-on-timeout", "lifecycle", "FAIL  a timed-out command's secrets are gone", nil},
	// box secrets (zm-eakh.12.48.3)
	{"box", "box-secrets-unlisted", "box-secrets", "FAIL  " + bsRefused, nil},
	{"box", "box-secrets-not-visible", "box-secrets", "FAIL  " + bsVisible, nil},
	{"box", "box-secrets-world-readable", "box-secrets", "FAIL  " + bsModes, nil},
	{"box", "box-secrets-on-disk", "box-secrets", "FAIL  " + bsMemory, nil},
	{"box", "box-secrets-in-env", "box-secrets", "FAIL  " + bsNoEnv, nil},
	{"box", "box-secrets-in-argv", "box-secrets", "FAIL  " + bsNoEnv, nil},
	{"box", "box-secrets-in-state", "box-secrets", "FAIL  " + bsNoState, nil},
	{"box", "box-secrets-in-answer", "box-secrets", "FAIL  no secret on stderr or in any answer", nil},
	{"box", "box-secrets-no-unset", "box-secrets-ops", "FAIL  " + bsUnset, nil},
	// box authority (zm-eakh.12.48.40.24)
	{"box", "box-secrets-not-visible", "box-authority", "FAIL  " + baReach, withAuthority},
	{"box", "box-secrets-no-unset", "box-authority", "FAIL  " + baGone, withAuthority},
	{"box", "box-secrets-exec-clobbers", "box-secrets-ops", "FAIL  " + bsOwn, nil},
	{"box", "box-secrets-partial-write", "box-secrets-ops", "FAIL  " + bsAtomic, nil},
	{"box", "box-secrets-any-name", "box-secrets-ops", "FAIL  " + bsRefuse, nil},
	{"box", "box-secrets-after-release", "box-secrets-ops", "FAIL  " + bsConflict, nil},
	{"box", "box-secrets-survive-release", "box-secrets", "FAIL  " + bsGone, nil},
	{"box", "box-secrets-swapped", "box-secrets", "FAIL  " + bsSwap, nil},
	{"box", "box-secrets-wrong-owner", "box-secrets", "FAIL  " + bsOwner, nil},
	{"box", "box-secrets-follows-links", "box-secrets", "FAIL  " + bsPlanted, nil},
	{"box", "box-secrets-any-owner", "box-secrets", "FAIL  " + bsForeign, nil},
	{"box", "box-secrets-kept-on-stop", "persist", "FAIL  " + bsStop, nil},
	{"box", "", "box-secrets", fmt.Sprintf("%q skipped: give --state-dir", bsNoState), func(o *Options) { o.Strict, o.StateDirs = true, []string{} }},
	// background processes and base64
	{"box", "base64-text-out", "lifecycle", "FAIL  exec encoding base64", nil},
	{"box", "base64-text-out", "procs", "FAIL  encoding base64: stdin and output", nil},
	{"box", "spawn-unlisted", "procs", "FAIL  spawn answers not_implemented when not listed", nil},
	{"box", "spawn-only", "procs", "FAIL  a driver listing spawn lists output and signal", nil},
	{"box", "acquire-refused", "procs", "FAIL  acquire a box for background processes", nil},
	{"box", "spawn-no-pid", "procs", "FAIL  spawn answers a pid", nil},
	{"box", "spawn-same-pid", "procs", "FAIL  another request_id answers another pid", nil},
	{"box", "spawn-not-idempotent", "procs", "FAIL  the same request_id answers the same pid", nil},
	{"box", "output-no-wait", "procs", "FAIL  output waits up to wait_s", nil},
	{"box", "signal-leader-only", "procs", "FAIL  signal TERM reaches the process group", nil},
	{"box", "signal-sent-always", "procs", "FAIL  signal after the end answers sent false", nil},
	// the agent runner job kind (agentrunners)
	{"box", "acquire-refused", "agent-runner", "FAIL  acquire a box for agent runner jobs", nil},
	{"box", "box-no-stdin", "agent-runner", "FAIL  agent runner job: its files arrive", nil},
	{"box", "secrets-loose", "agent-runner", "FAIL  agent runner job: its files arrive", nil},
	{"box", "spawn-secrets-early", "agent-runner", "FAIL  agent runner job: the key's file is still there", nil},
	{"box", "signal-term-as-kill", "agent-runner", "FAIL  agent runner job: TERM lets the runner hand back its work", nil},
	{"box", "output-no-cap", "procs", "FAIL  output pages 64 KiB by default", nil},
	{"box", "output-keep-little", "procs", "FAIL  output keeps a stream's newest 8 MiB", nil},
	{"box", "output-drop-unreported", "procs", "FAIL  output keeps a stream's newest 8 MiB", nil},
	{"box", "spawn-secrets-left", "procs", "FAIL  spawn secrets", nil},
	{"box", "output-blocks", "procs", "FAIL  output pages a 20 s process while it runs", nil},
	{"box", "procs-after-release", "procs", "FAIL  spawn, output and signal after release answer conflict", nil},
	// kept disks (zm-eakh.12.5.23)
	{"box", "persist-cap-unlisted", "persist", "FAIL  a driver listing stop and start lists capability persist", nil},
	{"box", "acquire-refused", "persist", "FAIL  acquire a box for kept disks", nil},
	{"box", "stop-any-box", "persist", "FAIL  stop of a box acquired without persist answers bad_request", nil},
	{"box", "stop-still-ready", "persist", "FAIL  stop keeps a persistent box's disk", nil},
	{"box", "persist-wipes", "persist", "FAIL  start brings the same disk back", nil},
	{"box", "release-stopped-false", "persist", "FAIL  release of a stopped box", nil},
	{"box", "persist-deadline-expires", "persist", "FAIL  a persistent box's deadline stops it", nil},
	// named volumes (zm-eakh.12.32.1)
	{"box", "acquire-refused", "volumes", "FAIL  acquire with a volume answers it", nil},
	{"box", "volume-no-answer", "volumes", "FAIL  acquire with a volume answers it", nil},
	{"box", "volume-rw-shared", "volumes", "FAIL  a rw volume is one box's", nil},
	{"box", "volume-wipes", "volumes", "FAIL  a volume outlives its box", nil},
	{"box", "volume-shared-owners", "volumes", "FAIL  another owner's volume of the same name is another volume", nil},
	{"box", "volume-ro-writable", "volumes", "FAIL  a ro volume refuses writes", nil},
	{"box", "volume-cow-writes-through", "volumes", "FAIL  a cow box's writes are its own", nil},
	{"box", "volume-any-path", "volumes", "FAIL  a volume named, placed or moded badly answers bad_request", nil},
	{"box", "volume-readers-exclusive", "volumes", "FAIL  ro and cow boxes share a volume", nil},
	{"box", "volume-rw-ignores-readers", "volumes", "FAIL  rw waits for readers", nil},
	{"box", "volume-key-ignores-volumes", "volumes", "FAIL  a request_id reused with other volumes or another owner", nil},
	// acquire progress and cancel
	{"box", "progress-cap-unlisted", "acquire-progress", "FAIL  " + pgCaps, nil},
	{"box", "progress-unknown-ok", "acquire-progress", "FAIL  " + pgUnknown, nil},
	{"box", "cancel-unknown-not-found", "acquire-progress", "FAIL  " + pgFirst, nil},
	{"box", "acquire-refused", "acquire-progress", "FAIL  acquire, watched with progress", nil},
	{"box", "progress-no-phase", "acquire-progress", "FAIL  " + pgInFlight, slowAcquire},
	{"box", "progress-not-found-in-flight", "acquire-progress", "FAIL  " + pgInFlight, slowAcquire},
	{"run", "progress-not-found-in-flight", "acquire-progress", "FAIL  " + pgCancel, slowAcquire},
	{"box", "progress-no-ready", "acquire-progress", "FAIL  " + pgReady, nil},
	{"box", "cancel-ready-true", "acquire-progress", "FAIL  " + pgLate, nil},
	{"box", "cancel-keeps-box", "acquire-progress", "FAIL  " + pgCancel, slowAcquire},
	{"run", "cancel-keeps-box", "acquire-progress", "FAIL  " + pgCancel, slowAcquire},
	{"box", "cancel-reacquire", "acquire-progress", "FAIL  " + pgAfter, slowAcquire},
	{"box", "cancel-in-flight-says-ready", "acquire-progress", "FAIL  " + pgCancel, slowAcquire},
	{"box", "cancel-failed-false", "acquire-progress", "FAIL  " + pgFailed, nil},
	{"box", "failed-overwrites-ready", "acquire-progress", "FAIL  " + pgLate, nil},
	{"box", "progress-waits", "acquire-progress", "FAIL  " + pgInFlight, slowAcquire},
	{"box", "progress-waits", "acquire-progress", "FAIL  " + pgCancel, slowAcquire},
	{"box", "cancel-waits", "acquire-progress", "FAIL  " + pgCancel, slowAcquire},
	// cow and ro views are each box's own (zm-eakh.12.65.2)
	{"box", "volume-cow-shared-upper", "volumes", "FAIL  cow siblings do not see each other's writes", nil},
	{"box", "volume-cow-left-behind", "volumes", "FAIL  a plain box after a cow box (and a ro box) sees nothing of them", nil},
	{"box", "volume-cow-reused", "volumes", "FAIL  a later cow box sees none of an ended cow box's writes", nil},
	{"box", "volume-copy-follows-links", "volumes", "FAIL  a symlink in a volume stays a symlink in a ro or cow view", nil},
	{"box", "volume-cow-left-in-tmp", "volumes", "FAIL  a plain box after a cow box (and a ro box) sees nothing of them", nil},
	{"box", "volume-cow-left-in-host-tmp", "volumes", "FAIL  a plain box after a cow box (and a ro box) sees nothing of them", nil},
	{"box", "volume-ro-sees-last-cow", "volumes", "FAIL  a later ro box sees none of an ended cow box's writes", nil},
	// sealed volumes (zm-eakh.12.65.3)
	{"box", "seal-missing-wrong-name", "volumes", "FAIL  a missing sealed volume identifies its name", nil},
	{"box", "seal-partial-kept", "volumes", "FAIL  a build volume that is not sealed is gone", nil},
	{"box", "seal-rw-allowed", "volumes", "FAIL  a sealed volume refuses rw and build", nil},
	{"box", "seal-not-checked", "volumes", "FAIL  sealed is refused for a volume that is not", nil},
	{"box", "seal-two-builders", "volumes", "FAIL  a second build waits", nil},
	{"box", "seal-cross-owner", "volumes", "FAIL  another owner's sealed volume of the same name is another volume", nil},
	{"box", "seal-failed-acquire-kept", "volumes", "FAIL  a refused acquire leaves no build volume behind", nil},
	{"box", "seal-before-end", "volumes", "FAIL  a build box's processes end before its volume is sealed", nil},
	{"box", "seal-every-volume", "volumes", "FAIL  sealed is refused for a volume that is not", nil},
	{"box", "seal-bad-args-ok", "volumes", "FAIL  sealed with rw or build, and build with persist, answer bad_request", nil},
	{"box", "seal-ignored-without-cap", "volumes", "FAIL  without volumes-seal, a volume asking sealed answers bad_request", func(o *Options) { o.Config["seal"] = "0" }},
	// a box's checkout in a cow volume (zm-eakh.12.65.6)
	{"box", "checkout-bad-args-ok", "volumes", "FAIL  checkout with rw, ro or build, or on two volumes, answers bad_request", nil},
	{"box", "checkout-volume-ignored", "volumes", "FAIL  a box's checkout goes into its cow volume marked checkout", nil},
	{"box", "checkout-volume-wipes", "volumes", "FAIL  a box's checkout goes into its cow volume marked checkout", nil},
	{"box", "checkout-volume-writes-through", "volumes", "FAIL  a box's checkout goes into its cow volume marked checkout", nil},
	{"box", "checkout-ignored-without-cap", "volumes", "FAIL  without volumes-seal, a volume asking checkout answers bad_request", func(o *Options) { o.Config["seal"] = "0" }},
	{"box", "checkout-volume-trusts-git", "volumes", "FAIL  a .git a checkout volume holds never changes the box's checkout", nil},
	// keep at seal (zm-eakh.12.65.21)
	{"box", "seal-keep-bad-args-ok", "volumes", "FAIL  keep without seal, or naming a path that is not a volume path, answers bad_request", nil},
	{"box", "seal-keep-ignored", "volumes", "FAIL  a release with seal and keep seals only the kept trees", nil},
	{"box", "seal-keep-follows-links", "volumes", "FAIL  a release with seal and keep seals only the kept trees", nil},
	// a sealed volume's setuid files and device nodes (zm-eakh.12.65.22)
	{"box", "seal-keeps-setuid", "volumes", "FAIL  a sealed volume carries no setuid or setgid file and no device node, FIFO or socket", nil},
	// the package shelf (zm-eakh.12.30.6)
	{"box", "shelf-no-path", "shelf", "FAIL  describe's shelf says where a box finds it", shelfOn},
	{"box", "shelf-writable", "shelf", "FAIL  the shelf refuses writes", shelfOn},
	{"box", "shelf-shared-copy", "shelf", "FAIL  a write to the shelf path reaches no other box", shelfOn},
	{"box", "shelf-sticky-link", "shelf", "FAIL  a write to the shelf path reaches no other box", shelfOn},
	{"box", "shelf-harvests-box", "shelf", "FAIL  the shelf takes nothing a box wrote", shelfOn},
	{"box", "shelf-cache-kept", "shelf", "FAIL  a shelf box leaves nothing behind", shelfOn},
	// warm boxes (zm-eakh.12.5.28)
	{"box", "warm-bad-describe", "warm", "FAIL  describe's warm says how many", warmOn},
	{"box", "warm-listed", "warm", "FAIL  a warm box is nobody's", warmOn},
	{"box", "warm-old-clock", "warm", "FAIL  a taken warm box's lease starts at the take", warmOn},
	{"box", "warm-no-refill", "warm", "FAIL  the pool refills after a take", warmOn},
	{"box", "warm-reused", "warm", "FAIL  a warm box is a fresh box", warmOn},
	{"box", "warm-volume-reused", "warm", "FAIL  a box with a volume is never a warm box's successor", warmOn},
	{"box", "warm-shelf-cache-kept", "warm", "FAIL  a warm box with a shelf is a fresh box", func(o *Options) { warmOn(o); shelfOn(o) }},
	{"box", "warm-tmp-kept", "warm", "FAIL  a warm box is a fresh box", warmOn}, // HOME wiped, /tmp kept (zm-eakh.12.65.17)
	{"box", "warm-reused-late", "warm", "FAIL  a warm box is a fresh box", warmOn},
	{"box", "warm-reused-late", "warm", "FAIL  a warm box is a fresh box", func(o *Options) { // a full binding never ends the takes early
		warmOn(o)
		o.Config["warm"], o.Config["boxes"] = "2", "1"
	}},
	// root and docker (zm-eakh.12.5.26)
	{"box", "docker-without-root", "root", "FAIL  a driver listing docker lists root", nil},
	{"box", "acquire-refused", "root", "FAIL  acquire a box for root and docker", nil},
	{"box", "root-no-sudo", "root", "FAIL  sudo works in the box", nil},
	{"box", "root-net-admin", "root", "FAIL  root in the box holds no NET_ADMIN", nil},
	{"box", "root-net-raw", "root", "FAIL  root in the box holds no NET_RAW", nil},
	{"box", "root-mknod", "root", "FAIL  root in the box holds no MKNOD", nil},
	{"box", "root-net-admin", "root", "PASS  root in the box holds no NET_RAW", nil}, // each bit under its own name
	{"box", "root-unlisted", "root", "FAIL  without capability root, sudo does not work in the box", nil},
	{"box", "", "root", "FAIL  acquire a box for root and docker: image zm/other@sha256:", func(o *Options) {
		o.Config["images"] = "ref-image,zm/other@sha256:" + strings.Repeat("ab", 32) // every bound image is checked, not the default alone
	}},
	{"box", "", "root", "FAIL  root in a macOS box cannot reach what the net checks forbid", func(o *Options) { o.Config["guest_os"] = "darwin" }}, // no --net-forbid: nothing proven, a FAIL (TestRootMacGuest: root past the fence)
	{"box", "docker-down", "root", "FAIL  docker run works in the box", nil},
	{"box", "docker-run-fails", "root", "FAIL  docker run works in the box", nil},
}

func strict(o *Options) { o.Strict = true }

// slowAcquire makes the reference driver's acquire take 6 s, past
// inFlightKnown, so the suite sees it in flight on a loaded machine; only the
// acquire-progress group runs.
func slowAcquire(o *Options) {
	o.Config["acquire_ms"] = "6000"
	o.Only = []string{"acquire-progress"}
}

func warmOn(o *Options) { o.Config["warm"] = "1"; o.RunWait = 3 * time.Second }

func shelfOn(o *Options) { o.Config["shelf"] = "1" }

// Every check can fail: each bad driver fails the check it breaks, and no
// check a good driver passes goes without a bad driver failing it.
func TestBrokenDrivers(t *testing.T) {
	t.Run("drivers", func(t *testing.T) {
		for _, c := range brokenDrivers {
			t.Run(c.kind+"/"+c.bug+"/"+c.group, func(t *testing.T) {
				t.Parallel()
				path, opt := fixture(t, c.kind, c.bug)
				opt.Only = []string{c.group}
				if c.edit != nil {
					c.edit(&opt)
				}
				res := Run(context.Background(), path, opt)
				record(res, failedSet)
				out := Format(res)
				if c.group == "ports" {
					t.Log(out)
				}
				if !strings.Contains(out, c.want) {
					t.Fatalf("want %q in:\n%s", c.want, out)
				}
				if strings.Contains(out, "sekrit") {
					t.Fatalf("a secret in the results:\n%s", out)
				}
			})
		}
	})
	if t.Failed() || len(proven.passed) < 2 { // -run picked part of the suite
		return
	}
	for name := range proven.passed {
		if name != "ok" && !proven.failed[name] {
			t.Errorf("no broken driver fails %q", name)
		}
	}
}

// Late tells a run that failed on time (a verb past its limit) from one that
// failed on a fault, reading the suite's own output: share calls the first a
// busy machine.
func TestLateReadsTheSuitesTimingFailure(t *testing.T) {
	for _, c := range []struct {
		bug  string
		edit func(*Options)
		late bool
	}{
		{"slow-status", func(o *Options) { o.VerbLimit = 3 * time.Second }, true},
		{"acquire-blocks", func(o *Options) { o.AcquireLimit = 3 * time.Second }, true},
		{"no-commit-status", nil, false},
	} {
		t.Run(c.bug, func(t *testing.T) {
			t.Parallel()
			path, opt := fixture(t, "run", c.bug)
			opt.Only = []string{"lifecycle"}
			if c.edit != nil {
				c.edit(&opt)
			}
			out := Format(Run(context.Background(), path, opt))
			if FirstFailure(out) == "" {
				t.Fatalf("the broken driver passed:\n%s", out)
			}
			if Late(out) != c.late {
				t.Fatalf("Late = %v, want %v:\n%s", !c.late, c.late, out)
			}
		})
	}
}

// The driver gets the allowlist and nothing else from the suite's environment.
func TestOnlyAllowlistedEnvironment(t *testing.T) {
	t.Setenv("ZM_CONFORMANCE_CANARY", "seen")
	t.Setenv("TZ", "UTC")
	path, opt := fixture(t, "run", "dump-env")
	opt.Only = []string{"handles"}
	if res := Run(context.Background(), path, opt); Failed(res) > 0 {
		t.Fatal(Format(res))
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), "state", "env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "CANARY") {
		t.Fatalf("the canary reached the driver:\n%s", b)
	}
	for _, k := range []string{"PATH=", "TZ=UTC", "HOME=" + os.Getenv("HOME")} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("%s did not reach the driver:\n%s", k, b)
		}
	}
}

// A driver that needs more than the allowlist fails, and never gets more.
func TestDriverNeedingMoreEnvironmentFails(t *testing.T) {
	t.Setenv("ZM_REF_EXTRA", "1")
	path, opt := fixture(t, "run", "needs-env")
	out := Format(Run(context.Background(), path, opt))
	if !strings.Contains(out, "FAIL  describe") {
		t.Fatal(out)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), "state", "env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ZM_REF_EXTRA") {
		t.Fatalf("a call carried the suite's whole environment:\n%s", b)
	}
}

// liveRecords reads refdriver's state: the handles still ready or running.
func liveRecords(t *testing.T, path string) []string {
	t.Helper()
	ps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "state", "h-*.json"))
	var live []string
	for _, p := range ps {
		var r struct{ Handle, State string }
		b, _ := os.ReadFile(p)
		if json.Unmarshal(b, &r) == nil && (r.State == "ready" || r.State == "running") {
			live = append(live, r.Handle)
		}
	}
	return live
}

// What a timed-out acquire made, or a driver made behind the suite's back,
// is ended before Run returns.
func TestRunEndsWhatItCannotSee(t *testing.T) {
	for _, c := range []struct {
		kind, bug, group string
		edit             func(*Options)
	}{
		{"run", "acquire-blocks", "lifecycle", func(o *Options) { o.AcquireLimit = 3 * time.Second }},
		{"db", "leak", "refuse", nil},
	} {
		t.Run(c.bug, func(t *testing.T) {
			t.Parallel()
			path, opt := fixture(t, c.kind, c.bug)
			opt.Only = []string{c.group}
			if c.edit != nil {
				c.edit(&opt)
			}
			res := Run(context.Background(), path, opt)
			if live := liveRecords(t, path); len(live) > 0 {
				t.Fatalf("left live %v:\n%s", live, Format(res))
			}
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "state", "db-*"))
			if len(files) > 0 {
				t.Fatalf("left %v:\n%s", files, Format(res))
			}
		})
	}
}

// A handle another run makes mid-run is not this run's: not counted, not ended.
func TestAnotherRunsHandleIsLeftAlone(t *testing.T) {
	path, opt := fixture(t, "db", "")
	opt.Only = []string{"lifecycle"}
	var once sync.Once
	var otherHandle string
	probe := opt.DBExists
	opt.DBExists = func(ctx context.Context, u string) (bool, error) {
		once.Do(func() {
			o := newRun(ctx, path, opt)
			defer o.close()
			o.cfg["kind"] = "db"
			a, e, err := o.acquire(o.dbArgs("someone-else", "someone_else", 600), o.cfg)
			if err != nil || e != nil {
				t.Errorf("other acquire: %v %v", e, err)
			}
			otherHandle = a.Handle
		})
		return probe(ctx, u)
	}
	res := Run(context.Background(), path, opt)
	if Failed(res) > 0 {
		t.Fatal(Format(res))
	}
	if live := liveRecords(t, path); len(live) != 1 || live[0] != otherHandle {
		t.Fatalf("live %v, want only %s", live, otherHandle)
	}
}

// A commit at the ref's tip proves nothing about a driver ignoring commit.
func TestHeadAtTipIsNotProven(t *testing.T) {
	for _, bug := range []string{"wrong-head", "run-worktree"} {
		t.Run(bug, func(t *testing.T) {
			t.Parallel()
			path, opt := fixture(t, "run", bug)
			opt.Commit = worktreeHead(opt.Worktree) // main's tip
			opt.Only = []string{"code"}
			opt.Worktree = ""
			out := Format(Run(context.Background(), path, opt))
			if !strings.Contains(out, "SKIP  a run's HEAD is the commit sent") || !strings.Contains(out, "--commit is the tip of --ref") {
				t.Fatal(out)
			}
		})
	}
}

func TestCredentialedTargetWarns(t *testing.T) {
	path, opt := fixture(t, "db", "credentialed")
	opt.Only = []string{"handles"}
	res := Run(context.Background(), path, opt)
	if out := Format(res); Failed(res) > 0 || !strings.Contains(out, "WARN  the class is credentialed or unisolated") {
		t.Fatal(out)
	}
}

func TestDriverThatKeepsItsPipesOpenFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "zm-run-held")
	body := "#!/bin/sh\nsleep 30 &\necho '{\"ok\":false,\"error\":{\"code\":\"unavailable\",\"message\":\"no\"}}'\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), p, Options{Kind: "run"})
	if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Detail, "kept stdout or stderr open") {
		t.Fatal(Format(res))
	}
}

// A driver that exits non-zero and leaves a child holding its pipes is failed
// for the exit, and the child does not outlive the call (as in the daemon).
func TestDriverExitingNonZeroLeavesNoChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	p := filepath.Join(dir, "zm-run-held-exit")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 300 &\necho $! > '"+pidFile+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), p, Options{Kind: "run"})
	if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Detail, "every answer exits 0") {
		t.Fatal(Format(res))
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) { // gone once reaped
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Error("the child the driver left outlived the failed call")
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func TestCrashingDriver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "zm-run-crash")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := Run(context.Background(), p, Options{}); len(res) != 1 || res[0].OK {
		t.Fatal(Format(res))
	}
	if k := Kind(context.Background(), p, Options{}); k != "" {
		t.Fatalf("kind %q", k)
	}
}

// A cancelled run still ends what it made.
func TestCancelledRunReleases(t *testing.T) {
	path, opt := fixture(t, "db", "")
	opt.Only = []string{"lifecycle"}
	ctx, cancel := context.WithCancel(context.Background())
	opt.DBExists = func(c context.Context, u string) (bool, error) {
		cancel() // right after the first acquire
		return fileDB(c, u)
	}
	res := Run(ctx, path, opt)
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "state", "db-*"))
	if len(files) != 0 {
		t.Fatalf("left %v:\n%s", files, Format(res))
	}
}

// An edit made while the worktree was dirty is kept, and undo says so.
func TestDirtyKeepsAnotherEdit(t *testing.T) {
	repo, err := refdriver.NewRepo(t.TempDir(), ProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(repo.Worktree, "probe.sh")
	undo, err := dirty(repo.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("someone else's edit\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := undo(); err == nil || !strings.Contains(err.Error(), "probe.sh") {
		t.Fatalf("undo: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "someone else's edit\n" {
		t.Fatalf("the edit was clobbered: %q", b)
	}
	if _, err := os.Stat(filepath.Join(repo.Worktree, "zm-run-conformance-dirty")); err == nil {
		t.Fatal("untracked file left")
	}
}

func TestDirtyPutsTheWorktreeBack(t *testing.T) {
	repo, err := refdriver.NewRepo(t.TempDir(), ProbeScript)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(repo.Worktree, "probe.sh"))
	undo, err := dirty(repo.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	during, _ := os.ReadFile(filepath.Join(repo.Worktree, "probe.sh"))
	if string(during) == string(before) {
		t.Fatal("tracked file not dirtied")
	}
	if err := undo(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(repo.Worktree, "probe.sh"))
	if string(after) != string(before) {
		t.Fatal("tracked file not put back")
	}
	if _, err := os.Stat(filepath.Join(repo.Worktree, "zm-run-conformance-dirty")); err == nil {
		t.Fatal("untracked file left")
	}
}

func TestValidate(t *testing.T) {
	if err := (Options{}).Validate("db"); err != nil {
		t.Fatal(err)
	}
	if err := (Options{Ref: "main"}).Validate("run"); err == nil || !strings.Contains(err.Error(), "--commit, --script") {
		t.Fatalf("got %v", err)
	}
	if err := (Options{Only: []string{"bogus"}}).Validate("db"); err == nil {
		t.Fatal("bad group passed")
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "zm-run-x")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, name := range []string{"x", "zm-run-x", p} {
		if got, err := Find(name); err != nil || got != p {
			t.Fatalf("%s: %s %v", name, got, err)
		}
	}
	if _, err := Find("nope"); err == nil {
		t.Fatal("found nope")
	}
	if _, err := Find(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("found a missing path")
	}
}

func TestSecretsLeaveResults(t *testing.T) {
	r := &run{out: []Result{{Name: "x", Detail: "refused postgres://u:hunter22@h/db"}}}
	r.addSecret("postgres://u:hunter22@h/db")
	if out := Format(r.scrubbed()); strings.Contains(out, "hunter22") {
		t.Fatal(out)
	}
	if !r.seen([]byte("pw=hunter22")) {
		t.Fatal("password alone not caught")
	}
}

// boot (zm host share's check of each size after the first) runs only when
// asked: a full run's lifecycle already covers it.
func TestBootOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	full, boot := newRun(ctx, "x", Options{}), newRun(ctx, "x", Options{Only: []string{"boot"}})
	defer full.close()
	defer boot.close()
	if full.group("boot") || !full.group("handles") {
		t.Errorf("a full run: boot %v, handles %v", full.group("boot"), full.group("handles"))
	}
	if !boot.group("boot") || boot.group("handles") {
		t.Errorf("--only boot: boot %v, handles %v", boot.group("boot"), boot.group("handles"))
	}
	path, opt := fixture(t, "box", "")
	opt.Only = []string{"boot"}
	out := Format(Run(ctx, path, opt))
	for _, want := range []string{"PASS  acquire answers handle", "PASS  acquire answers its size", "PASS  exec runs the command", "PASS  release of a ready box", "PASS  nothing left behind"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "a new box does not see") {
		t.Errorf("boot ran lifecycle:\n%s", out)
	}
}

// A result that does not fit is a failure line the driver cannot make long.
func TestDecodeBoundsAResultThatDoesNotFit(t *testing.T) {
	var out struct {
		N int `json:"n"`
	}
	stdout := []byte(`{"ok":true,"result":{"n":1` + strings.Repeat("0", 500) + `}}`)
	_, err := decode("describe", stdout, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "result does not decode") || len(err.Error()) > 200 {
		t.Fatalf("got %v", err)
	}
}
