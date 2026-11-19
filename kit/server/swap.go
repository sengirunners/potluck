package server

// Swaps (SPEC 2, "Swaps"): the daemon's manifest, the swap record, the
// stop guard's swap lock and the start-up self-check. The logic that does
// not need a daemon is in kit/swap.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/swap"
)

// Files in $ZM_HOME.
const (
	// FileServed is the last clean self-check: what this home's daemon
	// served, and its build.
	FileServed = "served.json"
	// FileLastSwap is the last swap record.
	FileLastSwap = "swap-last.json"
	// FileLastUpdate is the last update check (machine numbers' zm.last).
	FileLastUpdate = "update-last.json"
)

// Self-check pacing.
var (
	SelfCheckRetry = time.Minute      // while problems remain
	SelfCheckEvery = 10 * time.Minute // when clean
)

// stopNote is the stopping event's data.
type stopNote struct {
	By    string `json:"by"`
	What  string `json:"what,omitempty"`
	Force bool   `json:"force,omitempty"`
}

type swapState struct {
	mu     sync.Mutex
	self   *api.SelfCheck
	last   *api.SwapRecord
	update *api.UpdateCheck
}

// Schema lists the migrations this daemon registers, per database kind:
// the store's (its base and what the wired modules added), plus the
// databases' the store does not open (core.Deps.AddSchema).
func (dm *Daemon) Schema() map[string][]string {
	st := dm.Deps.Store
	m := map[string][]string{
		swap.KindRegistry: st.Migrations(store.KindRegistry),
		swap.KindProject:  st.Migrations(store.KindProject),
		swap.KindReplica:  st.Migrations(store.KindReplica),
	}
	for k, list := range dm.Deps.Schemas() {
		m[k] = append(m[k], list...)
	}
	return m
}

func (dm *Daemon) build() api.BuildManifest {
	var pats []string
	for _, rt := range dm.Router.Routes() {
		pats = append(pats, rt.Pattern)
	}
	proxies := func(string, string) bool { return false }
	if pl, ok := dm.Deps.Team.(core.ProxyLister); ok {
		proxies = pl.Proxies
	}
	return swap.NewBuild(pats, proxies, dm.Schema())
}

// ThisBuild describes the build of a daemon wired by w: it wires a
// throwaway daemon in a temp home (nothing listens, nothing starts) to list
// its routes.
func ThisBuild(ctx context.Context, w Wiring) (api.BuildManifest, error) {
	home, err := os.MkdirTemp("", "zmb")
	if err != nil {
		return api.BuildManifest{}, err
	}
	defer os.RemoveAll(home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		return api.BuildManifest{}, err
	}
	cfg.Machine = "preflight"
	dm, err := New(ctx, cfg, discardLog(), w)
	if err != nil {
		return api.BuildManifest{}, err
	}
	defer dm.Close()
	return dm.build(), nil
}

// manifest is GET /v0/daemon/manifest.
func (dm *Daemon) manifest(w http.ResponseWriter, r *http.Request) {
	m := api.DaemonManifest{Build: dm.build(), Used: nonNil(dm.Router.Used()), Joined: []string{}}
	m.Exe, _ = os.Executable()
	m.Args = append([]string{}, os.Args[1:]...)
	if rs, ok := dm.Deps.Team.(core.JoinedLister); ok {
		m.Joined = nonNil(rs.JoinedSlugs())
	}
	applied, err := swap.Applied(r.Context(), dm.Deps.Config.Home)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	m.Applied = applied
	api.WriteJSON(w, http.StatusOK, m)
}

// recordSwap is POST /v0/daemon/swaps: the caller is who swapped.
func (dm *Daemon) recordSwap(w http.ResponseWriter, r *http.Request) {
	var rec api.SwapRecord
	if err := api.DecodeJSON(r, &rec); err != nil {
		api.WriteError(w, err)
		return
	}
	switch rec.What {
	case "stop", "restart", "upgrade", "update":
	default:
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "what must be stop, restart, upgrade or update"))
		return
	}
	rec.By, rec.At = core.SubjectOf(r.Context()).Address, time.Now().UTC()
	if rec.Result == "" {
		rec.Result = "ok"
	}
	if rec.What == "update" {
		dm.publish(r.Context(), api.EventDaemonUpdateChecked, dm.Deps.Config.Machine, "", "", rec)
		dm.noteUpdate(api.UpdateCheck{At: rec.At, Result: rec.Result, To: rec.To, Reason: rec.Reason})
	} else {
		dm.noteSwap(r.Context(), rec)
	}
	api.WriteJSON(w, http.StatusOK, rec)
}

func (dm *Daemon) noteSwap(ctx context.Context, rec api.SwapRecord) {
	d := dm.Deps
	d.Log.Warn("daemon swap", "what", rec.What, "by", rec.By, "from", rec.From, "to", rec.To, "result", rec.Result,
		"forced", rec.Forced, "overridden", strings.Join(rec.Overridden, "; "), "reason", rec.Reason)
	dm.publish(ctx, api.EventDaemonSwapped, d.Config.Machine, "", "", rec)
	dm.swap.mu.Lock()
	dm.swap.last = &rec
	dm.swap.mu.Unlock()
	if b, err := json.Marshal(rec); err == nil {
		_ = writeFile(d.Config.Path(FileLastSwap), b)
	}
}

func (dm *Daemon) loadLastSwap() {
	if b, err := os.ReadFile(dm.Deps.Config.Path(FileLastUpdate)); err == nil {
		var u api.UpdateCheck
		if json.Unmarshal(b, &u) == nil && u.Result != "" {
			dm.swap.update = &u
		}
	}
	b, err := os.ReadFile(dm.Deps.Config.Path(FileLastSwap))
	if err != nil {
		return
	}
	var rec api.SwapRecord
	if json.Unmarshal(b, &rec) == nil && rec.What != "" {
		dm.swap.last = &rec
	}
}

// noteUpdate keeps the last update check for machine numbers.
func (dm *Daemon) noteUpdate(u api.UpdateCheck) {
	dm.swap.mu.Lock()
	dm.swap.update = &u
	dm.swap.mu.Unlock()
	if b, err := json.Marshal(u); err == nil {
		_ = writeFile(dm.Deps.Config.Path(FileLastUpdate), b)
	}
}

// zmState is this machine's zm build and release channel state, for its
// machine numbers (core.Deps.Zm; SPEC 2 "Release channel"): the release it
// installed, the hold switch, the last update check, and the releases it
// vouches for (ran release.Soak with no restart, the one it runs now
// included once it has) or vetoes (failed here).
func (dm *Daemon) zmState() *api.MachineZm {
	d := dm.Deps
	home := d.Config.Home
	z := &api.MachineZm{Version: api.Version, Commit: api.Commit, Started: d.Started}
	st, installed := release.ReadState(home) // a home that never installed may still have failed installs
	if st.Runs(installed, api.Version, api.Commit) == release.RunsInstalled {
		z.Release, z.Installed = st.Version, st.Installed
	}
	z.Stable, z.Failed = slices.Clone(st.Stable), st.FailedVersions()
	if installed && st.StableNow(api.Version, api.Commit, d.Started, time.Now()) && !slices.Contains(z.Stable, st.Version) {
		z.Stable = append([]string{st.Version}, z.Stable...)
	}
	if h, held, err := release.ReadHold(release.HoldPath(home)); held || err != nil {
		z.Held, z.HeldSince = cmp.Or(h.Reason, "held"), h.Since
	}
	dm.swap.mu.Lock()
	if u := dm.swap.update; u != nil {
		c := *u
		z.Last = &c
	}
	dm.swap.mu.Unlock()
	return z
}

// stopGuard refuses a stop that would break a swap in progress or end
// managed sessions (unless force).
func (dm *Daemon) stopGuard(ctx context.Context, req api.DaemonStopRequest) error {
	if h, held := swap.Holder(dm.Deps.Config.Home); held && (req.Swap == "" || req.Swap != h.Nonce) {
		return api.Errorf(api.CodeConflict, "%s", (&swap.BusyError{Holder: h}).Error()).
			WithHint("wait for it to finish; " + config.Command("status") + " shows the last swap")
	}
	if req.Force {
		return nil
	}
	if h := dm.Deps.SessionHooks(); h != nil && h.StopBlocked != nil {
		return h.StopBlocked(ctx)
	}
	return nil
}

// selfCheckLoop checks at start what this daemon serves, then every
// SelfCheckRetry while problems remain, else every SelfCheckEvery.
func (dm *Daemon) selfCheckLoop(ctx context.Context) {
	for {
		sc := dm.SelfCheck(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := SelfCheckEvery
		if !sc.OK {
			wait = SelfCheckRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// SelfCheck runs the self-check once: the served set through this
// daemon's own socket handler, every database's migrations against this
// build, and what the last build on this home served that this one does
// not. A clean check becomes the new served.json.
func (dm *Daemon) SelfCheck(ctx context.Context) api.SelfCheck {
	d := dm.Deps
	c := client.WithHandler(dm.SocketHandler())
	now := swap.Probe(ctx, c, d.Probes()...)
	problems := swap.Failures(now)
	applied, err := swap.Applied(ctx, d.Config.Home)
	if err != nil {
		problems = append(problems, "schema: "+err.Error())
	}
	pf := swap.Check(dm.build(), nil, applied, swap.Databases(d.Config.Home))
	problems = append(problems, pf.Problems...)
	if prev, ok := readServed(d.Config.Path(FileServed)); ok && prev.Commit != api.Commit {
		for _, p := range swap.Regressions(prev, now) {
			if strings.HasSuffix(p, "missing now") { // a failing one is listed already
				problems = append(problems, fmt.Sprintf("%s (the previous build %s served it)", p, prev.Version+" ("+prev.Commit+")"))
			}
		}
	}
	sc := api.SelfCheck{OK: len(problems) == 0, Checked: time.Now().UTC(), Problems: problems}
	if ctx.Err() != nil {
		return sc
	}
	if sc.OK {
		if b, err := json.Marshal(now); err == nil {
			_ = writeFile(d.Config.Path(FileServed), b)
		}
	}
	dm.swap.mu.Lock()
	prev := dm.swap.self
	dm.swap.self = &sc
	dm.swap.mu.Unlock()
	changed := prev == nil && !sc.OK || prev != nil && !slices.Equal(prev.Problems, sc.Problems)
	switch {
	case !changed:
	case sc.OK:
		d.Log.Info("self-check: every project and issue home is served again")
		_, _ = d.Events.Publish(ctx, events.Input{Type: api.EventDaemonSelfCheck, Subject: d.Config.Machine, Data: sc})
	default:
		for _, p := range sc.Problems {
			d.Log.Error("self-check: cannot serve", "problem", p)
		}
		_, _ = d.Events.Publish(ctx, events.Input{Type: api.EventDaemonSelfCheck, Subject: d.Config.Machine, Data: sc})
	}
	return sc
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func readServed(path string) (api.ServeCheck, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return api.ServeCheck{}, false
	}
	var sc api.ServeCheck
	return sc, json.Unmarshal(b, &sc) == nil && sc.Commit != ""
}

// writeFile replaces path atomically (0600).
func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
