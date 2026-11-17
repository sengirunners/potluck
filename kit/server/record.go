package server

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/daemonrecord"
)

// writeRecord writes this daemon's machine record (daemonrecord), listening
// on sock: its program, binary, home, socket, process, build and the
// modules its routes come from. ok: written (Serve removes it on the way
// out). A record that cannot be written is logged: the daemon serves on,
// and clients with no home of their own do not find it.
func (dm *Daemon) writeRecord(ctx context.Context, sock string) (daemonrecord.Record, bool) {
	d := dm.Deps
	exe, err := os.Executable()
	if err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
	}
	rec := daemonrecord.Record{Program: dm.program, Exe: exe, Home: d.Config.Home, Socket: sock, PID: os.Getpid(),
		Protocol: api.WireProtocol, Version: api.Version, Modules: dm.routeModules(), Started: d.Started}
	written, err := daemonrecord.Write(ctx, daemonrecord.Dir(), rec)
	switch {
	case err != nil:
		d.Log.Warn("could not write this daemon's machine record", "dir", daemonrecord.Dir(), "err", err)
	case !written && daemonrecord.Dir() != "":
		d.Log.Info("another daemon of this program already holds the machine record; this one is not the machine's", "program", dm.program)
	}
	return rec, written && err == nil
}

// removeRecord removes rec if it is still this daemon's.
func (dm *Daemon) removeRecord(rec daemonrecord.Record) {
	if err := daemonrecord.Remove(daemonrecord.Dir(), rec); err != nil {
		dm.Deps.Log.Warn("could not remove this daemon's machine record", "err", err)
	}
}

// routeModules are the modules the router's routes come from, sorted.
func (dm *Daemon) routeModules() []string {
	set := map[string]bool{}
	if dm.Router != nil {
		for _, r := range dm.Router.Routes() {
			if r.Module != "" {
				set[r.Module] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}
