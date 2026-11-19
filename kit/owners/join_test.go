package owners_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
)

// node calls the authority as a tailnet node that WhoIs identified.
type node struct {
	x  *ox
	rm core.Remote
}

func (n node) do(t *testing.T, method, path string, in, out any) error {
	t.Helper()
	var body bytes.Buffer
	if in != nil {
		_ = json.NewEncoder(&body).Encode(in)
	}
	req := httptest.NewRequest(method, path, &body)
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req = req.WithContext(core.WithRemote(req.Context(), n.rm))
	rec := httptest.NewRecorder()
	n.x.dm.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		var e struct {
			Error *api.Error `json:"error"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &e) == nil && e.Error != nil {
			return e.Error
		}
		t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	if out != nil {
		return json.Unmarshal(rec.Body.Bytes(), out)
	}
	return nil
}

// join runs a whole GitHub join from n.
func (n node) join(t *testing.T, project, machine string) (api.GitHubLoginOutcome, error) {
	t.Helper()
	var st api.GitHubLoginStart
	if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: project, Machine: machine}, &st); err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var out api.GitHubLoginOutcome
		if err := n.do(t, "POST", "/v0/login/github/"+st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000}, &out); err != nil || !out.Pending {
			return out, err
		}
	}
	t.Fatal("the join never ended")
	return api.GitHubLoginOutcome{}, nil
}

func carlNode(x *ox) node {
	return node{x: x, rm: core.Remote{Login: "carl@example.com", Node: "host-b", NodeID: "nCARL1EXMPL", NodeKey: "nodekey:c1"}}
}

func teamProject(t *testing.T, x *ox, slug string) {
	t.Helper()
	x.projects(t, slug)
	if _, err := x.owner.ProjectTeam(context.Background(), slug); err != nil {
		t.Fatal(err)
	}
}

func (x *ox) projects(t *testing.T, slugs ...string) {
	t.Helper()
	for _, s := range slugs {
		if _, err := x.owner.CreateProject(context.Background(), api.NewProjectRequest{Slug: s}); err != nil {
			t.Fatal(err)
		}
	}
}

// The authority's half of "Carl joins": a grant by handle, a join with no
// code, a member token for Carl's node that works.
func TestGitHubJoin(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	carl, _ := x.gh.AddUser("carlexample", "User")
	n := carlNode(x)
	// Discover says only "a potluck authority": no project input, no list.
	var au api.GitHubAuthority
	if err := n.do(t, "GET", "/v0/join/github?project=orchid", nil, &au); err != nil || !au.Authority || au.Machine != "mini" || !au.GitHub || au.Protocol != api.WireProtocol {
		t.Fatalf("discover = %+v %v", au, err)
	}
	g, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: "owner", Project: "orchid"})
	if err != nil || g.Account != "github:"+jsonNum(carl.ID) || g.Kind != "user" {
		t.Fatalf("grant = %+v %v", g, err)
	}
	// GitHub renames don't matter: the grant is by id.
	x.gh.Rename(carl.ID, "carl")
	x.gh.Approver(carl.ID, 1)
	out, err := n.join(t, "orchid", "host-b")
	if err != nil {
		t.Fatal(err)
	}
	j := out.Join
	if j == nil || j.Token == "" || j.Cap.Slug != "orchid" || j.Cap.Role != "owner" || j.Identity != "carl" || j.Machine != "host-b" || j.Authority != "mini" {
		t.Fatalf("join = %+v", out)
	}
	// The token is node-bound: it works from Carl's node, nowhere else.
	var p api.Project
	tok := func(nd node) error {
		req := httptest.NewRequest("GET", "/v0/projects/orchid", nil)
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		req.Header.Set(api.HeaderAuthorization, "Bearer "+j.Token)
		req = req.WithContext(core.WithRemote(req.Context(), nd.rm))
		rec := httptest.NewRecorder()
		x.dm.Handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			return api.Errorf(api.CodeUnauthenticated, "%d", rec.Code)
		}
		return json.Unmarshal(rec.Body.Bytes(), &p)
	}
	if err := tok(n); err != nil || p.Slug != "orchid" {
		t.Fatalf("Carl's token from Carl's node: %v", err)
	}
	other := node{x: x, rm: core.Remote{Login: "eve@example.com", NodeID: "nEVE", NodeKey: "nodekey:e"}}
	if err := tok(other); err == nil {
		t.Fatal("Carl's token worked from another node")
	}
	// Joining again from the same node replaces the token.
	x.gh.Approver(carl.ID, 0)
	again, err := n.join(t, "orchid", "host-b")
	if err != nil || again.Join.Token == j.Token || again.Join.Identity != "carl" {
		t.Fatalf("join again = %+v %v", again, err)
	}
	if err := tok(n); err == nil {
		t.Fatal("the replaced token still works")
	}
	// The first join from a node mailed the owners and the identity, naming
	// the node; joining again from it did not.
	ms := x.mail.to("orchid", "owners")
	if len(ms) != 1 || !strings.Contains(ms[0], "nCARL1EXMPL") || !strings.Contains(ms[0], "@carl") {
		t.Fatalf("owners' mail = %q", ms)
	}
	// The identity is linked: grants list it, and it shows in the registry.
	gl, err := x.owner.GitHubGrants(ctx, "orchid")
	if err != nil || len(gl.Grants) != 1 || gl.Grants[0].Role != "owner" {
		t.Fatalf("grants = %+v %v", gl, err)
	}
}

func TestGitHubJoinOrgAndRefusals(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	x.projects(t, "solo") // not a team project
	org, _ := x.gh.AddUser("acme-org", "Organization")
	ana, _ := x.gh.AddUser("ana", "User")
	bob, _ := x.gh.AddUser("bob", "User")
	// An org grant needs a lookup token that sees the org's members.
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "acme-org", Role: "member", Project: "orchid"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an org grant the authority cannot re-check: %v, want forbidden", err)
	}
	x.gh.AddMember(org.ID, x.viewer, "active")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "acme-org", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	n := carlNode(x)
	// Not a member yet, then a pending invitation: refused, live each time.
	for _, state := range []string{"", "pending"} {
		if state != "" {
			x.gh.AddMember(org.ID, ana.ID, state)
		}
		x.gh.Approver(ana.ID, 0)
		if _, err := n.join(t, "orchid", "anas-mac"); !api.IsCode(err, api.CodeForbidden) {
			t.Fatalf("org membership %q: %v, want forbidden", state, err)
		}
	}
	x.gh.AddMember(org.ID, ana.ID, "active")
	x.gh.Approver(ana.ID, 0)
	out, err := n.join(t, "orchid", "anas-mac")
	if err != nil || out.Join.Cap.Role != "member" || out.Join.Identity != "ana" {
		t.Fatalf("org member join = %+v %v", out, err)
	}
	// A name taken by an unlinked identity is never merged: -2.
	if _, err := x.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "bob", Kind: api.IdentityHuman}); err != nil {
		t.Fatal(err)
	}
	x.gh.AddMember(org.ID, bob.ID, "active")
	x.gh.Approver(bob.ID, 0)
	bn := node{x: x, rm: core.Remote{Login: "bob@example.com", NodeID: "nBOB", NodeKey: "nodekey:b"}}
	if out, err := bn.join(t, "orchid", "bobs-mac"); err != nil || out.Join.Identity != "bob-2" {
		t.Fatalf("bob = %+v %v, want identity bob-2", out, err)
	}
	// Refusals before any login starts.
	var st api.GitHubLoginStart
	for what, tc := range map[string]struct {
		n       node
		project string
		code    string
	}{
		"a tagged node": {node{x: x, rm: core.Remote{NodeID: "nTAG", Tagged: true}}, "orchid", api.CodeForbidden},
		"no WhoIs node": {node{x: x, rm: core.Remote{}}, "orchid", api.CodeUnauthenticated},
		"this machine":  {node{x: x, rm: core.Remote{NodeID: "nSELF", Self: true}}, "orchid", api.CodeForbidden},
	} {
		if err := tc.n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: tc.project, Machine: "m1"}, &st); !api.IsCode(err, tc.code) {
			t.Errorf("%s: %v, want %s", what, err, tc.code)
		}
	}
	// The socket never starts a GitHub join.
	if err := x.owner.Do(ctx, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "m1"}, &st); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("the socket: %v, want forbidden", err)
	}
	// An account with no grant: one refusal.
	eve, _ := x.gh.AddUser("eve", "User")
	x.gh.Approver(eve.ID, 0)
	if _, err := n.join(t, "orchid", "eves-mac"); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "no grant") {
		t.Errorf("no grant: %v", err)
	}
	// Only the node that started a join collects it.
	x.gh.Approver(ana.ID, 0)
	if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "anas-mac"}, &st); err != nil {
		t.Fatal(err)
	}
	var out2 api.GitHubLoginOutcome
	if err := bn.do(t, "POST", "/v0/login/github/"+st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 3000}, &out2); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("another node collecting: %v, want not_found", err)
	}
	// Revoking a GitHub grant: by the login it was granted under.
	if _, err := x.owner.RevokeGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme-org", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if gl, err := x.owner.GitHubGrants(ctx, "orchid"); err != nil || len(gl.Grants) != 0 {
		t.Fatalf("after revoke: %+v %v", gl, err)
	}
}

var _ = client.WithHandler

// An unknown project, a project that is not a team project, a team project
// with no GitHub grants, and one granting someone else: one refusal, the
// same bytes but for the name typed, and only after the device flow.
func TestJoinRefusalIsUniform(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	teamProject(t, x, "orchid")
	teamProject(t, x, "bare")
	x.projects(t, "solo")
	bob, _ := x.gh.AddUser("bob", "User")
	if _, err := x.owner.GrantGitHub(context.Background(), api.GitHubGrantRequest{Owner: "@bob", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	_ = bob
	eve, _ := x.gh.AddUser("eve", "User")
	n := carlNode(x)
	var bodies []string
	for _, p := range []string{"nope", "solo", "bare", "orchid"} {
		x.gh.Approver(eve.ID, 0)
		var st api.GitHubLoginStart
		if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: p, Machine: "eves-mac"}, &st); err != nil {
			t.Fatalf("%s: the login must start for any name: %v", p, err)
		}
		_, err := n.join2(t, st)
		e, ok := err.(*api.Error)
		if !ok || e.Code != api.CodeForbidden {
			t.Fatalf("%s: %v, want forbidden", p, err)
		}
		b, _ := json.Marshal(e)
		bodies = append(bodies, strings.ReplaceAll(string(b), p, "X"))
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("refusals differ:\n%s\n%s", bodies[0], b)
		}
	}
}

// A join acts only when the node collects: GitHub's answer alone creates
// no identity and no token, and an uncollected outcome expires.
func TestJoinActsOnCollect(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	x.svc.Collect = 200 * time.Millisecond
	teamProject(t, x, "orchid")
	carl, _ := x.gh.AddUser("carlexample", "User")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: "owner", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	x.gh.Approver(carl.ID, 0)
	n := carlNode(x)
	var st api.GitHubLoginStart
	if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b"}, &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.For, "host-b") || !strings.Contains(st.For, "nCARL1EXMPL") || st.Authority != "mini" {
		t.Fatalf("who is asking: %+v", st)
	}
	for deadline := time.Now().Add(10 * time.Second); len(x.gh.Issued()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("GitHub never answered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond) // past Collect
	ids, _ := x.owner.Identities(ctx)
	for _, id := range ids {
		if id.Name == "carlexample" {
			t.Fatal("an identity was created before the outcome was collected")
		}
	}
	if _, err := n.join2(t, st); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an expired outcome: %v, want not_found", err)
	}
}

// join2 polls a started join until it ends.
func (n node) join2(t *testing.T, st api.GitHubLoginStart) (api.GitHubLoginOutcome, error) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		var out api.GitHubLoginOutcome
		if err := n.do(t, "POST", "/v0/login/github/"+st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000}, &out); err != nil || !out.Pending {
			return out, err
		}
	}
	t.Fatal("the join never ended")
	return api.GitHubLoginOutcome{}, nil
}

// An org grant is by the org's id: a renamed org's members still join, and
// a new org squatting the old login admits nobody.
func TestOrgGrantByID(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	org, _ := x.gh.AddUser("acme", "Organization")
	x.gh.AddMember(org.ID, x.viewer, "active")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.AddMember(org.ID, ana.ID, "active")
	x.gh.Rename(org.ID, "acme-labs")
	// The squatter takes the freed login, as a new org with itself in it.
	squat, _ := x.gh.AddUser("acme", "Organization")
	mal, _ := x.gh.AddUser("mallory", "User")
	x.gh.AddMember(squat.ID, mal.ID, "active")
	n := carlNode(x)
	x.gh.Approver(mal.ID, 0)
	if _, err := n.join(t, "orchid", "mals-mac"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the squatter: %v, want the refusal", err)
	}
	x.gh.Approver(ana.ID, 0)
	an := node{x: x, rm: core.Remote{Login: "ana@example.com", NodeID: "nANA", NodeKey: "nodekey:a"}}
	if out, err := an.join(t, "orchid", "anas-mac"); err != nil || out.Join.Cap.Role != "member" {
		t.Fatalf("a renamed org's member: %+v %v", out, err)
	}
}

// Revoking a GitHub grant revokes what it gave: the grant row and the
// node-bound token of every identity it admitted. Leaving an org loses
// the org's grant at the next re-check.
func TestRevokeCascadesAndOrgLapse(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	carl, _ := x.gh.AddUser("carlexample", "User")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	org, _ := x.gh.AddUser("acme", "Organization")
	x.gh.AddMember(org.ID, x.viewer, "active")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.AddMember(org.ID, ana.ID, "active")
	cn, an := carlNode(x), node{x: x, rm: core.Remote{Login: "ana@example.com", NodeID: "nANA", NodeKey: "nodekey:a"}}
	x.gh.Approver(carl.ID, 0)
	cj := mustJoin(t, cn, "orchid", "host-b")
	x.gh.Approver(ana.ID, 0)
	aj := mustJoin(t, an, "orchid", "anas-mac")
	if !works(t, x, cn, cj.Token) || !works(t, x, an, aj.Token) {
		t.Fatal("fresh tokens do not work")
	}
	r, err := x.owner.RevokeGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Project: "orchid"})
	if err != nil || !r.Revoked || r.Admissions != 1 {
		t.Fatalf("revoke = %+v %v", r, err)
	}
	if works(t, x, cn, cj.Token) {
		t.Fatal("Carl's token works after his grant was revoked")
	}
	gs, _ := x.owner.Grants(ctx, "orchid")
	for _, g := range gs {
		if g.Identity == "carlexample" {
			t.Fatalf("Carl's grant row stayed: %+v", g)
		}
	}
	// Ana is still a member: the re-check keeps her.
	if err := x.svc.Recheck(ctx); err != nil || !works(t, x, an, aj.Token) {
		t.Fatalf("a member lost her token at a re-check: %v", err)
	}
	x.gh.RemoveMember(org.ID, ana.ID)
	if err := x.svc.Recheck(ctx); err != nil || works(t, x, an, aj.Token) {
		t.Fatalf("Ana left the org and her token still works (%v)", err)
	}
}

// A GitHub join never downgrades a node: joining again with a weaker grant
// keeps the stronger role.
func TestJoinKeepsTheStrongerRole(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	carl, _ := x.gh.AddUser("carlexample", "User")
	grant := func(role string) {
		if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Role: role, Project: "orchid"}); err != nil {
			t.Fatal(err)
		}
	}
	grant("owner")
	n := carlNode(x)
	x.gh.Approver(carl.ID, 0)
	if j := mustJoin(t, n, "orchid", "host-b"); j.Cap.Role != "owner" {
		t.Fatalf("first join role %q", j.Cap.Role)
	}
	grant("member")
	x.gh.Approver(carl.ID, 0)
	if j := mustJoin(t, n, "orchid", "host-b"); j.Cap.Role != "owner" {
		t.Fatalf("a weaker join downgraded the node to %q", j.Cap.Role)
	}
}

// GitHub failing a membership check is the same refusal, never an outage
// that tells the project apart.
func TestMembershipErrorIsTheRefusal(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	org, _ := x.gh.AddUser("acme", "Organization")
	x.gh.AddMember(org.ID, x.viewer, "active")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.AddMember(org.ID, ana.ID, "active")
	x.gh.FailMemberships(true)
	x.gh.Approver(ana.ID, 0)
	_, err := carlNode(x).join(t, "orchid", "anas-mac")
	if e, ok := err.(*api.Error); !ok || e.Code != api.CodeForbidden || !strings.Contains(e.Message, "no grant for you") {
		t.Fatalf("a failed membership check: %v, want the one refusal", err)
	}
}

func mustJoin(t *testing.T, n node, project, machine string) api.RedeemResponse {
	t.Helper()
	out, err := n.join(t, project, machine)
	if err != nil || out.Join == nil {
		t.Fatalf("join: %+v %v", out, err)
	}
	return *out.Join
}

// works says whether tok reads the project from n's node.
func works(t *testing.T, x *ox, n node, tok string) bool {
	req := httptest.NewRequest("GET", "/v0/projects/orchid", nil)
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderAuthorization, "Bearer "+tok)
	req = req.WithContext(core.WithRemote(req.Context(), n.rm))
	rec := httptest.NewRecorder()
	x.dm.Handler.ServeHTTP(rec, req)
	return rec.Code == 200
}

// A re-check that cannot run removes nobody: the owners are told once, and
// after the grace new joins through org grants stop.
func TestOrgChecksFailing(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	org, _ := x.gh.AddUser("acme", "Organization")
	x.gh.AddMember(org.ID, x.viewer, "active")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@acme", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	ana, _ := x.gh.AddUser("ana", "User")
	bob, _ := x.gh.AddUser("bob", "User")
	x.gh.AddMember(org.ID, ana.ID, "active")
	x.gh.AddMember(org.ID, bob.ID, "active")
	an := node{x: x, rm: core.Remote{Login: "ana@example.com", NodeID: "nANA", NodeKey: "nodekey:a"}}
	x.gh.Approver(ana.ID, 0)
	aj := mustJoin(t, an, "orchid", "anas-mac")
	x.gh.Revoke(x.viewerTok) // the lookup token stops working
	for i := 0; i < 2; i++ {
		if err := x.svc.Recheck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !works(t, x, an, aj.Token) {
		t.Fatal("a check that could not run removed a member")
	}
	ms := x.mail.to("orchid", "owners")
	n := 0
	for _, m := range ms {
		if strings.Contains(m, "org membership checks failing") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d failing-check mails, want 1: %q", n, ms)
	}
	// Within the grace a new org member still joins; past it, not.
	bn := node{x: x, rm: core.Remote{Login: "bob@example.com", NodeID: "nBOB", NodeKey: "nodekey:b"}}
	x.gh.Approver(bob.ID, 0)
	mustJoin(t, bn, "orchid", "bobs-mac")
	x.svc.OrgFailGrace = 0
	cn := node{x: x, rm: core.Remote{Login: "bob@example.com", NodeID: "nBOB2", NodeKey: "nodekey:b2"}}
	x.gh.Approver(bob.ID, 0)
	if _, err := cn.join(t, "orchid", "bobs-other"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an org join after the grace: %v, want the refusal", err)
	}
	if !works(t, x, an, aj.Token) {
		t.Fatal("the freeze revoked a member")
	}
}

// The machine owner's own links have their pool: remote starts filling
// theirs never starve them.
func TestLocalPoolSurvivesRemoteStarts(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	x.svc.MaxLogins = 1
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 100)
	var st api.GitHubLoginStart
	if err := carlNode(x).do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b"}, &st); err != nil {
		t.Fatal(err)
	}
	other := node{x: x, rm: core.Remote{Login: "eve@example.com", NodeID: "nEVE", NodeKey: "nodekey:e"}}
	if err := other.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "eves-mac"}, &st); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a second remote start in a full pool: %v, want rate_limited", err)
	}
	if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{}); err != nil {
		t.Fatalf("the machine owner's link with the remote pool full: %v", err)
	}
}

// A failure after the new identity is made and before its link rolls both
// back: no orphan identity.
func TestJoinIdentityAndLinkAreOne(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	teamProject(t, x, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	if _, err := x.owner.GrantGitHub(ctx, api.GitHubGrantRequest{Owner: "@ana", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	owners.SetWithinFault(x.svc, func() error { return api.Errorf(api.CodeConflict, "injected") })
	x.gh.Approver(ana.ID, 0)
	if _, err := carlNode(x).join(t, "orchid", "anas-mac"); err == nil {
		t.Fatal("the injected failure was not reported")
	}
	ids, _ := x.owner.Identities(ctx)
	for _, id := range ids {
		if strings.HasPrefix(id.Name, "ana") {
			t.Fatalf("an orphan identity %s", id.Name)
		}
	}
	var n int
	_ = x.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM owner_links`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d links left", n)
	}
	// With the fault gone the same join works, and its grant row names its
	// source by GitHub id.
	owners.SetWithinFault(x.svc, nil)
	x.gh.Approver(ana.ID, 0)
	j := mustJoin(t, carlNode(x), "orchid", "anas-mac")
	var by string
	if err := x.dm.Deps.Store.Registry().QueryRow(`SELECT g.created_by FROM grants g JOIN identities i ON i.id = g.subject WHERE i.name = ?`,
		j.Identity).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if want := "github:" + jsonNum(ana.ID) + " grant:github:" + jsonNum(ana.ID) + ":"; by != want {
		t.Fatalf("created_by = %q, want %q", by, want)
	}
}
