package grants

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// joined is when the tests' identities joined the pool; made is when their
// grants were made, after.
var (
	joined = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	made   = "2026-02-01T00:00:00.000000000Z"
)

// whoFor is Check's who from a request's subject alone (its scope; no
// member machine link): what decide sees.
func whoFor(req Request) who {
	if req.Job != nil {
		return who{job: req.Job}
	}
	sub := req.Subject
	if sub == nil {
		return who{}
	}
	o := authz.Object{Project: req.Project, Area: req.Area}
	return who{identityID: sub.Identity.ID, rel: sub.Scope.Relation(o), operate: sub.Can(authz.Operate, o), since: map[string]time.Time{"": joined, "team": joined}}
}

func run(rows []computeapi.SecretGrant, req Request, iss Issuer) (map[string]string, string) {
	return decide(rows, req, whoFor(req), iss)
}

const proj = "01PROJ"

func person(id string, rel authz.Relation, area string) *authz.Subject {
	obj := authz.ProjectObject(proj)
	if area != "" {
		obj = authz.AreaObject(proj, area)
	}
	return &authz.Subject{Identity: api.Identity{ID: id, Name: id}, Address: "human:" + id,
		Scope: authz.FromTuples([]authz.Tuple{{Subject: id, Relation: rel, Object: obj}}, nil)}
}

func valueReq(sub *authz.Subject, sizes map[string]int) Request {
	return Request{Project: proj, Area: "team", Subject: sub, Kind: computeapi.SecretKindValue, Scope: computeapi.SecretScopeExec,
		Target: "box", HostClass: api.MachineTrusted, Sizes: sizes}
}

func grant(id string, mut func(*computeapi.SecretGrant)) computeapi.SecretGrant {
	g := computeapi.SecretGrant{ID: id, Area: "team", To: computeapi.GrantTo{IdentityID: "ANA"}, Kind: computeapi.SecretKindValue,
		Name: "API_KEY", Ceiling: json.RawMessage(`{"max_bytes":100}`), HostClass: api.MachineTrusted, Scopes: []string{"exec"}, Created: made}
	if mut != nil {
		mut(&g)
	}
	return g
}

// The default alone is today's behaviour: a member (or owner) may send any
// names up to 64 KiB on exec to a trusted box; a guest may not; a public
// host or an unknown class gets nothing.
func TestDecideDefault(t *testing.T) {
	rows := []computeapi.SecretGrant{Defaults()[0]}
	ana := person("ANA", authz.Member, "team")
	if g, why := run(rows, valueReq(ana, map[string]int{"API_KEY": 10, "OTHER": 65000}), nil); why != "" || g["API_KEY"] != "default" || g["OTHER"] != "default" {
		t.Fatalf("member under the default: %v %q", g, why)
	}
	if _, why := run(rows, valueReq(person("OWN", authz.Owner, ""), map[string]int{"A": 1}), nil); why != "" {
		t.Fatalf("owner under the default: %q", why)
	}
	for name, req := range map[string]Request{
		"a guest":         valueReq(person("GUS", authz.Guest, "team"), map[string]int{"A": 1}),
		"another area":    valueReq(person("BOB", authz.Member, "other"), map[string]int{"A": 1}),
		"no subject":      valueReq(nil, map[string]int{"A": 1}),
		"over 64 KiB":     valueReq(ana, map[string]int{"A": valueMax + 1}),
		"scope box":       func() Request { r := valueReq(ana, map[string]int{"A": 1}); r.Scope = "box"; return r }(),
		"a public host":   func() Request { r := valueReq(ana, map[string]int{"A": 1}); r.HostClass = api.MachinePublic; return r }(),
		"no host class":   func() Request { r := valueReq(ana, map[string]int{"A": 1}); r.HostClass = ""; return r }(),
		"a made-up class": func() Request { r := valueReq(ana, map[string]int{"A": 1}); r.HostClass = "Trusted"; return r }(),
		"a job, not a person": func() Request {
			r := valueReq(nil, map[string]int{"A": 1})
			r.Job = &jobrule.Job{Repo: "acme/app", HeadRepo: "acme/app", RepoID: 9, HeadRepoID: 9, Workflow: ".github/workflows/x.yml", Ref: "refs/heads/main", DefaultBranch: "main", Event: "push", RunID: 1}
			return r
		}(),
	} {
		if g, why := run(rows, req, nil); why == "" {
			t.Errorf("%s: allowed %v", name, g)
		}
	}
	off := Defaults()[0]
	off.Off = true
	if _, why := run([]computeapi.SecretGrant{off}, valueReq(ana, map[string]int{"A": 1}), nil); why == "" {
		t.Error("a default that is off allowed a secret")
	}
}

// A public host gets nothing even from a row that names it (rows say
// trusted at add; one edited by hand still matches nothing).
func TestDecidePublicHostFailsClosed(t *testing.T) {
	ana := person("ANA", authz.Member, "team")
	g := grant("g1", func(g *computeapi.SecretGrant) { g.HostClass = api.MachinePublic })
	req := valueReq(ana, map[string]int{"API_KEY": 1})
	if _, why := run([]computeapi.SecretGrant{g, Defaults()[0]}, func() Request { r := req; r.HostClass = api.MachinePublic; return r }(), nil); why != whyPublic {
		t.Fatalf("public host: %q", why)
	}
	if _, why := run([]computeapi.SecretGrant{g}, req, nil); why == "" {
		t.Fatal("a row saying public matched a trusted host's request")
	}
}

func TestDecideRows(t *testing.T) {
	ana, bob := person("ANA", authz.Member, "team"), person("BOB", authz.Member, "team")
	rows := []computeapi.SecretGrant{
		grant("g1", nil),
		grant("g2", func(g *computeapi.SecretGrant) {
			g.To, g.Name, g.Targets = computeapi.GrantTo{Role: "member"}, "*", []string{"big@lender"}
		}),
		grant("g3", func(g *computeapi.SecretGrant) { g.To, g.Name, g.Area = computeapi.GrantTo{Role: "owner"}, "ROOT", "" }),
		grant("bad", func(g *computeapi.SecretGrant) { g.Name, g.Ceiling = "BROKEN", json.RawMessage(`{"max_bytes":"x"}`) }),
		grant("two", func(g *computeapi.SecretGrant) { g.Name, g.To.Role = "TWO", "member" }),
	}
	at := func(sub *authz.Subject, target string, sizes map[string]int) Request {
		r := valueReq(sub, sizes)
		r.Target = target
		return r
	}
	ok := []struct {
		name string
		req  Request
		want map[string]string
	}{
		{"ana's own name", at(ana, "box", map[string]int{"API_KEY": 100}), map[string]string{"API_KEY": "g1"}},
		{"any name on the named target", at(bob, "big@lender", map[string]int{"X": 5, "Y": 6}), map[string]string{"X": "g2", "Y": "g2"}},
		{"names from two grants", at(ana, "big@lender", map[string]int{"API_KEY": 1, "Z": 1}), map[string]string{"API_KEY": "g1", "Z": "g2"}},
		{"an owner's project-wide row", at(person("OWN", authz.Owner, ""), "box", map[string]int{"ROOT": 1}), map[string]string{"ROOT": "g3"}},
	}
	for _, c := range ok {
		g, why := run(rows, c.req, nil)
		if why != "" || len(g) != len(c.want) {
			t.Errorf("%s: %v %q", c.name, g, why)
			continue
		}
		for k, v := range c.want {
			if g[k] != v {
				t.Errorf("%s: %s by %s, want %s", c.name, k, g[k], v)
			}
		}
	}
	for name, req := range map[string]Request{
		"bob holds no identity grant": at(bob, "box", map[string]int{"API_KEY": 1}),
		"over the ceiling":            at(ana, "box", map[string]int{"API_KEY": 101}),
		"another target":              at(bob, "box", map[string]int{"X": 1}),
		"one name uncovered":          at(ana, "box", map[string]int{"API_KEY": 1, "OTHER": 1}),
		"a member on an owner's row":  at(ana, "box", map[string]int{"ROOT": 1}),
		"a malformed ceiling":         at(ana, "box", map[string]int{"BROKEN": 1}),
		"a row with two subjects":     at(ana, "box", map[string]int{"TWO": 1}),
		"a row of another area":       func() Request { r := at(ana, "box", map[string]int{"API_KEY": 1}); r.Area = "else"; return r }(),
		"a different kind than the row": func() Request {
			r := at(ana, "box", nil)
			r.Kind, r.Name = computeapi.SecretKindGitHubToken, "API_KEY"
			return r
		}(),
	} {
		if g, why := run(rows, req, nil); why == "" {
			t.Errorf("%s: allowed %v", name, g)
		}
	}
}

// fakeIssuer is a minted kind for the seam's test: params within when
// equal to the ceiling.
type fakeIssuer struct{}

func (fakeIssuer) Kind() string { return computeapi.SecretKindGitHubToken }
func (fakeIssuer) Ceiling(raw json.RawMessage) (json.RawMessage, error) {
	return raw, nil
}
func (fakeIssuer) Within(c, p json.RawMessage) error {
	if string(c) != string(p) {
		return errors.New("wider")
	}
	return nil
}

var _ Issuer = fakeIssuer{}

// A job grant matches only the job GitHub named, never a person; a minted
// kind asks its issuer whether the params sit inside the ceiling.
func TestDecideJobAndIssuer(t *testing.T) {
	job := grant("j1", func(g *computeapi.SecretGrant) {
		g.Kind, g.Name, g.Ceiling = computeapi.SecretKindGitHubToken, "GH_TOKEN", json.RawMessage(`{"repo":"acme/app"}`)
		g.To = computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/lander.yml", Ref: "refs/heads/main", Events: []string{"push"}}}
	})
	req := func(j *jobrule.Job, params string) Request {
		return Request{Project: proj, Area: "team", Job: j, Kind: computeapi.SecretKindGitHubToken, Scope: "exec", Target: "box",
			HostClass: api.MachineTrusted, Name: "GH_TOKEN", Params: json.RawMessage(params)}
	}
	right := &jobrule.Job{Repo: "ACME/app", HeadRepo: "ACME/app", RepoID: 9, HeadRepoID: 9, Workflow: ".github/workflows/lander.yml", Ref: "refs/heads/main",
		DefaultBranch: "main", Event: "push", RunID: 7}
	iss := fakeIssuer{}
	if _, why := run([]computeapi.SecretGrant{job}, req(right, `{"repo":"acme/app"}`), iss); why != "" {
		t.Fatalf("the right job: %q", why)
	}
	with := func(mut func(j *jobrule.Job)) *jobrule.Job { j := *right; mut(&j); return &j }
	for name, r := range map[string]Request{
		"another ref":      req(with(func(j *jobrule.Job) { j.Ref, j.DefaultBranch = "refs/heads/feature", "feature" }), `{"repo":"acme/app"}`),
		"not the default":  req(with(func(j *jobrule.Job) { j.DefaultBranch = "trunk" }), `{"repo":"acme/app"}`),
		"a fork's run":     req(with(func(j *jobrule.Job) { j.HeadRepo = "stranger/app" }), `{"repo":"acme/app"}`),
		"another workflow": req(with(func(j *jobrule.Job) { j.Workflow = ".github/workflows/pr.yml" }), `{"repo":"acme/app"}`),
		"another event":    req(with(func(j *jobrule.Job) { j.Event = "pull_request" }), `{"repo":"acme/app"}`),
		"a dispatch":       req(with(func(j *jobrule.Job) { j.Event = "workflow_dispatch" }), `{"repo":"acme/app"}`),
		"wider params":     req(right, `{"repo":"acme/other"}`),
		"a person": func() Request {
			r := req(nil, `{"repo":"acme/app"}`)
			r.Subject = person("ANA", authz.Owner, "")
			return r
		}(),
	} {
		if _, why := run([]computeapi.SecretGrant{job}, r, iss); why == "" {
			t.Errorf("%s: allowed", name)
		}
	}
	if _, why := run([]computeapi.SecretGrant{job}, req(right, `{"repo":"acme/app"}`), nil); why == "" {
		t.Error("allowed with no issuer")
	}
}

// An identity grant needs the identity's current membership to predate it
// (one that left and came back finds it stale) and the caller's own scope,
// cap included, to operate in the area. A role grant is judged by the role
// Check reads (a member machine's link by its identity's grants). A corrupt
// row matches nothing.
func TestDecideWho(t *testing.T) {
	ana := person("ANA", authz.Member, "team")
	req := valueReq(ana, map[string]int{"API_KEY": 1})
	rows := []computeapi.SecretGrant{grant("g1", nil)}
	w := whoFor(req)
	if _, why := decide(rows, req, w, nil); why != "" {
		t.Fatalf("ana: %q", why)
	}
	for name, mut := range map[string]func(*who){
		"left and came back":    func(w *who) { w.since = map[string]time.Time{"team": time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)} },
		"left the grant's area": func(w *who) { w.since = map[string]time.Time{"": joined} },
		"not in the pool":       func(w *who) { w.since = nil },
		"a capped token":        func(w *who) { w.operate = false },
		"another identity":      func(w *who) { w.identityID = "BOB" },
	} {
		x := w
		mut(&x)
		if _, why := decide(rows, req, x, nil); why == "" {
			t.Errorf("%s: allowed", name)
		}
	}
	owners := []computeapi.SecretGrant{grant("o1", func(g *computeapi.SecretGrant) { g.To = computeapi.GrantTo{Role: "owner"} })}
	if _, why := decide(owners, req, w, nil); why == "" {
		t.Error("a member matched an owner's grant")
	}
	x := w
	x.rel = authz.Owner // an owner's member machine: its link is capped at member, its grants are not
	if _, why := decide(owners, req, x, nil); why != "" {
		t.Errorf("an owner through a member machine: %q", why)
	}
	bad := grant("c1", func(g *computeapi.SecretGrant) { g.Corrupt = true })
	if _, why := decide([]computeapi.SecretGrant{bad}, req, w, nil); why == "" {
		t.Error("a corrupt row matched")
	}
}

// A jit_config: the default covers a member on a trusted host's box scope,
// as before grants; off, nothing does; a github-jit grant's ceiling is {}.
func TestDecideJIT(t *testing.T) {
	jit := func(sub *authz.Subject) Request {
		return Request{Project: proj, Area: "team", Subject: sub, Kind: computeapi.SecretKindGitHubJIT, Scope: computeapi.SecretScopeBox,
			Target: "box", HostClass: api.MachineTrusted, Name: computeapi.SecretNameJIT}
	}
	if g, why := run(Defaults(), jit(person("RUN", authz.Member, "team")), nil); why != "" || g[computeapi.SecretNameJIT] != computeapi.GrantDefaultJITID {
		t.Fatalf("the broker under the default: %v %q", g, why)
	}
	if _, why := run(Defaults(), jit(person("GUS", authz.Guest, "team")), nil); why == "" {
		t.Error("a guest's jit_config allowed")
	}
	off := Defaults()
	for i := range off {
		off[i].Off = true
	}
	if _, why := run(off, jit(person("RUN", authz.Member, "team")), nil); why == "" {
		t.Error("a jit_config allowed with the defaults off")
	}
	wide := Defaults()[1]
	wide.ID, wide.Ceiling = "w", json.RawMessage(`{"repos":["*"]}`)
	if _, why := run([]computeapi.SecretGrant{wide}, jit(person("RUN", authz.Member, "team")), nil); why == "" {
		t.Error("a github-jit row with a ceiling matched")
	}
}
