package owners_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
)

// fakeKeys stands in for the approvals brick behind core.Deps.KeyLogin.
type fakeKeys struct {
	mu       sync.Mutex
	checkErr error
	bindErr  error
	checks   []core.KeyStart
	hdrs     []http.Header
	binds    []core.KeyBind
}

func (f *fakeKeys) Check(_ context.Context, _ *authz.Subject, hdr http.Header, ks core.KeyStart) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks, f.hdrs = append(f.checks, ks), append(f.hdrs, hdr)
	return f.checkErr
}

func (f *fakeKeys) Bind(ctx context.Context, tx *sql.Tx, b core.KeyBind) (api.KeyBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The link is already in tx: the bind sees it.
	var acc string
	if err := tx.QueryRowContext(ctx, `SELECT account FROM owner_links WHERE identity = ?`, b.Identity.ID).Scan(&acc); err != nil || acc != b.Account {
		return api.KeyBinding{}, api.Errorf(api.CodeInternal, "the link is not in the bind's transaction: %q %v", acc, err)
	}
	f.binds = append(f.binds, b)
	if f.bindErr != nil {
		return api.KeyBinding{}, f.bindErr
	}
	// Account and Machine here are wrong on purpose: the authority's own win.
	return api.KeyBinding{Approver: "key-" + b.Machine + "-0011223344556677", Fingerprint: "p256:0011223344556677",
		State: "active", ReceiptKey: "rk-test", Account: "github:999", Machine: "elsewhere"}, nil
}

func (f *fakeKeys) counts() (checks, binds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.checks), len(f.binds)
}

// newKeyOX is an authority whose KeyLogin is f, with project orchid.
func newKeyOX(t *testing.T, f *fakeKeys) *ox {
	t.Helper()
	x := newOX(t, "Iv1.fakeclient", func(d *core.Deps) { d.KeyLogin = f })
	x.projects(t, "orchid")
	return x
}

func keyReq() api.GitHubLoginRequest {
	return api.GitHubLoginRequest{Purpose: owners.PurposeKey, Project: "orchid",
		KeyAttach: api.KeyAttach{Key: "BAAA", Device: "laptop", Install: "inst-1", Nonce: "n-1", Time: "2026-01-02T03:04:05Z", Proof: "sig"}}
}

// collect polls login st as sub at the service until it ends.
func collect(t *testing.T, x *ox, sub *authz.Subject, st api.GitHubLoginStart) (api.GitHubLoginOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		out, err := x.svc.Poll(ctx, sub, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000})
		if err != nil || !out.Pending {
			return out, err
		}
	}
}

func (x *ox) linkedTo(t *testing.T, identity string) string {
	t.Helper()
	var acc string
	err := x.dm.Deps.Store.Registry().QueryRow(`SELECT l.account FROM owner_links l JOIN identities i ON i.id = l.identity WHERE i.name = ?`, identity).Scan(&acc)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return acc
}

// The socket owner binds a key: one login links the identity and binds the
// key; the bind gets the authority's view of node, machine and account;
// the outcome is collected once; signing in again with the same account
// binds again without a new link.
func TestKeyLoginBinds(t *testing.T) {
	f := &fakeKeys{}
	x := newKeyOX(t, f)
	ctx := context.Background()
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 2)
	var st api.GitHubLoginStart
	if err := x.owner.Do(ctx, http.MethodPost, "/v0/login/github/start?project=orchid", keyReq(), &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.For, "signing key on machine mini") {
		t.Errorf("for = %q", st.For)
	}
	if out, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 1}); err != nil || !out.Pending {
		t.Fatalf("pending poll: %+v %v", out, err)
	}
	if _, binds := f.counts(); binds != 0 {
		t.Fatalf("bound before the outcome was collected")
	}
	var out api.GitHubLoginOutcome
	for {
		var err error
		if out, err = x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000}); err != nil {
			t.Fatal(err)
		}
		if !out.Pending {
			break
		}
	}
	acc := "github:" + jsonNum(ana.ID)
	k := out.Key
	if k == nil || k.Account != acc || k.Login != "ana" || k.Machine != "mini" || k.Approver != "key-mini-0011223344556677" || k.State != "active" || out.Link != nil {
		t.Fatalf("outcome = %+v (key %+v)", out, k)
	}
	if len(f.checks) != 1 || f.checks[0].Node != core.KeyNodeLocal || f.checks[0].Machine != "mini" || f.checks[0].Project.Slug != "orchid" ||
		f.checks[0].Key != keyReq().KeyAttach || f.hdrs[0] == nil {
		t.Fatalf("checks = %+v", f.checks)
	}
	if b := f.binds[0]; b.Identity.Name != "judy" || b.Account != acc || b.Login != "ana" || b.Node != core.KeyNodeLocal || b.Key.Proof != "sig" {
		t.Fatalf("bind = %+v", b)
	}
	if got := x.linkedTo(t, "judy"); got != acc {
		t.Fatalf("judy linked to %q", got)
	}
	mail := x.mail.to("orchid", "human:judy")
	if len(mail) != 2 || !strings.Contains(mail[0], "was linked to identity judy") || !strings.Contains(mail[1], "now answers as GitHub @ana") || !strings.Contains(mail[1], "from machine mini") {
		t.Fatalf("judy's mail = %q", mail)
	}
	if ms := x.mail.to("orchid", "owners"); len(ms) != 2 {
		t.Fatalf("owners' mail = %q", ms)
	}
	// Replaying the collected outcome is the 404 of a missing login.
	if _, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("replay: %v", err)
	}
	if _, binds := f.counts(); binds != 1 {
		t.Fatalf("binds = %d after a replay", binds)
	}
	// Already linked to the same account: binds, no new link mail.
	x.gh.Approver(ana.ID, 0)
	st2, err := x.owner.StartGitHubLogin(ctx, keyReq())
	if err != nil {
		t.Fatal(err)
	}
	owner := &authz.Subject{Identity: x.identity(t, "judy", "", ""), Via: authz.ViaLocal}
	if out, err := collect(t, x, owner, st2); err != nil || out.Key == nil {
		t.Fatalf("second bind: %+v %v", out, err)
	}
	if mail := x.mail.to("orchid", "human:judy"); len(mail) != 3 || strings.Contains(mail[2], "was linked") {
		t.Fatalf("judy's mail after a second bind = %q", mail)
	}
}

// Who may start a key login, and what the start checks before it
// reserves anything.
func TestKeyLoginStartRefusals(t *testing.T) {
	ctx := context.Background()
	// An authority without the hook answers as an old one does.
	if _, err := newOX(t, "Iv1.fakeclient").owner.StartGitHubLogin(ctx, keyReq()); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), `unknown login purpose "key"`) {
		t.Fatalf("no hook: %v", err)
	}
	f := &fakeKeys{}
	x := newKeyOX(t, f)
	mint, err := x.owner.MintToken(ctx, api.MintTokenRequest{As: "judy", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*client.Client{"socket": client.WithHandler(x.dm.SocketHandler()), "network": client.WithHandler(x.dm.Handler)} {
		c.Token = mint.Secret
		if _, err := c.StartGitHubLogin(ctx, keyReq()); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("minted token on the %s: %v, want forbidden", name, err)
		}
	}
	judy := x.identity(t, "judy", "", "")
	agent := api.Identity{ID: "01AGENT", Name: "bot-1", Kind: "agent"}
	for what, tc := range map[string]struct {
		sub  *authz.Subject
		code string
	}{
		"a tailnet login":       {&authz.Subject{Identity: judy, Via: authz.ViaTailnet, Remote: true, Scope: authz.All()}, api.CodeForbidden},
		"a token with no node":  {&authz.Subject{Identity: judy, Via: authz.ViaToken, Remote: true, TokenID: "t1", Scope: authz.All()}, api.CodeForbidden},
		"an agent's node token": {&authz.Subject{Identity: agent, Via: authz.ViaToken, Remote: true, TokenID: "t2", Node: "n2", Scope: authz.All()}, api.CodeForbidden},
		"no subject":            {nil, api.CodeUnauthenticated},
	} {
		if _, err := x.svc.StartLogin(ctx, tc.sub, keyReq()); !api.IsCode(err, tc.code) {
			t.Errorf("%s: %v, want %s", what, err, tc.code)
		}
	}
	noProject := keyReq()
	noProject.Project = ""
	if _, err := x.owner.StartGitHubLogin(ctx, noProject); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("no project: %v", err)
	}
	var st api.GitHubLoginStart
	if err := x.owner.Do(ctx, http.MethodPost, "/v0/login/github/start?project=other", keyReq(), &st); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("body and route disagree: %v", err)
	}
	if checks, _ := f.counts(); checks != 0 {
		t.Fatalf("refused callers reached the check: %d", checks)
	}
	// The check refuses: nothing is reserved and GitHub is never asked.
	f.checkErr = api.Errorf(api.CodeForbidden, "bad proof")
	before := x.gh.Starts()
	if _, err := x.owner.StartGitHubLogin(ctx, keyReq()); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("check refused: %v", err)
	}
	if x.gh.Starts() != before {
		t.Fatalf("GitHub was asked after the check refused")
	}
}

// Collects that must link nothing and bind nothing.
func TestKeyLoginCollectRefusals(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, x *ox) error {
		t.Helper()
		st, err := x.owner.StartGitHubLogin(ctx, keyReq())
		if err != nil {
			t.Fatal(err)
		}
		_, err = collect(t, x, &authz.Subject{Identity: x.identity(t, "judy", "", ""), Via: authz.ViaLocal}, st)
		return err
	}
	t.Run("an org approves", func(t *testing.T) {
		f := &fakeKeys{}
		x := newKeyOX(t, f)
		org, _ := x.gh.AddUser("acme", "Organization")
		x.gh.Approver(org.ID, 0)
		if err := run(t, x); !api.IsCode(err, api.CodeForbidden) {
			t.Fatalf("%v", err)
		}
		if _, b := f.counts(); b != 0 || x.linkedTo(t, "judy") != "" {
			t.Fatalf("binds %d, link %q", b, x.linkedTo(t, "judy"))
		}
	})
	t.Run("identity linked to another account", func(t *testing.T) {
		f := &fakeKeys{}
		x := newKeyOX(t, f)
		bob, _ := x.gh.AddUser("bob", "User")
		ana, _ := x.gh.AddUser("ana", "User")
		if err := owners.LinkNow(x.svc, ctx, x.identity(t, "judy", "", ""), owners.Account{ID: "github:" + jsonNum(bob.ID), Login: "bob", Kind: owners.KindUser}); err != nil {
			t.Fatal(err)
		}
		x.gh.Approver(ana.ID, 0)
		if err := run(t, x); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("%v", err)
		}
		if _, b := f.counts(); b != 0 || x.linkedTo(t, "judy") != "github:"+jsonNum(bob.ID) {
			t.Fatalf("binds %d, link %q", b, x.linkedTo(t, "judy"))
		}
	})
	t.Run("account linked to another identity", func(t *testing.T) {
		f := &fakeKeys{}
		x := newKeyOX(t, f)
		ana, _ := x.gh.AddUser("ana", "User")
		acc := "github:" + jsonNum(ana.ID)
		if err := owners.LinkNow(x.svc, ctx, x.identity(t, "carl", "orchid", "member"), owners.Account{ID: acc, Login: "ana", Kind: owners.KindUser}); err != nil {
			t.Fatal(err)
		}
		x.gh.Approver(ana.ID, 0)
		err := run(t, x)
		if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "already belongs to another identity") {
			t.Fatalf("%v", err)
		}
		if _, b := f.counts(); b != 0 || x.linkedTo(t, "judy") != "" {
			t.Fatalf("binds %d, link %q", b, x.linkedTo(t, "judy"))
		}
	})
	t.Run("the bind refuses", func(t *testing.T) {
		f := &fakeKeys{bindErr: api.Errorf(api.CodeConflict, "that key is another person's")}
		x := newKeyOX(t, f)
		ana, _ := x.gh.AddUser("ana", "User")
		x.gh.Approver(ana.ID, 0)
		if err := run(t, x); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("%v", err)
		}
		if _, b := f.counts(); b != 1 {
			t.Fatalf("binds tried %d", b)
		}
		if got := x.linkedTo(t, "judy"); got != "" {
			t.Fatalf("a refused bind left a link: %q", got)
		}
		if ms := x.mail.to("orchid", "human:judy"); len(ms) != 0 {
			t.Fatalf("mail after a refused bind: %q", ms)
		}
	})
}

// The latest key login of a person from one node wins; one from another
// node, or another purpose, is left alone; only the starting node collects.
func TestKeyLoginLatestWins(t *testing.T) {
	f := &fakeKeys{}
	x := newKeyOX(t, f)
	ctx := context.Background()
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 1000) // stays pending
	judy := x.identity(t, "judy", "", "")
	owner := &authz.Subject{Identity: judy, Address: "human:judy", Via: authz.ViaLocal, Scope: authz.All()}
	member := &authz.Subject{Identity: judy, Address: "human:judy", Via: authz.ViaToken, Remote: true, TokenID: "t1", Node: "n1", Machine: "laptop", Scope: authz.All()}
	first, err := x.svc.StartLogin(ctx, owner, keyReq())
	if err != nil {
		t.Fatal(err)
	}
	link, err := x.svc.StartLogin(ctx, owner, api.GitHubLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := x.svc.StartLogin(ctx, member, keyReq())
	if err != nil {
		t.Fatal(err)
	}
	if f.checks[1].Node != "n1" || f.checks[1].Machine != "laptop" {
		t.Fatalf("member's check = %+v", f.checks[1])
	}
	second, err := x.svc.StartLogin(ctx, owner, keyReq())
	if err != nil {
		t.Fatal(err)
	}
	poll := func(sub *authz.Subject, st api.GitHubLoginStart) error {
		_, err := x.svc.Poll(ctx, sub, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 1})
		return err
	}
	if err := poll(owner, first); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("the replaced login: %v, want not_found", err)
	}
	for what, tc := range map[string]struct {
		sub *authz.Subject
		st  api.GitHubLoginStart
	}{"the latest": {owner, second}, "another node's": {member, other}, "a link": {owner, link}} {
		if err := poll(tc.sub, tc.st); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	// The same identity from another node does not collect this node's login.
	if err := poll(member, second); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("collect from another node: %v", err)
	}
	// A poll already waiting when its login is replaced answers the 404 too
	// (not a retryable error) and binds nothing.
	a, err := x.svc.StartLogin(ctx, member, keyReq())
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() {
		_, err := x.svc.Poll(ctx, member, a.ID, api.GitHubLoginPoll{Secret: a.Secret, WaitMS: 10000})
		waiting <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := x.svc.StartLogin(ctx, member, keyReq()); err != nil {
		t.Fatal(err)
	}
	if err := <-waiting; !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a waiting poll on a replaced login: %v, want not_found", err)
	}
	if _, b := f.counts(); b != 0 {
		t.Fatalf("binds = %d", b)
	}
}

// Key logins keep their own limits: other logins of the same identity
// filling the in-flight limit and the socket's pool do not stop a key
// login, and a key start the hourly limit refuses drops no earlier key
// login.
func TestKeyLoginLimits(t *testing.T) {
	f := &fakeKeys{}
	x := newKeyOX(t, f)
	ctx := context.Background()
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 1000) // stays pending
	x.svc.MaxPerOwner, x.svc.MaxLocal = 2, 2
	for i := 0; i < 2; i++ {
		if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{}); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a third link: %v, want rate_limited", err)
	}
	first, err := x.owner.StartGitHubLogin(ctx, keyReq())
	if err != nil {
		t.Fatalf("a key login behind full link limits: %v", err)
	}
	x.svc.MaxStarts = 1
	if _, err := x.owner.StartGitHubLogin(ctx, keyReq()); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("a key start over its hourly limit: %v, want rate_limited", err)
	}
	owner := &authz.Subject{Identity: x.identity(t, "judy", "", ""), Via: authz.ViaLocal}
	if out, err := x.svc.Poll(ctx, owner, first.ID, api.GitHubLoginPoll{Secret: first.Secret, WaitMS: 1}); err != nil || !out.Pending {
		t.Fatalf("the earlier key login after a refused start: %+v %v", out, err)
	}
}
