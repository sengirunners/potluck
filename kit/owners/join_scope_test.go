package owners_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
)

// tunnelNode calls the authority as a signed tunnel caller: the door sets
// Via and the caller's key id as NodeID, and nothing else.
func tunnelNode(x *ox, key string) node {
	return node{x: x, rm: core.Remote{Via: core.ViaTunnel, NodeID: key, Addr: "203.0.113.7"}}
}

// scoped runs a whole GitHub join from n asking for area and role.
func (n node) scoped(t *testing.T, project, machine, area, role string) (api.GitHubLoginOutcome, error) {
	t.Helper()
	var st api.GitHubLoginStart
	if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: project, Machine: machine, Area: area, Role: role}, &st); err != nil {
		return api.GitHubLoginOutcome{}, err
	}
	return n.join2(t, st)
}

func scopedFixture(t *testing.T) *ox {
	t.Helper()
	x := newOX(t, "Iv1.fakeclient")
	teamProject(t, x, "orchid")
	for _, a := range []string{"sprout", "billing"} {
		if _, err := x.owner.AddArea(context.Background(), "orchid", api.Area{Name: a, Work: []string{"local-" + a + "-*"}}); err != nil {
			t.Fatal(err)
		}
	}
	return x
}

func grantTo(t *testing.T, x *ox, who, role, area string) {
	t.Helper()
	if _, err := x.owner.GrantGitHub(context.Background(), api.GitHubGrantRequest{Owner: "@" + who, Role: role, Project: "orchid", Area: area}); err != nil {
		t.Fatal(err)
	}
}

// A tunnel caller can start and collect; only its own key collects; the
// admission is one area as a guest by default.
func TestTunnelGitHubJoinIsOneAreaAsGuest(t *testing.T) {
	x := scopedFixture(t)
	carl, _ := x.gh.AddUser("carlexample", "User")
	grantTo(t, x, "carlexample", "member", "") // a project-level member grant
	x.gh.Approver(carl.ID, 0)
	n := tunnelNode(x, "zmkey:aaaaaaaaaaaaaaaaaaaaaaaaaa")
	var disc api.GitHubAuthority
	if err := n.do(t, "GET", "/v0/join/github", nil, &disc); err != nil || !disc.Authority || !disc.GitHub {
		t.Fatalf("discover over the tunnel: %+v %v", disc, err)
	}
	var st api.GitHubLoginStart
	if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b", Area: "sprout"}, &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.For, "key zmkey:aaaaaaaaaaaaaaaaaaaaaaaaaa") || !strings.Contains(st.For, "sprout") || !strings.Contains(st.For, "guest") {
		t.Fatalf("the approver is told: %q", st.For)
	}
	// Another key, with the id and secret, collects nothing.
	var out api.GitHubLoginOutcome
	if err := tunnelNode(x, "zmkey:bbbbbbbbbbbbbbbbbbbbbbbbbb").do(t, "POST", "/v0/login/github/"+st.ID, api.GitHubLoginPoll{Secret: st.Secret}, &out); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("another key collecting: %v", err)
	}
	got, err := n.join2(t, st)
	if err != nil || got.Join == nil {
		t.Fatalf("join: %+v %v", got, err)
	}
	if c := got.Join.Cap; c.Area != "sprout" || c.Role != api.RoleGuest || c.Slug != "orchid" {
		t.Fatalf("cap = %+v, want sprout as guest (not the member grant)", c)
	}
}

func TestScopedJoinCappedByGrant(t *testing.T) {
	x := scopedFixture(t)
	ctx := context.Background()
	carl, _ := x.gh.AddUser("carlexample", "User")
	ana, _ := x.gh.AddUser("ana", "User")
	bob, _ := x.gh.AddUser("bob", "User")
	grantTo(t, x, "carlexample", "member", "") // whole project
	grantTo(t, x, "ana", "member", "sprout")   // one area
	grantTo(t, x, "bob", "owner", "")
	// A guest asks over the tunnel; a member or owner over the tailnet.
	nodeFor := func(machine, role string) node {
		if role == "" || role == "guest" {
			return tunnelNode(x, "zmkey:"+machine)
		}
		return node{x: x, rm: core.Remote{Login: machine + "@example.com", Node: machine, NodeID: "n" + machine, NodeKey: "nodekey:" + machine}}
	}
	ok := func(who int64, machine, area, role, wantRole, wantArea string) {
		t.Helper()
		x.gh.Approver(who, 0)
		out, err := nodeFor(machine, role).scoped(t, "orchid", machine, area, role)
		if err != nil || out.Join == nil || out.Join.Cap.Role != wantRole || out.Join.Cap.Area != wantArea {
			t.Fatalf("%s %s/%s: %+v %v, want %s in %s", machine, area, role, out, err, wantRole, wantArea)
		}
	}
	refused := func(who int64, machine, area, role string) string {
		t.Helper()
		x.gh.Approver(who, 0)
		_, err := nodeFor(machine, role).scoped(t, "orchid", machine, area, role)
		e, isErr := err.(*api.Error)
		if !isErr || e.Code != api.CodeForbidden || !strings.Contains(e.Message, "no grant for you") {
			t.Fatalf("%s %s/%s: %v, want the one refusal", machine, area, role, err)
		}
		b, _ := json.Marshal(e)
		return string(b)
	}
	// A project-level member grant covers any area, up to member.
	ok(carl.ID, "m1", "billing", "", "guest", "billing")
	ok(carl.ID, "m2", "sprout", "member", "member", "sprout")
	// An area grant covers only its own area.
	ok(ana.ID, "m3", "sprout", "guest", "guest", "sprout")
	b1 := refused(ana.ID, "m4", "billing", "guest")
	// Asking for more than the grants give is the same refusal, and so is an
	// unknown area.
	b2 := refused(carl.ID, "m5", "sprout", "owner")
	b3 := refused(bob.ID, "m6", "nowhere", "guest")
	for _, b := range []string{b2, b3} {
		if b != b1 {
			t.Fatalf("refusals differ:\n%s\n%s", b1, b)
		}
	}
	// An owner grant gives an owner only when asked, in that area.
	ok(bob.ID, "m7", "billing", "owner", "owner", "billing")
	// A scoped tailnet join says no area: the whole project needs a whole
	// project grant.
	tn := carlNode(x)
	x.gh.Approver(ana.ID, 0)
	if _, err := tn.scoped(t, "orchid", "m8", "", "guest"); err == nil {
		t.Fatal("an area grant admitted a whole-project guest")
	}
	_ = ctx
}

// An explicit lower request never downgrades the node: the same node
// joining again as guest keeps its stronger token.
func TestScopedJoinNeverDowngrades(t *testing.T) {
	x := scopedFixture(t)
	carl, _ := x.gh.AddUser("carlexample", "User")
	grantTo(t, x, "carlexample", "owner", "")
	n := carlNode(x) // the tailnet: a tunnel join is a guest's
	x.gh.Approver(carl.ID, 0)
	if out, err := n.scoped(t, "orchid", "host-b", "sprout", "member"); err != nil || out.Join.Cap.Role != "member" {
		t.Fatalf("first: %+v %v", out, err)
	}
	x.gh.Approver(carl.ID, 0)
	out, err := n.scoped(t, "orchid", "host-b", "sprout", "guest")
	if err != nil || out.Join.Cap.Role != "member" {
		t.Fatalf("a lower request: %+v %v, want the stronger member kept", out, err)
	}
}

func TestTunnelJoinRules(t *testing.T) {
	x := scopedFixture(t)
	carl, _ := x.gh.AddUser("carlexample", "User")
	grantTo(t, x, "carlexample", "owner", "")
	x.gh.Approver(carl.ID, 0)
	n := tunnelNode(x, "zmkey:eeeeeeeeeeeeeeeeeeeeeeeeee")
	// No area over the tunnel: a clear 400, and no login started.
	var st api.GitHubLoginStart
	err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b"}, &st)
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "one area") {
		t.Fatalf("tunnel without an area: %v", err)
	}
	if x.gh.Starts() != 0 {
		t.Fatalf("GitHub asked %d times for a refused start", x.gh.Starts())
	}
	// A tunnel join is a guest's: a member or owner is a clear 400, with
	// no login started (nobody is asked to approve a stranger's code).
	for _, role := range []string{"member", "owner"} {
		err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b", Area: "sprout", Role: role}, &st)
		if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "guest") || !strings.Contains(err.Error(), "tailnet") {
			t.Fatalf("tunnel role %s: %v", role, err)
		}
	}
	if x.gh.Starts() != 0 {
		t.Fatalf("GitHub asked %d times for refused starts", x.gh.Starts())
	}
	if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b", Area: "sprout", Role: "guest"}, &st); err != nil {
		t.Fatalf("explicit guest: %v", err)
	}
	// A bad role or area is a 400 too.
	for _, r := range []api.GitHubJoinRequest{
		{Project: "orchid", Machine: "host-b", Area: "sprout", Role: "admin"},
		{Project: "orchid", Machine: "host-b", Area: "../x"},
	} {
		if err := n.do(t, "POST", "/v0/join/github", r, &st); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	// The authority's own key is refused, as on the tailnet.
	self := node{x: x, rm: core.Remote{Via: core.ViaTunnel, NodeID: "zmkey:ffffffffffffffffffffffffff", Self: true}}
	if err := self.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b", Area: "sprout"}, &st); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the home's own key: %v", err)
	}
	// Per-node limits still apply to a key: four in flight (one is already).
	x.gh.Approver(carl.ID, 1000)
	for i := 0; i < 3; i++ {
		if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b", Area: "sprout"}, &st); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if err := n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b", Area: "sprout"}, &st); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a fifth login for one key: %v", err)
	}
}

// Over the tailnet nothing changes without area and role: the strongest
// grant, in the grant's own area.
func TestTailnetJoinWithoutScopeIsTheStrongestGrant(t *testing.T) {
	x := scopedFixture(t)
	carl, _ := x.gh.AddUser("carlexample", "User")
	grantTo(t, x, "carlexample", "member", "")
	grantTo(t, x, "carlexample", "owner", "billing")
	x.gh.Approver(carl.ID, 0)
	out, err := carlNode(x).join(t, "orchid", "host-b")
	if err != nil || out.Join == nil || out.Join.Cap.Role != "owner" || out.Join.Cap.Area != "billing" {
		t.Fatalf("join = %+v %v, want owner in billing", out, err)
	}
}

// Unauthenticated tunnel callers have their own small pool, and limits per
// client address; refused starts leave nothing behind.
func TestTunnelPoolAndAddressLimits(t *testing.T) {
	x := scopedFixture(t)
	carl, _ := x.gh.AddUser("carlexample", "User")
	grantTo(t, x, "carlexample", "member", "")
	x.gh.Approver(carl.ID, 1000)
	start := func(n node, machine string) error {
		var st api.GitHubLoginStart
		return n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: machine, Area: "sprout"}, &st)
	}
	from := func(key, addr string) node {
		return node{x: x, rm: core.Remote{Via: core.ViaTunnel, NodeID: "zmkey:" + key, Addr: addr}}
	}
	// The pool: 2 in all, whatever the keys and addresses; the tailnet's own
	// pool is untouched.
	x.svc.MaxTunnel = 2
	for i, a := range []string{"198.51.100.1", "198.51.100.2"} {
		if err := start(from(fmt.Sprint("k", i), a), "m"+fmt.Sprint(i)); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if err := start(from("k9", "198.51.100.9"), "m9"); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a third tunnel login: %v, want rate limited", err)
	}
	var st api.GitHubLoginStart
	if err := carlNode(x).do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "host-b"}, &st); err != nil {
		t.Fatalf("a tailnet start with the tunnel pool full: %v", err)
	}
	// The refused key left no entry in the start limits.
	before := owners.StartsLen(x.svc)
	for i := 0; i < 5; i++ {
		_ = start(from(fmt.Sprint("minted", i), "198.51.100.50"), "mm")
	}
	if got := owners.StartsLen(x.svc); got != before {
		t.Fatalf("%d owners remembered after refused starts, was %d", got, before)
	}
	// Per address, in flight: keys are free to make, an address is not.
	x.svc.MaxTunnel, x.svc.MaxPerAddr = 100, 2
	for i := 0; i < 2; i++ {
		if err := start(from(fmt.Sprint("a", i), "203.0.113.5"), "ma"+fmt.Sprint(i)); err != nil {
			t.Fatalf("address start %d: %v", i, err)
		}
	}
	if err := start(from("a2", "203.0.113.5"), "ma2"); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a third login from one address: %v", err)
	}
	if err := start(from("b0", "203.0.113.6"), "mb0"); err != nil {
		t.Fatalf("another address: %v", err)
	}
	// Per address, per hour (even with nothing in flight).
	x.svc.MaxPerAddr, x.svc.MaxAddrStarts = 100, 2
	for i := 0; i < 2; i++ {
		if err := start(from(fmt.Sprint("c", i), "203.0.113.7"), "mc"+fmt.Sprint(i)); err != nil {
			t.Fatalf("hourly start %d: %v", i, err)
		}
	}
	if err := start(from("c2", "203.0.113.7"), "mc2"); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a third start this hour from one address: %v", err)
	}
}

// IPv6 clients are limited by their /64, IPv4 by the exact address.
func TestAddressLimitsBucketIPv6(t *testing.T) {
	x := scopedFixture(t)
	carl, _ := x.gh.AddUser("carlexample", "User")
	grantTo(t, x, "carlexample", "member", "")
	x.gh.Approver(carl.ID, 1000)
	x.svc.MaxPerAddr = 2
	start := func(key, addr string) error {
		var st api.GitHubLoginStart
		n := node{x: x, rm: core.Remote{Via: core.ViaTunnel, NodeID: "zmkey:" + key, Addr: addr}}
		return n.do(t, "POST", "/v0/join/github", api.GitHubJoinRequest{Project: "orchid", Machine: "m" + key, Area: "sprout"}, &st)
	}
	for i, a := range []string{"2001:db8:1:2::1", "2001:db8:1:2:aaaa:bbbb:cccc:dddd"} {
		if err := start(fmt.Sprint("v", i), a); err != nil {
			t.Fatalf("v6 start %d: %v", i, err)
		}
	}
	if err := start("v2", "2001:db8:1:2:1234::9"); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a third address in one /64: %v, want rate limited", err)
	}
	if err := start("w0", "2001:db8:1:3::1"); err != nil {
		t.Fatalf("another /64: %v", err)
	}
	// IPv4: the exact address, so a neighbour is not limited.
	for i := 0; i < 2; i++ {
		if err := start(fmt.Sprint("x", i), "192.0.2.10"); err != nil {
			t.Fatalf("v4 start %d: %v", i, err)
		}
	}
	if err := start("x2", "192.0.2.10"); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a third from one IPv4 address: %v", err)
	}
	if err := start("y0", "192.0.2.11"); err != nil {
		t.Fatalf("a neighbouring IPv4 address: %v", err)
	}
}
