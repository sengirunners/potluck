package server_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

// setProgram makes this test's daemons program p's.
func setProgram(t *testing.T, p string) {
	old := config.Program
	config.Program = p
	t.Cleanup(func() { config.Program = old })
}

// A listening daemon is in the machine's records, with its program,
// binary, home, socket, process and route modules; it removes its record
// when it stops.
func TestDaemonWritesItsMachineRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(daemonrecord.EnvDir, dir)
	setProgram(t, "supper")
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dm, err := New(ctx, cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	var live []daemonrecord.Record
	for deadline := time.Now().Add(10 * time.Second); len(live) == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		live, _ = daemonrecord.Live(ctx, nil)
	}
	if len(live) != 1 {
		t.Fatalf("live records: %+v", live)
	}
	r := live[0]
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if r.Program != "supper" || r.Home != home || r.Socket != cfg.ListenSocketPath() || r.PID != os.Getpid() || r.Exe != exe ||
		!slices.Contains(r.Modules, "teamlink") || !slices.Contains(r.Modules, "core") || r.Serves(wiring.ComputeModules...) {
		t.Fatalf("record: %+v", r)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if all, err := daemonrecord.Read(dir); err != nil || len(all) != 0 {
		t.Fatalf("records after the daemon stopped: %+v %v", all, err)
	}
}

// A daemon never opens a home another program's daemon owns: the first
// daemon on a home claims it for its program.
func TestDaemonRefusesAnotherProgramsHome(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	setProgram(t, "supper")
	dm, err := New(context.Background(), cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	dm.Close()
	config.Program = "potluck"
	if _, err := New(context.Background(), cfg, nil, wiring.Kit); err == nil || !strings.Contains(err.Error(), "home of supper's daemon") {
		t.Fatalf("potluck's daemon on supper's home: %v", err)
	}
	// A home claimed before its database was made: refused before it opens one.
	claimed := shortHome(t)
	if err := config.ClaimHome(claimed, "supper"); err != nil {
		t.Fatal(err)
	}
	ccfg, _ := config.LoadFrom(claimed)
	if _, err := New(context.Background(), ccfg, nil, wiring.Kit); err == nil {
		t.Fatal("potluck's daemon opened a home claimed by supper")
	}
	if _, err := os.Stat(filepath.Join(claimed, config.FileRegistry)); !os.IsNotExist(err) {
		t.Fatalf("a refused daemon opened %s: %v", config.FileRegistry, err)
	}
	config.Program = "supper"
	dm, err = New(context.Background(), cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatalf("supper's daemon on its own home again: %v", err)
	}
	dm.Close()
}
