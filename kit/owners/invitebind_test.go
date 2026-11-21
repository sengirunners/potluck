package owners_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/project"
)

const homeFP = "hhhhhhhhhhhhhhhhhhhhhhhhhh" // 26: a tunnel home's key fingerprint

// redeemOverTunnel makes an invite to orchid (area, role) as identity as
// and redeems it from key over the tunnel, as the door does once the proof
// matched: the member's key-bound token.
func redeemOverTunnel(t *testing.T, x *ox, as, area, role, key, machine string) api.RedeemResponse {
	t.Helper()
	ctx := context.Background()
	reg := x.dm.Deps.Projects
	inv, err := reg.CreateInvite(ctx, reg.Local(""), project.InviteInput{
		InviteRequest: api.InviteRequest{Project: "orchid", Area: area, As: as, Role: role},
		Authority:     "mini", Fingerprint: homeFP, URL: "https://home.example"})
	if err != nil {
		t.Fatal(err)
	}
	secret := inv.Code[strings.LastIndex(inv.Code, ":")+1:]
	hash := reg.MatchProof(ctx, project.RedeemProof(secret, homeFP, key, machine), homeFP, key, machine)
	red, err := reg.Redeem(ctx, project.RedeemInput{Hash: hash, Node: key, Machine: machine})
	if err != nil {
		t.Fatal(err)
	}
	return red
}

// doToken is n.do with a bearer token: an error for any refusal.
func (n node) doToken(t *testing.T, token, method, path string, in, out any) error {
	t.Helper()
	var body bytes.Buffer
	if in != nil {
		_ = json.NewEncoder(&body).Encode(in)
	}
	req := httptest.NewRequest(method, path, &body)
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	if token != "" {
		req.Header.Set(api.HeaderAuthorization, "Bearer "+token)
	}
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
		return api.Errorf(api.CodeInternal, "%d %s", rec.Code, rec.Body.String())
	}
	if out != nil {
		return json.Unmarshal(rec.Body.Bytes(), out)
	}
	return nil
}

// works says whether token reads orchid from n.
func (n node) works(t *testing.T, token string) bool {
	t.Helper()
	var p api.Project
	return n.doToken(t, token, "GET", "/v0/projects/orchid", nil, &p) == nil && p.Slug == "orchid"
}

func (n node) startInvite(t *testing.T, token string) (api.GitHubLoginStart, error) {
	t.Helper()
	var st api.GitHubLoginStart
	err := n.doToken(t, token, "POST", "/v0/login/github/start", api.GitHubLoginRequest{Purpose: owners.PurposeInvite}, &st)
	return st, err
}

// A friend joins over the tunnel with a guest invite to one area, then
// signs in with GitHub: the identity the invite made is linked to the
// account, and the owner's GitHub grant (owner of the whole project) now
// applies to that key, exactly as a GitHub join's would: a new token at the
// stronger role, the invite's token gone. Revoking the grant takes back
// what it gave.
func TestInviteGitHubAppliesTheGrant(t *testing.T) {
	x := scopedFixture(t)
	ctx := context.Background()
	carl, _ := x.gh.AddUser("carlexample", "User")
	grantTo(t, x, "carlexample", "owner", "")
	key := "zmkey:aaaaaaaaaaaaaaaaaaaaaaaaaa"
	n := tunnelNode(x, key)
	red := redeemOverTunnel(t, x, "carla", "sprout", "guest", key, "carla-mac")
	if red.Cap.Role != api.RoleGuest || red.Cap.Area != "sprout" || !n.works(t, red.Token) {
		t.Fatalf("redeemed: %+v", red.Cap)
	}
	x.gh.Approver(carl.ID, 1)
	st, err := n.startInvite(t, red.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.For, "identity carla") || !strings.Contains(st.For, "carla-mac") || !strings.Contains(st.For, key) || !strings.Contains(st.For, "after an invite") {
		t.Fatalf("the approver is told: %q", st.For)
	}
	// Another key, with the id and secret, collects nothing.
	var none api.GitHubLoginOutcome
	if err := tunnelNode(x, "zmkey:bbbbbbbbbbbbbbbbbbbbbbbbbb").do(t, "POST", "/v0/login/github/"+st.ID, api.GitHubLoginPoll{Secret: st.Secret}, &none); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("another key collecting: %v", err)
	}
	out, err := n.join2(t, st)
	if err != nil || out.Join == nil || out.Link == nil {
		t.Fatalf("outcome: %+v %v", out, err)
	}
	if c := out.Join.Cap; c.Role != api.RoleOwner || c.Area != "" || out.Join.Identity != "carla" || out.Join.Machine != "carla-mac" || out.Link.Login != "carlexample" {
		t.Fatalf("admitted: %+v, link %+v", out.Join, out.Link)
	}
	if got := x.linkedTo(t, "carla"); got != "github:"+jsonNum(carl.ID) {
		t.Fatalf("carla linked to %q", got)
	}
	// Both work until the member gives the invite's back (a lost answer
	// never leaves it with no live token); then only the new one.
	if !n.works(t, red.Token) || !n.works(t, out.Join.Token) {
		t.Fatal("the invite's token and the new one should both work until the member gives the old one back")
	}
	if err := n.doToken(t, red.Token, "POST", "/v0/team/leave", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if n.works(t, red.Token) || !n.works(t, out.Join.Token) {
		t.Fatal("giving the invite's token back revoked the wrong one")
	}
	if tunnelNode(x, "zmkey:bbbbbbbbbbbbbbbbbbbbbbbbbb").works(t, out.Join.Token) {
		t.Fatal("the new token works from another key")
	}
	// The project's owners are told who signed in, and how to undo it.
	if told := strings.Join(x.mail.to("orchid", "owners"), "\n"); !strings.Contains(told, "@carlexample") || !strings.Contains(told, "carla") || !strings.Contains(told, "revoke") {
		t.Fatalf("owners told: %q", told)
	}
	// The outcome is spent.
	if _, err := n.join2(t, st); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("collecting again: %v", err)
	}
	// Revoking the GitHub grant takes back what it gave.
	if _, err := x.owner.RevokeGitHub(ctx, api.GitHubGrantRequest{Owner: "@carlexample", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if n.works(t, out.Join.Token) {
		t.Fatal("the token the grant gave outlived its revocation")
	}
	var rows int
	if err := x.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM grants g JOIN identities i ON i.id = g.subject
		WHERE i.name = 'carla' AND g.area = '' AND g.project = (SELECT project FROM grants WHERE subject = i.id LIMIT 1)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("carla's whole-project grant after the revocation: %d rows %v", rows, err)
	}
}

// With no GitHub grant for the account the sign-in only links: the invite's
// token keeps working, at the invite's role.
func TestInviteGitHubWithoutAGrantOnlyLinks(t *testing.T) {
	x := scopedFixture(t)
	dana, _ := x.gh.AddUser("danaexample", "User")
	key := "zmkey:cccccccccccccccccccccccccc"
	n := tunnelNode(x, key)
	red := redeemOverTunnel(t, x, "dana", "sprout", "guest", key, "dana-box")
	x.gh.Approver(dana.ID, 0)
	st, err := n.startInvite(t, red.Token)
	if err != nil {
		t.Fatal(err)
	}
	out, err := n.join2(t, st)
	if err != nil || out.Join != nil || out.Link == nil || out.Link.Identity != "dana" || out.Link.Login != "danaexample" {
		t.Fatalf("outcome: %+v %v", out, err)
	}
	if !n.works(t, red.Token) {
		t.Fatal("the invite's token stopped working")
	}
	if got := x.linkedTo(t, "dana"); got != "github:"+jsonNum(dana.ID) {
		t.Fatalf("dana linked to %q", got)
	}
}

// Who may start one: only a replica's node-bound token from its own node,
// a person's identity; an identity linked to another account is a
// conflict at collection and keeps its invite.
func TestInviteGitHubRefusals(t *testing.T) {
	x := scopedFixture(t)
	ctx := context.Background()
	erin, _ := x.gh.AddUser("erinexample", "User")
	other, _ := x.gh.AddUser("otherexample", "User")
	grantTo(t, x, "erinexample", "member", "")
	key := "zmkey:dddddddddddddddddddddddddd"
	n := tunnelNode(x, key)
	red := redeemOverTunnel(t, x, "erin", "sprout", "guest", key, "erin-box")
	// No token: refused before anything starts.
	if _, err := n.startInvite(t, ""); err == nil {
		t.Fatal("a start with no token passed")
	}
	// The token from another key is not this node's.
	if _, err := tunnelNode(x, "zmkey:eeeeeeeeeeeeeeeeeeeeeeeeee").startInvite(t, red.Token); err == nil {
		t.Fatal("the token from another key started a sign-in")
	}
	// The machine owner's socket has no invite to sign in after.
	if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{Purpose: owners.PurposeInvite}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the socket: %v", err)
	}
	if x.gh.Starts() != 0 {
		t.Fatalf("GitHub asked %d times for refused starts", x.gh.Starts())
	}
	// erin is linked to another account already: the sign-in by erinexample
	// is a conflict, nothing is admitted, and the invite's token stands.
	who, err := x.dm.Deps.Projects.Identity(ctx, red.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := owners.LinkNow(x.svc, ctx, who, acct(other, "User")); err != nil {
		t.Fatal(err)
	}
	x.gh.Approver(erin.ID, 0)
	st, err := n.startInvite(t, red.Token)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := n.join2(t, st); !api.IsCode(err, api.CodeConflict) || out.Join != nil {
		t.Fatalf("linked elsewhere: %+v %v", out, err)
	}
	if !n.works(t, red.Token) {
		t.Fatal("a refused sign-in cost the invite's token")
	}
}

// An account already linked to another identity here cannot sign in for
// the identity an invite made: an invitee who got a member to approve the
// code would otherwise take the member's GitHub grants. The invite stands.
func TestInviteGitHubRefusesAnotherIdentitysAccount(t *testing.T) {
	x := scopedFixture(t)
	ctx := context.Background()
	owner, _ := x.gh.AddUser("ownerexample", "User")
	grantTo(t, x, "ownerexample", "owner", "")
	if err := owners.LinkNow(x.svc, ctx, x.identity(t, "olga", "", ""), acct(owner, "User")); err != nil {
		t.Fatal(err)
	}
	key := "zmkey:ffffffffffffffffffffffffff"
	n := tunnelNode(x, key)
	red := redeemOverTunnel(t, x, "mallory", "sprout", "guest", key, "mallory-box")
	x.gh.Approver(owner.ID, 0) // olga's account approves mallory's code
	st, err := n.startInvite(t, red.Token)
	if err != nil {
		t.Fatal(err)
	}
	out, err := n.join2(t, st)
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "already identity olga") || out.Join != nil {
		t.Fatalf("another identity's account: %+v %v", out, err)
	}
	if x.linkedTo(t, "mallory") != "" || !n.works(t, red.Token) {
		t.Fatal("the refused sign-in linked mallory, or cost the invite's token")
	}
}

// A grant only applies when it gives more than the invite while keeping
// what it gave: a member grant on another area would cost the invite's
// area, and a guest grant gives nothing more, so both only link.
func TestInviteGitHubOnlyWidens(t *testing.T) {
	x := scopedFixture(t)
	for i, c := range []struct{ login, role, area string }{{"billingonly", "member", "billing"}, {"guestonly", "guest", "sprout"}} {
		u, _ := x.gh.AddUser(c.login, "User")
		grantTo(t, x, c.login, c.role, c.area)
		key := "zmkey:" + strings.Repeat(string(rune('p'+i)), 26)
		n := tunnelNode(x, key)
		red := redeemOverTunnel(t, x, "who"+c.login, "sprout", "guest", key, "box-"+c.login)
		x.gh.Approver(u.ID, 0)
		st, err := n.startInvite(t, red.Token)
		if err != nil {
			t.Fatal(err)
		}
		out, err := n.join2(t, st)
		if err != nil || out.Join != nil || out.Link == nil {
			t.Fatalf("%s: %+v %v, want the link alone", c.login, out, err)
		}
		if !n.works(t, red.Token) {
			t.Fatalf("%s: the invite's token stopped working", c.login)
		}
	}
	// A member grant on the invite's own area would raise the invite's
	// grant in place, which revoking the GitHub grant could not take back:
	// linked only, and the owner raises it by identity instead.
	u, _ := x.gh.AddUser("sproutmember", "User")
	grantTo(t, x, "sproutmember", "member", "sprout")
	key := "zmkey:zzzzzzzzzzzzzzzzzzzzzzzzzz"
	n := tunnelNode(x, key)
	red := redeemOverTunnel(t, x, "sprouter", "sprout", "guest", key, "box-sprouter")
	x.gh.Approver(u.ID, 0)
	st, err := n.startInvite(t, red.Token)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := n.join2(t, st); err != nil || out.Join != nil || out.Link == nil {
		t.Fatalf("member on the invite's own area: %+v %v, want the link alone", out, err)
	}
}

// A sign-in whose token is gone by the time it is collected (the member
// left, or the owner revoked the machine) admits nothing.
func TestInviteGitHubNeedsItsTokenLive(t *testing.T) {
	x := scopedFixture(t)
	hal, _ := x.gh.AddUser("halexample", "User")
	grantTo(t, x, "halexample", "owner", "")
	key := "zmkey:gggggggggggggggggggggggggg"
	n := tunnelNode(x, key)
	red := redeemOverTunnel(t, x, "hal", "sprout", "guest", key, "hal-box")
	x.gh.Approver(hal.ID, 2)
	st, err := n.startInvite(t, red.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.doToken(t, red.Token, "POST", "/v0/team/leave", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	if out, err := n.join2(t, st); !api.IsCode(err, api.CodeForbidden) || out.Join != nil {
		t.Fatalf("after leaving: %+v %v, want nothing admitted", out, err)
	}
	var live int
	if err := x.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM tokens WHERE node = ? AND revoked IS NULL`, key).Scan(&live); err != nil || live != 0 {
		t.Fatalf("live tokens for the key after leaving: %d %v", live, err)
	}
}
