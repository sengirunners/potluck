package grants

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// wireTime keeps times fixed-width (RFC 3339 with every digit), so the
// table's text order is time order.
const wireTime = "2006-01-02T15:04:05.000000000Z"

// Service is the module that manages the table (its routes). The check
// (Check) does not need it: a daemon that wires run targets without this
// module still enforces the table and the defaults, with no way to edit
// them.
type Service struct {
	d   *core.Deps
	now func() time.Time
}

var _ core.Module = (*Service)(nil)

// New returns the grants module. Construct it after teamlink is provided
// (wiring.NewKit): on a member's machine its routes go to the pool's
// authority, where the table lives.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, now: time.Now}
	if link := teamlink.From(d); link != nil {
		once := func(p, why string) teamlink.RouteRule {
			return teamlink.RouteRule{Pattern: p, Kind: teamlink.KindWrite, Hedge: teamlink.Once, Why: why}
		}
		link.RegisterRoutes("grants",
			teamlink.RouteRule{Pattern: "GET /v0/secret_grants", Kind: teamlink.KindWrite, Hedge: teamlink.Hedged},
			teamlink.RouteRule{Pattern: "POST /v0/secret_grants", Kind: teamlink.KindWrite, Hedge: teamlink.Once,
				Why:  "an admin's edit: a second copy would add a second grant",
				Hook: &teamlink.RouteHook{Seal: func([]byte) bool { return true }}}, // it names a secret: sealed on every leg
			once("DELETE /v0/secret_grants/{id}", "an admin's edit; a repeat answers not_found"),
			once("POST /v0/secret_grants/default", "an admin's switch, sent once"))
	}
	return s
}

// Routes registers /v0/secret_grants.
func (s *Service) Routes(r *core.Router) {
	r = r.For("grants")
	r.HandleFunc("GET /v0/secret_grants", core.Authed, "operate on the resolved project; the defaults, and rows in areas the caller operates in", s.handle(s.list))
	r.HandleFunc("POST /v0/secret_grants", core.Authed, "admin on the grant's area (the whole project for none)", s.handle(s.add))
	r.HandleFunc("DELETE /v0/secret_grants/{id}", core.Authed, "admin on the grant's area; one in an area the caller cannot operate in answers not_found", s.handle(s.remove))
	r.HandleFunc("POST /v0/secret_grants/default", core.Authed, "admin on the whole resolved project", s.handle(s.setDefault))
}

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

// admin is whether sub administers area of project ("" = the whole
// project), a member machine's link judged by its identity's grants
// (core.AreaRelation).
func (s *Service) admin(ctx context.Context, sub *authz.Subject, project, area string) (bool, error) {
	rel, err := s.d.AreaRelation(ctx, sub, project, area)
	return rel.Allows(authz.Admin), err
}

func (s *Service) list(r *http.Request) (any, error) {
	ctx := r.Context()
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return nil, err
	}
	sub := core.SubjectOf(ctx)
	// Rows in the areas the caller may operate in (judged as admin is, so a
	// member machine's link capped to another area still lists its own).
	p, err := s.d.Projects.Get(s.d.System().Scope, t.Project.ID)
	if err != nil {
		return nil, err
	}
	may := map[string]bool{}
	for _, a := range append([]string{""}, areaNames(p.Areas)...) {
		rel, err := s.d.AreaRelation(ctx, sub, t.Project.ID, a)
		if err != nil {
			return nil, err
		}
		may[a] = rel.Allows(authz.Operate)
	}
	any := false
	for _, ok := range may {
		any = any || ok
	}
	if !any {
		return nil, api.Errorf(api.CodeForbidden, "secret grants are listed to those who may use the pool's sandboxes (a member)")
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Read, t.Project.ID)
	if err != nil {
		return nil, err
	}
	all, on, err := load(ctx, db, `1=1`)
	if err != nil {
		return nil, err
	}
	var rows []computeapi.SecretGrant
	for _, g := range all {
		if may[g.Area] || may[""] {
			rows = append(rows, g)
		}
	}
	since := map[string]time.Time{} // identity id and area -> membership start
	for i, g := range rows {
		id := g.To.IdentityID
		if id == "" {
			continue
		}
		k := id + "\x00" + g.Area
		at, seen := since[k]
		if !seen {
			if m, ok, err := s.d.Projects.MemberSince(ctx, id, t.Project.ID, g.Area); err != nil {
				return nil, err
			} else if ok {
				at = m
			}
			since[k] = at
		}
		rows[i].Stale = staleFor(g, at)
	}
	defs := Defaults()
	for i := range defs {
		defs[i].Off = !on
	}
	out := computeapi.SecretGrantList{Project: t.Project.Slug, Grants: append(defs, rows...)}
	if core.OverTunnel(ctx) {
		for i := range out.Grants {
			out.Grants[i] = withheld(out.Grants[i])
		}
		out.Note = computeapi.SecretNamesWithheld
	}
	return out, nil
}

func (s *Service) add(r *http.Request) (any, error) {
	ctx := r.Context()
	var req computeapi.AddSecretGrantRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return nil, err
	}
	sub := core.SubjectOf(ctx)
	if req.Area != "" { // judged as admin is: a member machine's link may be capped to another area
		rel, err := s.d.AreaRelation(ctx, sub, t.Project.ID, req.Area)
		if err != nil {
			return nil, err
		}
		if !s.d.Projects.HasArea(t.Project.ID, req.Area) || rel == authz.None {
			return nil, api.NotFound("area", req.Area)
		}
	}
	if ok, err := s.admin(ctx, sub, t.Project.ID, req.Area); err != nil {
		return nil, err
	} else if !ok {
		return nil, api.Errorf(api.CodeForbidden, "only an admin of the area adds a secret grant")
	}
	g, err := s.validate(ctx, t.Project.ID, req)
	if err != nil {
		return nil, err
	}
	g.ID, g.Area = ids.New(), req.Area
	g.CreatedBy, g.Created = sub.Identity.Name, s.now().UTC().Format(wireTime) // fixed width: text order is time order
	err = s.edit(ctx, t.Project.ID, func(tx *sql.Tx) error {
		if err := insert(ctx, tx, g); err != nil {
			return err
		}
		return reserving(ctx, s.d, tx, g)
	},
		func() events.Input { return grantEvent(computeapi.EventSecretGrantAdded, g) })
	if core.OverTunnel(ctx) {
		g = withheld(g)
	}
	return g, err
}

// validate turns an add request into the row to store, the stricter way
// wherever there is a choice.
func (s *Service) validate(ctx context.Context, project string, req computeapi.AddSecretGrantRequest) (computeapi.SecretGrant, error) {
	bad := func(f string, a ...any) (computeapi.SecretGrant, error) {
		return computeapi.SecretGrant{}, api.Errorf(api.CodeBadRequest, f, a...)
	}
	g := computeapi.SecretGrant{Kind: req.Kind, Name: req.Name, HostClass: req.HostClass, Targets: req.Targets}
	empty := len(bytes.TrimSpace(req.Ceiling)) == 0 || string(bytes.TrimSpace(req.Ceiling)) == "null"
	// Kind, ceiling and name.
	switch g.Kind {
	case computeapi.SecretKindValue:
		c := req.Ceiling
		if empty {
			c = json.RawMessage(`{"max_bytes":65536}`)
		}
		n, ok := valueCeiling(c)
		if !ok {
			return bad(`a value grant's ceiling is {"max_bytes": 1..65536}`)
		}
		g.Ceiling, _ = json.Marshal(map[string]int{"max_bytes": n})
		if g.Name != "*" && !secretName.MatchString(g.Name) {
			return bad("name is [A-Z][A-Z0-9_]{0,63}, or * for any name of a value grant")
		}
	case computeapi.SecretKindGitHubJIT:
		if !empty && !emptyCeiling(req.Ceiling) {
			return bad("a github-jit grant's ceiling is {}: a jit_config has nothing to cap")
		}
		g.Ceiling = json.RawMessage(`{}`)
		if g.Name != computeapi.SecretNameJIT {
			return bad("a github-jit grant's name is %s", computeapi.SecretNameJIT)
		}
	case computeapi.SecretKindGitHubToken, computeapi.SecretKindPotluckToken:
		iss := IssuerOf(s.d, g.Kind)
		if iss == nil {
			return bad("this pool's authority mints no %s secrets yet: no grant of that kind can be added", g.Kind)
		}
		c, err := iss.Ceiling(req.Ceiling)
		if err != nil {
			return bad("ceiling: %v", err)
		}
		g.Ceiling = c
		if !secretName.MatchString(g.Name) {
			return bad("a %s grant names its one file name, [A-Z][A-Z0-9_]{0,63}", g.Kind)
		}
	default:
		return bad("kind is value, github-token, potluck-token or github-jit")
	}
	// To: exactly one.
	to := req.To
	set := 0
	for _, b := range []bool{to.Identity != "" || to.IdentityID != "", to.Role != "", to.Job != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		return bad("to names exactly one of identity, role or job")
	}
	switch {
	case to.Identity != "" || to.IdentityID != "":
		ref := to.Identity
		if ref == "" {
			ref = to.IdentityID
		}
		id, err := s.d.Projects.Identity(ctx, ref)
		if err != nil {
			return computeapi.SecretGrant{}, err
		}
		// Only someone in the pool: anyone else answers as missing.
		if _, ok, err := s.d.Projects.MemberSince(ctx, id.ID, project, req.Area); err != nil {
			return computeapi.SecretGrant{}, err
		} else if !ok {
			return computeapi.SecretGrant{}, api.NotFound("identity", ref)
		}
		g.To = computeapi.GrantTo{Identity: id.Name, IdentityID: id.ID}
	case to.Role != "":
		if to.Role != api.RoleMember && to.Role != api.RoleOwner {
			return bad("role is member or owner")
		}
		g.To = computeapi.GrantTo{Role: to.Role}
	default:
		if callerHeld(g.Kind) {
			return bad("a job grant is for a kind the authority mints: a job holds no value to send")
		}
		j := *to.Job
		switch {
		case !jobrule.ValidRepo(j.Repo):
			return bad("job: repo is owner/name")
		case !jobrule.ValidWorkflow(j.Workflow):
			return bad("job: workflow is a file under .github/workflows/ (.yml or .yaml)")
		case !validRef(j.Ref):
			return bad("job: ref is a branch's full ref, refs/heads/<name>, with no pattern characters")
		}
		evs, err := jobrule.RuleEvents(j.Events)
		if err != nil {
			return bad("job: %v", err)
		}
		g.To = computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: j.Repo, Workflow: j.Workflow, Ref: j.Ref, Events: evs}}
	}
	// Host class: trusted only, in v1.
	switch g.HostClass {
	case "", api.MachineTrusted:
		g.HostClass = api.MachineTrusted
	default:
		return bad("host_class is trusted: a public host is given no secrets")
	}
	// Scopes.
	if len(req.Scopes) == 0 {
		g.Scopes = []string{computeapi.SecretScopeExec}
		if g.Kind == computeapi.SecretKindGitHubJIT {
			g.Scopes = []string{computeapi.SecretScopeBox} // it lives the box's life
		}
	}
	for _, sc := range req.Scopes {
		if sc != computeapi.SecretScopeExec && sc != computeapi.SecretScopeBox {
			return bad("a scope is exec or box")
		}
		if !slices.Contains(g.Scopes, sc) {
			g.Scopes = append(g.Scopes, sc)
		}
	}
	sort.Strings(g.Scopes)
	// Targets.
	for _, t := range g.Targets {
		if t == "" || len(t) > 200 || strings.ContainsFunc(t, func(r rune) bool { return r <= ' ' || r == ',' || r > '~' }) {
			return bad("a target is a run target's name as listed")
		}
	}
	return g, nil
}

func (s *Service) remove(r *http.Request) (any, error) {
	ctx := r.Context()
	id := r.PathValue("id")
	if id == computeapi.GrantDefaultID || id == computeapi.GrantDefaultJITID {
		return nil, api.Errorf(api.CodeBadRequest, "the default grants are turned off, not removed").
			WithHint("potluck secrets grant default off")
	}
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return nil, err
	}
	sub := core.SubjectOf(ctx)
	var g computeapi.SecretGrant
	err = s.edit(ctx, t.Project.ID, func(tx *sql.Tx) error {
		var ok bool
		var err error
		if g, ok, err = byID(ctx, tx, id); err != nil {
			return err
		}
		rel, err := s.d.AreaRelation(ctx, sub, t.Project.ID, g.Area)
		switch {
		case err != nil:
			return err
		case !ok || !rel.Allows(authz.Operate):
			return api.NotFound("secret grant", id)
		case !rel.Allows(authz.Admin):
			return api.Errorf(api.CodeForbidden, "only an admin of the area removes a secret grant")
		}
		return remove(ctx, tx, id)
	}, func() events.Input { return grantEvent(computeapi.EventSecretGrantRemoved, g) })
	if err != nil {
		return nil, err
	}
	grantRemoved(ctx, s.d, t.Project.ID, g.ID)
	if core.OverTunnel(ctx) {
		g = withheld(g)
	}
	return g, nil
}

func (s *Service) setDefault(r *http.Request) (any, error) {
	ctx := r.Context()
	var req computeapi.SecretGrantDefaultRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		return nil, err
	}
	if ok, err := s.admin(ctx, core.SubjectOf(ctx), t.Project.ID, ""); err != nil {
		return nil, err
	} else if !ok {
		return nil, api.Errorf(api.CodeForbidden, "only an admin of the whole pool turns the default grants on or off")
	}
	g := Defaults()[0]
	g.Off = !req.On
	if err := s.edit(ctx, t.Project.ID, func(tx *sql.Tx) error { return setDefault(ctx, tx, req.On) },
		func() events.Input { return grantEvent(computeapi.EventSecretGrantDefault, g) }); err != nil {
		return nil, err
	}
	if !req.On {
		for _, d := range Defaults() {
			grantRemoved(ctx, s.d, t.Project.ID, d.ID)
		}
	}
	return g, nil
}

// validRef is a job grant's ref: a branch's full ref written exactly, with
// nothing git refuses in a ref name and no pattern character (jobrule
// checks at issue time that the run's ref is the default branch).
func validRef(ref string) bool {
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || name == "" || len(ref) > 255 || strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.Contains(name, "@{") || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") ||
		strings.ContainsFunc(name, func(r rune) bool { return r <= ' ' || r == 0x7f || strings.ContainsRune(`*?[]~^:\{}`, r) }) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// withheld is g as an answer over a tunnel shows it: a row's secret name
// withheld (a built-in's, * and JIT_CONFIG, are no secret's).
func withheld(g computeapi.SecretGrant) computeapi.SecretGrant {
	if !g.Builtin {
		g.Name = ""
	}
	return g
}

func areaNames(as []api.Area) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

// grantEvent is a table edit's event: for the area's admins only.
func grantEvent(typ string, g computeapi.SecretGrant) events.Input {
	return events.Input{Type: typ, Subject: g.ID, Area: g.Area, Data: g, Audience: LedgerAudience(authz.Admin, computeapi.SecretGrantWithheld)}
}

// edit makes a table edit and its event commit together: in a team
// project (a pool) in one transaction on its writer (events.Bus.TeamTx);
// elsewhere the edit commits and the event follows at once. ev is built
// after fn ran, so it may read what fn loaded.
func (s *Service) edit(ctx context.Context, project string, fn func(*sql.Tx) error, ev func() events.Input) error {
	if s.d.Events != nil && s.d.Store.IsTeam(project) {
		return s.d.Events.TeamTx(ctx, project, func(tx *sql.Tx, emit func(events.Input)) error {
			if err := fn(tx); err != nil {
				return err
			}
			emit(ev())
			return nil
		}, nil)
	}
	db, err := s.d.Store.Project(s.d.System().Scope, authz.Write, project)
	if err != nil {
		return err
	}
	if err := store.Tx(ctx, db, fn); err != nil {
		return err
	}
	if s.d.Events != nil {
		in := ev()
		in.Project = project
		if _, err := s.d.Events.Publish(context.WithoutCancel(ctx), in); err != nil && s.d.Log != nil {
			s.d.Log.Warn("grants: publish", "type", in.Type, "err", err)
		}
	}
	return nil
}
