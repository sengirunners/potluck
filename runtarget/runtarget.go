// Package runtarget is the daemon's side of run targets (SPEC 20): owner-only
// bindings in project.toml, and describe, acquire, status, release, list
// and renew through each binding's potluck-driver-<driver> (or zm-run-<driver>) process. Drivers are
// plugins, never linked here; conformance is the suite
// they pass (the reference driver and potluck-driver-tart today; a farm driver over
// zm machines and a cloud Linux driver next). This package decides what a
// driver may be asked: routing by a live ls-remote, the stricter class,
// local-only acquire for guarded classes and databases, the scripts
// allowlist, caps and budget. Owned by run targets (zm-compute-runs lane).
package runtarget

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/placement"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// Service is the run targets module.
type Service struct {
	d *core.Deps
	// lookPath finds a driver in DriverDirs (tests point it at the
	// reference driver); pins checks what it found (pin.go).
	lookPath func(driver string) (string, error)
	pins     *pins
	// acquireWithin, when set, is an acquire's time limit in place of
	// acquireLimit (tests).
	acquireWithin time.Duration
	// now is the clock (tests).
	now func() time.Time
	// depsPoll, when set, is the wait between asks for a dependency layer
	// another box is building, in place of the driver's retry_s (tests).
	depsPoll time.Duration
	// depsActive counts each caller's acquires with deps now (deps.go).
	depsMu     sync.Mutex
	depsActive map[string]int
	// acquireMu serialises acquires per binding, so caps and budget are
	// judged against every earlier acquire.
	acquireMu sync.Map // project/target -> *sync.Mutex
	// requestMu serialises a box acquire's request id across the project's
	// bindings (taken before acquireMu), so two asks of one id on two
	// bindings cannot both find it unused and make two boxes.
	requestMu keyLocks // project/actor key -> lock
	// secretsMu serialises a box's holder secrets calls, so the names one
	// records and forgets are its own (boxsecret_names.go).
	secretsMu sync.Map // project/handle -> *sync.Mutex
	// inflight is the acquires being served now (acquires.go).
	inflight inFlight
	// cancels bounds cancels of request_ids never received; progressCaps
	// keeps whether a binding's driver serves progress (acquires.go).
	cancels      cancelQuota
	progressCaps sync.Map // project, binding, driver, host -> capsSeen
	// host is this machine's shared bindings (host.go).
	host hostState
	// images moves them to this build's image pins (hostimages.go).
	images hostImages
	// endMu guards ends and boxChecks, the OnEnd and OnBoxAcquire
	// subscribers.
	endMu     sync.Mutex
	ends      []rt.EndFunc
	boxChecks []rt.BoxAcquireFunc
	// link is teamlink (nil in a daemon without it); adverts and rtmem are
	// the authority's side of boxes across machines (relay.go).
	link    *teamlink.Service
	adverts runAdverts
	seal    sealState // the pool's sealing, as its machine heard it (sealing.go)
	rtmem   teamlink.OpMem
	// secretOps maps a relayed op whose arguments carry secrets or a
	// jit_config to its host, so the host's class is asked again when the
	// op is handed over (relay.go opArgs).
	secretOps sync.Map
	// opClass is the host class asked at that hand-over (tests set it; nil
	// asks teamlink).
	opClass func(ctx context.Context, project, machine string) string
	// enrolled says whether a machine may lend boxes to a project (tests set
	// it; nil asks teamlink).
	enrolled func(ctx context.Context, project, machine string) bool
	// relay stands in for RelayRunTarget (tests set it: a host with no team
	// link between).
	relay func(ctx context.Context, project, machine string, op computeapi.RunTargetOp, wait time.Duration) (json.RawMessage, error)
}

var _ core.Module = (*Service)(nil)

// Migrations are the run targets module's tables (New adds them).
var Migrations = slices.Concat(hostKeyMigrations, handleMigrations, settingsMigrations, boxSecretNameMigrations)

// New returns the run targets module.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, now: time.Now}
	grants.OnReserve(d, refuseReserved) // a holder's value never stays under a name an issuer's grant takes
	s.seal.since = time.Now().UTC()
	dirs := DriverDirs(d.Config.Home)
	s.lookPath = func(driver string) (string, error) { return FindDriver(dirs, driver) }
	var log func(string, ...any)
	if d.Log != nil {
		log = d.Log.Info
	}
	s.pins = newPins(d.Config.Home, log)
	d.Provide(rt.Name, s)
	d.Provide(rt.DoorName, s)
	d.Provide(core.SealingService, core.SealingReporter(s))
	if link := teamlink.From(d); link != nil {
		s.relayOn(link) // boxes across machines
	}
	return s
}

var _ rt.Service = (*Service)(nil)

// SetNow sets the clock (tests).
func (s *Service) SetNow(f func() time.Time) { s.now = f }

// HostClass is the class of the machine serving project's boxes on host
// ("" = this machine, trusted), as the grant check sees it: public unless
// the authority says trusted. A credential issuer asks it (issuers.ClassOf).
func (s *Service) HostClass(ctx context.Context, project, host string) string {
	if host == "" {
		return api.MachineTrusted
	}
	return s.hostClass(ctx, computeapi.RunTarget{HostProject: project, Host: host})
}

// SetLookPath points driver lookup elsewhere (tests). What it finds is still
// checked and pinned like any driver.
func (s *Service) SetLookPath(f func(driver string) (string, error)) { s.lookPath = f }

func (s *Service) handle(f func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := f(r)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

// owner is the machine's owner on the socket without a token.
func owner(ctx context.Context) bool {
	sub := core.SubjectOf(ctx)
	return core.IsLocal(ctx) && sub != nil && !sub.Remote && sub.Via == authz.ViaLocal
}

// actingFor is whom an acquire acts for: the caller, or the actor it names
// when the caller is the machine's owner (who alone acts for another).
func actingFor(ctx context.Context, named string) (string, error) {
	actor := core.SubjectOf(ctx).Address
	if named != "" && named != actor {
		if !owner(ctx) {
			return "", api.Errorf(api.CodeForbidden, "actor must be the caller")
		}
		actor = named
	}
	return actor, nil
}

// rowHidden says whether the caller sees row o as missing: another actor's
// row, or one in an area of project the caller may not operate in. The
// machine's owner sees every row.
func rowHidden(ctx context.Context, project string, o row) bool {
	sub := core.SubjectOf(ctx)
	return !owner(ctx) && (o.Actor != sub.Address || !sub.Can(authz.Operate, authz.Object{Project: project, Area: o.Area}))
}

// target resolves the project with action a and the binding named in the
// path. A binding the caller may not see answers like a missing one.
func (s *Service) target(r *http.Request, a authz.Action) (core.Target, computeapi.RunTarget, *sql.DB, error) {
	t, err := s.d.WriteTarget(r.Context(), "", a)
	if err != nil {
		return t, computeapi.RunTarget{}, nil, err
	}
	b, db, err := s.bindingNamed(r.Context(), t.Project.ID, r.PathValue("target"))
	return t, b, db, err
}

// bindingNamed is project's binding called name (its own, or a host's) and
// the project's database, with no caller check: target's after the
// caller's, and the authority's own lookups (door.go).
func (s *Service) bindingNamed(ctx context.Context, project, name string) (computeapi.RunTarget, *sql.DB, error) {
	for _, b := range rt.ProjectTargets(s.d.Projects, project) {
		if b.Name == name {
			db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
			return b, db, err
		}
	}
	if _, _, ok := SplitRemoteName(name); ok { // a host's binding (remote.go)
		db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
		if err != nil {
			return computeapi.RunTarget{}, nil, err
		}
		if b, ok := s.remoteByName(ctx, project, name, db); ok {
			return b, db, nil
		}
	}
	return computeapi.RunTarget{}, nil, api.NotFound("run target", name)
}

func (s *Service) list(r *http.Request) (any, error) {
	if r.URL.Query().Get("host") == "1" {
		if !owner(r.Context()) {
			return nil, api.Errorf(api.CodeForbidden, "only the machine's owner lists its machine-level run targets, on the socket without a token")
		}
		return s.listHost(r.Context(), r.URL.Query().Get("live") == "1")
	}
	t, err := s.d.WriteTarget(r.Context(), "", authz.Read)
	if err != nil {
		return nil, err
	}
	out := computeapi.RunTargetList{Project: t.Project.Slug, Targets: []computeapi.RunTargetView{}}
	ask := r.URL.Query().Get("describe") == "1"
	live := r.URL.Query().Get("live") == "1"
	if live && !owner(r.Context()) {
		return nil, api.Errorf(api.CodeForbidden, "only the machine's owner lists its run targets' live handles, on the socket without a token")
	}
	var db *sql.DB // opened for the first budgeted binding
	for _, b := range rt.ProjectTargets(s.d.Projects, t.Project.ID) {
		v := computeapi.RunTargetView{RunTarget: shown(r.Context(), b), Effective: stricter(b, nil), Host: s.d.Config.Machine, HostClass: api.MachineTrusted}
		if ask {
			d, err := s.describeOf(r.Context(), b)
			if err != nil {
				v.Error = errText(err)
			} else {
				v.Describe, v.Effective = d, stricter(b, d)
			}
			if b.Budget != nil && db == nil {
				db, _ = s.d.Store.Project(s.d.System().Scope, authz.Write, t.Project.ID)
			}
			v.BudgetSpent = s.budgetSpent(r.Context(), db, b)
		}
		if live {
			s.addLive(r.Context(), &v, b)
		}
		out.Targets = append(out.Targets, v)
	}
	out.Targets = append(out.Targets, s.remoteViews(r.Context(), t.Project.ID, ask)...)
	return out, nil
}

func (s *Service) describe(r *http.Request) (any, error) {
	_, b, db, err := s.target(r, authz.Read)
	if err != nil {
		return nil, err
	}
	v := computeapi.RunTargetView{RunTarget: shown(r.Context(), b), Effective: stricter(b, nil), Host: s.d.Config.Machine, HostClass: api.MachineTrusted}
	if b.Host != "" {
		v.Host, v.HostClass = b.Host, s.hostClass(r.Context(), b)
	}
	d, err := s.describeOf(r.Context(), b)
	if err != nil {
		v.Error = errText(err)
	} else {
		v.Describe, v.Effective = d, stricter(b, d)
	}
	v.BudgetSpent = s.budgetSpent(r.Context(), db, b)
	return v, nil
}

// budgetSpent is b's recorded spend this budget period, for a view (nil:
// no budget, a remote binding, or the store did not answer). Whoever may
// read the project sees it, as they see the budget. It does not ask
// drivers for live figures, and a missing figure shows no spend: acquire
// refreshes, refuses on its own, and decides.
func (s *Service) budgetSpent(ctx context.Context, db *sql.DB, b computeapi.RunTarget) *float64 {
	bg := b.Budget
	if bg == nil || b.Host != "" || db == nil {
		return nil
	}
	spent, err := periodSpend(ctx, db, b.Name, bg.Unit, rt.PeriodStart(bg.Period, s.now()))
	if err != nil {
		return nil
	}
	return &spent
}

// budgetAllowed refuses a new lease on b (an acquire, or the start of a
// stopped box) once b's budget for the period is spent. The spend is read
// after refreshing the live handles' figures, and a spend that cannot be
// read refuses too.
func (s *Service) budgetAllowed(ctx context.Context, projectID string, b computeapi.RunTarget, db *sql.DB) error {
	bg := b.Budget
	if bg == nil {
		return nil
	}
	s.refreshLive(ctx, projectID, b, db) // the other handles' figures as of now, not as of their acquire
	spent, err := periodSpend(ctx, db, b.Name, bg.Unit, rt.PeriodStart(bg.Period, s.now()))
	if err != nil {
		return err
	}
	if why := bg.Refusal(spent); why != "" {
		// Marked: made before anything is recorded, so a sandbox create may ask another binding.
		return markRefused(api.Errorf(api.CodeForbidden, "run target %s %s", b.Name, why), computeapi.RefusedBudget)
	}
	return nil
}

// shown is a binding as the caller may see it: driver config and create_in
// (hosts, file paths) only for the machine's owner.
func shown(ctx context.Context, b computeapi.RunTarget) computeapi.RunTarget {
	if !owner(ctx) {
		b.Config, b.CreateIn = nil, nil
	}
	return b
}

func errText(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// describeOf asks the driver and checks the answer's shape.
func (s *Service) describeOf(ctx context.Context, b computeapi.RunTarget) (*computeapi.RunTargetDescribe, error) {
	var d computeapi.RunTargetDescribe
	if b.Host != "" { // answered from the host's last advert, never by a call
		rd, err := s.remoteDescribe(ctx, b)
		if err != nil {
			return nil, err
		}
		d = *rd
	} else if err := s.call(ctx, b, verbDescribe, "", nil, &d); err != nil {
		return nil, err
	}
	if d.Kind != b.Kind {
		return nil, api.Errorf(api.CodeBadRequest, "potluck-driver-%s serves kind %q, the binding says %q", b.Driver, d.Kind, b.Kind)
	}
	required := []string{verbDescribe, verbAcquire, verbStatus, verbRelease, verbList}
	if b.Kind == computeapi.RunTargetKindBox {
		required = append(required, verbExec)
		if d.Size == nil || d.Size.Name == "" {
			// potluck-driver-tart leaves the size out when it cannot read its
			// image (gone after a disk swap, or Docker unreadable): its why
			// says which, and host share repairs and rebinds the size.
			why := ""
			if d.Why != "" {
				why = " (it says: " + d.Why + ")"
			}
			return nil, api.Errorf(api.CodeBadRequest, "potluck-driver-%s describes a box without its size%s: run %s on the machine that lends it, which checks the machine again, pulls its image if that is missing and rebinds the size", b.Driver, why, config.PotluckCommand("host"))
		}
	}
	for _, v := range required {
		if !slices.Contains(d.Verbs, v) {
			return nil, api.Errorf(api.CodeBadRequest, "potluck-driver-%s does not list required verb %s", b.Driver, v)
		}
	}
	d.Runners = protocol.CleanRunners(d.Runners, protocol.MaxRunners) // an unknown state or time is no result, as an unknown kind is no target
	d.Host = protocol.CleanHost(d.Host)                               // a host block outside its sets is none
	if b.Host == "" {
		d.ImageName = b.Config["image_name"]
		d.Images = nil // an advert's, from its binding's pins (HostAdverts), never a driver's word
	}

	return &d, nil
}

func (s *Service) bind(r *http.Request) (any, error) {
	if !owner(r.Context()) {
		return nil, api.Errorf(api.CodeForbidden, "only the machine's owner binds run targets, on the socket without a token")
	}
	var req computeapi.BindRunTargetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.Host {
		return s.bindHost(r.Context(), req)
	}
	if len(req.Share) > 0 {
		return nil, api.Errorf(api.CodeBadRequest, "share is for a machine-level binding (host)")
	}
	t, err := s.d.WriteTarget(r.Context(), "", authz.Admin)
	if err != nil {
		return nil, err
	}
	b := req.Target
	f, ok := s.d.Projects.File(t.Project.ID)
	if !ok {
		return nil, api.NotFound("project", t.Project.Slug)
	}
	if err := rt.ValidateRunTarget(f.Repos.Remotes, b); err != nil {
		return nil, err
	}
	d, err := s.describeOf(r.Context(), b)
	if err != nil {
		return nil, err
	}
	if bg := b.Budget; bg != nil {
		if !d.ReportsSpend {
			return nil, api.Errorf(api.CodeBadRequest, "budget on potluck-driver-%s, which does not report spend", b.Driver)
		}
		if bg.Unit != d.Cost.Unit {
			return nil, api.Errorf(api.CodeBadRequest, "budget unit %q is not the driver's cost unit %q", bg.Unit, d.Cost.Unit)
		}
	}
	eff := stricter(b, d)
	if b.Kind == computeapi.RunTargetKindBox && !eff.UntrustedCapable() {
		return nil, api.Errorf(api.CodeBadRequest, "%s, so it cannot serve a box: a box runs whatever its holder sends", eff.CannotIsolate("potluck-driver-"+b.Driver))
	}
	if b.Kind == computeapi.RunTargetKindRun && eff.Guarded() && len(b.Scripts) == 0 {
		return nil, api.Errorf(api.CodeBadRequest, "%s, so the binding needs scripts: the only commands it may run", eff.CannotIsolate("potluck-driver-"+b.Driver))
	}
	unlock := s.lock(t.Project.ID, b.Name)
	defer unlock()
	if req.Replace {
		if err := s.noLive(r.Context(), t.Project.ID, b.Name); err != nil {
			return nil, err
		}
	} else if slices.ContainsFunc(rt.ProjectTargets(s.d.Projects, t.Project.ID), func(o computeapi.RunTarget) bool { return o.Name == b.Name }) {
		return nil, api.Errorf(api.CodeConflict, "run target %q is bound; pass replace to rebind it", b.Name)
	}
	// Setup's side effects come only once every refusal zm can foresee is past.
	var notes []string
	if req.Setup {
		if notes, err = s.setup(r.Context(), b, d); err != nil {
			return nil, err
		}
	}
	_, err = rt.SetProjectTargets(r.Context(), s.d.Projects, t.Project.ID, func(old []computeapi.RunTarget) ([]computeapi.RunTarget, error) {
		for i, o := range old {
			if o.Name == b.Name {
				if !req.Replace {
					return nil, api.Errorf(api.CodeConflict, "run target %q is bound; pass replace to rebind it", b.Name)
				}
				old[i] = b
				return old, nil
			}
		}
		return append(old, b), nil
	})
	if err != nil {
		return nil, err
	}
	return computeapi.RunTargetView{RunTarget: b, Effective: eff, Describe: d, SetupNotes: notes}, nil
}

// setup runs the driver's one-time setup for an owner's bind (SPEC 20,
// section 11): a dry run first, then the real one; never on acquire.
func (s *Service) setup(ctx context.Context, b computeapi.RunTarget, d *computeapi.RunTargetDescribe) ([]string, error) {
	if !slices.Contains(d.Verbs, verbSetup) {
		return nil, api.Errorf(api.CodeBadRequest, "potluck-driver-%s has no setup verb", b.Driver)
	}
	var ans struct {
		Notes []string `json:"notes"`
	}
	if err := s.callWithin(ctx, b, verbSetup, "", map[string]any{"dry_run": true}, &ans, acquireLimit); err != nil {
		return nil, err
	}
	dry := ans.Notes
	ans.Notes = nil
	if err := s.callWithin(ctx, b, verbSetup, "", map[string]any{"dry_run": false}, &ans, acquireLimit); err != nil {
		return nil, err
	}
	const maxNotes = 20
	dry, ans.Notes = dry[:min(len(dry), maxNotes)], ans.Notes[:min(len(ans.Notes), maxNotes)]
	out := make([]string, 0, len(dry)+len(ans.Notes))
	for _, n := range dry {
		out = append(out, "plan: "+oneLine(n))
	}
	for _, n := range ans.Notes {
		out = append(out, oneLine(n))
	}
	return out, nil
}

func (s *Service) noLive(ctx context.Context, projectID, name string) error {
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, projectID)
	if err != nil {
		return err
	}
	rows, err := listRows(ctx, db, name, true, "1=1", nil)
	if err != nil {
		return err
	}
	live := 0
	for _, rw := range rows {
		// An acquire no driver answered for 10 minutes made nothing zm can release.
		if c, err := time.Parse(time.RFC3339Nano, rw.Created); rw.Handle == "" && err == nil && s.now().Sub(c) > 10*time.Minute {
			continue
		}
		live++
	}
	if live > 0 {
		return api.Errorf(api.CodeConflict, "run target %q has %d live handles; release them first", name, live)
	}
	return nil
}

func (s *Service) unbind(r *http.Request) (any, error) {
	if !owner(r.Context()) {
		return nil, api.Errorf(api.CodeForbidden, "only the machine's owner unbinds run targets, on the socket without a token")
	}
	if r.URL.Query().Get("host") == "1" {
		return s.unbindHost(r.PathValue("target"))
	}
	t, b, _, err := s.target(r, authz.Admin)
	if err != nil {
		return nil, err
	}
	if b.Host != "" {
		return nil, api.Errorf(api.CodeBadRequest, "run target %s is %s's: unbind it there", b.Name, b.Host)
	}
	unlock := s.lock(t.Project.ID, b.Name)
	defer unlock()
	if err := s.noLive(r.Context(), t.Project.ID, b.Name); err != nil {
		return nil, err
	}
	_, err = rt.SetProjectTargets(r.Context(), s.d.Projects, t.Project.ID, func(old []computeapi.RunTarget) ([]computeapi.RunTarget, error) {
		return slices.DeleteFunc(old, func(o computeapi.RunTarget) bool { return o.Name == b.Name }), nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"removed": b.Name}, nil
}

// lock serialises acquires, binds and unbinds of one binding.
func (s *Service) lock(projectID, name string) func() {
	mu, _ := s.acquireMu.LoadOrStore(projectID+"/"+name, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	return mu.(*sync.Mutex).Unlock
}

// lifetimeCap is the lower of the binding's and describe's ttl_max_s (0: none).
func lifetimeCap(b computeapi.RunTarget, d *computeapi.RunTargetDescribe) int {
	if d == nil {
		return placement.LifeCap(b.TTLMaxS, 0)
	}
	return placement.LifeCap(b.TTLMaxS, d.TTLMaxS)
}

// sizeName is a box size from an answer: a name, or an object with one.
func sizeName(raw json.RawMessage) string {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name
	}
	var obj struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.Name
}

// offPin reports a box whose booted digest is none of its binding's pins
// (a drifted host whose release failed): no exec, and the sweep releases it.
func offPin(b computeapi.RunTarget, rw row) bool {
	if b.Kind != computeapi.RunTargetKindBox || len(b.Images) == 0 || rw.ImageDigest == "" {
		return false
	}
	for _, im := range b.Images {
		if pinDigest(im) == rw.ImageDigest {
			return false
		}
	}
	return true
}

// pinDigest is a pinned image's digest (sha256:<hex>), "" with no pin.
func pinDigest(pin string) string {
	if i := strings.LastIndex(pin, "@"); i > 0 {
		return pin[i+1:]
	}
	return ""
}

// boxPin is the binding image a box acquire boots: pin, the owner's pinned
// image ("" when the binding names none), or tmplImage, a template's image
// name for a remote binding whose host holds the images.
func boxPin(b computeapi.RunTarget, req computeapi.AcquireRunTargetRequest) (pin, tmplImage string, err error) {
	// zm-eakh.12.5.20: a template is the binding's image of the template's name
	if req.Template != "" {
		var ok bool
		switch tmplImage, ok = computeapi.BoxTemplateImage(req.Template); {
		case b.Kind != computeapi.RunTargetKindBox:
			return "", "", api.Errorf(api.CodeBadRequest, "template is for a box target")
		case req.Image != "":
			return "", "", api.Errorf(api.CodeBadRequest, "name a template or an image, not both")
		case !ok:
			return "", "", api.Errorf(api.CodeBadRequest, "no box template %q (%s's: %s)", req.Template, config.Program, strings.Join(computeapi.BoxTemplates(), ", "))
		}
	}
	if b.Kind == computeapi.RunTargetKindBox && len(b.Images) > 0 {
		pin = b.Images[0]
		if want := cmp.Or(req.Image, tmplImage); want != "" {
			pin = ""
			for _, im := range b.Images {
				if rt.ImageName(im) == want {
					pin = im
				}
			}
			switch {
			case pin == "" && req.Template != "":
				return "", "", api.Errorf(api.CodeBadRequest, "run target %s lends no %s template image (%s)", b.Name, req.Template, tmplImage).
					WithHint("its owner lends it beside the default by running " + config.PotluckCommand("host") + " again")
			case pin == "":
				return "", "", api.Errorf(api.CodeBadRequest, "image %q is not one of run target %s's images", req.Image, b.Name)
			}
		}
	} else if req.Image != "" || tmplImage != "" && b.Host == "" {
		return "", "", api.Errorf(api.CodeBadRequest, "run target %s names no images", b.Name)
	}
	return pin, tmplImage, nil
}

type acquireAnswer struct {
	Database    *protocol.Database         `json:"database,omitempty"`
	Size        json.RawMessage            `json:"size"`
	ImageDigest string                     `json:"image_digest"`
	Handle      string                     `json:"handle"`
	ExpiresAt   string                     `json:"expires_at"`
	TTLS        int                        `json:"ttl_s"`
	Commit      string                     `json:"commit"`
	URL         string                     `json:"url"`
	Log         string                     `json:"log"`
	Spend       *computeapi.RunTargetSpend `json:"spend"`
	Net         string                     `json:"net"`     // a box: on or off, as the driver made it
	Volumes     []computeapi.BoxVolume     `json:"volumes"` // a box's named volumes, as the driver mounted them
}

// scrubJIT takes a jit_config out of a driver's error before it is answered
// or logged.
func scrubJIT(err error, jit string) error {
	var e *api.Error
	if err == nil || jit == "" || !errors.As(err, &e) {
		return err
	}
	c := *e
	c.Message = strings.ReplaceAll(c.Message, jit, "[jit_config]")
	c.Hint = strings.ReplaceAll(c.Hint, jit, "[jit_config]")
	return &c
}

// record writes a driver's answer and, for the one write that ends the
// handle, calls the end hook with the row as recorded (its final figures).
func (s *Service) record(ctx context.Context, pid string, db *sql.DB, target, requestID string, u upd) error {
	ctx = context.WithoutCancel(ctx)
	endedNow, err := update(ctx, db, target, requestID, u, s.now().UTC().Format(tsLayout))
	if err != nil || !endedNow {
		return err
	}
	if rw, ok, err := byRequest(ctx, db, target, requestID); err == nil && ok {
		if rw.State == stateExpired || rw.State == stateReclaimed {
			s.leaseExpired(ctx, pid, rw)
		}
		s.ended(ctx, pid, rw.RunTargetHandle)
	}
	return nil
}

// reconcile finds requestID's handle in the driver's list (SPEC 20: after
// an acquire times out, zm reconciles from list and never acquires afresh).
func (s *Service) reconcile(ctx context.Context, b computeapi.RunTarget, requestID string) (computeapi.RunTargetHandle, bool, error) {
	var l struct {
		Handles []struct {
			Handle    string `json:"handle"`
			RequestID string `json:"request_id"`
			State     string `json:"state"`
			ExpiresAt string `json:"expires_at"`
		} `json:"handles"`
	}
	var listArgs any
	if b.Host != "" {
		listArgs = map[string]string{"request_id": requestID} // one binding serves several projects: only ours
	}
	if err := s.call(ctx, b, verbList, "", listArgs, &l); err != nil {
		return computeapi.RunTargetHandle{}, false, err
	}
	for _, h := range l.Handles {
		if h.RequestID == requestID && strings.HasPrefix(h.Handle, b.Kind+":"+b.Name+"/") && driverStates[h.State] {
			return computeapi.RunTargetHandle{Handle: h.Handle, State: h.State, ExpiresAt: h.ExpiresAt}, true, nil
		}
	}
	return computeapi.RunTargetHandle{}, false, nil
}

// refreshLive asks the driver for each live handle's status (at most 32),
// so a budget sums current spend.
func (s *Service) refreshLive(ctx context.Context, pid string, b computeapi.RunTarget, db *sql.DB) {
	// Bounded: an acquire runs it under the binding's lock, and a slow driver
	// must not stall every acquire (a figure it misses stays as last seen).
	ctx, cancel := context.WithTimeout(ctx, refreshBudget)
	defer cancel()
	rows, err := listRows(ctx, db, b.Name, true, "1=1", nil)
	if err != nil {
		return
	}
	for i, rw := range rows {
		if i == 32 {
			break
		}
		if rw.Handle != "" {
			_, _ = s.refresh(ctx, pid, b, db, rw)
		}
	}
}

const refreshBudget = 10 * time.Second

// actorKey is the request key zm keeps and sends the driver for actor's
// request_id: a tag of the actor, then the caller's key (splitActorKey
// takes it apart).
func actorKey(actor, requestID string) string {
	sum := sha256.Sum256([]byte(actor))
	return hex.EncodeToString(sum[:actorTagBytes]) + "-" + requestID
}

const actorTagBytes = 5

// splitActorKey is actorKey's tag and request_id, ok when key has the
// shape actorKey makes (a lending host is sent whatever its authority's
// build made, so it checks).
func splitActorKey(key string) (tag, requestID string, ok bool) {
	tag, requestID, found := strings.Cut(key, "-")
	if !found || len(tag) != 2*actorTagBytes {
		return "", key, false
	}
	if _, err := hex.DecodeString(tag); err != nil {
		return "", key, false
	}
	return tag, requestID, true
}

// unitSpend keeps a spend figure only in describe's cost unit.
func (s *Service) unitSpend(d *computeapi.RunTargetDescribe, sp *computeapi.RunTargetSpend) *computeapi.RunTargetSpend {
	if sp == nil || d == nil || sp.Unit != d.Cost.Unit {
		return nil
	}
	return sp
}

// handleOf is the full handle for the path's id.
func handleOf(r *http.Request, b computeapi.RunTarget) (string, error) {
	id := r.PathValue("id")
	if id == "" || strings.ContainsAny(id, "/:") || len(id) > 200 {
		return "", api.Errorf(api.CodeBadRequest, "bad handle id %q", id)
	}
	return b.Kind + ":" + b.Name + "/" + id, nil
}

// loadHandle is the path's handle of the path's binding; one this binding
// never issued is not_found.
func (s *Service) loadHandle(r *http.Request, a authz.Action) (string, computeapi.RunTarget, *sql.DB, row, error) {
	t, b, db, err := s.target(r, a)
	if err != nil {
		return "", b, nil, row{}, err
	}
	h, err := handleOf(r, b)
	if err != nil {
		return "", b, nil, row{}, err
	}
	rw, ok, err := byHandle(r.Context(), db, h)
	if err != nil {
		return "", b, nil, row{}, err
	}
	// A handle in an area the caller may not act in answers like a missing one.
	// A dependency layer's builder is the daemon's alone (deps.go): its holder reaches it as a missing one.
	if !ok || rw.Target != b.Name || builderRow(rw) || !core.SubjectOf(r.Context()).Can(a, authz.Object{Project: t.Project.ID, Area: rw.Area}) {
		return "", b, nil, row{}, rt.NoHandle(api.NotFound("handle", h))
	}
	return t.Project.ID, b, db, rw, nil
}

func (s *Service) handles(r *http.Request) (any, error) {
	t, b, db, err := s.target(r, authz.Read)
	if err != nil {
		return nil, err
	}
	areaSQL, areaArgs := core.SubjectOf(r.Context()).Scope.AreaSQL(t.Project.ID, "area", authz.Read)
	rows, err := listRows(r.Context(), db, b.Name, r.URL.Query().Get("live") == "1", areaSQL, areaArgs)
	if err != nil {
		return nil, err
	}
	// mine=1 keeps the caller's own (core.Deps.OwnAddress: any of their sessions). Without it the machine owner and
	// an owner or admin of the whole project see every handle; anyone else (a friend in a pool) still only their own.
	mine := r.URL.Query().Get("mine") == "1"
	all := false
	if !mine {
		if all, err = s.seesAll(r.Context(), t.Project.ID); err != nil {
			return nil, err
		}
	}
	sub := core.SubjectOf(r.Context())
	out := computeapi.RunTargetHandles{Handles: []computeapi.RunTargetHandle{}}
	for _, rw := range rows {
		if !all && !s.d.OwnAddress(sub, rw.Actor) {
			continue
		}
		out.Handles = append(out.Handles, rw.RunTargetHandle) // an acquiring row has no handle yet
	}
	return out, nil
}

// seesAll reports whether the caller sees every sandbox and run in project
// pid: this machine's owner, or an owner or admin of the whole project. A
// member (a friend in a pool) sees only their own (core.Deps.OwnAddress).
func (s *Service) seesAll(ctx context.Context, pid string) (bool, error) {
	if owner(ctx) {
		return true, nil
	}
	return s.d.ProjectAdmin(ctx, core.SubjectOf(ctx), pid)
}

type statusAnswer struct {
	State     string                     `json:"state"`
	Commit    string                     `json:"commit"`
	Exit      *int                       `json:"exit"`
	ExpiresAt string                     `json:"expires_at"`
	Spend     *computeapi.RunTargetSpend `json:"spend"`
}

var driverStates = map[string]bool{stateReady: true, stateRunning: true, stateStopped: true, stateEnded: true, stateExpired: true,
	stateReleased: true, stateReclaimed: true, stateLost: true}

func (s *Service) status(r *http.Request) (any, error) {
	pid, b, db, rw, err := s.loadHandle(r, authz.Read)
	if err != nil {
		return nil, err
	}
	// As the list (handles): another member's sandbox answers as missing.
	if all, err := s.seesAll(r.Context(), pid); err != nil {
		return nil, err
	} else if !all && !s.d.OwnAddress(core.SubjectOf(r.Context()), rw.Actor) {
		return nil, api.NotFound("handle", r.PathValue("id"))
	}
	return s.refresh(r.Context(), pid, b, db, rw)
}

// refresh asks the driver for a handle's status and records it.
func (s *Service) refresh(ctx context.Context, pid string, b computeapi.RunTarget, db *sql.DB, rw row) (computeapi.RunTargetHandle, error) {
	if terminal(rw.State) {
		// Ended: its record stands (the write that ended it took the final
		// state, exit and spend) and its target is not asked again. So a
		// handle the sweep recorded lost answers lost whatever its host says
		// or fails to say, and an ended handle's status is never an error.
		return rw.RunTargetHandle, nil
	}
	var ans statusAnswer
	if err := s.call(ctx, b, verbStatus, rw.Handle, nil, &ans); err != nil {
		var e *api.Error
		switch {
		case rt.IsNoHandle(err):
			ans.State = stateLost // the target no longer knows it
		case errors.As(err, &e) && e.Code == api.CodeNotFound:
			// Not about the handle: a host that is out of the project (it
			// dropped out of the pool, or is mid restart) or no longer shares
			// the binding, a machine the relay does not know. Nobody spoke
			// about the box, so its record stands and the caller is told the
			// target could not be asked, never that the handle is missing.
			return rw.RunTargetHandle, api.Errorf(api.CodeUnavailable, "run target %s could not be asked about %s: %s", b.Name, rw.Handle, e.Message)
		default:
			return rw.RunTargetHandle, err
		}
	}
	if !driverStates[ans.State] {
		return rw.RunTargetHandle, api.Errorf(api.CodeInternal, "potluck-driver-%s answered state %q", b.Driver, ans.State)
	}
	if ans.State != stateEnded && ans.State != stateExpired {
		ans.Exit = nil // only an ended (or expired, 124) run reports an exit
	}
	var d *computeapi.RunTargetDescribe
	if ans.Spend != nil {
		d, _ = s.describeOf(ctx, b)
	}
	if err := s.record(ctx, pid, db, b.Name, rw.RequestID, upd{state: ans.State, commit: ans.Commit,
		exit: ans.Exit, expiresAt: ans.ExpiresAt, spend: s.unitSpend(d, ans.Spend)}); err != nil {
		return rw.RunTargetHandle, err
	}
	out, _, err := byHandle(ctx, db, rw.Handle)
	return out.RunTargetHandle, err
}

// holder checks the caller holds rw (or is the machine owner, when force).
func holder(ctx context.Context, rw row, ownerToo bool) error {
	sub := core.SubjectOf(ctx)
	if sub != nil && sub.Address == rw.Actor {
		return nil
	}
	if ownerToo && owner(ctx) {
		return nil
	}
	return api.Errorf(api.CodeForbidden, "handle %s is held by another actor", rw.Handle)
}

// Exec limits (SPEC 20 "Boxes").
const (
	execDefaultS = rt.ExecDefaultS
	execMaxS     = rt.ExecMaxS
	execMaxArgv  = 256
	execMaxStdin = 1 << 20
	execMaxOut   = 1 << 20
)

// exec runs a literal argv in a box, for its holder. The output is data.
func (s *Service) exec(r *http.Request) (any, error) {
	pid, b, _, rw, err := s.loadHandle(r, authz.Operate)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	if b.Kind != computeapi.RunTargetKindBox {
		return nil, api.Errorf(api.CodeBadRequest, "exec is for a box target; %s is a %s target", b.Name, b.Kind)
	}
	if err := holder(ctx, rw, false); err != nil {
		return nil, err
	}
	// secrets decode on their own: Go's decode errors quote a map key's path.
	var body struct {
		computeapi.ExecRunTargetRequest
		Secrets json.RawMessage `json:"secrets"`
	}
	if err := api.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	req := body.ExecRunTargetRequest
	if t := strings.TrimSpace(string(body.Secrets)); t != "" && t != "null" {
		if err := json.Unmarshal(body.Secrets, &req.Secrets); err != nil {
			return nil, api.Errorf(api.CodeBadRequest, "secrets must be an object of string values")
		}
	}
	if err := checkCommand(req.Argv, req.Stdin, req.Cwd, req.Encoding); err != nil { // procs.go
		return nil, err
	}
	if req.TimeoutS < 0 || req.TimeoutS > execMaxS {
		return nil, api.Errorf(api.CodeBadRequest, "timeout_s must be between 1 and %d", execMaxS)
	}
	if rw.State != stateReady {
		return nil, api.Errorf(api.CodeConflict, "sandbox %s is %s, not ready", sandboxID(rw.Handle), rw.State)
	}
	if offPin(b, rw) {
		return nil, api.Errorf(api.CodeConflict, "sandbox %s booted %s, not one of the pinned images; it is being released", sandboxID(rw.Handle), rw.ImageDigest)
	}
	timeout := req.TimeoutS
	if timeout == 0 {
		timeout = execDefaultS
	}
	if exp, err := time.Parse(time.RFC3339Nano, rw.ExpiresAt); err == nil {
		left := int(time.Until(exp) / time.Second)
		if left <= 0 {
			return nil, api.Errorf(api.CodeConflict, "sandbox %s is past its expires_at", sandboxID(rw.Handle))
		}
		timeout = min(timeout, left) // cut to the lease
	}
	args := map[string]any{"argv": req.Argv, "timeout_s": timeout}
	if req.Stdin != "" {
		args["stdin"] = req.Stdin
	}
	if req.Cwd != "" {
		args["cwd"] = req.Cwd
	}
	if req.Encoding != "" {
		if err := s.encodingAllowed(ctx, b, req.Encoding); err != nil {
			return nil, err
		}
		args["encoding"] = req.Encoding
	}
	dec, err := s.secretsGranted(ctx, pid, b, rw, verbExec, req.Secrets) // grants.go: before anything is sent
	if err != nil {
		return nil, err
	}
	if len(req.Secrets) > 0 {
		if err := s.secretsAllowed(ctx, b, req.Secrets); err != nil {
			return nil, err
		}
		args["secrets"] = req.Secrets
	}
	var out computeapi.RunTargetExec
	if err := s.secretsSent(ctx, dec, func() error {
		return s.callWithin(ctx, b, verbExec, rw.Handle, args, &out, time.Duration(timeout+30)*time.Second)
	}); err != nil {
		return nil, scrubSecrets(err, req.Secrets)
	}
	capExec(&out) // zm holds the cap itself: a host's output is data, at most 1 MiB each
	return out, nil
}

func (s *Service) renew(r *http.Request) (any, error) {
	_, b, db, rw, err := s.loadHandle(r, authz.Operate)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := holder(ctx, rw, false); err != nil {
		return nil, err
	}
	if baseBuilder(rw) {
		return nil, api.Errorf(api.CodeForbidden, "a push base builder cannot be renewed")
	}
	var req computeapi.RenewRunTargetRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.TTLS <= 0 {
		return nil, api.Errorf(api.CodeBadRequest, "ttl_s must be positive")
	}
	if terminal(rw.State) || rw.State == stateStopped { // a stopped box gets a lease by start
		return nil, api.Errorf(api.CodeConflict, "handle %s is %s", rw.Handle, rw.State)
	}
	d, err := s.describeOf(ctx, b)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(d.Verbs, verbRenew) {
		return nil, api.Errorf(api.CodeNotImplemented, "potluck-driver-%s does not renew", b.Driver)
	}
	if c := lifetimeCap(b, d); c > 0 {
		created, err := time.Parse(time.RFC3339Nano, capFrom(rw)) // persist.go: acquire, or the last start
		if err == nil && s.now().Add(time.Duration(req.TTLS)*time.Second).After(created.Add(time.Duration(c)*time.Second)) {
			return nil, api.Errorf(api.CodeBadRequest, "renewing by %ds passes the lifetime cap of %ds since acquire (or start)", req.TTLS, c)
		}
	}
	var ans struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := s.call(ctx, b, verbRenew, rw.Handle, map[string]any{"ttl_s": req.TTLS}, &ans); err != nil {
		return nil, err
	}
	now := s.now().UTC().Format(tsLayout)
	if _, err := update(context.WithoutCancel(ctx), db, b.Name, rw.RequestID, upd{expiresAt: ans.ExpiresAt}, now); err != nil {
		return nil, err
	}
	out, _, err := byHandle(ctx, db, rw.Handle)
	return out.RunTargetHandle, err
}
