// Package project is the project registry: projects (registry row +
// project.toml + project.db), areas, identities, grant tuples, tokens, the
// Subjects built from them, and project resolution ("zm project why").
// Owned by the integrator. See SPEC.md "Projects and areas" and "Who is calling".
package project

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/atomicfile"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

func init() {
	store.RegisterRegistry("project/0001_init", `
CREATE TABLE projects (
	id      TEXT PRIMARY KEY,
	slug    TEXT NOT NULL UNIQUE,
	name    TEXT NOT NULL,
	created INTEGER NOT NULL
);
CREATE TABLE identities (
	id              TEXT PRIMARY KEY,
	name            TEXT NOT NULL UNIQUE,
	kind            TEXT NOT NULL,
	tailscale_login TEXT UNIQUE,
	local           INTEGER NOT NULL DEFAULT 0,
	created         INTEGER NOT NULL
);
CREATE TABLE grants (
	subject    TEXT NOT NULL REFERENCES identities(id),
	relation   TEXT NOT NULL,
	object     TEXT NOT NULL,
	project    TEXT NOT NULL,
	area       TEXT NOT NULL DEFAULT '',
	created    INTEGER NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (subject, object)
);
CREATE INDEX grants_project ON grants (project);
CREATE TABLE tokens (
	id        TEXT PRIMARY KEY,
	hash      TEXT NOT NULL UNIQUE,
	identity  TEXT NOT NULL REFERENCES identities(id),
	project   TEXT NOT NULL DEFAULT '',
	area      TEXT NOT NULL DEFAULT '',
	role      TEXT NOT NULL,
	note      TEXT NOT NULL DEFAULT '',
	created   INTEGER NOT NULL,
	expires   INTEGER,
	revoked   INTEGER,
	last_used INTEGER
);`)
}

// Registry is the in-memory view of every project plus the registry tables.
// Safe for concurrent use.
type Registry struct {
	st  *store.Store
	cfg *config.Config

	mu     sync.RWMutex
	byID   map[string]*entry
	bySlug map[string]string
	local  api.Identity
	guard  func(ctx context.Context, name string) error
	fresh  func(ctx context.Context, tx *sql.Tx, project, machine string) error

	// machines caches MachineProjects per replica machine name (Visible
	// asks it for every remote address it redacts); every write to a
	// node-bound token (redeem, revoke) clears it (and an expiry: machineEntry).
	machinesMu sync.Mutex
	machines   map[string]machineEntry
}

// SetIdentityGuard installs a check every new identity name must pass (the
// server wires approvals' lookalike rule: no identity may look like an
// approver). Set once before serving.
func (r *Registry) SetIdentityGuard(f func(ctx context.Context, name string) error) {
	r.mu.Lock()
	r.guard = f
	r.mu.Unlock()
}

// OnFreshJoin installs what runs inside the transaction of Admit and Redeem when a machine
// joins a project (project is its ULID) with no live join by that node there
// (a first join, or one after its token was revoked or expired, or its grant
// went). The team module forgets what the owner set for the machine, so a
// host that left starts at its invite's or role's class, not at the one it
// held before. Its error rolls the admission back. Set once before serving.
func (r *Registry) OnFreshJoin(f func(ctx context.Context, tx *sql.Tx, project, machine string) error) {
	r.mu.Lock()
	r.fresh = f
	r.mu.Unlock()
}

type entry struct {
	created time.Time
	file    File
	written string // project.toml as the daemon last read or wrote it (see rewrite)
	team    bool   // a team project: this machine is its authority (SPEC 17.3)
}

// Open loads every project and ensures the machine owner's identity
// (config human, kind human, local).
func Open(ctx context.Context, st *store.Store, cfg *config.Config) (*Registry, error) {
	r := &Registry{st: st, cfg: cfg, byID: map[string]*entry{}, bySlug: map[string]string{}}
	db := st.Registry()
	rows, err := db.QueryContext(ctx, `SELECT id, slug, name, created, team FROM projects`)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, slug, name string
		created        int64
		team           bool
	}
	var all []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.slug, &x.name, &x.created, &x.team); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, x)
	}
	rows.Close()
	for _, x := range all {
		f := File{ID: x.id, Slug: x.slug, Name: x.name}
		path := filepath.Join(st.ProjectDir(x.id), FileName)
		if b, err := os.ReadFile(path); err == nil {
			pf, err := ParseFile(string(b))
			if err != nil {
				return nil, err
			}
			// Everything in the file but what the row is authoritative for:
			// a field left out here is dropped by the rewrite below.
			pf.ID, pf.Slug, pf.Name = f.ID, f.Slug, f.Name
			f = pf
		}
		if _, err := st.CreateProject(ctx, x.id); err != nil { // no-op when open; creates a lost project.db
			return nil, err
		}
		e := &entry{created: store.FromMillis(x.created), file: f, team: x.team}
		if x.team {
			if err := st.SetTeam(ctx, x.id, true); err != nil { // no-op when store.Open saw it
				return nil, err
			}
		}
		r.byID[x.id] = e
		r.bySlug[x.slug] = x.id
		if err := atomicfile.WritePrivate(path, []byte(f.Encode())); err != nil {
			return nil, err
		}
		e.written = f.Encode()
	}
	local, err := r.ensureIdentity(ctx, cfg.Human, api.IdentityHuman)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `UPDATE identities SET local = (id = ?)`, local.ID); err != nil {
		return nil, err
	}
	r.local = local
	return r, nil
}

// LocalIdentity is the machine owner (the OS user behind the Unix socket).
func (r *Registry) LocalIdentity() api.Identity { return r.local }

// ---- projects ----

// Lookup finds a project by slug or ULID, ignoring scope. Daemon-internal
// only; anything answering a caller uses Get.
func (r *Registry) Lookup(ref string) (api.ProjectRef, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id := r.idOf(ref)
	if id == "" {
		return api.ProjectRef{}, false
	}
	return api.ProjectRef{ID: id, Slug: r.byID[id].file.Slug}, true
}

func (r *Registry) idOf(ref string) string {
	if _, ok := r.byID[ref]; ok {
		return ref
	}
	if _, ok := r.byID[strings.ToUpper(ref)]; ok && ids.Valid(strings.ToUpper(ref)) {
		return strings.ToUpper(ref)
	}
	return r.bySlug[strings.ToLower(ref)]
}

// Slug is the display slug of a project ULID ("none" for none, "" if unknown).
func (r *Registry) Slug(id string) string {
	if id == api.ProjectNone || id == "" {
		return api.ProjectNone
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.byID[id]; ok {
		return e.file.Slug
	}
	return ""
}

// Get returns the project as sc may see it, or api.NotFound("project", ref).
func (r *Registry) Get(sc authz.Scope, ref string) (api.Project, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id := r.idOf(ref)
	if id == "" || !sc.SeesProject(id) {
		return api.Project{}, api.NotFound("project", ref)
	}
	return r.view(id, sc), nil
}

// List returns every project sc sees, by slug.
func (r *Registry) List(sc authz.Scope) []api.Project {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []api.Project
	for id := range r.byID {
		if sc.SeesProject(id) {
			out = append(out, r.view(id, sc))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// view renders a project for sc: area guests see only their areas, never
// the project-level repo rules or other area names.
func (r *Registry) view(id string, sc authz.Scope) api.Project {
	e := r.byID[id]
	p := api.Project{ID: id, Slug: e.file.Slug, Name: e.file.Name, Created: e.created, Role: string(sc.Role(id))}
	whole, areas := sc.Areas(id)
	vis := map[string]bool{}
	for _, a := range areas {
		vis[a] = true
	}
	if whole {
		p.RepoRules = cloneRules(e.file.Repos)
	}
	for _, a := range e.file.Areas {
		if whole || vis[a.Name] {
			p.Areas = append(p.Areas, a)
		}
	}
	for _, x := range extrasList() {
		if x.Project != nil {
			x.Project(&p, e.file, sc)
		}
	}
	return p
}

func cloneRules(x api.RepoRules) api.RepoRules {
	return api.RepoRules{Paths: append([]string(nil), x.Paths...), Remotes: append([]string(nil), x.Remotes...)}
}

// File returns a copy of a project's config, ignoring scope (daemon-internal).
func (r *Registry) File(id string) (File, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	if !ok {
		return File{}, false
	}
	return e.file, true
}

// Create makes a project (row, project.toml, project.db) and makes the
// creating identity its owner. Local callers only (the router enforces it).
func (r *Registry) Create(ctx context.Context, sub *authz.Subject, req api.NewProjectRequest) (api.Project, error) {
	slug := strings.ToLower(strings.TrimSpace(req.Slug))
	f := File{ID: ids.New(), Slug: slug, Name: strings.TrimSpace(req.Name), Repos: normRules(req.RepoRules)}
	if f.Name == "" {
		f.Name = slug
	}
	if err := f.Validate(); err != nil {
		return api.Project{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.bySlug[slug]; taken {
		return api.Project{}, api.Errorf(api.CodeConflict, "project %q already exists", slug)
	}
	now := time.Now().UTC()
	db := r.st.Registry()
	if _, err := db.ExecContext(ctx, `INSERT INTO projects (id, slug, name, created) VALUES (?, ?, ?, ?)`,
		f.ID, f.Slug, f.Name, store.Millis(now)); err != nil {
		if store.IsUnique(err) {
			return api.Project{}, api.Errorf(api.CodeConflict, "project %q already exists", slug)
		}
		return api.Project{}, err
	}
	if _, err := r.st.CreateProject(ctx, f.ID); err != nil {
		return api.Project{}, err
	}
	if err := atomicfile.WritePrivate(filepath.Join(r.st.ProjectDir(f.ID), FileName), []byte(f.Encode())); err != nil {
		return api.Project{}, err
	}
	owner := sub.Identity.ID
	if owner == "" {
		owner = r.local.ID
	}
	if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO grants (subject, relation, object, project, area, created, created_by)
		VALUES (?, 'owner', ?, ?, '', ?, ?)`, owner, authz.ProjectObject(f.ID), f.ID, store.Millis(now), sub.Address); err != nil {
		return api.Project{}, err
	}
	r.byID[f.ID] = &entry{created: store.FromMillis(store.Millis(now)), file: f, written: f.Encode()}
	r.bySlug[slug] = f.ID
	return r.view(f.ID, sub.Scope), nil
}

func normRules(x api.RepoRules) api.RepoRules {
	var out api.RepoRules
	for _, p := range x.Paths {
		if p = strings.TrimSpace(p); p != "" {
			out.Paths = append(out.Paths, p)
		}
	}
	for _, u := range x.Remotes {
		if u = NormalizeRemote(u); u != "" {
			out.Remotes = append(out.Remotes, u)
		}
	}
	return out
}

// Update changes slug, name or repo rules, and what modules take from req
// (Extras.Update). Admin on the project; repo rules need the machine's
// owner (machineOwner).
func (r *Registry) Update(ctx context.Context, sub *authz.Subject, ref string, req api.UpdateProjectRequest) (api.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.idOf(ref)
	if err := sub.Check(authz.Admin, authz.Object{Project: id}, "project", ref); err != nil || id == "" {
		if id == "" {
			return api.Project{}, api.NotFound("project", ref)
		}
		return api.Project{}, err
	}
	owner := machineOwner(sub)
	e := r.byID[id]
	f := e.file
	taken := false
	for _, x := range extrasList() {
		taken = taken || x.Update != nil
	}
	if what := moduleSettings(req); what != "" && !taken {
		// Answering 200 and storing nothing would tell the caller it was set.
		return api.Project{}, api.Errorf(api.CodeBadRequest, "this build has no module for the project setting %s: nothing was changed", what)
	}
	for _, x := range extrasList() {
		if x.Update != nil {
			g, err := x.Update(f, req, owner)
			if err != nil {
				return api.Project{}, err
			}
			// A module changes its keys and sections only: the registry's
			// own fields (and the owner check on repo rules below) are
			// not a hook's to set.
			f.Keys, f.Sections = g.Keys, g.Sections
		}
	}
	if req.Repos != nil && !owner {
		return api.Project{}, errLocalSettings("repo rules")
	}
	f = f.withSettings(req)
	if req.Repos != nil {
		f.Repos = normRules(*req.Repos)
	}
	if err := f.Validate(); err != nil {
		return api.Project{}, err
	}
	if other, taken := r.bySlug[f.Slug]; taken && other != id {
		return api.Project{}, api.Errorf(api.CodeConflict, "project %q already exists", f.Slug)
	}
	if _, err := r.st.Registry().ExecContext(ctx, `UPDATE projects SET slug = ?, name = ? WHERE id = ?`, f.Slug, f.Name, id); err != nil {
		return api.Project{}, err
	}
	if err := r.rewrite(id, e, f); err != nil {
		return api.Project{}, err
	}
	delete(r.bySlug, e.file.Slug)
	r.bySlug[f.Slug] = id
	e.file = f
	return r.view(id, sub.Scope), nil
}

// rewrite writes f as project id's project.toml, live (caller holds r.mu and
// applies f to e only on success). It refuses when the file on disk is no
// longer what the daemon last read or wrote: a hand edit made since the
// daemon started is loaded only by a restart, and a live change must not
// silently discard it (SPEC 3). A missing file is simply written.
func (r *Registry) rewrite(id string, e *entry, f File) error {
	path := filepath.Join(r.st.ProjectDir(id), FileName)
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil && string(b) != e.written {
		return api.Errorf(api.CodeConflict, "project.toml was edited by hand since the daemon loaded it; "+
			"restart the daemon to load the edit (zm daemon stop; the next command starts it), then retry")
	}
	text := f.Encode()
	if err := atomicfile.WritePrivate(path, []byte(text)); err != nil {
		return err
	}
	e.written = text
	return nil
}

// moduleSettings names the fields of req that only a module's Extras.Update
// can apply ("" when it carries none).
func moduleSettings(req api.UpdateProjectRequest) string {
	var what []string
	if req.Trackers != nil {
		what = append(what, "trackers")
	}
	if req.GuestIssues != nil {
		what = append(what, "guest_issues")
	}
	return strings.Join(what, ", ")
}

// machineOwner reports whether sub is the machine's owner on its own socket
// (no token) or the daemon itself. Only they may set what the daemon runs or
// reads on this machine: repo paths (which feed the --sandbox policy) and
// what a module's Extras.Update guards the same way.
// Admin on a project from the network (a token, a tailnet login) never
// includes that: it would reach other projects' files through this one.
func machineOwner(sub *authz.Subject) bool {
	return sub != nil && !sub.Remote && (sub.Via == authz.ViaLocal || sub.Via == authz.ViaSystem)
}

// LocalSettingsError is the refusal of a change to what only the machine's
// owner may set (an Extras.Update whose owner is false answers it).
func LocalSettingsError(what string) *api.Error { return errLocalSettings(what) }

func errLocalSettings(what string) *api.Error {
	return api.Errorf(api.CodeForbidden, "%s are machine settings: only the machine's owner on its own socket may change them", what).
		WithHint("ask the machine's owner to run zm project set / zm area add / zm area set on that machine")
}

// AddArea adds an area. Admin on the project; repo rules need the machine's
// owner (machineOwner).
func (r *Registry) AddArea(ctx context.Context, sub *authz.Subject, ref string, a api.Area) (api.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.idOf(ref)
	if id == "" {
		return api.Project{}, api.NotFound("project", ref)
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: id}, "project", ref); err != nil {
		return api.Project{}, err
	}
	if (len(a.Paths) > 0 || len(a.Remotes) > 0) && !machineOwner(sub) {
		return api.Project{}, errLocalSettings("an area's repo rules")
	}
	e := r.byID[id]
	f := e.file
	a.Name = strings.ToLower(strings.TrimSpace(a.Name))
	a.RepoRules = normRules(a.RepoRules)
	a.Labels = trimAll(a.Labels)
	a.Work = trimAll(a.Work)
	f.Areas = append(append([]api.Area(nil), f.Areas...), a)
	if err := f.Validate(); err != nil {
		return api.Project{}, err
	}
	if err := r.rewrite(id, e, f); err != nil {
		return api.Project{}, err
	}
	e.file = f
	return r.view(id, sub.Scope), nil
}

// areaIndex is the index of area name in e, or -1.
func (e *entry) areaIndex(name string) int {
	for i, a := range e.file.Areas {
		if a.Name == name {
			return i
		}
	}
	return -1
}

// visibleArea finds a project and one of its areas for sc. A hidden project
// answers like a missing one (NotFound project); a hidden area like a
// missing one (NotFound area). Callers hold r.mu.
func (r *Registry) visibleArea(sc authz.Scope, ref, name string) (string, int, error) {
	id := r.idOf(ref)
	if id == "" || !sc.SeesProject(id) {
		return "", -1, api.NotFound("project", ref)
	}
	name = strings.ToLower(strings.TrimSpace(name))
	i := r.byID[id].areaIndex(name)
	if i < 0 || !sc.Can(authz.Read, authz.Object{Project: id, Area: name}) {
		return "", -1, api.NotFound("area", name)
	}
	return id, i, nil
}

// areaView renders area i of project id for sc. Callers hold r.mu.
func (r *Registry) areaView(id string, i int, sc authz.Scope) api.AreaView {
	e := r.byID[id]
	a := e.file.Areas[i]
	v := api.AreaView{Project: api.ProjectRef{ID: id, Slug: e.file.Slug}, Role: string(sc.Relation(authz.Object{Project: id, Area: a.Name}))}
	v.Name, v.RepoRules = a.Name, cloneRules(a.RepoRules)
	v.Work, v.Labels = append([]string(nil), a.Work...), append([]string(nil), a.Labels...)
	for _, x := range extrasList() {
		if x.Area != nil {
			x.Area(&v, e.file, sc)
		}
	}
	return v
}

// GetArea returns one area as sc may see it ("zm project show acme/api").
func (r *Registry) GetArea(sc authz.Scope, ref, name string) (api.AreaView, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, i, err := r.visibleArea(sc, ref, name)
	if err != nil {
		return api.AreaView{}, err
	}
	return r.areaView(id, i, sc), nil
}

// UpdateArea changes an area's rules and applies them at once: resolution,
// work-item areas and label filters all read the registry, so nothing needs
// a restart. Admin on the project (an area's own owner could otherwise widen
// it over items it cannot see); repo rules need the machine's owner.
func (r *Registry) UpdateArea(ctx context.Context, sub *authz.Subject, ref, name string, req api.UpdateAreaRequest) (api.AreaView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, i, err := r.visibleArea(sub.Scope, ref, name)
	if err != nil {
		return api.AreaView{}, err
	}
	if !sub.Can(authz.Admin, authz.Object{Project: id}) {
		// The caller sees this area (visibleArea), so saying no leaks nothing.
		return api.AreaView{}, api.Errorf(api.CodeForbidden, "changing an area's rules needs admin on project %s", r.byID[id].file.Slug)
	}
	if (req.Paths != nil || req.Remotes != nil) && !machineOwner(sub) {
		return api.AreaView{}, errLocalSettings("an area's repo rules")
	}
	e := r.byID[id]
	f := e.file
	f.Areas = append([]api.Area(nil), f.Areas...)
	a := f.Areas[i]
	if req.Paths != nil || req.Remotes != nil {
		rules := cloneRules(a.RepoRules)
		if req.Paths != nil {
			rules.Paths = *req.Paths
		}
		if req.Remotes != nil {
			rules.Remotes = *req.Remotes
		}
		a.RepoRules = normRules(rules)
	}
	if req.Work != nil {
		a.Work = trimAll(*req.Work)
	}
	if req.Labels != nil {
		a.Labels = trimAll(*req.Labels)
	}
	f.Areas[i] = a
	if err := f.Validate(); err != nil {
		return api.AreaView{}, err
	}
	if err := r.rewrite(id, e, f); err != nil {
		return api.AreaView{}, err
	}
	e.file = f
	return r.areaView(id, i, sub.Scope), nil
}

// HasArea reports whether project id defines area (daemon-internal).
func (r *Registry) HasArea(id, area string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	if !ok {
		return false
	}
	for _, a := range e.file.Areas {
		if a.Name == area {
			return true
		}
	}
	return false
}

func trimAll(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Export writes project.db (a consistent copy) and project.toml into dir.
// Local callers only. Leases, sessions and grants live in registry.db and
// are not part of the export (SPEC "Storage").
func (r *Registry) Export(ctx context.Context, sub *authz.Subject, ref, dir string) (api.ExportResponse, error) {
	p, err := r.Get(sub.Scope, ref)
	if err != nil {
		return api.ExportResponse{}, err
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: p.ID}, "project", ref); err != nil {
		return api.ExportResponse{}, err
	}
	if !filepath.IsAbs(dir) {
		return api.ExportResponse{}, api.Errorf(api.CodeBadRequest, "export dir must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return api.ExportResponse{}, api.Errorf(api.CodeBadRequest, "cannot create %s: %v", dir, err)
	}
	dbPath := filepath.Join(dir, store.FileProject)
	if _, err := os.Stat(dbPath); err == nil {
		return api.ExportResponse{}, api.Errorf(api.CodeConflict, "%s already exists", dbPath)
	}
	// Owner-only event keys (a shell's terminal title, a session's fg and
	// ports) and members-only ones (its prompts) never leave in an export.
	if err := r.st.CopyProject(ctx, p.ID, dbPath, events.ScrubOwnerOnly, events.ScrubMembersOnly, func(ctx context.Context, db *sql.DB) error { return store.FilterExport(ctx, db, sub.Address) }); err != nil {
		return api.ExportResponse{}, err
	}
	f, _ := r.File(p.ID)
	tomlPath := filepath.Join(dir, FileName)
	if err := os.WriteFile(tomlPath, []byte(f.Encode()), 0o600); err != nil {
		return api.ExportResponse{}, err
	}
	return api.ExportResponse{Files: []string{dbPath, tomlPath}}, nil
}

// ---- identities ----

func (r *Registry) ensureIdentity(ctx context.Context, name, kind string) (api.Identity, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if id, err := r.identity(ctx, name); err == nil {
		return id, nil
	} else if !api.IsCode(err, api.CodeNotFound) {
		return api.Identity{}, err
	}
	return r.AddIdentity(ctx, api.NewIdentityRequest{Name: name, Kind: kind})
}

// AddIdentity creates an identity. Local callers only.
func (r *Registry) AddIdentity(ctx context.Context, req api.NewIdentityRequest) (api.Identity, error) {
	name := strings.ToLower(strings.TrimSpace(req.Name))
	if !api.ValidName(name) {
		return api.Identity{}, api.Errorf(api.CodeBadRequest, "bad identity name %q", req.Name)
	}
	kind := req.Kind
	if kind == "" {
		kind = api.IdentityHuman
	}
	if kind != api.IdentityHuman && kind != api.IdentityAgent && kind != api.IdentityMachine {
		return api.Identity{}, api.Errorf(api.CodeBadRequest, "identity kind must be human, agent or machine")
	}
	r.mu.RLock()
	guard := r.guard
	r.mu.RUnlock()
	if guard != nil {
		if err := guard(ctx, name); err != nil {
			return api.Identity{}, err
		}
	}
	id := api.Identity{ID: ids.New(), Name: name, Kind: kind, TailscaleLogin: strings.TrimSpace(req.TailscaleLogin), Created: time.Now().UTC()}
	var login any
	if id.TailscaleLogin != "" {
		login = id.TailscaleLogin
	}
	_, err := r.st.Registry().ExecContext(ctx, `INSERT INTO identities (id, name, kind, tailscale_login, created) VALUES (?, ?, ?, ?, ?)`,
		id.ID, id.Name, id.Kind, login, store.Millis(id.Created))
	if store.IsUnique(err) {
		return api.Identity{}, api.Errorf(api.CodeConflict, "identity %q (or that tailscale login) already exists", name)
	}
	id.Created = store.FromMillis(store.Millis(id.Created))
	return id, err
}

// Identities lists every identity. Local callers only.
func (r *Registry) Identities(ctx context.Context) ([]api.Identity, error) {
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT id, name, kind, coalesce(tailscale_login, ''), created FROM identities ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Identity
	for rows.Next() {
		var id api.Identity
		var created int64
		if err := rows.Scan(&id.ID, &id.Name, &id.Kind, &id.TailscaleLogin, &created); err != nil {
			return nil, err
		}
		id.Created = store.FromMillis(created)
		out = append(out, id)
	}
	return out, rows.Err()
}

// Identity finds an identity by name or ULID, ignoring scope (daemon
// internal: a session's who, SPEC 8). Missing is api.NotFound.
func (r *Registry) Identity(ctx context.Context, ref string) (api.Identity, error) {
	return r.identity(ctx, ref)
}

// identity finds an identity by name or ULID.
func (r *Registry) identity(ctx context.Context, ref string) (api.Identity, error) {
	var id api.Identity
	var created int64
	err := r.st.Registry().QueryRowContext(ctx, `SELECT id, name, kind, coalesce(tailscale_login, ''), created
		FROM identities WHERE id = ? OR name = ?`, strings.ToUpper(ref), strings.ToLower(ref)).
		Scan(&id.ID, &id.Name, &id.Kind, &id.TailscaleLogin, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Identity{}, api.NotFound("identity", ref)
	}
	id.Created = store.FromMillis(created)
	return id, err
}

// ---- grants ----

// Tuples returns an identity's grant tuples.
func (r *Registry) Tuples(ctx context.Context, identityID string) ([]authz.Tuple, error) {
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT relation, object FROM grants WHERE subject = ?`, identityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.Tuple
	for rows.Next() {
		t := authz.Tuple{Subject: identityID}
		if err := rows.Scan(&t.Relation, &t.Object); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Registry) grantTarget(sub *authz.Subject, project, area string) (string, string, error) {
	r.mu.RLock()
	id := r.idOf(project)
	r.mu.RUnlock()
	if id == "" || !sub.Scope.SeesProject(id) {
		return "", "", api.NotFound("project", project)
	}
	area = strings.ToLower(strings.TrimSpace(area))
	// An area the caller cannot read answers exactly like a missing one
	// (the admin check below would otherwise name the project instead).
	if area != "" && !sub.Scope.WholeProject(id) && (!r.HasArea(id, area) || !sub.Scope.Can(authz.Read, authz.Object{Project: id, Area: area})) {
		return "", "", api.NotFound("area", area)
	}
	if area != "" && !r.HasArea(id, area) {
		return "", "", api.Errorf(api.CodeNotFound, "no area %q in project %s", area, r.Slug(id)).
			WithHint("add it: zm area add " + area + " --project " + r.Slug(id))
	}
	if err := sub.Check(authz.Admin, authz.Object{Project: id, Area: area}, "project", project); err != nil {
		return "", "", err
	}
	return id, area, nil
}

// Grant adds (or changes) a tuple. Admin on the object. A missing identity
// is created as kind human. An admin lowering a grant leaves a ceiling
// (setCeiling), so a GitHub join does not raise it back (ownerCap), and
// lowers the other identities of the same GitHub account (lowerSameAccount);
// created_by stays, so revoking the GitHub grant that first gave it still
// removes it (RevokeAdmitted).
func (r *Registry) Grant(ctx context.Context, sub *authz.Subject, req api.GrantRequest) (api.Grant, error) {
	rel, err := authz.ParseRelation(req.Role)
	if err != nil {
		return api.Grant{}, err
	}
	pid, area, err := r.grantTarget(sub, req.Project, req.Area)
	if err != nil {
		return api.Grant{}, err
	}
	who, err := r.ensureIdentity(ctx, req.Identity, api.IdentityHuman)
	if err != nil {
		return api.Grant{}, err
	}
	obj := authz.AreaObject(pid, area)
	now := time.Now().UTC()
	var also []string
	if err = store.Tx(ctx, r.st.Registry(), func(tx *sql.Tx) error {
		var prev string
		switch err := tx.QueryRowContext(ctx, `SELECT relation FROM grants WHERE subject = ? AND object = ?`, who.ID, obj).Scan(&prev); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		}
		if prev == string(rel) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO grants (subject, relation, object, project, area, created, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (subject, object) DO UPDATE SET relation = excluded.relation`,
			who.ID, string(rel), obj, pid, area, store.Millis(now), sub.Address); err != nil {
			return err
		}
		if err := setCeiling(ctx, tx, who.ID, pid, area, authz.Relation(prev), rel, sub.Address, now); err != nil {
			return err
		}
		if authz.Max(authz.Relation(prev), rel) != rel {
			if also, err = lowerSameAccount(ctx, tx, who.ID, []string{r.local.ID, sub.Identity.ID}, pid, area, rel, sub.Address, now); err != nil {
				return err
			}
		}
		return bumpScope(ctx, tx, who.ID)
	}); err != nil {
		return api.Grant{}, err
	}
	r.forgetMachines() // a replica's node-bound token whose last grant went may count again
	return api.Grant{Identity: who.Name, Role: string(rel), Project: r.Slug(pid), Area: area, Object: obj, Created: store.FromMillis(store.Millis(now)),
		AlsoLowered: also}, nil
}

// Revoke removes a tuple. Admin on the object. The local identity's owner
// grant on a project cannot be revoked (it would orphan the project). The
// removal leaves a ceiling (setCeiling): a GitHub join, or an invite made
// before it, does not give the grant back; it removes the grant of every
// other identity the same GitHub account is or was (lowerSameAccount).
func (r *Registry) Revoke(ctx context.Context, sub *authz.Subject, req api.GrantRequest) (api.Grant, error) {
	pid, area, err := r.grantTarget(sub, req.Project, req.Area)
	if err != nil {
		return api.Grant{}, err
	}
	who, err := r.identity(ctx, req.Identity)
	if err != nil {
		return api.Grant{}, err
	}
	obj := authz.AreaObject(pid, area)
	if who.ID == r.local.ID && area == "" {
		return api.Grant{}, api.Errorf(api.CodeConflict, "the machine owner always owns its projects")
	}
	var rel string
	var also []string
	err = store.Tx(ctx, r.st.Registry(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `DELETE FROM grants WHERE subject = ? AND object = ? RETURNING relation`, who.ID, obj).Scan(&rel); err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := setCeiling(ctx, tx, who.ID, pid, area, authz.Relation(rel), authz.None, sub.Address, now); err != nil {
			return err
		}
		var err error
		if also, err = lowerSameAccount(ctx, tx, who.ID, []string{r.local.ID, sub.Identity.ID}, pid, area, authz.None, sub.Address, now); err != nil {
			return err
		}
		return bumpScope(ctx, tx, who.ID)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return api.Grant{}, api.NotFound("grant", who.Name+" on "+r.Slug(pid)+areaSuffix(area))
	}
	if err != nil {
		return api.Grant{}, err
	}
	r.forgetMachines() // a replica's node-bound token may have lost its last grant
	return api.Grant{Identity: who.Name, Role: rel, Project: r.Slug(pid), Area: area, Object: obj, AlsoLowered: also}, nil
}

func areaSuffix(a string) string {
	if a == "" {
		return ""
	}
	return "/" + a
}

// listedProject is the project id a grants or ceilings listing names (""
// for all); a project sub cannot see answers like a missing one.
func (r *Registry) listedProject(sub *authz.Subject, project string) (string, error) {
	if project == "" {
		return "", nil
	}
	r.mu.RLock()
	pid := r.idOf(project)
	r.mu.RUnlock()
	if pid == "" || !sub.Scope.SeesProject(pid) {
		return "", api.NotFound("project", project)
	}
	return pid, nil
}

// Grants lists tuples on objects where sub has admin (one project, or all).
func (r *Registry) Grants(ctx context.Context, sub *authz.Subject, project string) ([]api.Grant, error) {
	pid, err := r.listedProject(sub, project)
	if err != nil {
		return nil, err
	}
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT i.name, g.relation, g.object, g.project, g.area, g.created
		FROM grants g JOIN identities i ON i.id = g.subject WHERE (? = '' OR g.project = ?) ORDER BY g.project, g.area, i.name`, pid, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Grant{}
	for rows.Next() {
		var g api.Grant
		var p string
		var created int64
		if err := rows.Scan(&g.Identity, &g.Role, &g.Object, &p, &g.Area, &created); err != nil {
			return nil, err
		}
		if !sub.Can(authz.Admin, authz.Object{Project: p, Area: g.Area}) {
			continue
		}
		g.Project, g.Created = r.Slug(p), store.FromMillis(created)
		out = append(out, g)
	}
	if project != "" && len(out) == 0 && !sub.Can(authz.Admin, authz.Object{Project: pid}) {
		return nil, api.Errorf(api.CodeForbidden, "your role cannot list grants in this project")
	}
	return out, rows.Err()
}

// ---- tokens ----

// Mint creates a bearer token for an identity, capped at a role (and a
// project/area). Unless NoGrant, the matching grant is added first. Local
// callers only. The secret is returned once; only its sha256 is stored.
func (r *Registry) Mint(ctx context.Context, sub *authz.Subject, req api.MintTokenRequest) (api.MintTokenResponse, error) {
	rel, err := authz.ParseRelation(req.Role)
	if err != nil {
		return api.MintTokenResponse{}, err
	}
	if req.TTLSeconds < 0 {
		return api.MintTokenResponse{}, api.Errorf(api.CodeBadRequest, "ttl must be positive")
	}
	if err := checkBoxKey(req); err != nil {
		return api.MintTokenResponse{}, err
	}
	var pid, area string
	if req.Project != "" {
		if !req.NoGrant {
			g, err := r.Grant(ctx, sub, api.GrantRequest{Identity: req.As, Role: req.Role, Project: req.Project, Area: req.Area})
			if err != nil {
				return api.MintTokenResponse{}, err
			}
			req.Area = g.Area
		}
		if pid, area, err = r.grantTarget(sub, req.Project, req.Area); err != nil {
			return api.MintTokenResponse{}, err
		}
	} else if req.Area != "" {
		return api.MintTokenResponse{}, api.Errorf(api.CodeBadRequest, "--area needs --project")
	}
	who, err := r.ensureIdentity(ctx, req.As, api.IdentityHuman)
	if err != nil {
		return api.MintTokenResponse{}, err
	}
	secret := authz.NewTokenSecret()
	now := time.Now().UTC()
	t := api.Token{ID: ids.New(), Identity: who.Name, Project: r.slugOrEmpty(pid), Area: area, Role: string(rel),
		Note: req.Note, Created: store.FromMillis(store.Millis(now))}
	var exp any
	if req.TTLSeconds > 0 {
		e := store.FromMillis(store.Millis(now.Add(time.Duration(req.TTLSeconds) * time.Second)))
		t.Expires, exp = &e, store.Millis(e)
	}
	if _, err := r.st.Registry().ExecContext(ctx, `INSERT INTO tokens (id, hash, identity, project, area, role, note, created, expires, box_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, authz.HashToken(secret), who.ID, pid, area, t.Role, t.Note, store.Millis(now), exp, req.BoxKey); err != nil {
		return api.MintTokenResponse{}, err
	}
	return api.MintTokenResponse{Token: t, Secret: secret}, nil
}

func (r *Registry) slugOrEmpty(id string) string {
	if id == "" {
		return ""
	}
	return r.Slug(id)
}

const tokenCols = `t.id, i.name, t.project, t.area, t.role, t.note, t.created, t.expires, t.revoked, t.last_used, t.node, t.machine`

func (r *Registry) scanToken(sc interface{ Scan(...any) error }) (api.Token, error) {
	var t api.Token
	var pid string
	var created int64
	var exp, rev, used sql.NullInt64
	if err := sc.Scan(&t.ID, &t.Identity, &pid, &t.Area, &t.Role, &t.Note, &created, &exp, &rev, &used, &t.Node, &t.Machine); err != nil {
		return t, err
	}
	t.Project, t.Created = r.slugOrEmpty(pid), store.FromMillis(created)
	t.Expires, t.Revoked, t.LastUsed = store.NullMillis(exp), store.NullMillis(rev), store.NullMillis(used)
	return t, nil
}

// Tokens lists every token (never secrets). Local callers only.
func (r *Registry) Tokens(ctx context.Context) ([]api.Token, error) {
	rows, err := r.st.Registry().QueryContext(ctx, `SELECT `+tokenCols+` FROM tokens t JOIN identities i ON i.id = t.identity ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Token{}
	for rows.Next() {
		t, err := r.scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes a token by id. Local callers only. Idempotent.
func (r *Registry) RevokeToken(ctx context.Context, id string) (api.Token, error) {
	id = strings.ToUpper(strings.TrimSpace(id))
	_, err := r.st.Registry().ExecContext(ctx, `UPDATE tokens SET revoked = coalesce(revoked, ?) WHERE id = ?`,
		store.Millis(time.Now()), id)
	r.forgetMachines()
	if err != nil {
		return api.Token{}, err
	}
	t, err := r.scanToken(r.st.Registry().QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens t JOIN identities i ON i.id = t.identity WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Token{}, api.NotFound("token", id)
	}
	return t, err
}

// TokenExpiry is when token id expires (zero: never). A token that is
// revoked, expired or unknown is an unauthenticated error. Local callers
// only.
func (r *Registry) TokenExpiry(ctx context.Context, id string) (time.Time, error) {
	var exp, rev sql.NullInt64
	err := r.st.Registry().QueryRowContext(ctx, `SELECT expires, revoked FROM tokens WHERE id = ?`, strings.ToUpper(strings.TrimSpace(id))).Scan(&exp, &rev)
	gone := api.Errorf(api.CodeUnauthenticated, "token %s is revoked, expired or unknown", id)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, gone
	}
	if err != nil {
		return time.Time{}, err
	}
	if rev.Valid || (exp.Valid && exp.Int64 <= store.Millis(time.Now())) {
		return time.Time{}, gone
	}
	if !exp.Valid {
		return time.Time{}, nil
	}
	return store.FromMillis(exp.Int64), nil
}

// RevokeTokenHash revokes the token whose secret hashes to hash
// (authz.HashToken). Local callers only. Idempotent; false when no token
// has that hash.
func (r *Registry) RevokeTokenHash(ctx context.Context, hash string) (bool, error) {
	res, err := r.st.Registry().ExecContext(ctx, `UPDATE tokens SET revoked = coalesce(revoked, ?) WHERE hash = ?`,
		store.Millis(time.Now()), hash)
	r.forgetMachines()
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ---- subjects ----

// Local is the subject for an unauthenticated call on the Unix socket: the
// OS user, who owns the machine (scope All). address is the caller's reply
// address (a local session, or the human).
func (r *Registry) Local(address string) *authz.Subject {
	if address == "" {
		address = "human:" + r.local.Name
	}
	s := &authz.Subject{Identity: r.local, Address: address, Via: authz.ViaLocal, Scope: authz.All()}
	if a, err := api.ParseAddress(address); err == nil && a.Kind == api.KindSession {
		s.Session = a.String()
	}
	return s
}

// FromToken authenticates a bearer secret. Unknown, revoked and expired
// tokens all get the same unauthenticated error. A node-bound token (a team
// replica's, SPEC 17.4) answers the same error from any node other than its
// own (FromTokenAt names the caller's), on a listener without WhoIs, and on
// any Unix socket (remote false). So does a box's token (box_key) from any
// caller but its box key: only the tunnel names a key as the node.
func (r *Registry) FromToken(ctx context.Context, secret string, remote bool) (*authz.Subject, error) {
	return r.FromTokenAt(ctx, secret, remote, "")
}

// FromTokenAt is FromToken for a caller WhoIs identified as tailnet node
// node (core.Authenticate passes the WhoIs stable node ID).
func (r *Registry) FromTokenAt(ctx context.Context, secret string, remote bool, node string) (*authz.Subject, error) {
	bad := api.Errorf(api.CodeUnauthenticated, "invalid or expired token")
	if !authz.LooksLikeToken(secret) {
		return nil, bad
	}
	var tid, identityID, pid, area, role, boundNode, machine, boxKey string
	var exp, rev, used sql.NullInt64
	err := r.st.Registry().QueryRowContext(ctx, `SELECT id, identity, project, area, role, expires, revoked, last_used, node, machine, box_key FROM tokens WHERE hash = ?`,
		authz.HashToken(secret)).Scan(&tid, &identityID, &pid, &area, &role, &exp, &rev, &used, &boundNode, &machine, &boxKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, bad
		}
		return nil, err
	}
	now := time.Now()
	if rev.Valid || (exp.Valid && exp.Int64 <= store.Millis(now)) {
		return nil, bad
	}
	if boundNode != "" && (!remote || node == "" || node != boundNode) {
		return nil, bad
	}
	if boxKey != "" && (!remote || node != boxKey) {
		return nil, bad
	}
	if !used.Valid || store.Millis(now)-used.Int64 > 60_000 {
		_, _ = r.st.Registry().ExecContext(ctx, `UPDATE tokens SET last_used = ? WHERE id = ?`, store.Millis(now), tid)
		if !used.Valid && boundNode != "" {
			// The replica's first authenticated call: redemption stops being
			// idempotent (the same code no longer returns this token).
			_, _ = r.st.Registry().ExecContext(ctx, `UPDATE invites SET pending_secret = '' WHERE token = ?`, tid)
		}
	}
	who, sv, err := r.identityVersion(ctx, identityID)
	if err != nil {
		return nil, bad
	}
	tuples, err := r.Tuples(ctx, who.ID)
	if err != nil {
		return nil, err
	}
	cp := &authz.Cap{Project: pid, Area: area, Role: authz.Relation(role)}
	sc := authz.FromTuples(tuples, cp)
	return &authz.Subject{Identity: who, Address: identityAddress(who), Via: authz.ViaToken, Remote: remote, TokenID: tid, Scope: sc,
		Node: boundNode, Machine: machine, BoxKey: boxKey, ScopeVersion: sv, Cap: cp}, nil
}

// FromTailscale authenticates a tailnet caller by its WhoIs login.
func (r *Registry) FromTailscale(ctx context.Context, login string) (*authz.Subject, error) {
	var idv string
	err := r.st.Registry().QueryRowContext(ctx, `SELECT id FROM identities WHERE tailscale_login = ?`, strings.TrimSpace(login)).Scan(&idv)
	if errors.Is(err, sql.ErrNoRows) || login == "" {
		return nil, api.Errorf(api.CodeUnauthenticated, "this tailnet login is not a zm identity").
			WithHint("the machine owner can run: zm identity add <name> --tailscale <login>, then zm grant")
	}
	if err != nil {
		return nil, err
	}
	who, err := r.identity(ctx, idv)
	if err != nil {
		return nil, err
	}
	tuples, err := r.Tuples(ctx, who.ID)
	if err != nil {
		return nil, err
	}
	return &authz.Subject{Identity: who, Address: identityAddress(who), Via: authz.ViaTailnet, Remote: true, Scope: authz.FromTuples(tuples, nil)}, nil
}

// identityAddress is an identity's mailbox: human:<name> (agents and humans
// alike; a remote identity is a named mailbox), machine:<name> for machines.
func identityAddress(id api.Identity) string {
	if id.Kind == api.IdentityMachine {
		return "machine:" + id.Name
	}
	return "human:" + id.Name
}
