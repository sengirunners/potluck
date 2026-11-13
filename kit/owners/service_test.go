package owners_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/owners/githubfake"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/wiring"
)

func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "DOLT_") {
			os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

// syncBuf is a log sink the daemon's goroutines may write to.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type ox struct {
	gh    *githubfake.GitHub
	cfg   *config.Config
	dm    *server.Daemon
	svc   *owners.Service
	owner *client.Client
	log   *syncBuf
	close func()
	// viewer is the GitHub account whose token is the authority's lookup
	// token (github.lookup_token): tests make it a member of the orgs they
	// grant.
	viewer    int64
	viewerTok string
	mail      *mailRec
}

// mailRec is the daemon's notifier (core.Deps.AddNotifier): owners tells
// people through it, and the kit wiring has no mail module (zm's mail
// module registers one that stores and routes it).
type mailRec struct {
	mu   sync.Mutex
	sent []sentMail
}

type sentMail struct{ project, to, body string }

func (r *mailRec) notify(_ context.Context, n core.Notice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, sentMail{n.Project.Slug, n.To, n.Body})
	return nil
}

// to is the bodies sent in project to to, in order.
func (r *mailRec) to(project, to string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.sent {
		if m.project == project && m.to == to {
			out = append(out, m.body)
		}
	}
	return out
}

func newOX(t *testing.T, clientID string) *ox {
	t.Helper()
	gh := githubfake.New(t)
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	viewer, vtok := gh.AddUser("zm-viewer", "User")
	vfile := filepath.Join(home, "gh-lookup")
	if err := os.WriteFile(vfile, []byte(vtok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.File.GitHub = config.GitHub{ClientID: clientID, APIURL: gh.URL, WebURL: gh.URL, LookupToken: "file:" + vfile}
	log, rec := &syncBuf{}, &mailRec{}
	wire := func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Kit(d)
		d.AddNotifier(rec.notify)
		return mods, err
	}
	dm, err := server.New(context.Background(), cfg, slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug})), wire)
	if err != nil {
		t.Fatal(err)
	}
	x := &ox{gh: gh, cfg: cfg, dm: dm, owner: client.WithHandler(dm.SocketHandler()), log: log, close: sync.OnceFunc(dm.Close), viewer: viewer.ID, viewerTok: vtok, mail: rec}
	t.Cleanup(x.close)
	for _, m := range dm.Modules() {
		if s, ok := m.(*owners.Service); ok {
			x.svc = s
		}
	}
	if x.svc == nil {
		t.Fatal("the owners module is not wired")
	}
	x.svc.DecideFloor = 0 // tests that need it set it
	return x
}

// login runs a whole login as c: start, then poll until it ends.
func login(t *testing.T, c *client.Client) (api.GitHubLoginStart, api.GitHubLoginOutcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := c.StartGitHubLogin(ctx, api.GitHubLoginRequest{Purpose: "link"})
	if err != nil {
		return st, api.GitHubLoginOutcome{}, err
	}
	for {
		out, err := c.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000})
		if err != nil || !out.Pending {
			return st, out, err
		}
	}
}

func TestLinkSocketOwner(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ana, _ := x.gh.AddUser("Ana", "User")
	x.gh.Approver(ana.ID, 1)
	st, out, err := login(t, x.owner)
	if err != nil {
		t.Fatal(err)
	}
	if st.UserCode == "" || !strings.HasPrefix(st.VerificationURI, x.gh.URL) || st.Secret == "" {
		t.Fatalf("start = %+v", st)
	}
	want := "github:" + jsonNum(ana.ID)
	l := out.Link
	if l == nil || l.Identity != "judy" || l.Account != want || l.Login != "Ana" || l.Linked.IsZero() {
		t.Fatalf("outcome = %+v, want judy -> %s @Ana", out, want)
	}
	// The link shows in the owner read (the socket owner owns its projects).
	ctx := context.Background()
	if _, err := x.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if st, err := x.owner.Owner(ctx, "orchid"); err != nil || !st.Owner || st.Identity != "judy" || st.GitHub != "Ana" {
		t.Fatalf("owner read = %+v %v", st, err)
	}
	// Collected once: a second poll is the same 404 as an unknown id.
	if _, err := x.owner.PollGitHubLogin(context.Background(), st.ID, api.GitHubLoginPoll{Secret: st.Secret}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("second collect: %v, want not_found", err)
	}
	// A GitHub rename keeps the link (by id); linking again refreshes the
	// login and keeps when it was made.
	x.gh.Rename(ana.ID, "ana-c")
	_, again, err := login(t, x.owner)
	if err != nil || again.Link.Account != want || again.Link.Login != "ana-c" || !again.Link.Linked.Equal(l.Linked) {
		t.Fatalf("relink = %+v %v", again, err)
	}
	// Another GitHub account approving for the same identity: never moved.
	bob, _ := x.gh.AddUser("bob", "User")
	x.gh.Approver(bob.ID, 0)
	if _, _, err := login(t, x.owner); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("second account: %v, want conflict", err)
	}
}

func TestLoginRefusals(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	org, _ := x.gh.AddUser("acme", "Organization")
	for what, tc := range map[string]struct {
		approver int64
		code     string
	}{"declined": {0, api.CodeForbidden}, "expired": {-1, api.CodeForbidden}, "an org": {org.ID, api.CodeForbidden}} {
		x.gh.Approver(tc.approver, 0)
		if _, _, err := login(t, x.owner); !api.IsCode(err, tc.code) {
			t.Errorf("%s: %v, want %s", what, err, tc.code)
		}
	}
	// A minted token (an agent's, a CI's) never starts a link, on the
	// socket or the network, even for its own identity.
	mint, err := x.owner.MintToken(ctx, api.MintTokenRequest{As: "judy", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*client.Client{"socket": client.WithHandler(x.dm.SocketHandler()), "network": client.WithHandler(x.dm.Handler)} {
		c.Token = mint.Secret
		if _, err := c.StartGitHubLogin(ctx, api.GitHubLoginRequest{}); !api.IsCode(err, api.CodeForbidden) {
			t.Errorf("minted token on the %s: %v, want forbidden", name, err)
		}
	}
	if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{Purpose: "steal"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("unknown purpose: %v", err)
	}
	// No client id: no logins.
	if _, err := newOX(t, "").owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{}); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("no client id: %v, want unavailable", err)
	}
}

// Only the starter, with the secret, collects; everything else is the
// same 404 as an unknown login.
func TestPollIsTheStartersOnly(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 100) // stays pending
	st, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	mint, _ := x.owner.MintToken(ctx, api.MintTokenRequest{As: "carl", Role: "owner"})
	other := client.WithHandler(x.dm.Handler)
	other.Token = mint.Secret
	anon := client.WithHandler(x.dm.Handler)
	for what, tc := range map[string]struct {
		c      *client.Client
		id, sc string
	}{"wrong secret": {x.owner, st.ID, st.Secret + "x"}, "unknown id": {x.owner, "01JUNKNOWN", st.Secret},
		"another identity": {other, st.ID, st.Secret}, "unauthenticated": {anon, st.ID, st.Secret}} {
		_, err := tc.c.PollGitHubLogin(ctx, tc.id, api.GitHubLoginPoll{Secret: tc.sc})
		e, _ := err.(*api.Error)
		if e == nil || e.Code != api.CodeNotFound || !strings.HasPrefix(e.Message, "no login ") {
			t.Errorf("%s: %v, want not_found", what, err)
		}
	}
	if out, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 50}); err != nil || !out.Pending {
		t.Fatalf("the starter while pending: %+v %v", out, err)
	}
	// In-flight limit per identity.
	x.svc.MaxPerOwner = 1
	if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{}); !api.IsCode(err, api.CodeRateLimited) {
		t.Fatalf("over the limit: %v", err)
	}
}

// The account linked to judy links carl too: one person, two names. judy
// is told; linking carl again is a refresh, told to no one; carl cannot
// move to another account; an agent identity never starts a link.
func TestLinkManyIdentities(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	x.projects(t, "orchid")
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 0)
	if _, _, err := login(t, x.owner); err != nil {
		t.Fatal(err)
	}
	// carl's own replica token (node-bound), simulated at the service.
	if _, err := x.owner.Grant(ctx, api.GrantRequest{Identity: "carl", Project: "orchid", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	ids, _ := x.owner.Identities(ctx)
	var carl api.Identity
	for _, id := range ids {
		if id.Name == "carl" {
			carl = id
		}
	}
	sub := &authz.Subject{Identity: carl, Address: "human:carl", Via: authz.ViaToken, Remote: true, TokenID: "t1", Node: "n1"}
	link := func() (api.GitHubLoginOutcome, error) {
		t.Helper()
		st, err := x.svc.StartLogin(ctx, sub, api.GitHubLoginRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return x.svc.Poll(ctx, sub, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 20000})
	}
	x.gh.Approver(ana.ID, 0)
	if out, err := link(); err != nil || out.Link == nil || out.Link.Identity != "carl" || out.Link.Login != "ana" {
		t.Fatalf("a second identity of the account: %+v, %v", out, err)
	}
	var n int
	if err := x.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM owner_links WHERE account = ?`, "github:"+jsonNum(ana.ID)).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the account's links: %d, %v; want 2", n, err)
	}
	told := x.mail.to("orchid", "human:judy")
	if len(told) != 2 || !strings.Contains(told[1], "also linked to identity carl") || !strings.Contains(told[1], "zm identity unlink github carl") {
		t.Fatalf("judy's mail = %q", told)
	}
	if ms := x.mail.to("orchid", "human:carl"); len(ms) != 1 || !strings.Contains(ms[0], "linked to identity carl") {
		t.Fatalf("carl's mail = %q", ms)
	}
	if acc, login := x.svc.LinkedAccount(ctx, carl.ID); acc != "github:"+jsonNum(ana.ID) || login != "ana" {
		t.Fatalf("carl's link: %q %q", acc, login)
	}
	x.gh.Approver(ana.ID, 0)
	if _, err := link(); err != nil {
		t.Fatalf("linking carl again: %v", err)
	}
	if ms := x.mail.to("orchid", "human:judy"); len(ms) != 2 {
		t.Fatalf("a refresh told judy: %q", ms)
	}
	bob, _ := x.gh.AddUser("bob", "User")
	x.gh.Approver(bob.ID, 0)
	if _, err := link(); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("carl to another account: %v, want conflict", err)
	}
	// Unlinking one leaves the other.
	if _, err := x.owner.UnlinkGitHubIdentity(ctx, "carl", false); err != nil {
		t.Fatal(err)
	}
	if x.svc.GitHubLogin(ctx, carl.ID) != "" || x.svc.GitHubLogin(ctx, x.dm.Deps.Projects.LocalIdentity().ID) != "ana" {
		t.Fatal("unlinking carl did not leave judy's link alone")
	}
	bot, err := x.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "worker", Kind: api.IdentityAgent})
	if err != nil {
		t.Fatal(err)
	}
	asBot := *sub
	asBot.Identity = bot
	if _, err := x.svc.StartLogin(ctx, &asBot, api.GitHubLoginRequest{}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an agent identity: %v, want forbidden", err)
	}
}

// No GitHub token is stored, logged or answered.
func TestTokenNeverKept(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 0)
	st, out, err := login(t, x.owner)
	if err != nil {
		t.Fatal(err)
	}
	tokens := x.gh.Issued()
	if len(tokens) != 1 {
		t.Fatalf("GitHub issued %d device tokens, want 1", len(tokens))
	}
	b, _ := json.Marshal([]any{st, out})
	x.close() // flush the registry to disk
	for _, f := range []string{"", "-wal", "-shm"} {
		if db, _ := os.ReadFile(x.cfg.RegistryPath() + f); bytes.Contains(db, []byte(tokens[0])) {
			t.Fatalf("registry.db%s holds the token", f)
		}
	}
	for what, s := range map[string]string{"answers": string(b), "log": x.log.String()} {
		if strings.Contains(s, tokens[0]) {
			t.Fatalf("the %s hold the token", what)
		}
	}
}

func jsonNum(n int64) string { b, _ := json.Marshal(n); return string(b) }

// The authority acts only when the outcome is collected: GitHub's answer
// alone links nothing, and an uncollected outcome expires with nothing done.
func TestActOnCollect(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	x.svc.Collect = 200 * time.Millisecond
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 0)
	st, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until GitHub has answered (the fake issued the token).
	for deadline := time.Now().Add(10 * time.Second); len(x.gh.Issued()) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("GitHub never answered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	var n int
	if err := x.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM owner_links`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("linked before collection: %d %v", n, err)
	}
	time.Sleep(300 * time.Millisecond) // past Collect
	if _, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an expired outcome: %v, want not_found", err)
	}
	if err := x.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM owner_links`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an uncollected outcome linked: %d %v", n, err)
	}
}

// The start answer says who is asking, and its poll secret has at least
// 128 bits.
func TestStartSaysWhoAndSecret(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 100)
	a, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Authority != "mini" || a.For != "identity judy" {
		t.Fatalf("who: %q %q", a.Authority, a.For)
	}
	raw, err := base64.RawURLEncoding.DecodeString(a.Secret)
	if err != nil || len(raw)*8 < 128 || owners.SecretBytes*8 < 128 || a.Secret == b.Secret {
		t.Fatalf("secret %q: %d bits, %v", a.Secret, len(raw)*8, err)
	}
}

// A first link mails the identity and the owners; unlink removes it (by
// the owner, never by a stranger), and the account links again afresh.
func TestLinkMailUnlinkRelink(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	if _, err := x.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 0)
	if _, _, err := login(t, x.owner); err != nil {
		t.Fatal(err)
	}
	ms := x.mail.to("orchid", "human:judy")
	if len(ms) != 1 || !strings.Contains(ms[0], "@ana") || !strings.Contains(ms[0], "this machine (mini)") ||
		!strings.Contains(ms[0], "zm identity unlink github judy") {
		t.Fatalf("link mail = %q", ms)
	}
	// A stranger cannot unlink (the 404 of a missing identity).
	mint, _ := x.owner.MintToken(ctx, api.MintTokenRequest{As: "eve", Role: "member", Project: "orchid"})
	eve := client.WithHandler(x.dm.Handler)
	eve.Token = mint.Secret
	if _, err := eve.UnlinkGitHubIdentity(ctx, "judy", false); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a member unlinking another: %v", err)
	}
	// judy's own minted token (an agent's, a CI's) is capped: it may not
	// unlink him either, on the socket or the network.
	own, _ := x.owner.MintToken(ctx, api.MintTokenRequest{As: "judy", Role: "guest", Project: "orchid"})
	for name, c := range map[string]*client.Client{"socket": client.WithHandler(x.dm.SocketHandler()), "network": client.WithHandler(x.dm.Handler)} {
		c.Token = own.Secret
		if _, err := c.UnlinkGitHubIdentity(ctx, "judy", true); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("his own minted token on the %s unlinking him: %v, want not_found", name, err)
		}
	}
	u, err := x.owner.UnlinkGitHubIdentity(ctx, "judy", true)
	if err != nil || u.Login != "ana" || u.Revoked == nil {
		t.Fatalf("unlink = %+v %v", u, err)
	}
	if _, err := x.owner.UnlinkGitHubIdentity(ctx, "judy", false); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("unlink twice: %v", err)
	}
	// Another account links now (the old one was released), as a fresh link.
	bob, _ := x.gh.AddUser("bob", "User")
	x.gh.Approver(bob.ID, 0)
	if _, out, err := login(t, x.owner); err != nil || out.Link.Login != "bob" {
		t.Fatalf("relink = %+v %v", out, err)
	}
}

// Concurrent starts cannot pass the limits together: the slot is reserved
// before GitHub is asked.
func TestConcurrentStartsKeepTheLimit(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ctx := context.Background()
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 100)
	x.svc.MaxLocal = 2
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !api.IsCode(err, api.CodeRateLimited) {
				t.Errorf("start: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 2 || x.gh.Starts() != 2 {
		t.Fatalf("%d starts succeeded and GitHub was asked %d times, want 2 and 2", ok, x.gh.Starts())
	}
}

// A collector that goes away while waiting does not spend the outcome.
func TestDroppedCollectorKeepsTheOutcome(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 0)
	st, err := x.owner.StartGitHubLogin(context.Background(), api.GitHubLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = x.owner.PollGitHubLogin(gone, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 20000})
	if err == nil {
		t.Fatal("a cancelled poll answered")
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	for {
		out, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000})
		if err != nil {
			t.Fatalf("after a dropped poll: %v", err)
		}
		if !out.Pending {
			if out.Link == nil || out.Link.Login != "ana" {
				t.Fatalf("outcome %+v", out)
			}
			return
		}
	}
}

// An act that fails for a passing reason leaves the outcome collectable:
// the next collect links (it would answer the 404 of a spent outcome if the
// login were marked collected before the act).
func TestTransientActFailureKeepsTheOutcome(t *testing.T) {
	x := newOX(t, "Iv1.fakeclient")
	ana, _ := x.gh.AddUser("ana", "User")
	x.gh.Approver(ana.ID, 0)
	fails := 1
	owners.SetActFault(x.svc, func() error {
		if fails > 0 {
			fails--
			return api.Errorf(api.CodeUnavailable, "the registry is busy")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := x.owner.StartGitHubLogin(ctx, api.GitHubLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var first error
	for {
		out, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000})
		if err == nil && out.Pending {
			continue
		}
		first = err
		break
	}
	if !api.IsCode(first, api.CodeUnavailable) {
		t.Fatalf("the failing act: %v, want unavailable", first)
	}
	out, err := x.owner.PollGitHubLogin(ctx, st.ID, api.GitHubLoginPoll{Secret: st.Secret, WaitMS: 5000})
	if err != nil || out.Link == nil || out.Link.Login != "ana" {
		t.Fatalf("the collect after a passing failure: %+v %v, want the link", out, err)
	}
}
