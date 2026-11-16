package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostswap"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/vmhost"
	"github.com/sengirunners/potluck/wiring"
)

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok { // the reference driver runs as this binary
		os.Exit(code)
	}
	// A host's adverts carry its swap state: not this machine's, whatever runs the tests.
	hostswap.Local = func(context.Context) hostswap.Reading { return hostswap.Reading{State: hostswap.Encrypted} }
	UseHome()
	// A command must never start a daemon here: it would run this test binary.
	os.Setenv(EnvNoAutostart, "1")
	os.Exit(m.Run())
}

const refPin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// machine is one daemon wired as `potluck daemon` wires it (wiring.Compute)
// in a temp home, its tailnet node on tn.
type machine struct {
	name string
	home string
	dm   *server.Daemon
}

func startMachine(t *testing.T, tn *tailnettest.Net, name, login string) *machine {
	t.Helper()
	return startNode(t, tn, name, tailnettest.Identity{NodeID: "n" + name, NodeKey: "nodekey:" + name, Login: login, Host: name})
}

// startNode is startMachine with the tailnet identity given.
func startNode(t *testing.T, tn *tailnettest.Net, name string, id tailnettest.Identity) *machine {
	t.Helper()
	home, err := os.MkdirTemp("", "pl") // short: the socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = name+"-human", name
	cfg.File.Admission = config.Admission{MinMemoryFreePercent: 1, MaxSwapUsedMB: 1 << 40, MinDiskFreeMB: 1}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wiring.Compute)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(id)
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", name)
		}
		os.RemoveAll(home)
	})
	if cfg.SocketPath() != filepath.Join(home, "potluck.sock") {
		t.Fatalf("socket %s", cfg.SocketPath())
	}
	if err := client.NewUnix(cfg.SocketPath()).WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return &machine{name: name, home: home, dm: dm}
}

// run runs one potluck command line against m's daemon, the way the binary
// does (cli.Run, over the socket in $POTLUCK_HOME).
func (m *machine) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return m.runIn(t, "", args...)
}

// runIn is run with stdin.
func (m *machine) runIn(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv(EnvHome, m.home)
	var o, e bytes.Buffer
	code = Run(context.Background(), args, strings.NewReader(stdin), &o, &e)
	if os.Getenv("POTLUCK_TEST_SHOW") != "" {
		t.Logf("%s$ potluck %s\n%s%s(exit %d)", m.name, strings.Join(args, " "), o.String(), e.String(), code)
	}
	return o.String(), e.String(), code
}

// ok runs a command that must exit 0 and returns its stdout.
func (m *machine) ok(t *testing.T, args ...string) string {
	t.Helper()
	o, e, code := m.run(t, args...)
	if code != 0 {
		t.Fatalf("%s: potluck %s: exit %d\nstdout:\n%s\nstderr:\n%s", m.name, strings.Join(args, " "), code, o, e)
	}
	return o
}

func module[T any](t *testing.T, m *machine) T {
	t.Helper()
	for _, x := range m.dm.Modules() {
		if v, ok := x.(T); ok {
			return v
		}
	}
	var zero T
	t.Fatalf("%s: no %T module", m.name, zero)
	return zero
}

type runnerFunc func(ctx context.Context, name string, args ...string) (hostprobe.Result, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) (hostprobe.Result, error) {
	return f(ctx, name, args...)
}

var hostCodeRe = regexp.MustCompile(`potluck host '([^']+)'`)

// The quick start, end to end through the command tree: on two daemons
// wired as `potluck daemon` is, A creates a pool and invites B to share a
// machine; B joins and shares one (the reference driver, with doctor's
// facts, the image pull and conformance faked); A creates a sandbox on it,
// runs commands in the foreground and in the background, lists it and
// removes it; B stops sharing and leaves.
func TestQuickStartAcrossTwoMachines(t *testing.T) {
	oldBeat, oldRunner, oldDetect, oldDriver := teamlink.BeatEvery, shareRunner, detectHost, shareDriver
	t.Cleanup(func() {
		teamlink.BeatEvery, shareRunner, detectHost, shareDriver = oldBeat, oldRunner, oldDetect, oldDriver
	})
	teamlink.BeatEvery = time.Hour // B beats by hand, below
	var ran []string
	shareRunner = runnerFunc(func(_ context.Context, name string, args ...string) (hostprobe.Result, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"

	tn := tailnettest.New()
	a := startMachine(t, tn, "mini", "judy@example.com")
	b := startMachine(t, tn, "lender", "carl@example.com")

	// B's sandboxes run the reference driver.
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	module[*runtarget.Service](t, b).SetLookPath(func(string) (string, error) { return drv, nil })
	// host's conformance runs the driver in B's drivers directory (DRIVERS.md).
	inHome := filepath.Join(b.home, runtarget.DriversDir, "zm-run-ref")
	if err := os.MkdirAll(filepath.Dir(inHome), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, inHome); err != nil {
		t.Fatal(err)
	}
	stop := beat(module[*teamlink.Service](t, b))
	defer stop()

	// A: a pool and an invite for a machine.
	if out := a.ok(t, "create", "friends"); !strings.Contains(out, "pool friends is ready") || !strings.Contains(out, "sealing: required") {
		t.Fatalf("create:\n%s", out)
	}
	if out := a.ok(t, "sealing", "-P", "friends"); !strings.Contains(out, "pool friends: sealing required") {
		t.Fatalf("a new pool's sealing:\n%s", out)
	}
	if out := a.ok(t, "invite", "ana"); !strings.Contains(out, "potluck join '") {
		t.Fatalf("member invite:\n%s", out)
	}
	out := a.ok(t, "invite", "carl", "--host")
	m := hostCodeRe.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "their machine will be trusted") {
		t.Fatalf("host invite:\n%s", out)
	}
	if _, e, code := a.run(t, "invite", "sam", "--trust", "public"); code == 0 || !strings.Contains(e, "--trust goes with --host") {
		t.Fatalf("--trust without --host: %d %s", code, e)
	}

	// B: join, then share this machine.
	if out := b.ok(t, "join", m[1]); !strings.Contains(out, "joined pool friends") || !strings.Contains(out, "potluck host") {
		t.Fatalf("join:\n%s", out)
	}
	out = b.ok(t, "host", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "lender", "--reclaim-idle-s", "0", "--no-sudo")
	if !strings.Contains(out, "shared lender-c1m2") || !strings.Contains(out, "shared with pool friends: lender-c1m2@lender") {
		t.Fatalf("host:\n%s", out)
	}
	if len(ran) != 2 || !strings.HasPrefix(ran[0], "docker pull") || !strings.Contains(ran[1], "--only "+hostprobe.QuickGroups) ||
		!strings.Contains(ran[1], " "+inHome+" ") {
		t.Fatalf("host ran %q", ran)
	}
	if out := a.ok(t, "status"); !strings.Contains(out, "pool friends (owner): sandboxes on lender-c1m2@lender") {
		t.Fatalf("status at A:\n%s", out)
	}
	// isolation lists what each machine itself lends: B its size, once
	// (not again as the pool's advert of it, with or without --pool); A,
	// the pool's authority, nothing (B's advert is B's).
	for _, args := range [][]string{{"isolation"}, {"isolation", "-P", "friends"}} {
		out := b.ok(t, args...)
		if !strings.Contains(out, "lender-c1m2 (sandbox size): isolated") || strings.Contains(out, "@lender") || strings.Contains(out, "could not be read") {
			t.Fatalf("%q at B:\n%s", args, out)
		}
	}
	if out := a.ok(t, "isolation"); strings.Contains(out, "lender-c1m2") || strings.Contains(out, "could not be read") {
		t.Fatalf("isolation at A:\n%s", out)
	}

	// A: a dependency layer on B's binding: A keys it by the pin B
	// advertises and builds it on B. The reference driver's boxes run sh
	// here, so an npm on PATH stands in for the real one.
	proj := t.TempDir()
	for f, c := range map[string]string{"package-lock.json": `{"lockfileVersion":3,"packages":{"":{}}}`, "package.json": "{}"} {
		if err := os.WriteFile(filepath.Join(proj, f), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	npmBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(npmBin, "npm"), []byte("#!/bin/sh\nmkdir -p node_modules\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", npmBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(proj)
	o, e, code := a.run(t, "sandbox", "create", "--deps", "npm", "--ttl", "5m")
	depsID, _, _ := strings.Cut(strings.TrimSpace(o), " ")
	if code != 0 || !strings.HasPrefix(depsID, "lender-c1m2@lender/") || !strings.Contains(e, "potluck: deps npm at checkout: built") {
		t.Fatalf("deps on a lent machine: exit %d stdout %q stderr %q", code, o, e)
	}
	a.ok(t, "sandbox", "rm", depsID)
	if _, e, code := a.run(t, "sandbox", "create", "--deps-wait", "5"); code == 0 || !strings.Contains(e, "--deps-wait goes with --deps") {
		t.Fatalf("--deps-wait alone: exit %d %q", code, e)
	}

	// A: a sandbox on B's machine.
	out = a.ok(t, "sandbox", "create", "--ttl", "5m")
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !strings.HasPrefix(id, "lender-c1m2@lender/") || !strings.Contains(out, " ready ") {
		t.Fatalf("sandbox create:\n%s", out)
	}
	if o, e, code := a.run(t, "sandbox", "exec", id, "--", "sh", "-c", "echo hi > note; cat note; echo err >&2; exit 3"); code != 3 || o != "hi\n" || e != "err\n" {
		t.Fatalf("exec: exit %d stdout %q stderr %q", code, o, e)
	}
	if o := a.ok(t, "sandbox", "exec", id, "--", "cat", "note"); o != "hi\n" {
		t.Fatalf("the sandbox kept no state between commands: %q", o)
	}
	pid := strings.TrimSpace(a.ok(t, "sandbox", "exec", id, "--detach", "--", "sh", "-c", "echo one; sleep 1; echo two; exit 5"))
	if pid == "" {
		t.Fatal("exec --detach printed no pid")
	}
	if o, e, code := a.run(t, "sandbox", "logs", id, pid, "-f"); code != 5 || o != "one\ntwo\n" {
		t.Fatalf("logs -f: exit %d stdout %q stderr %q", code, o, e)
	}
	// Secret grants: A (the owner) adds one and sends a secret to B's
	// trusted machine under it; B, a guest of the hosts area, can neither
	// list them through its daemon nor change them.
	if o := a.ok(t, "secrets", "grant", "add", "--name", "API_KEY", "--role", "member", "--max-bytes", "100"); !strings.Contains(o, "role member may put value API_KEY in every area") {
		t.Fatalf("secrets grant add:\n%s", o)
	}
	sec := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(sec, []byte("grant-zz-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o := a.ok(t, "sandbox", "exec", id, "--secret", "API_KEY=@"+sec, "--", "sh", "-c", `cat "$POTLUCK_SECRETS/API_KEY"`); o != "grant-zz-value" {
		t.Fatalf("exec with a granted secret on a trusted machine: %q", o)
	}
	if _, e, code := b.run(t, "secrets", "grant", "ls"); code != 1 || !strings.Contains(e, "a member") {
		t.Fatalf("secrets grant ls at B (a guest): %d %s", code, e)
	}
	if _, e, code := b.run(t, "secrets", "grant", "default", "off"); code != 1 || !strings.Contains(e, "only an admin") {
		t.Fatalf("secrets grant default off at B: %d %s", code, e)
	}
	// Made an owner of the team area, carl manages its grants from B, whose
	// link to the pool is capped at member.
	if _, err := client.WithHandler(a.dm.SocketHandler()).Grant(context.Background(), api.GrantRequest{Identity: "carl", Role: "owner", Project: "friends", Area: "team"}); err != nil {
		t.Fatal(err)
	}
	if o := b.ok(t, "secrets", "grant", "add", "--area", "team", "--name", "TEAM_KEY", "--role", "owner"); !strings.Contains(o, "role owner may put value TEAM_KEY in team") {
		t.Fatalf("an area owner adds a grant from a member machine:\n%s", o)
	}
	if o := b.ok(t, "secrets", "grant", "ls"); !strings.Contains(o, "TEAM_KEY") || strings.Contains(o, "API_KEY") {
		t.Fatalf("an area owner's ls from a member machine:\n%s", o)
	}
	if o := a.ok(t, "sandbox", "ls"); !strings.Contains(o, id+" ready by=") {
		t.Fatalf("sandbox ls:\n%s", o)
	}
	var hs computeapi.RunTargetHandles
	if err := json.Unmarshal([]byte(a.ok(t, "sandbox", "ls", "--live", "--json")), &hs); err != nil || len(hs.Handles) != 1 || hs.Handles[0].Handle != "box:"+id {
		t.Fatalf("sandbox ls --json: %+v %v", hs, err)
	}
	// B cannot stop sharing while A's sandbox runs there.
	if o, e, code := b.run(t, "host", "stop"); code != 1 || !strings.Contains(e, "1 sandbox is still running on lender-c1m2: stop them first or pass --force") || strings.Contains(o, "no longer shared") {
		t.Fatalf("host stop with a live sandbox: exit %d stdout %q stderr %q", code, o, e)
	}
	if o := a.ok(t, "sandbox", "exec", id, "--", "cat", "note"); o != "hi\n" {
		t.Fatalf("the sandbox is gone after a refused host stop: %q", o)
	}
	if o := a.ok(t, "sandbox", "rm", id); strings.TrimSpace(o) != "removed "+id {
		t.Fatalf("sandbox rm: %q", o)
	}
	if o := a.ok(t, "sandbox", "ls", "--live"); strings.Contains(o, id) {
		t.Fatalf("a removed sandbox is live:\n%s", o)
	}
	if o := a.ok(t, "sandbox", "ls"); !strings.Contains(o, id+" released by=") || !strings.Contains(o, " ended=") || strings.Contains(o, "expires=") {
		t.Fatalf("an ended sandbox in ls:\n%s", o)
	}

	// B: stop sharing, then leave.
	if o := b.ok(t, "host", "stop"); !strings.Contains(o, "no longer shared: lender-c1m2") {
		t.Fatalf("host stop:\n%s", o)
	}
	if o := b.ok(t, "host", "stop"); !strings.Contains(o, "this machine shares nothing") {
		t.Fatalf("host stop again:\n%s", o)
	}
	// Usage: A (the owner) reads the ended sandbox; B, a shared machine in
	// the hosts area, asks through its daemon and reads no member's rows.
	if o := a.ok(t, "usage"); !strings.Contains(o, id+" released ") {
		t.Fatalf("usage at A:\n%s", o)
	}
	if o := b.ok(t, "usage"); strings.Contains(o, id) || !strings.Contains(o, "minutes: 0") {
		t.Fatalf("usage at B:\n%s", o)
	}
	pool := module[*teamlink.Service](t, b).List()[0].Snapshot().Project.ID
	if _, e, code := b.run(t, "forget", pool, "--drop-host-state", "--yes"); code == 0 || !strings.Contains(e, "potluck leave friends first") {
		t.Fatalf("forget while joined: %d %s", code, e)
	}
	if o := b.ok(t, "leave"); strings.TrimSpace(o) != "left friends" { // the only pool B is in
		t.Fatalf("leave: %q", o)
	}
	if o := b.ok(t, "remotes"); !strings.Contains(o, "joined no pool") {
		t.Fatalf("remotes after leave:\n%s", o)
	}
	// B's owner forgets the pool's key: only with a choice for what B kept
	// as its host, and only once asked.
	if _, e, code := b.run(t, "forget", pool); code == 0 || !strings.Contains(e, "--keep-host-state or --drop-host-state") {
		t.Fatalf("forget without a choice: %d %s", code, e)
	}
	if _, e, code := b.runIn(t, "no\n", "forget", pool, "--drop-host-state"); code == 0 || !strings.Contains(e, "nothing was forgotten") {
		t.Fatalf("forget answered no: %d %s", code, e)
	}
	o, e, code = b.runIn(t, "yes\n", "forget", pool, "--drop-host-state")
	if code != 0 || !strings.Contains(e, "is tied to the key of mini") || !strings.Contains(o, "forgot key ") || !strings.Contains(o, "dropped: the pool's key tag here") || !strings.Contains(o, ", handle ") {
		t.Fatalf("forget: %d\n%s\n%s", code, o, e)
	}
	if _, e, code := b.run(t, "forget", pool, "--keep-host-state", "--yes"); code == 0 || !strings.Contains(e, "no authority key for project") {
		t.Fatalf("forget again: %d %s", code, e)
	}
}

// beat heartbeats link every 200 ms until the returned func is called.
func beat(link *teamlink.Service) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			link.Heartbeat(ctx)
			select {
			case <-ctx.Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	return func() { cancel(); <-done }
}

// A command with no daemon says so and exits 3; status never starts one.
func TestNoDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv(EnvHome, home)
	var o, e bytes.Buffer
	if code := Run(context.Background(), []string{"status"}, strings.NewReader(""), &o, &e); code != 3 || !strings.Contains(e.String(), "potluck daemon is not running") {
		t.Fatalf("status: %d %q %q", code, o.String(), e.String())
	}
}

// host status with no daemon answers its menu, saying so, and starts none.
func TestHostStatusNoDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv(EnvHome, home)
	hostNow = func(owner, deadline int) (protocol.HostStatus, error) {
		return protocol.HostStatus{Mode: protocol.HostDrain, State: protocol.HostDraining}, nil
	}
	t.Cleanup(func() { hostNow = vmhost.HostNow })
	var o, e bytes.Buffer
	if code := Run(context.Background(), []string{"--json", "host", "status"}, strings.NewReader(""), &o, &e); code != 0 {
		t.Fatalf("host status: %d %q %q", code, o.String(), e.String())
	}
	var st menubar.Status
	if err := json.Unmarshal(o.Bytes(), &st); err != nil || len(st.Menu.Items) != 3 || st.Cores < 1 || st.Mode == "" {
		t.Fatalf("host status: %v %s", err, o.String())
	}
	if st.Mode != protocol.HostDrain || st.Menu.Title != "Potluck is not running" || !st.Menu.Items[1].Checked {
		t.Errorf("host status: %+v", st)
	}
	root := (&app{stdout: io.Discard, stderr: io.Discard}).root()
	for _, path := range [][]string{{"host", "status"}, {"host", "set"}} {
		c, _, err := root.Find(path)
		if err != nil || c.Name() != path[1] || autostartAllowed(c, func(string) string { return "" }) {
			t.Errorf("potluck %s may start the daemon (%v)", strings.Join(path, " "), err)
		}
	}
}

// Help and usage say sandbox and machine: the words box, run target,
// handle and bind are plumbing (JSON field names keep them).
func TestHelpWords(t *testing.T) {
	bad := regexp.MustCompile(`(?i)\b(box(es)?|run[ -]targets?|handles?|bind(s|ing|ings)?|bound)\b`)
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range []string{c.Use, c.Short, c.Long, c.Example} {
			if w := bad.FindString(s); w != "" {
				t.Errorf("%s: %q in %q", c.CommandPath(), w, s)
			}
		}
		c.Flags().VisitAll(func(f *pflag.Flag) {
			// --shelf-fill's value is typed as it is: "boxes".
			usage := f.Usage
			if f.Name == "shelf-fill" {
				usage = strings.ReplaceAll(usage, `"boxes"`, "")
			}
			if w := bad.FindString(usage); w != "" {
				t.Errorf("%s --%s: %q in %q", c.CommandPath(), f.Name, w, f.Usage)
			}
		})
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk((&app{}).root())
}

// host --dry-run prints the plan without a daemon: it never starts one, and
// needs no pool when the image is explicit. An implicit image requires the
// earlier bindings so the plan cannot silently replace a named image.
func TestHostDryRunNeedsNoDaemon(t *testing.T) {
	oldRunner, oldDetect, oldDriver := shareRunner, detectHost, shareDriver
	t.Cleanup(func() { shareRunner, detectHost, shareDriver = oldRunner, oldDetect, oldDriver })
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		t.Error("a dry run ran something")
		return hostprobe.Result{}, nil
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv(EnvHome, home)
	var o, e bytes.Buffer
	code := Run(context.Background(), []string{"host", "--dry-run", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "lender", "--reclaim-idle-s", "0", "--no-sudo"},
		strings.NewReader(""), &o, &e)
	if code != 0 || !strings.Contains(o.String(), "would share lender-c1m2") {
		t.Fatalf("host --dry-run: exit %d\nstdout:\n%s\nstderr:\n%s", code, o.String(), e.String())
	}
	if _, err := os.Stat(filepath.Join(home, SocketName)); !os.IsNotExist(err) {
		t.Fatalf("a dry run started a daemon: %v", err)
	}
	o.Reset()
	e.Reset()
	code = Run(context.Background(), []string{"host", "--dry-run", "--sizes", "c1m2", "--name-prefix", "lender", "--reclaim-idle-s", "0", "--no-sudo"}, strings.NewReader(""), &o, &e)
	if code == 0 || !strings.Contains(e.String(), "could not read this machine's bindings") {
		t.Fatalf("implicit image with unread bindings: exit %d: %s", code, &e)
	}

}

// The public flag chooses a Mac image and its runner without a digest or path.
func TestHostWithXcodeDryRun(t *testing.T) {
	oldDetect := detectHost
	t.Cleanup(func() { detectHost = oldDetect })
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", Arch: "arm64", Cores: 16, RAMMB: 64 << 10, FreeDiskGB: 500,
			Layers: []hostprobe.Layer{{Name: "tart", Available: true, Supported: true}}}
	}
	t.Setenv(EnvHome, t.TempDir())
	var out, errs bytes.Buffer
	code := Run(context.Background(), []string{"host", "--with", "xcode", "--dry-run", "--json", "--sizes", "c2m4", "--name-prefix", "xcode", "--no-sudo"}, strings.NewReader(""), &out, &errs)
	var res hostprobe.ShareResult
	if code != 0 {
		t.Fatalf("exit %d: %s", code, &errs)
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.NamedImage != "xcode" || len(res.Images) != 1 || !strings.Contains(res.Images[0], "macos-tahoe-xcode@sha256:") || len(res.Sizes) != 1 || res.Sizes[0].Config["image_name"] != "xcode" || res.Sizes[0].Config["root"] != "on" || res.Sizes[0].Config["runner_dir"] != "actions-runner" {
		t.Fatalf("bad binding plan: %+v", res)
	}

}

// On a machine whose zm daemon is on the same tailnet, potluck's node takes
// its own name: its default machine name is the host name with -potluck,
// where zm's is the host name. A potluck daemon whose name a zm node holds
// gets potluck's hint, never zm's (rename or remove the other node).
func TestTailnetNameBesideZm(t *testing.T) {
	t.Setenv(config.EnvMachine, "")
	t.Setenv(EnvMachine, "")
	name := func() string {
		t.Helper()
		c, err := config.LoadFrom(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return c.Machine
	}
	ours := name()
	config.MachineEnv, config.MachineSuffix = config.EnvMachine, "" // zm's defaults
	zms := name()
	UseHome()
	if ours == zms || !strings.HasSuffix(ours, "-potluck") {
		t.Fatalf("potluck's default machine name %q, zm's %q", ours, zms)
	}
	tn := tailnettest.New()
	zm := startNode(t, tn, zms, tailnettest.Identity{NodeID: "nzm", NodeKey: "nodekey:zm", Login: "carl@example.com", Host: zms})
	zmOwner := client.NewUnix(filepath.Join(zm.home, SocketName))
	if st, err := zmOwner.TailnetUp(context.Background(), api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("zm's node: %+v %v", st, err)
	}
	pl := startNode(t, tn, ours, tailnettest.Identity{NodeID: "npl", NodeKey: "nodekey:pl", Login: "carl@example.com", Host: ours})
	pl.ok(t, "create", "friends")
	// Both homes are fresh, so both nodes get the new prefix (rename step
	// 2): the suffix is what keeps them apart.
	if out := pl.ok(t, "invite", "ana"); !strings.Contains(out, "potluck join 'potluck-"+ours+".") {
		t.Fatalf("invite:\n%s", out)
	}
	if tn.Node("potluck-"+zms) == nil || tn.Node("potluck-"+ours) == nil {
		t.Fatalf("nodes: potluck-%s %v, potluck-%s %v", zms, tn.Node("potluck-"+zms) != nil, ours, tn.Node("potluck-"+ours) != nil)
	}
	// A potluck daemon set to zm's name: its invite is refused with potluck's hint.
	clash := startNode(t, tn, zms, tailnettest.Identity{NodeID: "nclash", NodeKey: "nodekey:clash", Login: "carl@example.com", Host: zms})
	clash.ok(t, "create", "friends")
	_, e, code := clash.run(t, "invite", "ana")
	if code == 0 || !strings.Contains(e, "POTLUCK_MACHINE") || strings.Contains(e, "remove or rename") {
		t.Fatalf("invite under a taken name: exit %d\n%s", code, e)
	}
}

// A dry run prints the plan, not each conformance command line, unless
// --show-commands asks; a real run prints them.
func TestPrintShareDryRunHidesConformanceLines(t *testing.T) {
	res := hostprobe.ShareResult{Layer: "tart", DryRun: true, Sizes: []hostprobe.SizeResult{{Binding: "h-c1m2", Conformance: []string{"potluck-conformance", "tart"}}}}
	var b strings.Builder
	printShare(&b, res)
	if strings.Contains(b.String(), "potluck-conformance") || !strings.Contains(b.String(), "would share h-c1m2") {
		t.Errorf("default dry run:\n%s", b.String())
	}
	res.ShowCommands = true
	b.Reset()
	printShare(&b, res)
	if !strings.Contains(b.String(), "would run: potluck-conformance tart") {
		t.Errorf("--show-commands:\n%s", b.String())
	}
	res.DryRun, res.ShowCommands = false, false
	b.Reset()
	printShare(&b, res)
	if !strings.Contains(b.String(), "ran: potluck-conformance tart") {
		t.Errorf("real run:\n%s", b.String())
	}
}
