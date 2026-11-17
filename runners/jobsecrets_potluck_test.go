package runners_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// A job's box is in the broker's pool project, while the work it does is
// another project's (oncall): a potluck issuer's job-only rule for oncall
// gives the job a token that acts on oncall, because the broker says the
// box is its box for that job (issuers.JobBox). A box the broker does not
// record as the job's, or one of another area, gets nothing.
func TestJobGrantPotluckTokenActsInTheRuleProject(t *testing.T) {
	ctx := context.Background()
	df := newDoorFixture(t, false)
	reg := df.dm.Deps.Projects
	if _, err := df.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "oncall"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "lander", Role: "member", Project: "oncall"}); err != nil {
		t.Fatal(err)
	}
	df.restart(t, func(c *config.Config) {
		c.File.Tunnel.URL = "https://authority.example"
		c.File.Secrets = config.Secrets{Issuers: map[string]config.SecretsIssuer{"pool": {Kind: issuers.KindPotluck, As: "lander",
			Ceiling: []config.SecretsCeiling{{Project: "oncall", Role: "member", Repo: "acme/app", Workflows: []string{".github/workflows/x.yml"}, Events: []string{"push"}}}}}}
	})
	df.startIssuer(t)
	if _, err := computeclient.AddSecretGrant(ctx, df.owner, computeapi.AddSecretGrantRequest{
		To:   computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/x.yml", Ref: "refs/heads/main", Events: []string{"push"}}},
		Kind: computeapi.SecretKindPotluckToken, Name: "POTLUCK_TOKEN", Scopes: []string{computeapi.SecretScopeBox},
		Ceiling: json.RawMessage(`{"role":"member"}`)}); err != nil {
		t.Fatalf("the potluck-token job grant: %v", err)
	}

	df.set(func(d *doorGitHub) { d.runs[790] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	df.served(t, 7901, 790, 1)
	df.ready(t, 1)
	df.deliver(t, "workflow_job", df.startOn(7901, 790, "acme/app", "potluck-7901"))
	eventually(t, "the marker", df.marked)

	var cred string
	for _, s := range df.boxSecrets() {
		if s[0] == "POTLUCK_TOKEN" {
			cred = s[1]
		}
	}
	if cred == "" {
		t.Fatalf("no POTLUCK_TOKEN in the box: %v", df.boxSecrets())
	}
	c, err := tunnel.ParseBoxCredential(cred)
	if err != nil {
		t.Fatal(err)
	}
	reg = df.dm.Deps.Projects
	sub, err := reg.FromTokenAt(ctx, c.Token, true, tunnel.BoxKeyID(c.Key))
	if err != nil {
		t.Fatalf("the box's token: %v", err)
	}
	oncall, _ := reg.Lookup("oncall")
	pool, _ := reg.Lookup("pool")
	if sub.Identity.Name != "lander" || sub.Cap == nil || sub.Cap.Project != oncall.ID || sub.Cap.Role != "member" {
		t.Fatalf("the token acts as %s with cap %+v, want lander, member on oncall %s", sub.Identity.Name, sub.Cap, oncall.ID)
	}

	// The same job asking for a box the broker does not record as its own
	// (another handle of the pool project, or the right handle in another
	// area) is refused before anything is minted.
	hs, err := computeclient.RunTargetHandles(ctx, df.owner, "small", false)
	if err != nil || len(hs) != 1 {
		t.Fatalf("handles %+v %v", hs, err)
	}
	box := issuers.Box{Project: pool.ID, Area: hs[0].Area, ID: hs[0].Handle}
	if _, err := reg.AddArea(ctx, reg.Local(""), "pool", api.Area{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	elsewhere, _ := reg.Lookup("elsewhere")
	job := issuers.Job{Repo: "acme/app", HeadRepo: "acme/app", RepoID: repoID("acme/app"), HeadRepoID: repoID("acme/app"),
		Workflow: ".github/workflows/x.yml", Ref: "refs/heads/main", DefaultBranch: "main", Event: "push", RunID: 790}
	toks, _ := reg.Tokens(ctx)
	before := len(toks)
	for name, b := range map[string]issuers.Box{
		"another box":       {Project: box.Project, Area: box.Area, ID: "box:small/elsewhere"},
		"another area":      {Project: box.Project, Area: "other", ID: box.ID}, // an area of the pool, not the link's
		"another project":   {Project: elsewhere.ID, ID: box.ID},               // the job's handle, but not of the pool project
		"another run's box": box,
	} {
		j := job
		if name == "another run's box" {
			j.RunID = 791
		}
		_, err := issuers.From(df.dm.Deps).Issue(ctx, issuers.Request{Kind: computeapi.SecretKindPotluckToken, Name: "OTHER_TOKEN",
			Subject: issuers.Subject{Job: &j}, Box: b, Role: "member"})
		if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "runner broker's box") {
			t.Errorf("%s: %v, want forbidden: not the runner broker's box", name, err)
		}
	}
	if toks, _ := reg.Tokens(ctx); len(toks) != before {
		t.Fatalf("minted %d tokens for refused requests", len(toks)-before)
	}

	df.deliver(t, "workflow_job", jobEvent("completed", 7901, 790, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the token revoked", func() bool {
		_, err := reg.FromTokenAt(ctx, c.Token, true, tunnel.BoxKeyID(c.Key))
		return err != nil
	})
}
