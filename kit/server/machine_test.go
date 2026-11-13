package server_test

import (
	"context"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
)

func stubHost(t *testing.T, name *string) {
	old := config.HostName
	config.HostName = func() (string, bool) { return *name, true }
	t.Cleanup(func() { config.HostName = old })
}

// The daemon persists its machine name at the first start (from the host's
// stable name); a later host rename does not change it (dogfood: DHCP
// renamed the Mac and every live session's ZM_AS was refused).
func TestDaemonPersistsMachineName(t *testing.T) {
	home := shortHome(t)
	t.Setenv(config.EnvMachine, "")
	host := "Host-A-Laptop"
	stubHost(t, &host)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	dm, err := New(context.Background(), cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	dm.Close()
	host = "unknown42a411d299bc"
	again, _ := config.LoadFrom(home)
	if again.Machine != "host-a-laptop" || again.MachineFrom != config.MachineFromFile {
		t.Fatalf("after a host rename: %q (%s)", again.Machine, again.MachineFrom)
	}
}

// An install upgraded from a zm that did not persist the name keeps the name
// its daemon last ran as (its sessions use it), not the host's new one.
func TestDaemonAdoptsPreviousMachineName(t *testing.T) {
	home := shortHome(t)
	t.Setenv(config.EnvMachine, "")
	host := "studio"
	stubHost(t, &host)
	cfg, _ := config.LoadFrom(home)
	dm, err := New(context.Background(), cfg, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	// What an older daemon left: a daemon.started as "old-name", no machine in config.json.
	dm.Deps.Events.Publish(context.Background(), events.Input{Type: api.EventDaemonStarted, Subject: "old-name"})
	dm.Close()
	cfg.File.Machine = ""
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := config.LoadFrom(home)
	if cfg2.Machine != "studio" {
		t.Fatalf("setup: %q", cfg2.Machine)
	}
	dm2, err := New(context.Background(), cfg2, nil, wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	defer dm2.Close()
	if dm2.Deps.Config.Machine != "old-name" {
		t.Fatalf("daemon runs as %q", dm2.Deps.Config.Machine)
	}
	if c, _ := config.LoadFrom(home); c.Machine != "old-name" || c.MachineFrom != config.MachineFromFile {
		t.Fatalf("persisted: %q %s", c.Machine, c.MachineFrom)
	}
}
