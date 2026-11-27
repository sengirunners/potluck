package e2bgateway

import (
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/attach"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

type noop struct{}

func (noop) Routes(*core.Router) {}

// startOtherProgram runs, in home, the daemon of a program built on
// Potluck (its own names and config.Program, wiring.Compute plus a module of
// its own), then makes the process potluck's again (cli.UseHome). It returns
// once the daemon is in the machine's records.
func startOtherProgram(t *testing.T, home string) {
	t.Helper()
	prev, prevProgram, prevLacks, prevPrefix := config.Wired(), config.Program, config.Lacks, config.Prefix
	prevMachineEnv, prevSuffix, prevHumanEnv, prevHostEnv := config.MachineEnv, config.MachineSuffix, config.HumanEnv, config.HostEnv
	t.Cleanup(func() {
		config.Use(prev)
		config.Program, config.Lacks, config.Prefix = prevProgram, prevLacks, prevPrefix
		config.MachineEnv, config.MachineSuffix, config.HumanEnv, config.HostEnv = prevMachineEnv, prevSuffix, prevHumanEnv, prevHostEnv
	})
	config.Use(config.Names{HomeEnvs: []string{"SUPPER_HOME"}, HomeDirs: []string{".supper"}, Sockets: []string{"supper.sock"}})
	config.Program, config.Lacks = "supper", nil
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "supper-human", "solo"
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Compute(d)
		return append(mods, noop{}), err
	})
	if err != nil {
		cancel()
		t.Fatal(err)
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
	cli.UseHome()
}

// On a machine whose Potluck daemon is another program's and that has no
// ~/.potluck, potluck-e2b dials that daemon (the one-daemon rule of
// docs/daemon.md), never makes ~/.potluck (which would start potluck's own
// home beside it), and keeps its state only where it is told.
func TestGatewayUsesAnotherProgramsDaemon(t *testing.T) {
	user, err := os.MkdirTemp("", "pe") // short: the sockets under it have a limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(user) })
	user, _ = filepath.EvalSymlinks(user)
	t.Setenv("HOME", user)
	t.Setenv(daemonrecord.EnvDir, filepath.Join(user, ".local", "state", "potluck", "daemons"))
	t.Setenv("SUPPER_HOME", "")
	t.Setenv(cli.EnvHome, "")
	supperHome := filepath.Join(user, ".supper")
	startOtherProgram(t, supperHome)
	if _, err := os.Stat(filepath.Join(user, cli.HomeDir)); err == nil {
		t.Fatalf("~/%s exists before the gateway ran", cli.HomeDir)
	}

	state := filepath.Join(user, "e2b-state")
	dir, cfg, err := stateDir(state)
	if err != nil {
		t.Fatal(err)
	}
	if dir != state {
		t.Fatalf("state dir %q, want %q", dir, state)
	}
	c := client.New(cfg)
	c.As = "" // as the gateway's own client (serve)
	machines, err := c.Machines(context.Background())
	if err != nil || len(machines.Machines) != 1 {
		t.Fatalf("the gateway's client did not reach supper's daemon (socket %s): %+v %v", cfg.SocketPath(), machines, err)
	}
	if _, err := os.Stat(filepath.Join(user, cli.HomeDir)); err == nil {
		t.Fatalf("the gateway made ~/%s", cli.HomeDir)
	}

	// No --state: its default would be under the other program's home,
	// which the gateway never writes; it says what to do instead.
	if _, _, err := stateDir(""); err == nil || !strings.Contains(err.Error(), "--state") {
		t.Fatalf("default state dir beside another program's daemon: %v", err)
	}
	if _, err := os.Stat(filepath.Join(supperHome, "e2b")); err == nil {
		t.Fatal("the gateway wrote into supper's home")
	}
	if _, err := os.Stat(filepath.Join(user, cli.HomeDir)); err == nil {
		t.Fatalf("the gateway made ~/%s", cli.HomeDir)
	}
}

// The other program's daemon is restarting (its record is gone for a moment)
// while ~/.potluck does not exist: the gateway waits for it as potluck's
// commands do, then refuses a default state dir under it, rather than
// choosing ~/.potluck/e2b, which would make potluck's home beside it. The
// daemon is a record and a socket that answers, so no config global changes
// while the gateway reads them.
func TestGatewayWaitsForAnotherProgramsRestartingDaemon(t *testing.T) {
	user, err := os.MkdirTemp("", "pe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(user) })
	user, _ = filepath.EvalSymlinks(user)
	t.Setenv("HOME", user)
	records := filepath.Join(user, ".local", "state", "potluck", "daemons")
	t.Setenv(daemonrecord.EnvDir, records)
	t.Setenv(cli.EnvHome, "")
	if err := os.MkdirAll(records, 0o700); err != nil { // a daemon built on Potluck has run here
		t.Fatal(err)
	}
	prev, prevProgram := config.Wired(), config.Program
	t.Cleanup(func() { config.Use(prev); config.Program = prevProgram })
	cli.UseHome() // potluck's names, as potluck-e2b runs
	old := attach.StartWait
	t.Cleanup(func() { attach.StartWait = old })
	attach.StartWait = 20 * time.Second

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
		time.Sleep(500 * time.Millisecond)
		_, _ = daemonrecord.Write(context.Background(), records, daemonrecord.Record{Program: "supper", Home: filepath.Join(user, ".supper"),
			Socket: sock, PID: os.Getpid(), Modules: wiring.ComputeModules})
	}()
	dir, _, err := stateDir("")
	if err == nil || !strings.Contains(err.Error(), "--state") || !strings.Contains(err.Error(), "supper") {
		t.Fatalf("state dir %q, %v: want a refusal naming supper and --state", dir, err)
	}
	if _, err := os.Stat(filepath.Join(user, cli.HomeDir)); err == nil {
		t.Fatalf("the gateway made ~/%s", cli.HomeDir)
	}
}
