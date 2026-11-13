package issuers

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Module is the daemon's github-token issuer (docs/daemon.md "Secrets
// issuers"): built from config.json's secrets.issuers when it names one
// github-app issuer, hooked to the grants package (Register) and to every
// box's end (rt.Service.OnEnd) and to every machine class change
// (teamlink's ClassChanged: a demoted host's boxes lose their tokens at
// once), and run: Start once, Tick whenever it is due (and again after
// every Issue), a Sweep against the run targets' record and a class check
// of every box every SweepEvery (a class can also fall without a change
// being set: a join that ends), Close at shutdown. Wire it after run targets and
// before anything that issues (the runner broker): modules start in order
// and close in reverse, so its callers stop before it closes.
//
// With no issuer configured, or one the daemon cannot read as safe, it does
// nothing and From answers nil: nothing is minted.
type Module struct {
	d    *core.Deps
	iss  *GitHubToken
	door func() rt.Door

	// SweepEvery is how often boxes holding a token are checked against
	// the run targets' record (default 5 minutes).
	SweepEvery time.Duration

	started chan struct{} // closed once Start settled what an earlier run left
	wake    chan struct{}
	classes chan struct{} // a machine's class changed: check every box's
	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// Minter is the issuer as a caller that issues uses it (the runner broker):
// Issue waits until the issuer has started.
type Minter interface {
	Issue(ctx context.Context, req Request) (Issued, error)
	End(ctx context.Context, box Box, why EndReason) error
}

const minterService = "issuers.minter"

// From is the daemon's running issuer, or nil when none is configured.
func From(d *core.Deps) Minter {
	m, _ := d.Service(minterService).(Minter)
	return m
}

// NewModule builds the module from d's config. A configuration it cannot
// use is logged and leaves it idle.
func NewModule(d *core.Deps) *Module {
	d.AddMigrations(Migrations...)
	m := &Module{d: d, door: func() rt.Door { return rt.DoorFrom(d) }, SweepEvery: 5 * time.Minute,
		started: make(chan struct{}), wake: make(chan struct{}, 1), classes: make(chan struct{}, 1)}
	logErr := func(msg string, err error) {
		if d.Log != nil {
			d.Log.Error("secrets issuer: "+msg+"; nothing is minted", "err", err)
		}
	}
	specs, err := Specs(d.Config.File)
	if err != nil {
		logErr("secrets.issuers is not valid", err)
		return m
	}
	var names []string
	for name, s := range specs {
		if s.Kind == KindGitHubApp {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return m
	case 1:
	default:
		logErr("secrets.issuers names more than one github-app issuer", errors.New("one github-token issuer per daemon: "+names[0]+", "+names[1]))
		return m
	}
	iss, err := New(context.Background(), specs[names[0]], Options{
		Deliver:  DeliverFunc(m.deliver),
		Ledger:   registryLedger{d},
		ClassOf:  m.classOf,
		Live:     m.live,
		Check:    GrantCheck(d, m.classOf),
		JobHolds: m.jobHolds,
		Events:   eventsOf{d},
		Log:      d.Log,
	})
	if err != nil {
		logErr("the issuer could not be built", err)
		return m
	}
	m.iss = iss
	Register(d, iss)
	d.Provide(minterService, Minter(m))
	if link := teamlink.From(d); link != nil {
		link.Hooks.ClassChanged = append(link.Hooks.ClassChanged, func(context.Context, string, string, api.MachineClass) { m.classChanged() })
	}
	if svc := rt.From(d); svc != nil {
		svc.OnEnd(func(ctx context.Context, project string, h computeapi.RunTargetHandle) {
			if err := iss.End(ctx, Box{Project: project, Area: h.Area, ID: h.Handle}, endReason(h.State)); err != nil && d.Log != nil {
				d.Log.Warn("secrets issuer: revoking an ended box's tokens", "box", h.Handle, "err", err)
			}
		})
	}
	return m
}

// endReason is why an ended handle's tokens are revoked, from its state.
func endReason(state string) EndReason {
	switch state {
	case "released":
		return EndReleased
	case "expired":
		return EndExpired
	case "reclaimed":
		return EndReclaimed
	case "lost":
		return EndLost
	}
	return EndGone
}

// classChanged asks the loop to check every box's machine class now.
func (m *Module) classChanged() {
	select {
	case m.classes <- struct{}{}:
	default:
	}
}

const jobHoldsService = "issuers.jobholds"

// ProvideJobHolds registers the runner broker's answer to whether a job
// still holds its box (Options.JobHolds), before the daemon serves.
func ProvideJobHolds(d *core.Deps, f JobHolds) { d.Provide(jobHoldsService, f) }

// jobHolds is the broker's JobHolds, read at each call; with none
// registered no job holds anything.
func (m *Module) jobHolds(ctx context.Context, req Request) (bool, error) {
	f, _ := m.d.Service(jobHoldsService).(JobHolds)
	if f == nil {
		return false, nil
	}
	return f(ctx, req)
}

func (m *Module) deliver(ctx context.Context, box Box, name string, v secrets.Value) error {
	door := m.door()
	if door == nil {
		return api.Errorf(api.CodeUnavailable, "run targets are off on this machine")
	}
	return door.BoxSecret(ctx, box.Project, box.ID, name, v)
}

// classOf is the class of the machine serving box, from the run targets'
// record of it: never the request's word.
func (m *Module) classOf(ctx context.Context, box Box) (string, error) {
	door := m.door()
	if door == nil {
		return "", api.Errorf(api.CodeUnavailable, "run targets are off on this machine")
	}
	return door.BoxClass(ctx, box.Project, box.ID)
}

func (m *Module) live(ctx context.Context, box Box) (bool, error) {
	door := m.door()
	if door == nil {
		return false, api.Errorf(api.CodeUnavailable, "run targets are off on this machine")
	}
	return door.BoxLive(ctx, box.Project, box.ID)
}

// Routes: the module serves none (grants.New serves the grants' routes).
func (m *Module) Routes(*core.Router) {}

// Start runs the issuer in the background: its Start (retried each minute
// until it succeeds; until then Issue waits), then Tick and Sweep.
func (m *Module) Start(ctx context.Context) error {
	if m.iss == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return nil
	}
	ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(1)
	go m.run(ctx)
	return nil
}

func (m *Module) run(ctx context.Context) {
	defer m.wg.Done()
	for {
		err := m.iss.Start(ctx)
		if err == nil {
			break
		}
		if m.d.Log != nil {
			m.d.Log.Error("secrets issuer: start failed; nothing is issued until it succeeds", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryEvery):
		}
	}
	close(m.started)
	sweep := time.NewTicker(m.SweepEvery)
	defer sweep.Stop()
	for {
		next := m.iss.Tick(ctx)
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-m.wake:
		case <-t.C:
		case <-m.classes:
			m.iss.EndUntrusted(ctx)
		case <-sweep.C:
			m.iss.Sweep(ctx, func(b Box) bool {
				live, err := m.live(ctx, b)
				return live || err != nil // only the record's "ended" or "never was" ends a box
			})
			m.iss.EndUntrusted(ctx)
		}
		t.Stop()
	}
}

// Issue is the issuer's Issue once it has started; then Tick is re-armed so
// the new token's renewal is scheduled.
func (m *Module) Issue(ctx context.Context, req Request) (Issued, error) {
	select {
	case <-m.started:
	case <-ctx.Done():
		return Issued{}, api.Errorf(api.CodeUnavailable, "github-token: the issuer has not started yet")
	}
	out, err := m.iss.Issue(ctx, req)
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return out, err
}

// End revokes every token issued for box.
func (m *Module) End(ctx context.Context, box Box, why EndReason) error {
	return m.iss.End(ctx, box, why)
}

// Close stops the loop, then revokes everything still held.
func (m *Module) Close() error {
	if m.iss == nil {
		return nil
	}
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		m.wg.Wait()
	}
	ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	return m.iss.Close(ctx)
}

// registryLedger is SQLLedger over d's registry, read at each call (Deps
// fields are set before serving, not before construction).
type registryLedger struct{ d *core.Deps }

func (l registryLedger) led() SQLLedger { return SQLLedger{DB: l.d.Store.Registry()} }
func (l registryLedger) Record(ctx context.Context, e Entry) error {
	return l.led().Record(ctx, e)
}
func (l registryLedger) Outstanding(ctx context.Context, issuer string) ([]Entry, error) {
	return l.led().Outstanding(ctx, issuer)
}
func (l registryLedger) Assign(ctx context.Context, issuer string, req Request) error {
	return l.led().Assign(ctx, issuer, req)
}
func (l registryLedger) Unassign(ctx context.Context, issuer string, box Box, name string) error {
	return l.led().Unassign(ctx, issuer, box, name)
}
func (l registryLedger) Assigned(ctx context.Context, issuer string) ([]Request, error) {
	return l.led().Assigned(ctx, issuer)
}

// eventsOf is d's event log, read at each call.
type eventsOf struct{ d *core.Deps }

func (e eventsOf) Publish(ctx context.Context, in events.Input) (api.Event, error) {
	if e.d.Events == nil {
		return api.Event{}, api.Errorf(api.CodeUnavailable, "the event log is not running")
	}
	return e.d.Events.Publish(ctx, in)
}
