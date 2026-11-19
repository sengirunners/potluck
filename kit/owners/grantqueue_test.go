package owners_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/owners"
)

// until polls f for up to 15 seconds.
func until(t *testing.T, what string, f func() bool) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// work starts the module's background work (the daemon does at Serve),
// which includes the grant queue's worker.
func work(t *testing.T, x *ox) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := x.svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
}

func grantedLogins(t *testing.T, x *ox, project string) []string {
	t.Helper()
	l, err := x.owner.GitHubGrants(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range l.Grants {
		out = append(out, g.Login)
	}
	return out
}

// A login GitHub answered for lately is granted again with no GitHub call,
// and even while GitHub's rate limit is up; past the cache's TTL GitHub is
// asked again.
func TestGrantUsesTheOwnerLoginCache(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	c := x.clock()
	ctx := context.Background()
	x.projects(t, "orchid", "lily")
	x.gh.AddUser("carlexample", "User")
	req := func(project string) api.GitHubGrantRequest {
		return api.GitHubGrantRequest{Owner: "@CarlExample", Role: "member", Project: project}
	}
	if g, err := x.owner.GrantGitHub(ctx, req("orchid")); err != nil || g.Queued || g.Login != "carlexample" {
		t.Fatalf("first grant: %+v, %v", g, err)
	}
	asked := x.gh.Lookups()
	if asked == 0 {
		t.Fatal("the first grant did not ask GitHub")
	}
	x.gh.RateLimitUntil(time.Now().Add(time.Hour))
	if g, err := x.owner.GrantGitHub(ctx, req("lily")); err != nil || g.Queued || g.Account == "" {
		t.Fatalf("grant from the cache while GitHub refuses: %+v, %v", g, err)
	}
	if n := x.gh.Lookups() + x.gh.RateLimited(); n != asked {
		t.Fatalf("GitHub was asked %d times, want %d: the cache answers", n, asked)
	}
	c.add(2 * time.Hour) // past TTL: GitHub is asked, and refuses: queued, not lost
	if g, err := x.owner.GrantGitHub(ctx, req("lily")); err != nil || !g.Queued {
		t.Fatalf("grant past the TTL while GitHub refuses: %+v, %v; want queued", g, err)
	}
}

// A grant GitHub's rate limit refused finishes by itself after the reset,
// and the granter is mailed.
func TestRateLimitedGrantFinishesAfterTheReset(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	work(t, x)
	x.projects(t, "orchid")
	x.svc.QueueSlack = 0
	x.gh.AddUser("carlexample", "User")
	x.gh.RateLimitUntil(time.Now().Add(1200 * time.Millisecond))

	g, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"})
	if err != nil || !g.Queued || g.RetryAt == nil || g.Login != "carlexample" || g.Account != "" {
		t.Fatalf("grant while limited: %+v, %v; want queued with a retry time", g, err)
	}
	if got := grantedLogins(t, x, "orchid"); len(got) != 0 {
		t.Fatalf("granted before the reset: %v", got)
	}
	l, _ := x.owner.GitHubGrants(ctx, "orchid")
	if len(l.Queued) != 1 || l.Queued[0].Login != "carlexample" {
		t.Fatalf("list shows queued %+v, want carlexample waiting", l.Queued)
	}

	until(t, "the grant to be made", func() bool { return len(grantedLogins(t, x, "orchid")) == 1 })
	if got := grantedLogins(t, x, "orchid"); got[0] != "carlexample" {
		t.Fatalf("granted %v", got)
	}
	if l, _ := x.owner.GitHubGrants(ctx, "orchid"); len(l.Queued) != 0 {
		t.Fatalf("still queued after it was made: %+v", l.Queued)
	}
	until(t, "the mail", func() bool { return len(x.mail.to("orchid", "human:judy")) > 0 })
	if m := x.mail.to("orchid", "human:judy")[0]; !strings.Contains(m, "@carlexample") || !strings.Contains(m, "is made") {
		t.Fatalf("mail to the granter: %q", m)
	}
}

// A queued grant that GitHub then answers no for is told to the granter
// with the reason, and makes no grant.
func TestQueuedGrantToAMissingAccountIsReported(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	work(t, x)
	x.projects(t, "orchid")
	x.svc.QueueSlack = 0
	x.gh.RateLimitUntil(time.Now().Add(time.Second))
	if g, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@ghost", Role: "member", Project: "orchid"}); err != nil || !g.Queued {
		t.Fatalf("grant while limited: %+v, %v", g, err)
	}
	until(t, "the mail", func() bool { return len(x.mail.to("orchid", "human:judy")) > 0 })
	if m := x.mail.to("orchid", "human:judy")[0]; !strings.Contains(m, "was not made") || !strings.Contains(m, "no GitHub account @ghost") {
		t.Fatalf("mail to the granter: %q", m)
	}
	if got := grantedLogins(t, x, "orchid"); len(got) != 0 {
		t.Fatalf("granted: %v", got)
	}
}

// A revoke of a login whose grant is still queued cancels it.
func TestRevokeCancelsAQueuedGrant(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	c := x.clock()
	ctx := context.Background()
	x.projects(t, "orchid")
	x.gh.AddUser("carlexample", "User")
	x.gh.RateLimitUntil(time.Now().Add(time.Hour))
	req := api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"}
	if g, err := x.owner.GrantGitHub(ctx, req); err != nil || !g.Queued {
		t.Fatalf("grant while limited: %+v, %v", g, err)
	}
	if r, err := x.owner.RevokeGitHub(ctx, req); err != nil || !r.Revoked {
		t.Fatalf("revoke: %+v, %v", r, err)
	}
	x.gh.RateLimitUntil(time.Time{})
	c.add(2 * time.Hour) // due, were it still queued
	if err := x.svc.WorkGrantQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := grantedLogins(t, x, "orchid"); len(got) != 0 {
		t.Fatalf("a cancelled grant was made: %v", got)
	}
	if len(x.mail.to("orchid", "human:judy")) != 0 {
		t.Fatalf("a cancelled grant was reported: %q", x.mail.to("orchid", "human:judy"))
	}
}

// A revoke of a login holding a grant, with another ask for it still queued,
// removes both.
func TestRevokeTakesTheMadeGrantAndTheQueuedOne(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	c := x.clock()
	ctx := context.Background()
	x.projects(t, "orchid")
	x.gh.AddUser("carlexample", "User")
	req := api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"}
	if _, err := x.owner.GrantGitHub(ctx, req); err != nil {
		t.Fatal(err)
	}
	c.add(2 * time.Hour) // the cache is stale: the next ask goes to GitHub
	x.gh.RateLimitUntil(time.Now().Add(time.Hour))
	req.Role = "owner"
	if g, err := x.owner.GrantGitHub(ctx, req); err != nil || !g.Queued {
		t.Fatalf("role change while limited: %+v, %v", g, err)
	}
	if r, err := x.owner.RevokeGitHub(ctx, req); err != nil || !r.Revoked {
		t.Fatalf("revoke: %+v, %v", r, err)
	}
	if got := grantedLogins(t, x, "orchid"); len(got) != 0 {
		t.Fatalf("the made grant survived the revoke: %v", got)
	}
	if l, _ := x.owner.GitHubGrants(ctx, "orchid"); len(l.Queued) != 0 {
		t.Fatalf("the queued ask survived the revoke: %+v", l.Queued)
	}
}

// A grant made directly replaces one still queued for the login, and a
// grant asked again while queued takes the new role.
func TestNewerAskWinsOverAQueuedOne(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	c := x.clock()
	ctx := context.Background()
	x.projects(t, "orchid")
	x.gh.AddUser("carlexample", "User")
	x.gh.RateLimitUntil(time.Now().Add(time.Hour))
	req := api.GitHubGrantRequest{Owner: "@carlexample", Role: "owner", Project: "orchid"}
	if g, err := x.owner.GrantGitHub(ctx, req); err != nil || !g.Queued {
		t.Fatalf("grant while limited: %+v, %v", g, err)
	}
	req.Role = "guest"
	if g, err := x.owner.GrantGitHub(ctx, req); err != nil || !g.Queued || g.Role != "guest" {
		t.Fatalf("asked again while limited: %+v, %v", g, err)
	}
	if l, _ := x.owner.GitHubGrants(ctx, "orchid"); len(l.Queued) != 1 || l.Queued[0].Role != "guest" {
		t.Fatalf("queued %+v, want one ask with the latest role", l.Queued)
	}
	x.gh.RateLimitUntil(time.Time{})
	req.Role = "member"
	if g, err := x.owner.GrantGitHub(ctx, req); err != nil || g.Queued {
		t.Fatalf("direct grant: %+v, %v", g, err)
	}
	c.add(2 * time.Hour)
	if err := x.svc.WorkGrantQueue(ctx); err != nil {
		t.Fatal(err)
	}
	l, _ := x.owner.GitHubGrants(ctx, "orchid")
	if len(l.Queued) != 0 || len(l.Grants) != 1 || l.Grants[0].Role != "member" {
		t.Fatalf("grants %+v queued %+v: the direct grant should stand alone", l.Grants, l.Queued)
	}
}

// A reset GitHub names that is already past (the clocks differ) or absent
// does not make the queue ask again at once.
func TestRetryNeverSoonerThanQueueEvery(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	now := time.Now()
	for name, reset := range map[string]time.Time{"past": now.Add(-time.Hour), "unknown": {}, "far": now.Add(1000 * time.Hour)} {
		at := owners.RetryAfterFor(x.svc, reset)
		switch name {
		case "far":
			if d := at.Sub(now); d > x.svc.QueueGiveUp+time.Second {
				t.Fatalf("%s: retry in %v, want at most QueueGiveUp", name, d)
			}
		default:
			if d := at.Sub(now); d < x.svc.QueueEvery-time.Second {
				t.Fatalf("%s: retry in %v, want at least QueueEvery", name, d)
			}
		}
	}
}

// A queued grant of a granter whose token was revoked is not made.
func TestQueuedGrantNeedsTheGranterTokenLive(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	c := x.clock()
	ctx := context.Background()
	x.projects(t, "orchid")
	x.gh.AddUser("carlexample", "User")
	mint, err := x.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "orchid", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	ana, err := x.dm.Deps.Projects.FromToken(ctx, mint.Secret, false)
	if err != nil {
		t.Fatal(err)
	}
	x.gh.RateLimitUntil(time.Now().Add(time.Hour))
	if g, err := x.svc.Grant(ctx, ana, api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"}); err != nil || !g.Queued {
		t.Fatalf("grant while limited: %+v, %v", g, err)
	}
	if _, err := x.owner.RevokeToken(ctx, mint.Token.ID); err != nil {
		t.Fatal(err)
	}
	x.gh.RateLimitUntil(time.Time{})
	c.add(2 * time.Hour)
	if err := x.svc.WorkGrantQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := grantedLogins(t, x, "orchid"); len(got) != 0 {
		t.Fatalf("granted for a revoked token: %v", got)
	}
	if ms := x.mail.to("orchid", "human:ana"); len(ms) != 1 || !strings.Contains(ms[0], "no longer administer") {
		t.Fatalf("mail to ana: %q", ms)
	}
}

// A queued grant is the granter's act at that time: an admin who lost the
// role before it ran does not get it made, and is told.
func TestQueuedGrantNeedsTheGranterStillAdmin(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	work(t, x)
	x.projects(t, "orchid")
	x.svc.QueueSlack = 0
	x.gh.AddUser("carlexample", "User")
	id, err := x.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "ana", Kind: api.IdentityHuman})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: "ana", Project: "orchid", Role: "owner"}); err != nil {
		t.Fatal(err)
	}
	tuples, err := x.dm.Deps.Projects.Tuples(ctx, id.ID)
	if err != nil {
		t.Fatal(err)
	}
	ana := &authz.Subject{Identity: id, Address: "human:ana", Via: authz.ViaToken, Scope: authz.FromTuples(tuples, nil)}

	x.gh.RateLimitUntil(time.Now().Add(time.Second))
	if g, err := x.svc.Grant(ctx, ana, api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"}); err != nil || !g.Queued {
		t.Fatalf("grant while limited: %+v, %v", g, err)
	}
	if _, err := x.owner.Revoke(ctx, api.GrantRequest{Identity: "ana", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	until(t, "the mail", func() bool { return len(x.mail.to("orchid", "human:ana")) > 0 })
	if m := x.mail.to("orchid", "human:ana")[0]; !strings.Contains(m, "no longer administer") {
		t.Fatalf("mail to ana: %q", m)
	}
	if got := grantedLogins(t, x, "orchid"); len(got) != 0 {
		t.Fatalf("granted for an admin who lost the role: %v", got)
	}
}

// A grant GitHub keeps refusing is given up on after QueueGiveUp, with the
// granter told, never kept for ever.
func TestQueuedGrantGivesUp(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	c := x.clock()
	ctx := context.Background()
	x.projects(t, "orchid")
	x.gh.AddUser("carlexample", "User")
	x.gh.RateLimitUntil(time.Now().Add(time.Hour))
	if g, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"}); err != nil || !g.Queued {
		t.Fatalf("grant while limited: %+v, %v", g, err)
	}
	c.add(2 * time.Hour) // the reset has passed; GitHub, as the fake has it, still refuses
	if err := x.svc.WorkGrantQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if l, _ := x.owner.GitHubGrants(ctx, "orchid"); len(l.Queued) != 1 {
		t.Fatalf("queued %+v, want it kept while it is young", l.Queued)
	}
	c.add(25 * time.Hour)
	if err := x.svc.WorkGrantQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if l, _ := x.owner.GitHubGrants(ctx, "orchid"); len(l.Queued) != 0 {
		t.Fatalf("queued %+v, want it given up", l.Queued)
	}
	ms := x.mail.to("orchid", "human:judy")
	if len(ms) != 1 || !strings.Contains(ms[0], "was not made") || !strings.Contains(ms[0], "Grant it again later") {
		t.Fatalf("mail to the granter: %q", ms)
	}
}

// A pass that read a queued grant before a revoke or a newer ask does not
// make it: the claim is of the row it read.
func TestQueuedGrantIsClaimedNotAssumed(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	x.projects(t, "orchid")
	carl, _ := x.gh.AddUser("carlexample", "User")
	acc := acct(carl, "User")
	x.gh.RateLimitUntil(time.Now().Add(time.Hour))
	req := api.GitHubGrantRequest{Owner: "@carlexample", Role: "owner", Project: "orchid"}
	p, _ := x.dm.Deps.Projects.Lookup("orchid")

	if _, err := x.owner.GrantGitHub(ctx, req); err != nil {
		t.Fatal(err)
	}
	pass := owners.QueuedSnapshot(x.svc, ctx, p.ID, "", "carlexample", acc)
	req.Role = "guest" // asked again after the pass read the row
	time.Sleep(5 * time.Millisecond)
	if _, err := x.owner.GrantGitHub(ctx, req); err != nil {
		t.Fatal(err)
	}
	if made, err := pass(); err != nil || made {
		t.Fatalf("a pass holding the old ask: made %v, %v; want it not made", made, err)
	}

	pass = owners.QueuedSnapshot(x.svc, ctx, p.ID, "", "carlexample", acc)
	if _, err := x.owner.RevokeGitHub(ctx, req); err != nil { // revoked after the pass read the row
		t.Fatal(err)
	}
	if made, err := pass(); err != nil || made {
		t.Fatalf("a pass holding a revoked ask: made %v, %v; want it not made", made, err)
	}
	if got := grantedLogins(t, x, "orchid"); len(got) != 0 {
		t.Fatalf("granted: %v", got)
	}
}
