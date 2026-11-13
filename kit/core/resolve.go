package core

import (
	"context"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/project"
)

// Resolve answers "which project would a write from this request go to, and
// why" (SPEC "Which project"; GET /v0/projects/resolve).
func (d *Deps) Resolve(ctx context.Context) api.Resolution {
	sub := SubjectOf(ctx)
	if sub == nil {
		return api.Resolution{Steps: []api.ResolveStep{}}
	}
	in := ResolveInputOf(ctx)
	if sub.Remote {
		in.Cwd = "" // never resolve from a path a network caller names
	}
	if sub.Session != "" {
		if project, area, ok := d.SessionPlace(ctx, sub.Session); ok {
			in.SessionProject, in.SessionArea = project, area
		}
	}
	in.Default = d.Config.File.DefaultProject
	return d.Projects.Resolve(sub.Scope, in)
}

// Target is where a write goes.
type Target struct {
	Project api.ProjectRef
	Area    string
	Rule    string
}

// WriteTarget resolves the project for a write and checks that the caller
// may do a there. area, when not empty, overrides the resolved area (it must
// exist and be visible). No project is a bad_request that tells the caller
// how to pick one; it is never filed into a default silently.
func (d *Deps) WriteTarget(ctx context.Context, area string, a authz.Action) (Target, error) {
	sub := SubjectOf(ctx)
	res := d.Resolve(ctx)
	if res.Project == nil {
		last := ""
		if n := len(res.Steps); n > 0 {
			last = res.Steps[n-1].Rule + ": " + res.Steps[n-1].Result
		}
		if in := ResolveInputOf(ctx); in.Explicit != "" {
			return Target{}, api.NotFound("project", in.Explicit)
		}
		hint := "pass --project <slug>, set ZM_PROJECT, or run `zm project use <slug>` in the repo; `zm project why` explains"
		if IsLocal(ctx) && ResolveInputOf(ctx).Cwd != "" {
			// A socket caller in a directory no project claims: the first
			// step is to make it one (SPEC 17.1).
			hint = "run `zm init` here to make this directory a project; or " + hint
		}
		return Target{}, api.Errorf(api.CodeBadRequest, "no project for this call (%s)", last).WithHint(hint)
	}
	t := Target{Project: *res.Project, Area: res.Area, Rule: res.Rule}
	if area != "" {
		if !d.Projects.HasArea(t.Project.ID, area) || !sub.Can(authz.Read, authz.Object{Project: t.Project.ID, Area: area}) {
			return Target{}, api.NotFound("area", area)
		}
		t.Area = area
	}
	if err := sub.Check(a, authz.Object{Project: t.Project.ID, Area: t.Area}, "project", t.Project.Slug); err != nil {
		return Target{}, err
	}
	return t, nil
}

// TrackerTarget is the project a tracker command acts on: the resolved write
// target, admin (the routes are local, so the machine's owner). The tracker
// and homes modules both use it, so the check lives in one place.
func (d *Deps) TrackerTarget(ctx context.Context) (string, string, project.File, error) {
	t, err := d.WriteTarget(ctx, "", authz.Admin)
	if err != nil {
		return "", "", project.File{}, err
	}
	f, ok := d.Projects.File(t.Project.ID)
	if !ok {
		return "", "", project.File{}, api.NotFound("project", t.Project.Slug)
	}
	return t.Project.ID, t.Project.Slug, f, nil
}

// ReadScope is the caller's scope for a list/read call: the union of every
// project it sees, or just one when the request names one (?project=, else a
// Zm-Project header that came from --project; $ZM_PROJECT does not narrow
// lists). Lists never need a resolved project.
func (d *Deps) ReadScope(ctx context.Context, explicit string) (authz.Scope, error) {
	sub := SubjectOf(ctx)
	if in := ResolveInputOf(ctx); explicit == "" && in.ExplicitFrom == project.RuleFlag {
		explicit = in.Explicit
	}
	if explicit == "" {
		return sub.Scope, nil
	}
	if explicit == api.ProjectNone {
		if !sub.Scope.Machine() {
			return authz.Empty(), api.NotFound("project", explicit)
		}
		return sub.Scope.Only(api.ProjectNone), nil
	}
	ref, _, _ := cutArea(explicit)
	p, err := d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return authz.Empty(), err
	}
	return sub.Scope.Only(p.ID), nil
}

func cutArea(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
