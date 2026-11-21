package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/swap"
)

func swapDaemon(t *testing.T, home string) *Daemon {
	t.Helper()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := New(context.Background(), cfg, DiscardLog(), wiring.Kit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	return dm
}

// TestStopRefusedDuringSwap: while swap.lock is held, the stop route
// refuses everyone but its holder (by nonce); a dry run stops nothing.
func TestStopRefusedDuringSwap(t *testing.T) {
	ctx := context.Background()
	home := shortHome(t)
	dm := swapDaemon(t, home)
	c := client.WithHandler(dm.SocketHandler())
	l, err := swap.Acquire(home, api.SwapHolder{By: "mini/aaaa1111", What: "upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_, err = c.StopDaemon(ctx, api.DaemonStopRequest{Force: true})
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeConflict || !strings.Contains(ae.Message, "upgrade of this daemon is in progress: mini/aaaa1111") {
		t.Fatalf("stop during a swap: %v", err)
	}
	if _, err := c.StopDaemon(ctx, api.DaemonStopRequest{Swap: "wrong", DryRun: true}); err == nil {
		t.Fatal("a wrong nonce must be refused")
	}
	r, err := c.StopDaemon(ctx, api.DaemonStopRequest{Swap: l.Holder.Nonce, DryRun: true})
	if err != nil || r.Stopping {
		t.Fatalf("holder's dry run: %+v %v", r, err)
	}
	select {
	case <-dm.Stopped():
		t.Fatal("a refused or dry stop stopped the daemon")
	default:
	}
}

// TestSelfCheckReports: a database newer than the build and an issue home
// the previous build served are reported in health, not fatal.
func TestSelfCheckReports(t *testing.T) {
	ctx := context.Background()
	home := shortHome(t)
	dm := swapDaemon(t, home)
	c := client.WithHandler(dm.SocketHandler())
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if sc := dm.SelfCheck(ctx); !sc.OK {
		t.Fatalf("a fresh home must be clean: %v", sc.Problems)
	}
	if _, err := os.Stat(filepath.Join(home, FileServed)); err != nil {
		t.Fatal("a clean check writes served.json")
	}
	// A newer zm applied a migration; the previous build served orchid's issues.
	db, err := sql.Open("sqlite", filepath.Join(home, "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES ('future/9999_x', 0)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	prev := api.ServeCheck{Version: "v9", Commit: "older", Projects: []string{"orchid"},
		Items: []api.ServeItem{{Kind: swap.ItemIssues, Name: "orchid", OK: true}}}
	b, _ := json.Marshal(prev)
	if err := os.WriteFile(filepath.Join(home, FileServed), b, 0o600); err != nil {
		t.Fatal(err)
	}
	sc := dm.SelfCheck(ctx)
	all := strings.Join(sc.Problems, "\n")
	if sc.OK || !strings.Contains(all, "registry.db has migration future/9999_x") ||
		!strings.Contains(all, "issues orchid: served before, missing now (the previous build v9 (older) served it)") {
		t.Fatalf("self-check: %+v", sc)
	}
	h, err := c.Health(ctx)
	if err != nil || h.SelfCheck == nil || h.SelfCheck.OK || len(h.SelfCheck.Problems) != 2 {
		t.Fatalf("health: %+v %v", h.SelfCheck, err)
	}
	// The baseline is not overwritten by a check with problems.
	if got, _ := ReadServed(filepath.Join(home, FileServed)); got.Commit != "older" {
		t.Fatal("served.json overwritten by a failing check")
	}
	// Network callers never see it.
	if remote := dm.HealthBody(false); remote.SelfCheck != nil || remote.LastSwap != nil {
		t.Fatal("selfcheck and last swap are local only")
	}
}

func TestSwapRecorded(t *testing.T) {
	ctx := context.Background()
	home := shortHome(t)
	dm := swapDaemon(t, home)
	c := client.WithHandler(dm.SocketHandler())
	rec, err := c.RecordSwap(ctx, api.SwapRecord{What: "upgrade", From: "v1 (a)", To: "v2 (b)", Forced: true, Overridden: []string{"x"}})
	if err != nil || rec.By != "human:ana" || rec.Result != "ok" {
		t.Fatalf("record: %+v %v", rec, err)
	}

	if _, err := c.RecordSwap(ctx, api.SwapRecord{What: "update", Result: "held", Reason: "quiet window"}); err != nil {
		t.Fatal(err)
	}
	var updates []api.Event
	if err := c.Events(ctx, api.EventQuery{Types: api.EventDaemonUpdateChecked, Since: "0", NoLive: true}, func(e api.Event) error { updates = append(updates, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("update events: %+v", updates)
	}
	var update api.SwapRecord
	if err := json.Unmarshal(updates[0].Data, &update); err != nil || update.Result != "held" || update.By != "human:ana" {
		t.Fatalf("update event: %+v %v", update, err)
	}
	if _, err := c.RecordSwap(ctx, api.SwapRecord{What: "dance"}); err == nil {
		t.Fatal("unknown what accepted")
	}
	// The next daemon on this home, once this one stopped (it held the
	// home's writer lock).
	dm.Close()
	dm2 := swapDaemon(t, home)
	h, _ := client.WithHandler(dm2.SocketHandler()).Health(ctx)
	if h.LastSwap == nil || h.LastSwap.To != "v2 (b)" || !h.LastSwap.Forced {
		t.Fatalf("last swap after a restart: %+v", h.LastSwap)
	}
}

// Machine numbers carry this machine's zm build and release channel state
// (SPEC 2 "Release channel"): the release it runs, the releases it vouches
// for (the one it runs once it ran release.Soak with no restart) and
// vetoes, the hold switch, and the last update check, kept across a
// restart.
func TestZmStateInMachineNumbers(t *testing.T) {
	ctx := context.Background()
	old, oldVersion := api.Commit, api.Version
	api.Commit, api.Version = "1eacf23e722f2df10a113480c8496e29607ba005", "main-1eacf23e722f" // the release build
	t.Cleanup(func() { api.Commit, api.Version = old, oldVersion })
	home := shortHome(t)
	inst := time.Now().Add(-release.Soak - time.Minute)
	st := release.State{Version: "main-1eacf23e722f", Commit: api.Commit, Created: inst.Add(-time.Hour), Installed: inst, Stable: []string{"main-0000000older"}}
	st.MarkFailed(release.Failure{Version: "main-00000000bad", At: inst, Result: "rolled_back"})
	if err := release.WriteState(home, st); err != nil {
		t.Fatal(err)
	}
	dm := swapDaemon(t, home)
	z := dm.ZmState() // this daemon started after the install: a restart, so not yet a vouch
	if z.Version != api.Version || z.Release != st.Version || !z.Installed.Equal(inst.UTC()) || slices.Contains(z.Stable, st.Version) ||
		!slices.Equal(z.Stable, []string{"main-0000000older"}) || !slices.Equal(z.Failed, []string{"main-00000000bad"}) || z.Held != "" || z.Last != nil {
		t.Fatalf("zm %+v", z)
	}
	// Built from source at the release's commit (a modified tree): not the
	// release, so no release is claimed.
	api.Commit, api.Version = "1eacf23e722f", "v0.0.0-20261112000000-1eacf23e722f+dirty"
	if z := dm.ZmState(); z.Release != "" || !z.Installed.IsZero() {
		t.Fatalf("a source build claims the release: %+v", z)
	}
	api.Commit, api.Version = "1eacf23e722f2df10a113480c8496e29607ba005", st.Version
	dm.Deps.Started = inst.Add(-time.Second) // no restart since the install
	if z := dm.ZmState(); !slices.Equal(z.Stable, []string{st.Version, "main-0000000older"}) {
		t.Fatalf("stable %v", z.Stable)
	}
	if err := release.WriteHold(release.HoldPath(home), release.Hold{Reason: "testing a fix", Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	c := client.WithHandler(dm.SocketHandler())
	if _, err := c.RecordSwap(ctx, api.SwapRecord{What: "update", Result: "held", Reason: "hold file exists"}); err != nil {
		t.Fatal(err)
	}
	l, err := c.Machines(ctx)
	if err != nil || len(l.Machines) != 1 {
		t.Fatalf("machines %+v %v", l, err)
	}
	if z := l.Machines[0].Stats.Build(); z == nil || l.Machines[0].Stats.Zm != nil || z.Held != "testing a fix" || z.HeldSince.IsZero() || z.Last == nil || z.Last.Result != "held" || z.Last.Reason != "hold file exists" {
		t.Fatalf("machine numbers zm %+v", z)
	}
	// The next daemon on this home, once this one stopped (it held the
	// home's writer lock).
	dm.Close()
	dm2 := swapDaemon(t, home)
	if z := dm2.ZmState(); z.Last == nil || z.Last.Result != "held" {
		t.Fatalf("last check after a restart: %+v", z.Last)
	}
}

// Machine numbers carry the update job the program reports through
// Deps.UpdateJob, read each time they are built; a daemon with no hook
// reports none.
func TestUpdateJobInMachineNumbers(t *testing.T) {
	ctx := context.Background()
	dm := swapDaemon(t, shortHome(t))
	c := client.WithHandler(dm.SocketHandler())
	build := func() *api.MachineZm {
		t.Helper()
		l, err := c.Machines(ctx)
		if err != nil || len(l.Machines) != 1 || l.Machines[0].Stats.Build() == nil {
			t.Fatalf("machines %+v %v", l, err)
		}
		return l.Machines[0].Stats.Build()
	}
	if z := build(); z.Job != nil {
		t.Fatalf("no hook, yet a job: %+v", z.Job)
	}
	minAge := int64(0)
	dm.Deps.UpdateJob = func() *api.UpdateJob {
		if minAge < 0 {
			return &api.UpdateJob{}
		}
		v := minAge
		return &api.UpdateJob{MinAgeS: &v}
	}
	if z := build(); z.Job == nil || z.Job.MinAgeS == nil || *z.Job.MinAgeS != 0 {
		t.Fatalf("job taking a release at once: %+v", z.Job)
	}
	minAge = 3600
	if z := build(); z.Job == nil || z.Job.MinAgeS == nil || *z.Job.MinAgeS != 3600 {
		t.Fatalf("the job changed, the numbers did not: %+v", z.Job)
	}
	minAge = -1
	if z := build(); z.Job == nil || z.Job.MinAgeS != nil {
		t.Fatalf("min-age unknown: %+v", z.Job)
	}
	dm.Deps.UpdateJob = func() *api.UpdateJob { return nil }
	if z := build(); z.Job != nil {
		t.Fatalf("the hook knows no job, yet one is reported: %+v", z.Job)
	}
}
