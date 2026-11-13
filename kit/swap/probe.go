package swap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Item kinds.
const (
	ItemProjects = "projects" // the project list itself
	ItemJoined   = "joined"   // a joined team project is listed
	ItemIssues   = "issues"   // bd's read: a project's first issue
	ItemWork     = "work"     // a project's first work item
	ItemAdapter  = "adapter"  // a tracker's adapter answers (project/prefix)
	ItemAgent    = "agent"    // a task agent is available (zm-g3ua)
)

// ProbeTimeout bounds each read of a probe (a replica's read may wait for
// its tailnet node for up to 15 s after a start).
var ProbeTimeout = 20 * time.Second

// Probing is one probe in progress: what answered so far and the projects
// seen. Probe parts add to it.
type Probing struct {
	ctx    context.Context
	out    api.ServeCheck
	slugs  []string          // the project list's slugs
	slugOf map[string]string // project ULID -> slug
	seen   map[string]bool
}

// Call runs fn with ProbeTimeout.
func (p *Probing) Call(fn func(context.Context) error) error {
	cctx, cancel := context.WithTimeout(p.ctx, ProbeTimeout)
	defer cancel()
	return fn(cctx)
}

// Add records one item: OK unless err.
func (p *Probing) Add(kind, name string, err error) {
	p.out.Items = append(p.out.Items, Item(kind, name, err))
}

// Item is one probe item: OK unless err, else err's code and first line.
func Item(kind, name string, err error) api.ServeItem {
	it := api.ServeItem{Kind: kind, Name: name, OK: err == nil}
	if err != nil {
		it.Code, it.Error = errCode(err), OneLine(err.Error())
	}
	return it
}

// Project records a served project by slug and ULID.
func (p *Probing) Project(slug, id string) {
	if id != "" && slug != "" {
		p.slugOf[id] = slug
	}
	for _, s := range []string{slug, id} {
		if s != "" && !p.seen[s] {
			p.seen[s] = true
			p.out.Projects = append(p.out.Projects, s)
		}
	}
}

// Name is project id's slug when a part has seen it, else id.
func (p *Probing) Name(id string) string {
	if s := p.slugOf[id]; s != "" {
		return s
	}
	return id
}

// Slugs are the project list's slugs plus every joined project the list
// dropped (still read).
func (p *Probing) Slugs() []string {
	out := slices.Clone(p.slugs)
	for _, it := range p.out.Items {
		if it.Kind == ItemJoined && it.Name != "*" && !contains(out, it.Name) {
			out = append(out, it.Name)
		}
	}
	return out
}

// ProbePart asks c's daemon one thing and adds what answered to p.
type ProbePart func(c *client.Client, p *Probing)

// Part is a named probe part: a daemon's wiring adds its modules' parts
// (core.Deps.AddProbe), and a command that probes a daemon passes the same
// list to Probe.
type Part struct {
	Name string
	Run  ProbePart
}

// baseParts run first in every probe: health, the project list and the
// joined projects.
var baseParts = []Part{{"health", probeHealth}, {"projects", probeProjects}, {"joined", probeJoined}}

// registered are the parts RegisterProbe added, after the base.
var registered []Part

// Taken reports whether name is a part every probe in this process runs
// already: a base part or one RegisterProbe added. A wiring's part may not
// take it (core.Deps.AddProbe panics).
func Taken(name string) bool {
	return slices.ContainsFunc(slices.Concat(baseParts, registered), func(x Part) bool { return x.Name == name })
}

// RegisterProbe adds a part to every Probe in this process, after the base
// (a wiring's init). A name registered twice panics.
//
// Deprecated: a wiring adds its parts with core.Deps.AddProbe and passes
// them to Probe; this stays until zm's wiring does.
func RegisterProbe(name string, part ProbePart) {
	if Taken(name) {
		panic("swap: probe " + name + " registered twice")
	}
	registered = append(registered, Part{name, part})
}

// Probe asks c's daemon what bd and zm ask: the base parts, then those
// RegisterProbe added, then parts in order. It lists what answered. A name
// that two parts share panics: a programming error that would otherwise
// leave one of them unchecked.
func Probe(ctx context.Context, c *client.Client, parts ...Part) api.ServeCheck {
	all := slices.Concat(baseParts, registered, parts)
	ran := map[string]bool{}
	for _, part := range all {
		if ran[part.Name] {
			panic("swap: two probe parts are named " + part.Name)
		}
		ran[part.Name] = true
	}
	p := &Probing{ctx: ctx, out: api.ServeCheck{Checked: time.Now().UTC(), Projects: []string{}, Items: []api.ServeItem{}},
		slugOf: map[string]string{}, seen: map[string]bool{}}
	for _, part := range all {
		part.Run(c, p)
	}
	sort.Slice(p.out.Items, func(i, j int) bool { return p.out.Items[i].Key() < p.out.Items[j].Key() })
	return p.out
}

func probeHealth(c *client.Client, p *Probing) {
	_ = p.Call(func(ctx context.Context) error {
		h, err := c.Health(ctx)
		p.out.Version, p.out.Commit = h.Version, h.Commit
		return err
	})
}

func probeProjects(c *client.Client, p *Probing) {
	p.Add(ItemProjects, "*", p.Call(func(ctx context.Context) error {
		ps, err := c.Projects(ctx)
		for _, x := range ps {
			p.slugs = append(p.slugs, x.Slug)
			p.Project(x.Slug, x.ID)
		}
		return err
	}))
}

func probeJoined(c *client.Client, p *Probing) {
	_ = p.Call(func(ctx context.Context) error {
		rs, err := c.Remotes(ctx)
		if err != nil {
			p.Add(ItemJoined, "*", err)
			return err
		}
		for _, r := range rs {
			if r.State != "" && r.State != "ok" {
				continue
			}
			p.Project(r.Project.Slug, r.Project.ID)
			p.Add(ItemJoined, r.Project.Slug, nil)
		}
		return nil
	})
}

// Regressions are the items before served that after does not: what a
// swap broke. A local project after no longer lists left on purpose, and
// is not expected. A project joined before is always expected: a build
// that cannot serve one drops it from its lists too (2026-11-01: `no
// project "acme"`), so its absence is the regression, not a leave.
func Regressions(before, after api.ServeCheck) []string {
	local, remote := SplitRegressions(before, after)
	return append(local, remote...)
}

// SplitRegressions is Regressions in two. remote are those that can be
// the tailnet still coming up: an item of a project joined before that is
// missing now or fails unavailable (its reads go to the authority, and a
// daemon's tsnet node takes 30-60 s after a start, failing "authority ...
// unreachable" until then: 2026-11-03). local is everything else, which
// no wait fixes (2026-11-01's `no project "acme"` is a bad_request).
func SplitRegressions(before, after api.ServeCheck) (local, remote []string) {
	now := map[string]api.ServeItem{}
	for _, it := range after.Items {
		now[it.Key()] = it
	}
	still := set(after.Projects)
	joined := map[string]bool{}
	for _, it := range before.Items {
		if it.Kind == ItemJoined && it.OK && it.Name != "*" {
			joined[it.Name] = true
		}
	}
	for _, it := range before.Items {
		if !it.OK {
			continue
		}
		p := itemProject(it)
		if p != "*" && !still[p] && !joined[p] {
			continue
		}
		got, ok := now[it.Key()]
		var why string
		switch {
		case !ok:
			why = fmt.Sprintf("%s %s: served before, missing now", it.Kind, it.Name)
		case !got.OK:
			why = fmt.Sprintf("%s %s: served before, now %s", it.Kind, it.Name, got.Error)
		default:
			continue
		}
		if joined[p] && (!ok || got.Code == api.CodeUnavailable) {
			remote = append(remote, why)
		} else {
			local = append(local, why)
		}
	}
	return local, remote
}

// Failures lists check's items that did not answer.
func Failures(check api.ServeCheck) []string {
	var out []string
	for _, it := range check.Items {
		if !it.OK {
			out = append(out, fmt.Sprintf("%s %s: %s", it.Kind, it.Name, it.Error))
		}
	}
	return out
}

func itemProject(it api.ServeItem) string {
	if it.Kind == ItemAdapter {
		p, _, _ := strings.Cut(it.Name, "/")
		return p
	}
	if it.Kind == ItemAgent {
		return "*" // machine-wide, in no project
	}
	return it.Name
}

func errCode(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return api.CodeUnavailable
}

// OneLine is s's first line, at most 200 bytes.
func OneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
