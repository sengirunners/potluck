package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/runtarget"
)

// supper is a program that mounts Potluck's commands (Commands) in a tree
// of its own: its root, its --json and --pool, its own client for its own
// daemon. The tree is Potluck's under "supper pool", and the sandbox
// command again at its top level ("supper sandbox").
type supper struct {
	socket string // its daemon's
	home   string
	lent   *int // how many times its AfterLend ran
}

// run runs one supper command line and returns its streams and exit code,
// reporting an error as the potluck program does (Report).
func (s supper) run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	root, host := s.root(&o, &e, prefix("supper pool"), prefix("supper"))
	root.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		code = Report(host, err)
	}
	if os.Getenv("POTLUCK_TEST_SHOW") != "" {
		t.Logf("$ supper %s\n%s%s(exit %d)", strings.Join(args, " "), o.String(), e.String(), code)
	}
	return o.String(), e.String(), code
}

func (s supper) ok(t *testing.T, args ...string) string {
	t.Helper()
	o, e, code := s.run(t, args...)
	if code != 0 {
		t.Fatalf("supper %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, o, e)
	}
	return o
}

// root is supper's tree: Potluck's commands for prefix pool under its word
// "pool", and the sandbox command for prefix top at its top level. host is
// the pool mount's Host.
func (s supper) root(stdout, stderr *bytes.Buffer, pool, top prefix) (*cobra.Command, Host) {
	var asJSON bool
	var project string
	root := &cobra.Command{Use: top.name(), SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")
	root.PersistentFlags().StringVarP(&project, "project", "P", "", "the project")
	host := func(p prefix) Host {
		return Host{Prefix: string(p), JSON: func() bool { return asJSON }, Pool: func() string { return project },
			Client: func() (*client.Client, *config.Config, error) {
				cfg, err := config.LoadClientFrom(s.home)
				if err != nil {
					return nil, nil, err
				}
				return client.NewUnix(s.socket), cfg, nil // supper's: no autostart here
			},
			Stdout: stdout, Stderr: stderr,
			AfterLend: &AfterLend{Skip: "no-auto-update", SkipUsage: "share without turning on supper's daily update",
				Run: func(w io.Writer) { *s.lent++; fmt.Fprintln(w, "supper: daily update on") }}}
	}
	group := &cobra.Command{Use: strings.TrimPrefix(string(pool), top.name()+" "), Short: "Potluck's commands"}
	group.AddCommand(Commands(host(pool))...)
	root.AddCommand(group)
	for _, c := range Commands(host(top)) {
		if c.Name() == "sandbox" {
			root.AddCommand(c)
		}
	}
	return root, host(pool)
}

// A program with its own root, names, home and daemon mounts Potluck's
// commands: pools, hosting and a sandbox's life work through its client,
// hints name its words, --json is its flag, and nothing of the potluck
// program's (its home, its record, config.Program) is made or touched.
func TestAProgramMountsPotlucksCommands(t *testing.T) {
	oldBeat, oldRunner, oldDetect, oldDriver := teamlink.BeatEvery, shareRunner, detectHost, shareDriver
	t.Cleanup(func() {
		teamlink.BeatEvery, shareRunner, detectHost, shareDriver = oldBeat, oldRunner, oldDetect, oldDriver
	})
	teamlink.BeatEvery = time.Hour // beaten by hand, below
	shareRunner = runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	shareDriver = "ref"

	user, err := os.MkdirTemp("", "pm") // short: the sockets under it have a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(user) })
	user, _ = filepath.EvalSymlinks(user)
	t.Setenv("HOME", user)
	records := filepath.Join(user, ".local", "state", "potluck", "daemons")
	t.Setenv(daemonrecord.EnvDir, records)
	t.Setenv("SUPPER_HOME", "")

	m := startProgram(t, tailnettest.New(), filepath.Join(user, ".supper"))
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	module[*runtarget.Service](t, m).SetLookPath(func(string) (string, error) { return drv, nil })
	installed := filepath.Join(m.home, runtarget.DriversDir, "zm-run-ref")
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, installed); err != nil {
		t.Fatal(err)
	}
	stop := beat(module[*teamlink.Service](t, m))
	defer stop()

	// supper runs as its own program: its names and config.Program.
	config.Use(config.Names{HomeEnvs: []string{"SUPPER_HOME"}, HomeDirs: []string{".supper"}, Sockets: []string{"supper.sock"}})
	config.Program, config.Verbs, config.MachineSuffix = "supper", nil, ""
	t.Cleanup(UseHome)
	s := supper{socket: filepath.Join(m.home, "supper.sock"), home: m.home, lent: new(int)}
	sw := useSwitch(t)

	out := s.ok(t, "pool", "create", "--via", "tunnel", "trial")
	if !strings.Contains(out, "pool trial is ready") || !strings.Contains(out, "share this machine too: supper pool host\n") ||
		!strings.Contains(out, "next: supper pool invite <name> for a friend") {
		t.Fatalf("create:\n%s", out)
	}
	// host share, the old spelling a program runs, through the mount: the
	// host's AfterLend runs once the machine is lent, and its skip flag is
	// the host's.
	share := []string{"--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"}
	out = s.ok(t, append([]string{"pool", "host", "share", "--pool", "trial"}, share...)...)
	if !strings.Contains(out, "shared solo-c1m2") || !strings.Contains(out, "members: supper pool sandbox create") ||
		!strings.Contains(out, "supper: daily update on") || *s.lent != 1 {
		t.Fatalf("host share (AfterLend ran %d times):\n%s", *s.lent, out)
	}
	if o := s.ok(t, append([]string{"pool", "host", "trial", "--no-auto-update"}, share...)...); *s.lent != 1 || strings.Contains(o, "daily update") {
		t.Fatalf("host --no-auto-update ran AfterLend (%d times):\n%s", *s.lent, o)
	}
	s.ok(t, append([]string{"pool", "host", "--dry-run"}, share...)...)
	if *s.lent != 1 {
		t.Fatalf("a dry run ran AfterLend")
	}
	var hs menubar.Status // --json is supper's own flag
	if err := json.Unmarshal([]byte(s.ok(t, "--json", "pool", "host", "status")), &hs); err != nil || hs.Menu.Title == "Potluck is not running" {
		t.Fatalf("host status --json: %+v %v", hs, err)
	}
	// A pause and a resume through the mount are the one host switch.
	if out := s.ok(t, "pool", "host", "pause"); !strings.Contains(out, "supper pool host resume lends it again") || sw.mode != "closed" {
		t.Fatalf("pause: switch %s\n%s", sw.mode, out)
	}
	if err := json.Unmarshal([]byte(s.ok(t, "--json", "pool", "host", "status")), &hs); err != nil || hs.Menu.Title != "Paused: lending nothing" {
		t.Fatalf("host status while paused: %+v %v", hs.Menu, err)
	}
	if out := s.ok(t, "pool", "host", "resume"); !strings.Contains(out, "resumed: this machine takes work again (solo-c1m2)") || sw.mode != "auto" {
		t.Fatalf("resume: switch %s\n%s", sw.mode, out)
	}
	// The sandbox command at supper's top level, and under pool: one sandbox.
	out = s.ok(t, "sandbox", "create", "--ttl", "5m", "-P", "trial")
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !strings.Contains(out, " ready ") {
		t.Fatalf("sandbox create:\n%s", out)
	}
	if o := s.ok(t, "pool", "sandbox", "exec", id, "--", "echo", "hi"); o != "hi\n" {
		t.Fatalf("exec: %q", o)
	}
	// A sandbox command's own exit code reaches the host (ExitCode).
	if _, _, code := s.run(t, "sandbox", "exec", id, "--", "sh", "-c", "exit 7"); code != 7 {
		t.Fatalf("exec exit 7: exit %d", code)
	}
	// An old spelling a program still runs, through the mount: spawn is
	// exec --detach, output is logs.
	pid := strings.TrimSpace(s.ok(t, "sandbox", "spawn", id, "--", "echo", "old"))
	if o := s.ok(t, "pool", "sandbox", "output", id, pid, "--follow"); o != "old\n" {
		t.Fatalf("spawn, then output: %q", o)
	}
	if out := s.ok(t, "--json", "sandbox", "ls"); !json.Valid([]byte(out)) || !strings.Contains(out, strings.TrimPrefix(id, "sandbox ")) {
		t.Fatalf("sandbox ls --json:\n%s", out)
	}
	s.ok(t, "pool", "sandbox", "rm", id)

	// Refusals name supper's words.
	_, e, code := s.run(t, "sandbox", "cp", id, "a", "b")
	if code != 1 || !strings.HasPrefix(e, "supper: ") || !strings.Contains(e, "(supper sandbox cp <sandbox> ./data :work/,") {
		t.Fatalf("sandbox cp refusal: exit %d\n%s", code, e)
	}

	if config.Program != "supper" {
		t.Fatalf("mounting Potluck's commands changed config.Program to %q", config.Program)
	}
	if _, err := os.Stat(filepath.Join(user, HomeDir)); !os.IsNotExist(err) {
		t.Fatalf("supper's commands made potluck's home: %v", err)
	}
	if all, _ := daemonrecord.Read(records); len(all) != 1 || all[0].Program != "supper" {
		t.Fatalf("records: %+v", all)
	}
}

// Every word a mounted command prints that names a Potluck command comes
// from the host's prefix: --help on every command and a set of refusals,
// rendered for two programs, differ only by their prefixes, and neither
// names a command as the potluck program's.
func TestMountedCommandsSpeakTheHostsWords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	none := noDaemon(t)
	runs := [][]string{
		{"sandbox", "cp", "x", "a", "b"},
		{"ci", "doctor", "--probe"},
		{"sealing", "forget"},
		{"join", "not-a-code"},
		{"host", "shelf", "status"},
	}
	var paths [][]string
	var walk func(c *cobra.Command, p []string)
	walk = func(c *cobra.Command, p []string) {
		for _, s := range c.Commands() {
			q := append(slices.Clone(p), s.Name())
			paths = append(paths, q)
			walk(s, q)
		}
	}
	probe := &cobra.Command{Use: "x"}
	probe.AddCommand(Commands(Host{Prefix: "x", Client: none, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})...)
	walk(probe, nil)
	for _, p := range paths {
		runs = append(runs, append(slices.Clone(p), "--help"))
	}
	if len(runs) < 60 {
		t.Fatalf("walked %d commands", len(runs))
	}

	// Two programs: "alpha" mounts the tree at its top level, "bravo"
	// under its word "pool".
	render := func(top, group string, args []string) string {
		var o, e bytes.Buffer
		h := Host{Prefix: strings.TrimSpace(top + " " + group), Client: none, Stdout: &o, Stderr: &e}
		root := &cobra.Command{Use: top, SilenceUsage: true, SilenceErrors: true}
		root.PersistentFlags().Bool("json", false, "print JSON")
		parent := root
		if group != "" {
			parent = &cobra.Command{Use: group}
			root.AddCommand(parent)
			args = append([]string{group}, args...)
		}
		parent.AddCommand(Commands(h)...)
		root.SetArgs(args)
		root.SetOut(&o)
		root.SetErr(&e)
		if err := root.Execute(); err != nil {
			if _, show, ok := ExitCode(err); ok && show != nil {
				err = show
			}
			fmt.Fprintln(&e, err)
		}
		return o.String() + e.String()
	}
	var mounted []string
	for _, c := range probe.Commands() {
		mounted = append(mounted, c.Name())
	}
	names := append(slices.Concat(ProgramCommands, mounted), "help", "completion")
	for _, args := range runs {
		a := render("alpha", "", args)
		b := render("bravo", "pool", args)
		if strings.ReplaceAll(strings.ReplaceAll(b, "bravo pool", "alpha"), "bravo", "alpha") != a {
			t.Errorf("%s: the two programs' output differs by more than their prefixes:\n--- alpha\n%s\n--- bravo pool\n%s", strings.Join(args, " "), a, b)
		}
		for _, out := range []string{a, b} {
			if n := namesCommand(out, "potluck", names); n != "" {
				t.Errorf("%s: names the potluck program's command %q:\n%s", strings.Join(args, " "), n, out)
			}
		}
		// A mounted command is bravo's only under "bravo pool": a hint
		// built from the program's name alone would read "bravo host".
		if n := namesCommand(b, "bravo", mounted); n != "" {
			t.Errorf("%s: names %q without bravo's prefix:\n%s", strings.Join(args, " "), n, b)
		}
	}
}

// namesCommand is the first of cmds that out names as program's command
// ("potluck host", followed by a space, end of line or punctuation), or "".
func namesCommand(out, program string, cmds []string) string {
	for _, n := range cmds {
		for _, end := range []string{" ", "\n", ")", ",", ".", "`", "'", ";", ":"} {
			if strings.Contains(out, program+" "+n+end) {
				return n
			}
		}
	}
	return ""
}

// noDaemon is a host client whose daemon is not running and is never
// started.
func noDaemon(t *testing.T) func() (*client.Client, *config.Config, error) {
	home, err := os.MkdirTemp("", "nd") // short: a socket path has a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return func() (*client.Client, *config.Config, error) {
		cfg, err := config.LoadClientFrom(home)
		return client.NewUnix(filepath.Join(home, "none.sock")), cfg, err
	}
}

// The potluck program's own commands are ProgramCommands, and a host gets
// every other top-level command.
func TestProgramCommandsAreThePotluckPrograms(t *testing.T) {
	a := programApp(Program{}, nil, &bytes.Buffer{}, &bytes.Buffer{})
	var own []string
	for _, c := range a.programCommands() {
		own = append(own, c.Name())
	}
	if !slices.Equal(own, ProgramCommands) {
		t.Fatalf("programCommands %v, ProgramCommands %v", own, ProgramCommands)
	}
	root := a.root()
	mounted := Commands(Host{Prefix: "supper", Client: noDaemon(t), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	var got []string
	var noPersistent func(c *cobra.Command)
	noPersistent = func(c *cobra.Command) {
		if c.HasPersistentFlags() {
			t.Errorf("%s defines persistent flags", c.CommandPath())
		}
		for _, s := range c.Commands() {
			noPersistent(s)
		}
	}
	for _, c := range mounted {
		got = append(got, c.Name())
		noPersistent(c)
	}
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if slices.Contains(ProgramCommands, c.Name()) == slices.Contains(got, c.Name()) {
			t.Errorf("%s: in ProgramCommands %v, mounted %v", c.Name(), slices.Contains(ProgramCommands, c.Name()), slices.Contains(got, c.Name()))
		}
	}
}

// A mounted run with no daemon answering: the read-only commands never
// start the host's daemon (they take its client with autostart off),
// and Report gives Potluck's hint in the host's words and exit 3, as text
// and with the host's --json.
func TestMountedCommandsWithNoDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// supper's own words for kit's messages, as a host sets them.
	config.Program, config.Verbs = "supper", nil
	t.Cleanup(UseHome)
	oldDetect := detectHost // this machine's own probe is not the test's
	t.Cleanup(func() { detectHost = oldDetect })
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
			Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}}
	}
	none := noDaemon(t)
	trap := func() (*client.Client, *config.Config, error) {
		c, cfg, err := none()
		if c != nil {
			c.Autostart = func(context.Context) error {
				t.Error("a read-only command started the host's daemon")
				return errors.New("not here")
			}
		}
		return c, cfg, err
	}
	run := func(client func() (*client.Client, *config.Config, error), args ...string) (string, int) {
		var o, e bytes.Buffer
		var asJSON bool
		h := Host{Prefix: "supper pool", JSON: func() bool { return asJSON }, Client: client, Stdout: &o, Stderr: &e}
		root := &cobra.Command{Use: "supper", SilenceUsage: true, SilenceErrors: true}
		root.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")
		group := &cobra.Command{Use: "pool"}
		group.AddCommand(Commands(h)...)
		root.AddCommand(group)
		root.SetArgs(args)
		code := 0
		if err := root.Execute(); err != nil {
			code = Report(h, err)
		}
		return o.String() + e.String(), code
	}
	for _, args := range [][]string{{"pool", "host", "status"}, {"pool", "isolation"}, {"pool", "doctor"}} {
		out, _ := run(trap, args...)
		if n := namesCommand(out, "potluck", append(slices.Clone(ProgramCommands), "host")); n != "" || !strings.Contains(out, "supper") {
			t.Errorf("%s:\n%s", strings.Join(args, " "), out)
		}
	}
	out, code := run(none, "pool", "sandbox", "ls")
	if code != 3 || !strings.Contains(out, "supper: the supper daemon is not running") || !strings.Contains(out, "supper daemon --detach") {
		t.Errorf("sandbox ls with no daemon: exit %d\n%s", code, out)
	}
	out, code = run(none, "--json", "pool", "sandbox", "ls")
	var e struct {
		Error struct {
			Hint string `json:"hint"`
		} `json:"error"`
	}
	if code != 3 || json.Unmarshal([]byte(out), &e) != nil || !strings.Contains(e.Error.Hint, "supper daemon --detach") {
		t.Errorf("--json sandbox ls with no daemon: exit %d\n%s", code, out)
	}
}

// A Host must be usable as given: a prefix a shell takes unquoted, a
// client, streams; a client config with no home is refused, never
// replaced by potluck's own home.
func TestHostIsChecked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	none := noDaemon(t)
	for _, p := range []string{"", " supper", "supper ", "a  b", "su;per", "x\ny", "su'per", "50%"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("prefix %q was taken", p)
				}
			}()
			Commands(Host{Prefix: p, Client: none, Stdout: io.Discard, Stderr: io.Discard})
		}()
	}
	for _, p := range []string{"potluck", "zm pool", "supper", "my-prog.v2 pool"} {
		Commands(Host{Prefix: p, Client: none, Stdout: io.Discard, Stderr: io.Discard})
	}
	noHome := func() (*client.Client, *config.Config, error) {
		return client.NewUnix(filepath.Join(t.TempDir(), "s.sock")), &config.Config{}, nil
	}
	var e bytes.Buffer
	root := &cobra.Command{Use: "supper", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(Commands(Host{Prefix: "supper", Client: noHome, Stdout: io.Discard, Stderr: &e})...)
	root.SetArgs([]string{"doctor"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "no home") {
		t.Fatalf("doctor with no home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), HomeDir)); !os.IsNotExist(err) {
		t.Fatalf("a mounted run made potluck's home: %v", err)
	}
}
