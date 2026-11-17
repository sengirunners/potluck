package issuers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
)

// A GitHub job's box is in the runner broker's pool project (orchid here),
// while the work the job does is another project's (oncall). A job-only
// rule (one naming workflows) may name that other project: the job's token
// then acts there, as the issuer's identity, and only on the broker's box
// for that job (JobBox). These are the contract of docs/daemon.md "What a
// box can get (potluck-token)".

const landerWorkflow = ".github/workflows/lander.yml"

// oncallRule is a job-only rule for project oncall: mainJob's repository,
// workflow and event.
func oncallRule(area string) config.SecretsCeiling {
	return config.SecretsCeiling{Project: "oncall", Area: area, Role: "member", Repo: "acme/app", Workflows: []string{landerWorkflow}, Events: []string{"push"}}
}

// brokerBoxes is a switchable JobBox: whether every box is the broker's box
// for its job, an error to answer, and the requests it was asked about.
type brokerBoxes struct {
	mu    sync.Mutex
	yes   bool
	err   error
	asked []Request
}

func (b *brokerBoxes) set(yes bool, err error) { b.mu.Lock(); b.yes, b.err = yes, err; b.mu.Unlock() }

func (b *brokerBoxes) JobBox(_ context.Context, req Request) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, req)
	return b.yes, b.err
}

// jobProjectEnv is a potluck env whose issuer acts as lander, with project
// oncall (area ops) beside the pool project orchid. lander holds member on
// oncall only, so a token on orchid would be refused: a token that is
// issued acts on oncall or nowhere.
type jobProjectEnv struct {
	*potluckEnv
	oncall string // oncall's ULID
	boxes  *brokerBoxes
}

func newJobProjectEnv(t *testing.T, rules ...config.SecretsCeiling) *jobProjectEnv {
	t.Helper()
	ctx := context.Background()
	p := newPotluckEnvWith(t, config.SecretsIssuer{Kind: KindPotluck, As: "lander", Ceiling: rules})
	reg := p.d.Projects
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "oncall"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.AddArea(ctx, reg.Local(""), "oncall", api.Area{Name: "ops"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "lander", Role: "member", Project: "oncall"}); err != nil {
		t.Fatal(err)
	}
	ref, ok := reg.Lookup("oncall")
	if !ok {
		t.Fatal("no project oncall")
	}
	j := &jobProjectEnv{potluckEnv: p, oncall: ref.ID, boxes: &brokerBoxes{yes: true}}
	p.opt.JobBox = j.boxes.JobBox
	p.iss = p.load()
	return j
}

// minted is how many tokens the registry holds.
func (j *jobProjectEnv) minted(t *testing.T) int {
	t.Helper()
	toks, err := j.d.Projects.Tokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(toks)
}

// ledgerScopes is each ledger row's event, box project and token project.
func (j *jobProjectEnv) ledgerScopes(t *testing.T) [][4]string {
	t.Helper()
	rs, err := j.db.Query(`SELECT event, project, token_project, token_area FROM issuers_ledger ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out [][4]string
	for rs.Next() {
		var r [4]string
		if err := rs.Scan(&r[0], &r[1], &r[2], &r[3]); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// A job under a job-only rule of project oncall gets a token that acts on
// oncall (not on the box's pool project) as the issuer's identity; the
// ledger and the secret.* events name both the box's project and the
// token's.
func TestPotluckTokenJobActsInTheRuleProject(t *testing.T) {
	ctx := context.Background()
	j := newJobProjectEnv(t, oncallRule(""))
	req := j.req(trustedBox)
	out := j.mustIssue(req)
	if out.Permissions["role"] != "member" {
		t.Fatalf("issued %+v", out)
	}
	if len(j.boxes.asked) == 0 || j.boxes.asked[0].Box != req.Box || j.boxes.asked[0].Subject.Job.RunID != mainJob().RunID {
		t.Fatalf("JobBox asked about %+v, want the job's box", j.boxes.asked)
	}
	secret := j.bx.get(req.Box, "POTLUCK_TOKEN")
	sub, err := boxAuth(j.d.Projects, secret)
	if err != nil {
		t.Fatalf("the token does not authenticate: %v", err)
	}
	if sub.Identity.Name != "lander" || sub.Cap == nil || sub.Cap.Project != j.oncall || sub.Cap.Area != "" || sub.Cap.Role != authz.Member {
		t.Fatalf("the token acts as %s with cap %+v, want lander, member on oncall %s", sub.Identity.Name, sub.Cap, j.oncall)
	}
	if err := sub.Check(authz.Write, authz.Object{Project: j.oncall}, "project", "oncall"); err != nil {
		t.Errorf("the token cannot write on oncall: %v", err)
	}
	if err := sub.Check(authz.Read, authz.Object{Project: j.proj}, "project", "orchid"); err == nil {
		t.Error("the token reads the box's pool project")
	}

	j.iss.End(ctx, req.Box, EndReleased)
	if j.works(secret) {
		t.Fatal("the token works after its box ended")
	}
	scopes := j.ledgerScopes(t)
	if len(scopes) != 2 || scopes[0] != [4]string{EventIssued, j.proj, j.oncall, ""} || scopes[1] != [4]string{EventRevoked, j.proj, j.oncall, ""} {
		t.Fatalf("ledger (event, box project, token project, token area) %v, want issued and revoked on box project %s, token project %s",
			scopes, j.proj, j.oncall)
	}
	var revoked bool
	j.bus.mu.Lock()
	for _, in := range j.bus.ins {
		if in.Type != EventSecretRevoked {
			continue
		}
		revoked = true
		d, ok := in.Data.(SecretEvent)
		if !ok {
			b, _ := json.Marshal(in.Data)
			_ = json.Unmarshal(b, &d)
		}
		if in.Project != j.proj || d.TokenProject != j.oncall || d.TokenArea != "" {
			t.Errorf("secret.revoked on project %s naming token project %q, want box project %s and token project %s", in.Project, d.TokenProject, j.proj, j.oncall)
		}
	}
	j.bus.mu.Unlock()
	if !revoked {
		t.Error("no secret.revoked published")
	}
}

// A rule naming an area of its project caps the token to that area.
func TestPotluckTokenJobRuleArea(t *testing.T) {
	j := newJobProjectEnv(t, oncallRule("ops"))
	req := j.req(trustedBox)
	j.mustIssue(req)
	sub, err := boxAuth(j.d.Projects, j.bx.get(req.Box, "POTLUCK_TOKEN"))
	if err != nil || sub.Cap == nil || sub.Cap.Project != j.oncall || sub.Cap.Area != "ops" {
		t.Fatalf("cap %+v %v, want oncall area ops", sub.Cap, err)
	}
	if s := j.ledgerScopes(t); len(s) != 1 || s[0] != [4]string{EventIssued, j.proj, j.oncall, "ops"} {
		t.Fatalf("ledger %v", s)
	}
}

// Refused before anything is minted, each for its own reason.
func TestPotluckTokenJobProjectRefusals(t *testing.T) {
	job := func(f func(*Job)) func(*jobProjectEnv) Request {
		return func(j *jobProjectEnv) Request {
			r := j.req(trustedBox)
			f(r.Subject.Job)
			return r
		}
	}
	plain := func(j *jobProjectEnv) Request { return j.req(trustedBox) }
	cases := []struct {
		name  string
		rules []config.SecretsCeiling
		setup func(*jobProjectEnv)
		req   func(*jobProjectEnv) Request
		want  string // in the refusal
	}{
		{name: "a person's box", rules: []config.SecretsCeiling{oncallRule("")}, req: func(j *jobProjectEnv) Request {
			lander, err := j.d.Projects.Identity(context.Background(), "lander")
			if err != nil {
				j.t.Fatal(err)
			}
			r := j.req(trustedBox)
			r.Subject = Subject{IdentityID: lander.ID}
			return r
		}, want: "outside the issuer's ceiling"},
		{name: "a box the broker does not say is the job's", rules: []config.SecretsCeiling{oncallRule("")},
			setup: func(j *jobProjectEnv) { j.boxes.set(false, nil) }, req: plain, want: "runner broker's box"},
		{name: "no broker on this machine", rules: []config.SecretsCeiling{oncallRule("")},
			setup: func(j *jobProjectEnv) { j.opt.JobBox = nil; j.iss = j.load() }, req: plain, want: "runner broker's box"},
		{name: "another workflow", rules: []config.SecretsCeiling{oncallRule("")},
			req: job(func(jb *Job) { jb.Workflow = ".github/workflows/other.yml" }), want: "outside the issuer's ceiling"},
		{name: "an event the rule does not list", rules: []config.SecretsCeiling{oncallRule("")},
			req: job(func(jb *Job) { jb.Event = "schedule" }), want: "outside the issuer's ceiling"},
		{name: "another ref", rules: []config.SecretsCeiling{oncallRule("")},
			req: job(func(jb *Job) { jb.Ref = "refs/heads/dev" }), want: "default branch"},
		{name: "a fork's run", rules: []config.SecretsCeiling{oncallRule("")},
			req: job(func(jb *Job) { jb.HeadRepo, jb.HeadRepoID = "stranger/app", 77 }), want: "head repository"},
		{name: "another repository's run", rules: []config.SecretsCeiling{oncallRule("")},
			req: job(func(jb *Job) { jb.HeadRepo = "acme/web" }), want: "head repository"},
		{name: "a job of a repository the rule does not name", rules: []config.SecretsCeiling{oncallRule("")},
			req: job(func(jb *Job) { jb.Repo, jb.HeadRepo = "acme/web", "acme/web" }), want: "outside the issuer's ceiling"},
		{name: "a rule of another project naming no repository", rules: []config.SecretsCeiling{{Project: "oncall", Role: "member", Workflows: []string{landerWorkflow}, Events: []string{"push"}}},
			req: plain, want: "name it with repo"},
		{name: "the identity holds no role on the rule's project", rules: []config.SecretsCeiling{oncallRule("")},
			setup: func(j *jobProjectEnv) {
				if _, err := j.d.Projects.Revoke(context.Background(), j.d.Projects.Local(""), api.GrantRequest{Identity: "lander", Role: "member", Project: "oncall"}); err != nil {
					j.t.Fatal(err)
				}
			}, req: plain, want: "does not hold member on project oncall"},
		{name: "a rule project that does not exist", rules: []config.SecretsCeiling{{Project: "ghost", Role: "member", Repo: "acme/app", Workflows: []string{landerWorkflow}}},
			req: plain, want: "outside the issuer's ceiling"},
		{name: "an area not of the rule's project", rules: []config.SecretsCeiling{oncallRule("nope")},
			req: plain, want: "area nope is not an area of project oncall"},
		{name: "a rule of another project without workflows", rules: []config.SecretsCeiling{{Project: "oncall", Role: "member"}},
			req: plain, want: "outside the issuer's ceiling"},
		{name: "rules giving two projects", rules: []config.SecretsCeiling{oncallRule(""), {Project: "orchid", Role: "member", Repo: "acme/app", Workflows: []string{landerWorkflow}}},
			setup: func(j *jobProjectEnv) {
				if _, err := j.d.Projects.Grant(context.Background(), j.d.Projects.Local(""), api.GrantRequest{Identity: "lander", Role: "member", Project: "orchid"}); err != nil {
					j.t.Fatal(err)
				}
			}, req: plain, want: "more than one project"},
		{name: "a role above the rule's", rules: []config.SecretsCeiling{{Project: "oncall", Role: "guest", Repo: "acme/app", Workflows: []string{landerWorkflow}}},
			req: plain, want: "outside the issuer's ceiling"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := newJobProjectEnv(t, c.rules...)
			if c.setup != nil {
				c.setup(j)
			}
			_, err := j.issue(c.req(j))
			if codeOf(err) != api.CodeForbidden || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want forbidden naming %q", err, c.want)
			}
			if n := j.minted(t); n != 0 {
				t.Fatalf("minted %d tokens for a refused request", n)
			}
		})
	}
}

// The broker not answering is not a refusal: nothing is minted, and the
// caller may try again.
func TestPotluckTokenJobBoxUnanswered(t *testing.T) {
	j := newJobProjectEnv(t, oncallRule(""))
	j.boxes.set(true, errors.New("registry busy"))
	if _, err := j.issue(j.req(trustedBox)); codeOf(err) != api.CodeUnavailable {
		t.Fatalf("%v, want unavailable", err)
	}
	if n := j.minted(t); n != 0 {
		t.Fatalf("minted %d tokens", n)
	}
	j.boxes.set(true, nil)
	j.mustIssue(j.req(trustedBox))
}

// Each renewal asks the whole check again, the rule's project included: a
// box the broker no longer says is the job's, or an identity that lost its
// role on the rule's project, has its token ended, not renewed.
func TestPotluckTokenJobProjectRenewalRechecks(t *testing.T) {
	for _, name := range []string{"not the job's box", "role lost"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			j := newJobProjectEnv(t, oncallRule(""))
			req := j.req(trustedBox)
			j.mustIssue(req)
			first := j.bx.get(req.Box, "POTLUCK_TOKEN")
			if name == "not the job's box" {
				j.boxes.set(false, nil)
			} else if _, err := j.d.Projects.Revoke(ctx, j.d.Projects.Local(""), api.GrantRequest{Identity: "lander", Role: "member", Project: "oncall"}); err != nil {
				t.Fatal(err)
			}
			j.clk.add(41 * time.Minute)
			j.iss.Tick(ctx)
			if got := j.bx.get(req.Box, "POTLUCK_TOKEN"); got != first || j.works(first) {
				t.Fatalf("renewed %v, the old token still works %v", got != first, j.works(first))
			}
			if !strings.Contains(strings.Join(j.events(), ";"), "refused at renewal") {
				t.Fatalf("ledger %v", j.events())
			}
		})
	}
}

// A renewal that passes keeps the token on the rule's project.
func TestPotluckTokenJobProjectRenewed(t *testing.T) {
	ctx := context.Background()
	j := newJobProjectEnv(t, oncallRule(""))
	req := j.req(trustedBox)
	j.mustIssue(req)
	first := j.bx.get(req.Box, "POTLUCK_TOKEN")
	j.clk.add(41 * time.Minute)
	j.iss.Tick(ctx)
	second := j.bx.get(req.Box, "POTLUCK_TOKEN")
	if second == first {
		t.Fatal("not renewed")
	}
	sub, err := boxAuth(j.d.Projects, second)
	if err != nil || sub.Cap == nil || sub.Cap.Project != j.oncall {
		t.Fatalf("the renewed token's cap %+v %v, want oncall", sub, err)
	}
	s := j.ledgerScopes(t)
	if len(s) != 2 || s[1] != [4]string{EventRenewed, j.proj, j.oncall, ""} {
		t.Fatalf("ledger %v", s)
	}
}

// A rule's repo limits a job-only rule to one repository's jobs: on a rule
// without workflows, or not an owner/name, it is refused when read.
func TestPotluckRuleRepoNeedsWorkflows(t *testing.T) {
	for _, c := range []struct {
		name string
		rule config.SecretsCeiling
		ok   bool
	}{
		{"repo on a job-only rule", config.SecretsCeiling{Project: "oncall", Role: "member", Repo: "acme/app", Workflows: []string{landerWorkflow}}, true},
		{"repo without workflows", config.SecretsCeiling{Project: "oncall", Role: "member", Repo: "acme/app"}, false},
		{"repo that is not owner/name", config.SecretsCeiling{Project: "oncall", Role: "member", Repo: "app", Workflows: []string{landerWorkflow}}, false},
	} {
		if _, err := checkPotluckRule(c.rule); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
