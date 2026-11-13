package project

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Resolution rules, in order (SPEC "Which project"):
const (
	RuleFlag           = "flag"            // --project (Zm-Project, from flag)
	RuleEnv            = "env"             // $ZM_PROJECT (Zm-Project, from env)
	RuleCwdFile        = "cwd-file"        // <repo>/.zm/project.toml above the cwd
	RuleCwdPath        = "cwd-path"        // a project's/area's paths contain the cwd
	RuleCwdRemote      = "cwd-remote"      // the cwd's repo origin matches a project's/area's remotes
	RuleSession        = "session"         // the calling session's registered project
	RuleDefaultProject = "default_project" // config.json default_project
)

// ResolveInput is what a request tells us about where it wants to write.
type ResolveInput struct {
	Explicit     string // slug, ULID, or "slug/area"
	ExplicitFrom string // RuleFlag or RuleEnv
	// Cwd is the client's directory; empty for remote callers (never used from the network).
	Cwd string
	// SessionProject/SessionArea are the calling session's registration ("" or "none" = none).
	SessionProject string
	SessionArea    string
	// Default is config.json default_project (a slug).
	Default string
}

// Resolve picks the project for a write. Only projects sc can see are
// candidates. A rule that names a project explicitly but does not resolve
// (flag, env, cwd-file) stops resolution: Project stays nil and the last step
// says why, so nothing is silently filed elsewhere.
func (r *Registry) Resolve(sc authz.Scope, in ResolveInput) api.Resolution {
	res := api.Resolution{Steps: []api.ResolveStep{}}
	step := func(rule, input, result string) {
		res.Steps = append(res.Steps, api.ResolveStep{Rule: rule, Input: input, Result: result})
	}
	done := func(rule, id, area string) api.Resolution {
		res.Project = &api.ProjectRef{ID: id, Slug: r.Slug(id)}
		res.Area, res.Rule = area, rule
		return res
	}

	// 1, 2: --project, $ZM_PROJECT.
	if in.Explicit != "" {
		rule := in.ExplicitFrom
		if rule != RuleEnv {
			rule = RuleFlag
		}
		id, area, why := r.named(sc, in.Explicit)
		step(rule, in.Explicit, why)
		if id == "" {
			return res
		}
		return done(rule, id, area)
	}
	// 3-5: the working directory.
	if in.Cwd != "" {
		cwd := ExpandPath(in.Cwd)
		root := ""
		if cwd != "" {
			root = GitRoot(cwd)
		}
		if f, path, ok := findRepoFile(cwd, root); ok {
			if f.err != nil {
				step(RuleCwdFile, path, f.err.Error())
				return res
			}
			name := f.file.Project
			if f.file.Area != "" {
				name += "/" + f.file.Area
			}
			id, area, why := r.named(sc, name)
			step(RuleCwdFile, path, why)
			if id == "" {
				return res
			}
			return done(RuleCwdFile, id, area)
		}
		step(RuleCwdFile, in.Cwd, "no .zm/project.toml")
		if id, area, _ := r.MatchPath(sc, cwd); id != "" {
			step(RuleCwdPath, cwd, r.Slug(id)+areaSuffix(area))
			return done(RuleCwdPath, id, area)
		}
		step(RuleCwdPath, cwd, "no project path contains it")
		remote := ""
		if root != "" {
			remote = NormalizeRemote(OriginURL(root))
		}
		if remote != "" {
			if id, area := r.matchRemote(sc, remote); id != "" {
				step(RuleCwdRemote, remote, r.Slug(id)+areaSuffix(area))
				return done(RuleCwdRemote, id, area)
			}
			step(RuleCwdRemote, remote, "no project lists this remote")
		} else {
			step(RuleCwdRemote, root, "no git origin")
		}
	}
	// 6: the calling session's project.
	if p := in.SessionProject; p != "" && p != api.ProjectNone {
		if sc.SeesProject(p) && r.Slug(p) != "" {
			step(RuleSession, r.Slug(p), r.Slug(p)+areaSuffix(in.SessionArea))
			return done(RuleSession, p, in.SessionArea)
		}
	}
	step(RuleSession, in.SessionProject, "caller is not a session in a project")
	// 7: default_project.
	if in.Default != "" {
		id, area, why := r.named(sc, in.Default)
		step(RuleDefaultProject, in.Default, why)
		if id != "" {
			return done(RuleDefaultProject, id, area)
		}
		return res
	}
	step(RuleDefaultProject, "", "not set")
	return res
}

// named resolves "slug", "ULID" or "slug/area" among projects sc sees.
func (r *Registry) named(sc authz.Scope, name string) (id, area, why string) {
	ref, area, _ := strings.Cut(strings.TrimSpace(name), "/")
	r.mu.RLock()
	id = r.idOf(ref)
	r.mu.RUnlock()
	if id == "" || !sc.SeesProject(id) {
		return "", "", "no project " + ref
	}
	if area != "" && (!r.HasArea(id, area) || !sc.Can(authz.Read, authz.Object{Project: id, Area: area})) {
		return "", "", "no area " + area + " in " + r.Slug(id)
	}
	if area == "" {
		area = r.soleArea(sc, id)
	}
	return id, area, r.Slug(id) + areaSuffix(area)
}

// soleArea: a caller who can see exactly one area of a project (and not the
// whole project) writes there by default.
func (r *Registry) soleArea(sc authz.Scope, id string) string {
	if whole, areas := sc.Areas(id); !whole && len(areas) == 1 {
		return areas[0]
	}
	return ""
}

type repoFileResult struct {
	file RepoFile
	err  error
}

// findRepoFile looks for .zm/project.toml from cwd up to the repo root
// (or only in cwd when it is not in a repository).
func findRepoFile(cwd, root string) (repoFileResult, string, bool) {
	if cwd == "" {
		return repoFileResult{}, "", false
	}
	dir := cwd
	for {
		p := filepath.Join(dir, RepoFileDir, FileName)
		if b, err := os.ReadFile(p); err == nil {
			f, err := ParseRepoFile(string(b))
			return repoFileResult{file: f, err: err}, p, true
		}
		if root == "" || dir == root {
			return repoFileResult{}, "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return repoFileResult{}, "", false
		}
		dir = parent
	}
}

// Binding is where writes resolve to a project, or to one of its areas
// (Area set): its path rules and repo remotes (SPEC "Which project"). A
// local project's come from its file, a joined project's from its team
// header; both match the same way (BestPath, BestRemote).
type Binding struct {
	Project, Area string
	api.RepoRules
}

// BestPath is the binding with the longest path rule containing cwd; an
// area's wins over a project's of equal length. n is that length.
func BestPath(cwd string, bs []Binding) (b Binding, n int, ok bool) {
	n = -1
	for _, x := range bs {
		for _, p := range x.Paths {
			if m, hit := PathMatch(cwd, p); hit && (m > n || (m == n && x.Area != "" && b.Area == "")) {
				b, n, ok = x, m, true
			}
		}
	}
	return b, n, ok
}

// BestRemote is the binding listing remote (normalised); an area's wins.
func BestRemote(remote string, bs []Binding) (b Binding, ok bool) {
	for _, x := range bs {
		for _, u := range x.Remotes {
			if strings.EqualFold(NormalizeRemote(u), remote) && (!ok || (x.Area != "" && b.Area == "")) {
				b, ok = x, true
			}
		}
	}
	return b, ok
}

// bindings lists the path and remote rules of the projects sc sees, with
// every area's, or (readable) only the areas sc reads.
func (r *Registry) bindings(sc authz.Scope, readable bool) []Binding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var bs []Binding
	for id, e := range r.byID {
		if !sc.SeesProject(id) {
			continue
		}
		bs = append(bs, Binding{Project: id, RepoRules: e.file.Repos})
		for _, a := range e.file.Areas {
			if !readable || sc.Can(authz.Read, authz.Object{Project: id, Area: a.Name}) {
				bs = append(bs, Binding{Project: id, Area: a.Name, RepoRules: a.RepoRules})
			}
		}
	}
	return bs
}

// MatchPath finds the longest configured path containing cwd, among projects
// sc sees; area rules win over project rules of equal length. n is the
// match's length (-1 for none), to weigh it against a joined project's.
func (r *Registry) MatchPath(sc authz.Scope, cwd string) (id, area string, n int) {
	if cwd == "" {
		return "", "", -1
	}
	b, n, ok := BestPath(cwd, r.bindings(sc, false))
	if !ok {
		return "", "", -1
	}
	if b.Area == "" {
		b.Area = r.soleArea(sc, b.Project)
	}
	if b.Area != "" && !sc.Can(authz.Read, authz.Object{Project: b.Project, Area: b.Area}) {
		return "", "", -1
	}
	return b.Project, b.Area, n
}

// matchRemote finds the project (and area) listing remote; area rules win.
func (r *Registry) matchRemote(sc authz.Scope, remote string) (string, string) {
	b, ok := BestRemote(remote, r.bindings(sc, true))
	if !ok {
		return "", ""
	}
	if b.Area == "" {
		b.Area = r.soleArea(sc, b.Project)
	}
	return b.Project, b.Area
}

// AreaForPath is the area of path inside project id (longest area path match), "" if none.
func (r *Registry) AreaForPath(id, path string) string {
	cwd := ExpandPath(path)
	if cwd == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byID[id]
	if !ok {
		return ""
	}
	best, bestLen := "", -1
	for _, a := range e.file.Areas {
		for _, p := range a.Paths {
			if n, ok := PathMatch(cwd, p); ok && n > bestLen {
				best, bestLen = a.Name, n
			}
		}
	}
	return best
}
