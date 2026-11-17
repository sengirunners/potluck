package issuers

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Module is the daemon's issuers (docs/daemon.md "Secrets issuers"): built
// from config.json's secrets.issuers, at most one per kind (a github-app
// issuer mints github-token, a potluck issuer potluck-token), each hooked to
// the grants package (Register) and to every
// box's end (rt.Service.OnEnd) and to every machine class change
// (teamlink's ClassChanged: a demoted host's boxes lose their tokens at
// once), and run: Start once, Tick whenever it is due (and again after
// every Issue), a Sweep against the run targets' record and a class check
// of every box every SweepEvery (a class can also fall without a change
// being set: a join that ends), Close at shutdown. Wire it after run targets and
// before anything that issues (the runner broker): modules start in order
// and close in reverse, so its callers stop before it closes.
//
// With no issuer configured, or a section the daemon cannot read as safe, it
// does nothing and From answers nil: nothing is minted. Two issuers of one
// kind, or one it cannot build, leave that kind unminted.
type Module struct {
	d    *core.Deps
	iss  []*Minted
	door func() rt.Door

	// SweepEvery is how often boxes holding a token are checked against
	// the run targets' record (default 5 minutes).
	SweepEvery time.Duration

	started []chan struct{} // started[i] closes once iss[i]'s Start settled what an earlier run left
	wake    chan struct{}
	classes chan struct{} // a machine's class changed: check every box's
	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// Minter is the issuers as a caller that issues uses them (the runner
// broker): Issue goes to the issuer of the request's kind once every issuer
// has started; End ends the box at every issuer.
type Minter interface {
	Issue(ctx context.Context, req Request) (Issued, error)
	End(ctx context.Context, box Box, why EndReason) error
	// Mints reports whether an issuer of kind runs here.
	Mints(kind string) bool
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
		wake: make(chan struct{}, 1), classes: make(chan struct{}, 1)}
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
	byKind := map[string][]string{}
	for name, s := range specs {
		byKind[s.Kind] = append(byKind[s.Kind], name)
	}
	for _, kind := range []string{KindGitHubApp, KindPotluck} {
		names := byKind[kind]
		sort.Strings(names)
		switch len(names) {
		case 0:
			continue
		case 1:
		default:
			logErr("secrets.issuers names more than one "+kind+" issuer", errors.New("one "+kind+" issuer per daemon: "+names[0]+", "+names[1]))
			continue
		}
		iss, err := New(context.Background(), specs[names[0]], Options{
			Deliver:         DeliverFunc(m.deliver),
			Ledger:          registryLedger{d},
			ClassOf:         m.classOf,
			BoxTakesSecrets: m.boxTakesSecrets,
			Live:            m.live,
			Check:           GrantCheck(d, m.classOf, mints[kind]),
			JobHolds:        m.jobHolds,
			JobBox:          m.jobBox,
			Events:          eventsOf{d},
			Tokens:          registryTokens{d},
			BoxExpires:      m.boxExpires,
			Home:            m.home,
			Log:             d.Log,
		})
		if err != nil {
			logErr("the "+names[0]+" issuer could not be built", err)
			continue
		}
		m.iss = append(m.iss, iss)
		m.started = append(m.started, make(chan struct{}))
		Register(d, iss)
	}
	if len(m.iss) == 0 {
		return m
	}
	d.Provide(minterService, Minter(m))
	if link := teamlink.From(d); link != nil {
		link.Hooks.ClassChanged = append(link.Hooks.ClassChanged, func(context.Context, string, string, api.MachineClass) { m.classChanged() })
	}
	if svc := rt.From(d); svc != nil {
		svc.OnEnd(func(ctx context.Context, project string, h computeapi.RunTargetHandle) {
			if err := m.End(ctx, Box{Project: project, Area: h.Area, ID: h.Handle}, endReason(h.State)); err != nil && d.Log != nil {
				d.Log.Warn("secrets issuer: revoking an ended box's credentials", "box", h.Handle, "err", err)
			}
		})
	}
	return m
}

// home is this daemon's tunnel, as a box's credential names it.
func (m *Module) home() (string, string, error) {
	s := tunnel.From(m.d)
	if s == nil {
		return "", "", api.Errorf(api.CodeUnavailable, "this daemon runs no tunnel")
	}
	return s.Home()
}

// mints is the credential kind each issuer kind mints.
var mints = map[string]string{KindGitHubApp: KindGitHubToken, KindPotluck: KindPotluckToken}

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

const jobBoxService = "issuers.jobbox"

// ProvideJobBox registers the runner broker's answer to whether a box is
// its box for a GitHub job (Options.JobBox), before the daemon serves.
func ProvideJobBox(d *core.Deps, f JobBox) { d.Provide(jobBoxService, f) }

// jobBox is the broker's JobBox, read at each call; with none registered
// no box is a job's broker box.
func (m *Module) jobBox(ctx context.Context, req Request) (bool, error) {
	f, _ := m.d.Service(jobBoxService).(JobBox)
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

// boxTakesSecrets is why box's host may not be given a secret now, nil when it
// may (the run targets' check of its swap).
func (m *Module) boxTakesSecrets(ctx context.Context, box Box) error {
	door := m.door()
	if door == nil {
		return api.Errorf(api.CodeUnavailable, "run targets are off on this machine")
	}
	return door.BoxSwapSafe(ctx, box.Project, box.ID)
}

// boxExpires is box's lease end, from the run targets' record of it.
func (m *Module) boxExpires(ctx context.Context, box Box) (time.Time, error) {
	door := m.door()
	if door == nil {
		return time.Time{}, api.Errorf(api.CodeUnavailable, "run targets are off on this machine")
	}
	return door.BoxExpires(ctx, box.Project, box.ID)
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

// Start runs the issuers in the background: each one's Start (retried each
// minute until it succeeds; until then Issue for its kind waits), and Tick
// and Sweep for those that have started.
func (m *Module) Start(ctx context.Context) error {
	if len(m.iss) == 0 {
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
	// Each issuer starts on its own: one that cannot start (its source is
	// down) holds up neither the other kinds' issues nor their renewals.
	for i, iss := range m.iss {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			for {
				err := iss.Start(ctx)
				if err == nil {
					break
				}
				if m.d.Log != nil {
					m.d.Log.Error("secrets issuer: start failed; it issues nothing until it succeeds", "issuer", iss.Name(), "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(retryEvery):
				}
			}
			close(m.started[i])
			m.poke()
		}()
	}
	sweep := time.NewTicker(m.SweepEvery)
	defer sweep.Stop()
	for {
		live := m.running()
		next := time.Now().Add(tickCap)
		for _, iss := range live {
			if n := iss.Tick(ctx); n.Before(next) {
				next = n
			}
		}
		t := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-m.wake:
		case <-t.C:
		case <-m.classes:
			for _, iss := range live {
				iss.EndUntrusted(ctx)
			}
		case <-sweep.C:
			for _, iss := range live {
				iss.Sweep(ctx, func(b Box) bool {
					live, err := m.live(ctx, b)
					return live || err != nil // only the record's "ended" or "never was" ends a box
				})
				iss.EndUntrusted(ctx)
			}
		}
		t.Stop()
	}
}

// running is the issuers whose Start has succeeded.
func (m *Module) running() []*Minted {
	var out []*Minted
	for i, iss := range m.iss {
		select {
		case <-m.started[i]:
			out = append(out, iss)
		default:
		}
	}
	return out
}

// poke re-arms the loop now.
func (m *Module) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Mints reports whether an issuer of kind runs here (started or not).
func (m *Module) Mints(kind string) bool {
	for _, iss := range m.iss {
		if iss.Kind() == kind {
			return true
		}
	}
	return false
}

// Issue is the Issue of the issuer of req's kind once that issuer has
// started; then Tick is re-armed so the new credential's renewal is
// scheduled.
func (m *Module) Issue(ctx context.Context, req Request) (Issued, error) {
	at := -1
	for i, iss := range m.iss {
		if iss.Kind() == req.Kind {
			at = i
		}
	}
	if at < 0 {
		return Issued{}, api.Errorf(api.CodeUnavailable, "no %q issuer runs on this machine", req.Kind)
	}
	select {
	case <-m.started[at]:
	case <-ctx.Done():
		return Issued{}, api.Errorf(api.CodeUnavailable, "%s: the issuer has not started yet", req.Kind)
	}
	out, err := m.iss[at].Issue(ctx, req)
	m.poke()
	return out, err
}

// End revokes every credential issued for box, at every issuer.
func (m *Module) End(ctx context.Context, box Box, why EndReason) error {
	var first error
	for _, iss := range m.iss {
		if err := iss.End(ctx, box, why); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Close stops the loop, then revokes everything still held.
func (m *Module) Close() error {
	if len(m.iss) == 0 {
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
	var first error
	for _, iss := range m.iss {
		if err := iss.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
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

// registryTokens is d's project registry as the token mint, read at each
// call.
type registryTokens struct{ d *core.Deps }

var errNoRegistry = api.Errorf(api.CodeUnavailable, "the project registry is not running")

func (t registryTokens) Lookup(ref string) (api.ProjectRef, bool) {
	if t.d.Projects == nil {
		return api.ProjectRef{}, false
	}
	return t.d.Projects.Lookup(ref)
}
func (t registryTokens) HasArea(project, area string) bool {
	return t.d.Projects != nil && t.d.Projects.HasArea(project, area)
}
func (t registryTokens) Identity(ctx context.Context, ref string) (api.Identity, error) {
	if t.d.Projects == nil {
		return api.Identity{}, errNoRegistry
	}
	return t.d.Projects.Identity(ctx, ref)
}
func (t registryTokens) Tuples(ctx context.Context, id string) ([]authz.Tuple, error) {
	if t.d.Projects == nil {
		return nil, errNoRegistry
	}
	return t.d.Projects.Tuples(ctx, id)
}
func (t registryTokens) Local(address string) *authz.Subject {
	if t.d.Projects == nil {
		return nil
	}
	return t.d.Projects.Local(address)
}
func (t registryTokens) Mint(ctx context.Context, sub *authz.Subject, req api.MintTokenRequest) (api.MintTokenResponse, error) {
	if t.d.Projects == nil || sub == nil {
		return api.MintTokenResponse{}, errNoRegistry
	}
	return t.d.Projects.Mint(ctx, sub, req)
}
func (t registryTokens) RevokeToken(ctx context.Context, id string) (api.Token, error) {
	if t.d.Projects == nil {
		return api.Token{}, errNoRegistry
	}
	return t.d.Projects.RevokeToken(ctx, id)
}
func (t registryTokens) TokenExpiry(ctx context.Context, id string) (time.Time, error) {
	if t.d.Projects == nil {
		return time.Time{}, errNoRegistry
	}
	return t.d.Projects.TokenExpiry(ctx, id)
}
func (t registryTokens) RevokeTokenHash(ctx context.Context, hash string) (bool, error) {
	if t.d.Projects == nil {
		return false, errNoRegistry
	}
	return t.d.Projects.RevokeTokenHash(ctx, hash)
}
