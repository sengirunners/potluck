package server_test

import (
	"context"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

// hooksDaemon is a kit daemon whose session hooks are h (nil: none).
func hooksDaemon(t *testing.T, h *core.SessionHooks) (*Daemon, *client.Client) {
	t.Helper()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := New(context.Background(), cfg, DiscardLog(), func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Kit(d)
		if h != nil {
			d.SetSessionHooks(func() *core.SessionHooks { return h })
		}
		return mods, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	return dm, client.WithHandler(dm.SocketHandler())
}

func stopped(dm *Daemon) bool {
	select {
	case <-dm.Stopped():
		return true
	default:
		return false
	}
}

// The daemon stop and restart guards with session hooks missing or with
// nil fields: nothing is live, nothing blocks, nothing is ended; a forced
// stop with live sessions it cannot end is refused.
func TestDaemonStopWithNilSessionHooks(t *testing.T) {
	ctx := context.Background()
	for name, h := range map[string]*core.SessionHooks{"no hooks": nil, "nil fields": {}} {
		dm, c := hooksDaemon(t, h)
		r, err := c.StopDaemon(ctx, api.DaemonStopRequest{DryRun: true})
		if err != nil || r.ManagedSessions != 0 || r.Stopping {
			t.Fatalf("%s: dry run %+v %v", name, r, err)
		}
		if r, err := c.StopDaemon(ctx, api.DaemonStopRequest{Force: true}); err != nil || !r.Stopping || r.ManagedSessions != 0 || len(r.NotEnded) != 0 {
			t.Fatalf("%s: forced stop %+v %v", name, r, err)
		}
		if !stopped(dm) {
			t.Fatalf("%s: the daemon did not stop", name)
		}
	}
	// Live sessions but no End: a forced stop cannot report which ended.
	kept := 0
	dm, c := hooksDaemon(t, &core.SessionHooks{Live: func(context.Context) int { return 2 }, Keep: func() { kept++ }})
	if r, err := c.StopDaemon(ctx, api.DaemonStopRequest{DryRun: true}); err != nil || r.ManagedSessions != 2 {
		t.Fatalf("dry run with live sessions: %+v %v", r, err)
	}
	if _, err := c.StopDaemon(ctx, api.DaemonStopRequest{Force: true}); !api.IsCode(err, api.CodeUnavailable) || stopped(dm) || kept != 0 {
		t.Fatalf("forced stop with live sessions and no End: %v (stopped %v, kept %d)", err, stopped(dm), kept)
	}
	// nil StopBlocked: a plain stop goes ahead.
	if r, err := c.StopDaemon(ctx, api.DaemonStopRequest{}); err != nil || !r.Stopping || r.ManagedSessions != 2 || !stopped(dm) {
		t.Fatalf("plain stop with nil StopBlocked: %+v %v", r, err)
	}
	// StopBlocked refuses a plain stop; Force skips it.
	refusal := api.Errorf(api.CodeConflict, "busy")
	dm, c = hooksDaemon(t, &core.SessionHooks{StopBlocked: func(context.Context) error { return refusal }})
	if _, err := c.StopDaemon(ctx, api.DaemonStopRequest{}); !api.IsCode(err, api.CodeConflict) || stopped(dm) {
		t.Fatalf("blocked stop: %v", err)
	}
	if r, err := c.StopDaemon(ctx, api.DaemonStopRequest{Force: true}); err != nil || !r.Stopping {
		t.Fatalf("forced past a block: %+v %v", r, err)
	}
}
