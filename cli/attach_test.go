package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/attach"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/internal/upgrade"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// noop is a module that serves nothing: a program built on Potluck adds
// modules of its own on top of wiring.Compute.
type noop struct{}

func (noop) Routes(*core.Router) {}

// startProgram starts, in home, a daemon wired as a program built on
// Potluck runs it: its own names (home SUPPER_HOME or ~/.supper, socket
// supper.sock), config.Program "supper", wiring.Compute plus one module of its
// own. It returns once the daemon is in the machine's records.
func startProgram(t *testing.T, tn *tailnettest.Net, home string) *machine {
	t.Helper()
	config.Use(config.Names{HomeEnvs: []string{"SUPPER_HOME"}, HomeDirs: []string{".supper"}, Sockets: []string{"supper.sock"}})
	config.Program, config.Lacks, config.MachineSuffix = "supper", nil, ""
	defer UseHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "supper-human", "solo"
	cfg.File.Admission = config.Admission{MinMemoryFreePercent: 1, MaxSwapUsedMB: 1 << 40, MinDiskFreeMB: 1}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Compute(d)
		return append(mods, noop{}), err
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(tailnettest.Identity{NodeID: "nsupper", NodeKey: "nodekey:supper", Login: "judy@example.com", Host: "solo"})
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("supper's daemon did not stop")
		}
	})
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if live, _ := daemonrecord.Live(ctx, nil); len(live) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supper's daemon wrote no machine record")
		}
	}
	return &machine{name: "supper", home: home, dm: dm}
}

// runAs runs one potluck command line as the binary would on this machine,
// with POTLUCK_HOME as given ("" unset).
func runAs(t *testing.T, potluckHome string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	t.Setenv(EnvHome, potluckHome)
	var o, e bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code = Run(ctx, args, strings.NewReader(""), &o, &e)
	if os.Getenv("POTLUCK_TEST_SHOW") != "" {
		t.Logf("$ potluck %s\n%s%s(exit %d)", strings.Join(args, " "), o.String(), e.String(), code)
	}
	return o.String(), e.String(), code
}

func okAs(t *testing.T, potluckHome string, args ...string) string {
	t.Helper()
	o, e, code := runAs(t, potluckHome, args...)
	if code != 0 {
		t.Fatalf("potluck %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, o, e)
	}
	return o
}

// One daemon per machine: on a machine whose Potluck daemon is another
// program's (built on Potluck: wiring.Compute plus its own module), and no
// home of potluck's own, potluck's commands use that daemon: status, host
// status and a sandbox's life, all through its socket. Potluck makes no home
// of its own, starts no daemon, and refuses to start one or to write that
// program's home. With POTLUCK_HOME set, potluck runs its own daemon beside
// it, and both are in the machine's records.
func TestPotluckUsesAnotherProgramsDaemon(t *testing.T) {
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

	user, err := os.MkdirTemp("", "pu") // short: the sockets under it have a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(user) })
	user, _ = filepath.EvalSymlinks(user)
	t.Setenv("HOME", user)
	records := filepath.Join(user, ".local", "state", "potluck", "daemons")
	t.Setenv(daemonrecord.EnvDir, records)
	t.Setenv("SUPPER_HOME", "")

	supper := startProgram(t, tailnettest.New(), filepath.Join(user, ".supper"))
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	module[*runtarget.Service](t, supper).SetLookPath(func(string) (string, error) { return drv, nil })
	installed := filepath.Join(supper.home, runtarget.DriversDir, "zm-run-ref")
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, installed); err != nil {
		t.Fatal(err)
	}
	stop := beat(module[*teamlink.Service](t, supper))
	defer stop()

	ownHome := filepath.Join(user, HomeDir)
	if out := okAs(t, "", "status"); !strings.Contains(out, "using supper's daemon") || !strings.Contains(out, "~/.supper") {
		t.Fatalf("status:\n%s", out)
	}
	var st struct {
		Home     string `json:"home"`
		DaemonOf *struct {
			Program string `json:"program"`
		} `json:"daemon_of"`
	}
	if err := json.Unmarshal([]byte(okAs(t, "", "--json", "status")), &st); err != nil || st.Home != supper.home || st.DaemonOf == nil || st.DaemonOf.Program != "supper" {
		t.Fatalf("status --json: %+v %v", st, err)
	}
	if out := okAs(t, "", "create", "--via", "tunnel", "trial"); !strings.Contains(out, "pool trial is ready") {
		t.Fatalf("create:\n%s", out)
	}
	if out := okAs(t, "", "host", "--image", refPin, "--sizes", "c1m2", "--name-prefix", "solo", "--reclaim-idle-s", "0", "--no-sudo"); !strings.Contains(out, "shared solo-c1m2") {
		t.Fatalf("host:\n%s", out)
	}
	var hs menubar.Status
	if err := json.Unmarshal([]byte(okAs(t, "", "--json", "host", "status")), &hs); err != nil || hs.Menu.Title == "Potluck is not running" {
		t.Fatalf("host status: %+v %v", hs, err)
	}
	out := okAs(t, "", "sandbox", "create", "--ttl", "5m")
	id, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if !strings.Contains(out, " ready ") {
		t.Fatalf("sandbox create:\n%s", out)
	}
	if o := okAs(t, "", "sandbox", "exec", id, "--", "echo", "hi"); o != "hi\n" {
		t.Fatalf("exec: %q", o)
	}
	okAs(t, "", "sandbox", "rm", id)

	// What starts or stops a daemon, or writes supper's home itself, is refused.
	for _, args := range [][]string{{"daemon"}, {"daemon", "--detach"}, {"daemon", "stop"}, {"daemon", "preflight"}, {"driver", "pin", "ref"},
		{"isolation", "test", "solo-c1m2"}, {"host", "shelf", "status"}, {"host", "shelf", "fill", "--go-sum", "go.sum"}} {
		_, e, code := runAs(t, "", args...)
		if code != 1 || !strings.Contains(e, "supper's") || !strings.Contains(e, "POTLUCK_HOME=") {
			t.Errorf("potluck %s: exit %d\n%s", strings.Join(args, " "), code, e)
		}
	}
	for _, f := range []string{fileAutostartLock, release.StateFile, swap.FileLock, config.FileLog} {
		if _, err := os.Stat(filepath.Join(supper.home, f)); !os.IsNotExist(err) {
			t.Errorf("potluck wrote %s into supper's home: %v", f, err)
		}
	}
	if _, err := os.Stat(ownHome); !os.IsNotExist(err) {
		t.Fatalf("potluck made a home of its own beside supper's daemon: %v", err)
	}
	if all, _ := daemonrecord.Read(records); len(all) != 1 || all[0].Program != "supper" {
		t.Fatalf("records: %+v", all)
	}
	if _, err := client.NewUnix(filepath.Join(supper.home, "supper.sock")).Health(context.Background()); err != nil {
		t.Fatalf("supper's daemon after potluck's refusals: %v", err)
	}

	// POTLUCK_HOME naming supper's home chooses supper's daemon.
	if out := okAs(t, supper.home, "status"); !strings.Contains(out, "using supper's daemon") {
		t.Fatalf("status with POTLUCK_HOME=supper's home:\n%s", out)
	}

	// POTLUCK_HOME set to a home of potluck's own: potluck's own daemon,
	// beside supper's, and a record for each.
	pl := startMachine(t, tailnettest.New(), "pl", "judy@example.com")
	if out := okAs(t, pl.home, "status"); strings.Contains(out, "supper") || !strings.HasPrefix(out, "potluck ") {
		t.Fatalf("status with POTLUCK_HOME set:\n%s", out)
	}
	live, err := daemonrecord.Live(context.Background(), nil)
	slices.SortFunc(live, func(a, b daemonrecord.Record) int { return strings.Compare(b.Program, a.Program) }) // supper, potluck
	if err != nil || len(live) != 2 || live[0].Program != "supper" || live[1].Program != "potluck" || live[1].Home != pl.home {
		t.Fatalf("live records: %+v %v", live, err)
	}
	// Potluck's own daemon serves compute, so supper's commands' rule would
	// find it; potluck's never attaches to its own program's.
	if got, err := attach.Choose(attach.Facts{}, func(keep func(daemonrecord.Record) bool) ([]daemonrecord.Record, error) {
		if keep(live[1]) {
			return live[1:], nil
		}
		return nil, nil
	}); err != nil || got != nil {
		t.Fatalf("potluck attached to its own program's daemon: %+v %v", got, err)
	}
}

// A client never starts another program's daemon, nor a daemon on another
// program's home; it starts potluck's own when the command may.
func TestClientStartsOnlyPotlucksDaemon(t *testing.T) {
	t.Setenv(EnvNoAutostart, "")
	t.Setenv(EnvBoxCredential, "")
	root := programApp(Program{}, nil, io.Discard, io.Discard).root()
	create, _, err := root.Find([]string{"sandbox", "create"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadClientFrom(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	supper := &daemonrecord.Record{Program: "supper", Home: "/u/.supper", Socket: "/u/.supper/supper.sock"}
	for _, tc := range []struct {
		name  string
		home  daemonHome
		start bool
	}{
		{"potluck's own home", daemonHome{Cfg: cfg, Existed: true}, true},
		{"another program's daemon", daemonHome{Cfg: cfg, Other: supper, Owner: "supper", Existed: true}, false},
		{"another program's home, its daemon not running", daemonHome{Cfg: cfg, Owner: "supper", Existed: true}, false},
	} {
		a := programApp(Program{}, nil, io.Discard, io.Discard)
		a.running, a.homeDone, a.homeV = create, true, tc.home
		c, _, err := a.client()
		if err != nil {
			t.Fatal(err)
		}
		if (c.Autostart != nil) != tc.start {
			t.Errorf("%s: may start a daemon %v, want %v", tc.name, c.Autostart != nil, tc.start)
		}
	}
}

// potluck daemon, and what writes a home, refuse a home another program
// claimed, while its daemon is not running.
func TestPotluckRefusesAnotherProgramsHome(t *testing.T) {
	home, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err := config.ClaimHome(home, "supper"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"daemon"}, {"driver", "pin", "ref"}} {
		_, e, code := runAs(t, home, args...)
		if code != 1 || !strings.Contains(e, "is supper's home") {
			t.Errorf("potluck %s on supper's home: exit %d\n%s", strings.Join(args, " "), code, e)
		}
	}
	if ents, _ := os.ReadDir(home); len(ents) != 1 {
		t.Errorf("potluck wrote into supper's home: %d files", len(ents))
	}
}

// potluck upgrade installs the programs alone, and touches no home, when it
// has no home of its own yet, the home is another program's, or the rule
// that picks one refuses.
func TestUpgradeFilesOnly(t *testing.T) {
	cfg, _ := config.LoadClientFrom("/u/.potluck")
	supper := &daemonrecord.Record{Program: "supper", Home: "/u/.supper", PID: 7}
	for _, tc := range []struct {
		name string
		home daemonHome
		err  error
		want string // in the reason; "" upgrades the home and daemon too
	}{
		{"potluck's own home", daemonHome{Cfg: cfg, Existed: true}, nil, ""},
		{"no home yet", daemonHome{Cfg: cfg}, nil, "no home yet"},
		{"another program's daemon", daemonHome{Cfg: cfg, Other: supper, Owner: "supper", Existed: true}, nil,
			"this machine's Potluck daemon is supper's (home /u/.supper, pid 7), and potluck's commands use it; potluck upgrade leaves that daemon and its home as they are: supper keeps them, so update supper its own way"},
		{"another program's home", daemonHome{Cfg: cfg, Owner: "supper", Existed: true}, nil, "supper's home"},
		{"the rule refuses", daemonHome{}, errors.New("more than one Potluck daemon"), "more than one"},
	} {
		got := filesOnlyWhy(tc.home, tc.err)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	stage, dir := t.TempDir(), filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(filepath.Join(stage, "potluck"), []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	var o bytes.Buffer
	a := programApp(Program{}, nil, &o, io.Discard)
	if err := a.upgradeFilesOnly(t.Context(), dir, stage, upgrade.Staged{Names: []string{"potluck"}}, "the reason", false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "potluck")); err != nil || string(b) != "new" || !strings.Contains(o.String(), "the reason") {
		t.Fatalf("installed %q %v; said %q", b, err, o.String())
	}
}

// A command that would make potluck's first home waits a moment for a
// daemon of another program that is restarting, rather than starting a
// second daemon whose new home would keep the machine.
func TestAutostartWaitsForARestartingDaemon(t *testing.T) {
	user, err := os.MkdirTemp("", "pw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(user) })
	t.Setenv("HOME", user)
	t.Setenv(EnvHome, "")
	records := filepath.Join(user, "records")
	t.Setenv(daemonrecord.EnvDir, records)
	old := attach.StartWait
	t.Cleanup(func() { attach.StartWait = old })

	// No records directory: nothing built on Potluck ran here; no wait.
	attach.StartWait = time.Hour
	if o := programApp(Program{}, nil, io.Discard, io.Discard).otherStarting(context.Background()); o != nil {
		t.Fatalf("no records: %+v", o)
	}
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	attach.StartWait = 200 * time.Millisecond
	if o := programApp(Program{}, nil, io.Discard, io.Discard).otherStarting(context.Background()); o != nil {
		t.Fatalf("no daemon came back: %+v", o)
	}

	// supper's daemon comes back while the command waits.
	sock := filepath.Join(user, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.SetHeader(w.Header(), api.HeaderProtocol, api.WireProtocol)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, _ = daemonrecord.Write(context.Background(), records, daemonrecord.Record{Program: "supper", Home: filepath.Join(user, ".supper"),
			Socket: sock, PID: os.Getpid(), Modules: wiring.ComputeModules})
	}()
	attach.StartWait = 10 * time.Second
	if o := programApp(Program{}, nil, io.Discard, io.Discard).otherStarting(context.Background()); o == nil || o.Program != "supper" {
		t.Fatalf("supper's daemon came back: %+v", o)
	}
	if _, err := os.Stat(filepath.Join(user, HomeDir)); !os.IsNotExist(err) {
		t.Fatalf("made potluck's home: %v", err)
	}
}

// A files-only upgrade leaves the driver binaries alone where another
// program's daemon runs its drivers from (it pins them by checksum), and
// places them anywhere else.
func TestFilesOnlyUpgradeLeavesTheAttachedDaemonsDrivers(t *testing.T) {
	supper := &daemonrecord.Record{Program: "supper", Exe: "/opt/supper/bin/supper", Home: "/h/supper"}
	names := []string{"potluck", "potluck-driver-tart", "potluck-driver-local"}
	for _, tc := range []struct {
		name      string
		dir       string
		other     *daemonrecord.Record
		held      int
		placedLen int
	}{
		{"beside the daemon's program", "/opt/supper/bin", supper, 2, 1},
		{"its home's drivers", "/h/supper/drivers", supper, 2, 1},
		{"somewhere else", "/usr/local/bin", supper, 0, 3},
		{"no other daemon", "/opt/supper/bin", nil, 0, 3},
	} {
		place, held := withoutAttachedDrivers(names, tc.dir, tc.other)
		if len(held) != tc.held || len(place) != tc.placedLen {
			t.Errorf("%s: place %v held %v", tc.name, place, held)
		}
	}
}
