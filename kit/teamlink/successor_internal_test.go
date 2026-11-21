package teamlink

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
	"github.com/sengirunners/potluck/kit/store"
)

const succProject = "01KPROJECT0000000000000000"

// fakeTransport is a tailnet whose homes are handlers: at says which key
// answers at each address (host:port), homes is each key's handler. A
// client pinned to another key than the one at the address gets
// core.ErrPinMismatch, as the tailnet's own does.
type fakeTransport struct {
	core.TeamTransport        // the methods a follow does not use
	via                string // "" is the tailnet
	at                 map[string]string
	homes              map[string]http.Handler
}

func (f *fakeTransport) Via() string                 { return cmp.Or(f.via, api.ViaTailnet) }
func (f *fakeTransport) Explain(string, error) error { return nil }
func (f *fakeTransport) Client(target core.JoinTarget, opts core.ClientOptions) *http.Client {
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		for k, vs := range opts.Extra {
			r.Header[k] = vs
		}
		fp, ok := f.at[r.URL.Host]
		if !ok {
			return nil, errors.New("no route to " + r.URL.Host)
		}
		if fp != target.Fingerprint {
			return nil, core.ErrPinMismatch
		}
		pr, pw := io.Pipe()
		w := &streamWriter{h: http.Header{}, pw: pw, head: make(chan struct{})}
		go func() {
			f.homes[fp].ServeHTTP(w, r)
			w.WriteHeader(http.StatusOK)
			pw.Close()
		}()
		select {
		case <-w.head:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: w.code, Header: w.h, Body: pr, Request: r}, nil
	})}
}

// streamWriter is a handler's side of a fake call: its answer streams to
// the caller as written, and the handler sees the caller go (its request's
// context ends).
type streamWriter struct {
	h    http.Header
	pw   *io.PipeWriter
	code int
	once sync.Once
	head chan struct{}
}

func (w *streamWriter) Header() http.Header { return w.h }
func (w *streamWriter) WriteHeader(code int) {
	w.once.Do(func() { w.code = code; close(w.head) })
}
func (w *streamWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(b)
}
func (w *streamWriter) Flush() {}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// termHome answers a heartbeat with *a in its authority part, recording
// the token each beat carried.
func termHome(a *AuthorityAnswer, tokens *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*tokens = append(*tokens, r.Header.Get(api.HeaderAuthorization))
		b, _ := json.Marshal(*a)
		api.WriteJSON(w, http.StatusOK, api.PresenceResponse{Parts: map[string]json.RawMessage{AuthorityPart: b}})
	})
}

// successorService is a Service on a temp home with one joined remote of
// succProject at http://potluck-mini:7433 (key sha256:old), its key bound and
// its token file written, over ft (nothing answers anywhere until a test
// says so); log collects what it logs.
func successorService(t *testing.T) (*Service, *Remote, *bytes.Buffer) {
	s, x, log, _ := successorServiceOn(t)
	return s, x, log
}

func successorServiceOn(t *testing.T) (*Service, *Remote, *bytes.Buffer, *fakeTransport) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, Migrations...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var log bytes.Buffer
	ft := &fakeTransport{at: map[string]string{}, homes: map[string]http.Handler{}}
	d := &core.Deps{Store: st, Config: &config.Config{Home: home, Machine: "laptop"}, Log: slog.New(slog.NewTextHandler(&log, nil))}
	d.AddTransport(ft)
	s := &Service{d: d, remotes: map[string]*Remote{}}
	s.beat.kick = make(chan struct{}, 1)
	x := &Remote{Token: "tok", HC: s.client(core.JoinTarget{Via: api.ViaTailnet, Fingerprint: "sha256:old"}, nil), R: api.Remote{
		Project: api.ProjectRef{ID: succProject, Slug: "orchid"}, Authority: "mini", URL: "http://potluck-mini:7433",
		Fingerprint: "sha256:old", Via: api.ViaTailnet, Machine: "laptop", Identity: "ana", Joined: time.Now().UTC(), State: api.RemoteOK,
		TokenFile: s.TokenPath(succProject)}}
	if err := WriteFileAtomic(x.R.TokenFile, []byte("tok\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.bindAndSave(ctx, x, api.TeamCap{Project: succProject, Slug: "orchid"}, api.ViaTailnet, "sha256:old", "mini"); err != nil {
		t.Fatal(err)
	}
	s.remotes[succProject] = x
	return s, x, &log, ft
}

func successor(fp string) Successor {
	return Successor{Peer: hometerm.Peer{Via: api.ViaTailnet, URL: "http://potluck-mini2:7433", Fingerprint: fp}, Machine: "mini2"}
}

// A member follows a successor only when it answers for the same project
// that it serves, at an epoch above the one recorded.
func TestFollowable(t *testing.T) {
	for _, c := range []struct {
		name string
		a    AuthorityAnswer
		ok   bool
	}{
		{"serves higher", AuthorityAnswer{Project: succProject, State: hometerm.Serving, Epoch: 3}, true},
		{"equal epoch", AuthorityAnswer{Project: succProject, State: hometerm.Serving, Epoch: 2}, false},
		{"lower epoch", AuthorityAnswer{Project: succProject, State: hometerm.Serving, Epoch: 1}, false},
		{"another project", AuthorityAnswer{Project: "01KOTHER000000000000000000", State: hometerm.Serving, Epoch: 3}, false},
		{"standby", AuthorityAnswer{Project: succProject, State: hometerm.Standby, Epoch: 3}, false},
		{"frozen", AuthorityAnswer{Project: succProject, State: hometerm.Frozen, Epoch: 3}, false},
		{"fenced", AuthorityAnswer{Project: succProject, State: hometerm.Fenced, Epoch: 3}, false},
	} {
		if err := followable(succProject, 2, c.a); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// What a home answers in its beats is recorded, kept over a restart, and
// not taken when it is for another project or at a lower epoch; a home that
// names no successor any more takes its successor away.
func TestHeardHome(t *testing.T) {
	ctx := context.Background()
	s, x, _ := successorService(t)
	heard := func(a AuthorityAnswer) {
		t.Helper()
		b, _ := json.Marshal(a)
		s.heardHome(ctx, x, b)
	}
	loaded := func() HomeView {
		t.Helper()
		after := &Service{d: s.d, remotes: map[string]*Remote{}}
		if err := after.Load(ctx); err != nil {
			t.Fatal(err)
		}
		return after.ByName(succProject).Home()
	}
	succ := successor("sha256:new")
	heard(AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 2, State: hometerm.Serving, Successor: &succ})
	if h := x.Home(); h.Epoch != 2 || h.Heard.IsZero() || h.Successor == nil || *h.Successor != succ || h.NamedEpoch != 2 {
		t.Fatalf("after an answer: %+v", h)
	}
	if h := loaded(); h.Epoch != 2 || h.Heard.IsZero() || h.Successor == nil || *h.Successor != succ || h.NamedEpoch != 2 {
		t.Fatalf("after a restart: %+v", h)
	}
	other := successor("sha256:other")
	heard(AuthorityAnswer{Project: "01KOTHER000000000000000000", Epoch: 5, State: hometerm.Serving, Successor: &other})
	heard(AuthorityAnswer{Project: succProject, Epoch: 1, State: hometerm.Serving, Successor: &other})
	if h := x.Home(); h.Epoch != 2 || *h.Successor != succ {
		t.Fatalf("after answers for another project and at a lower epoch: %+v", h)
	}
	// A fenced home answers its successor's epoch: not the home's.
	heard(AuthorityAnswer{Project: succProject, Epoch: 4, State: hometerm.Fenced, Successor: &succ})
	if h := x.Home(); h.Epoch != 2 || *h.Successor != succ {
		t.Fatalf("after a fenced answer: %+v", h)
	}
	heard(AuthorityAnswer{Project: succProject, Epoch: 2, State: hometerm.Serving})
	if h := x.Home(); h.Successor != nil {
		t.Fatalf("after an answer that names none: %+v", h)
	}
	if h := loaded(); h.Successor != nil || h.Epoch != 2 {
		t.Fatalf("after a restart, none named: %+v", h)
	}
}

// A repoint rebinds the project key from the home's key to the
// successor's, saves the new address, epoch and machine, uses up the named
// successor, clears the 401 count, logs the rebinding and leaves the token
// file; it is refused, changing nothing, when the id is bound to another key
// than the home's or than the one the caller saw.
func TestRepoint(t *testing.T) {
	ctx := context.Background()
	s, x, log := successorService(t)
	succ := successor("sha256:new")
	b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Epoch: 0, State: hometerm.Serving, Successor: &succ})
	s.heardHome(ctx, x, b)
	var dropped bool
	s.Hooks.DropForgotten = []func(context.Context, string) ([]string, error){func(context.Context, string) ([]string, error) {
		dropped = true
		return nil, nil
	}}
	var old api.Remote
	s.Hooks.Repointed = func(_ *Remote, o api.Remote) { old = o }
	x.Unauth = []time.Time{time.Now()}
	at1 := AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 1, State: hometerm.Serving}

	if err := s.repoint(ctx, x, succ, at1, "sha256:other", "test"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a repoint naming another bound key: %v", err)
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_project_keys SET fingerprint = 'sha256:else'`); err != nil {
		t.Fatal(err)
	}
	if err := s.repoint(ctx, x, succ, at1, "", "test"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a repoint when the id is bound to another key than the home's: %v", err)
	}
	if r := x.Snapshot(); r.Fingerprint != "sha256:old" || len(x.Unauth) != 1 || x.Home().Successor == nil {
		t.Fatalf("a refused repoint changed the remote: %+v", r)
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_project_keys SET fingerprint = 'sha256:old'`); err != nil {
		t.Fatal(err)
	}

	if err := s.repoint(ctx, x, succ, at1, "sha256:old", "test"); err != nil {
		t.Fatal(err)
	}
	if r := x.Snapshot(); r.URL != succ.URL || r.Fingerprint != "sha256:new" || r.Authority != "mini" || x.Unauth != nil {
		t.Fatalf("the remote after a repoint: %+v (401s %v)", r, x.Unauth)
	}
	if h := x.Home(); h.Epoch != 1 || h.Successor != nil {
		t.Fatalf("its home after a repoint: %+v", h)
	}
	if k, err := s.projectKey(ctx, succProject); err != nil || k.Fingerprint != "sha256:new" || k.Authority != "mini" {
		t.Fatalf("the project key after a repoint: %+v %v", k, err)
	}
	if !strings.Contains(log.String(), "project key is rebound") || !strings.Contains(log.String(), "from_fingerprint=sha256:old") {
		t.Fatalf("the repoint was not logged: %s", log.String())
	}
	if old.Fingerprint != "sha256:old" {
		t.Fatalf("Repointed's old remote: %+v", old)
	}
	if dropped {
		t.Fatal("a repoint dropped the host state")
	}
	if b, err := os.ReadFile(x.R.TokenFile); err != nil || string(b) != "tok\n" {
		t.Fatalf("the token file after a repoint: %q %v", b, err)
	}
	after := &Service{d: s.d, remotes: map[string]*Remote{}}
	if err := after.Load(ctx); err != nil {
		t.Fatal(err)
	}
	y := after.ByName(succProject)
	if r := y.Snapshot(); r.URL != succ.URL || r.Fingerprint != "sha256:new" || r.Authority != "mini" || y.Home().Epoch != 1 || y.Home().Successor != nil {
		t.Fatalf("after a restart: %+v %+v", r, y.Home())
	}
}

// The owner's follow repoints to a successor its home never named when it
// names the bound key and the successor, reached at its pinned key with this
// machine's token, serves the project at a higher epoch; it refuses one at
// the recorded epoch, one whose key is not the one answering, and a join that
// was left.
func TestFollow(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	succ := successor("sha256:new")
	ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 0, State: hometerm.Serving}
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	unmoved := func(when string) {
		t.Helper()
		if r := x.Snapshot(); r.Fingerprint != "sha256:old" {
			t.Fatalf("%s: repointed to %+v", when, r)
		}
	}
	if _, err := s.Follow(ctx, succProject, succ, "sha256:old", FollowOptions{}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a follow to a successor at the recorded epoch: %v", err)
	}
	unmoved("equal epoch")
	ans.Epoch = 1
	ft.at["potluck-mini2:7433"] = "sha256:evil"
	if _, err := s.Follow(ctx, succProject, succ, "sha256:old", FollowOptions{}); err == nil {
		t.Fatal("a follow to an address another key answers at")
	}
	unmoved("pin mismatch")
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ans.Project = "01KOTHER000000000000000000"
	if _, err := s.Follow(ctx, succProject, succ, "sha256:old", FollowOptions{}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a follow to a successor that serves another project: %v", err)
	}
	unmoved("another project")
	ans.Project = succProject
	r, err := s.Follow(ctx, succProject, succ, "sha256:old", FollowOptions{})
	if err != nil || r.URL != succ.URL || r.Fingerprint != "sha256:new" || r.Authority != "mini2" {
		t.Fatalf("follow: %+v %v", r, err)
	}
	if len(tokens) == 0 || tokens[len(tokens)-1] != "Bearer tok" {
		t.Fatalf("the successor was asked with %q, want this machine's token", tokens)
	}
	if k, err := s.projectKey(ctx, succProject); err != nil || k.Fingerprint != "sha256:new" {
		t.Fatalf("the project key after a follow: %+v %v", k, err)
	}
	// A left join is not repointed.
	ans.Epoch = 2
	x.Mu.Lock()
	x.Left = true
	x.Mu.Unlock()
	if err := s.repoint(ctx, x, successor("sha256:new"), ans, "", "test"); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a repoint of a left join: %v", err)
	}
}

// A home that answers it does not serve, naming its successor, sends the
// member there once the whole beat is applied (followIfAway), not while its
// answer is being taken; a serving home's does not.
func TestFollowIfAway(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	succ := successor("sha256:new")
	ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 1, State: hometerm.Serving}
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	heard := func(st hometerm.State) {
		t.Helper()
		b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 0, State: st, Successor: &succ})
		s.heardHome(ctx, x, b)
	}
	heard(hometerm.Serving)
	s.followIfAway(ctx, x)
	if x.Snapshot().Fingerprint != "sha256:old" || len(tokens) != 0 {
		t.Fatalf("followed while the home serves: %+v (asked %d)", x.Snapshot(), len(tokens))
	}
	heard(hometerm.Frozen)
	if x.Snapshot().Fingerprint != "sha256:old" {
		t.Fatal("repointed while the home's answer was being taken")
	}
	s.followIfAway(ctx, x)
	if r := x.Snapshot(); r.Fingerprint != "sha256:new" || x.Home().Epoch != 1 {
		t.Fatalf("after a frozen home's beat: %+v %+v", r, x.Home())
	}
	if c, named, err := s.SuccessorClient(succProject); err == nil || c != nil || named.URL != "" {
		t.Fatalf("a successor client once the named one is used up: %v %+v %v", c, named, err)
	}
}

// A home is away only when it does not answer in its own words: every
// answer of its own, a refusal or unavailable, is the home itself.
func TestAwayErr(t *testing.T) {
	own := func(code string) error {
		e := api.Errorf(code, "x")
		e.HTTPStatus = 400
		return e
	}
	for _, c := range []struct {
		err  error
		away bool
	}{
		{errors.New("dial: connection refused"), true},
		{api.Errorf(api.CodeForbidden, "pin mismatch"), true}, // the transport's words, not the home's
		{api.Errorf(api.CodeUnavailable, "authority answered 502 Bad Gateway"), true},
		{own(api.CodeUnavailable), false},
		{own(api.CodeUnauthenticated), false},
		{own(api.CodeRescope), false},
		{own(api.CodeNotFound), false},
	} {
		if got := awayErr(c.err); got != c.away {
			t.Errorf("%v: away %v, want %v", c.err, got, c.away)
		}
	}
}

// A named successor is followed only when it answers for the same project
// that it serves, at an epoch above the one recorded: the home naming it is
// not enough.
func TestFollowNamedNeedsAServingSuccessorAbove(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	succ := successor("sha256:new")
	var ans AuthorityAnswer
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 1, State: hometerm.Frozen, Successor: &succ})
	s.heardHome(ctx, x, b)
	for _, c := range []struct {
		name string
		a    AuthorityAnswer
	}{
		{"equal epoch", AuthorityAnswer{Project: succProject, Epoch: 1, State: hometerm.Serving}},
		{"lower epoch", AuthorityAnswer{Project: succProject, Epoch: 0, State: hometerm.Serving}},
		{"another project", AuthorityAnswer{Project: "01KOTHER000000000000000000", Epoch: 2, State: hometerm.Serving}},
		{"standby", AuthorityAnswer{Project: succProject, Epoch: 2, State: hometerm.Standby}},
		{"epoch out of range", AuthorityAnswer{Project: succProject, Epoch: math.MaxInt64 + 1, State: hometerm.Serving}},
	} {
		ans = c.a
		s.followIfAway(ctx, x)
		if r := x.Snapshot(); r.Fingerprint != "sha256:old" {
			t.Fatalf("%s: followed to %+v", c.name, r)
		}
	}
	if len(tokens) != 5 {
		t.Fatalf("the successor was asked %d times, want 5", len(tokens))
	}
	ans = AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 2, State: hometerm.Serving}
	s.followIfAway(ctx, x)
	if r := x.Snapshot(); r.Fingerprint != "sha256:new" || r.URL != succ.URL || x.Home().Epoch != 2 {
		t.Fatalf("a successor serving above: %+v %+v", r, x.Home())
	}
}

// A home withdraws the successor it named, and its members drop it with
// their next beat.
func TestUndesignate(t *testing.T) {
	ctx := context.Background()
	s, x, _ := successorService(t) // the home and, for this test, its own member
	succ := successor("sha256:new")
	succ.Machine = "studio"
	beat := func() {
		t.Helper()
		raw, err := s.serveAuthority(ctx, api.Project{ID: succProject}, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.heardHome(ctx, x, raw)
	}
	if _, err := s.Undesignate(ctx); err != nil {
		t.Fatalf("withdrawing when none is named: %v", err)
	}
	if _, err := s.Designate(ctx, succ); err != nil {
		t.Fatal(err)
	}
	beat()
	if h := x.Home(); h.Successor == nil || *h.Successor != succ {
		t.Fatalf("the member after Designate: %+v", h)
	}
	tm, err := s.Undesignate(ctx)
	if err != nil || tm.Successor != nil {
		t.Fatalf("Undesignate: %+v %v", tm, err)
	}
	if tm, err := hometerm.Load(s.d.Config.Home); err != nil || tm.Successor != nil || tm.State != hometerm.Serving {
		t.Fatalf("the term after Undesignate: %+v %v", tm, err)
	}
	beat()
	if h := x.Home(); h.Successor != nil {
		t.Fatalf("the member after Undesignate: %+v", h)
	}
	var n int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT COUNT(*) FROM team_successors`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("successor rows after Undesignate: %d %v", n, err)
	}
	if _, err := hometerm.Apply(s.d.Config.Home, func(t hometerm.Term) (hometerm.Term, error) {
		return t.Fence(succ.Peer, t.Epoch+1)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Undesignate(ctx); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a fenced home withdrawing its successor: %v", err)
	}
}

// homeAt makes h answer at x's home (http://potluck-mini:7433, sha256:old).
func homeAt(ft *fakeTransport, h http.HandlerFunc) {
	ft.at["potluck-mini:7433"] = "sha256:old"
	ft.homes["sha256:old"] = h
}

// presence writes a beat answer whose authority part is a.
func presence(w http.ResponseWriter, a AuthorityAnswer, more map[string]json.RawMessage) {
	b, _ := json.Marshal(a)
	parts := map[string]json.RawMessage{AuthorityPart: b}
	for k, v := range more {
		parts[k] = v
	}
	api.WriteJSON(w, http.StatusOK, api.PresenceResponse{Parts: parts})
}

// opsRun counts the ops this machine took (TakeOps records each).
func opsRun(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if err := s.d.Store.Registry().QueryRow(`SELECT COUNT(*) FROM team_ops_run`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Silence is counted in this process from the first beat the home did not
// answer at all: not from when it last answered (a daemon that was down
// itself), and an answer in the home's own words, or any answer to a beat,
// starts it again.
func TestHomeSilent(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	t0 := time.Now()
	clock := t0
	homeNow = func() time.Time { return clock }
	t.Cleanup(func() { homeNow = time.Now })
	succ := successor("sha256:new")
	ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 1, State: hometerm.Serving}
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 0, State: hometerm.Serving, Successor: &succ})
	s.heardHome(ctx, x, b)
	x.Mu.Lock()
	x.home.Heard = t0.Add(-time.Hour) // last answered long ago, as after this daemon was down
	x.Mu.Unlock()
	silent := errors.New("dial: connection refused")
	at := func(d time.Duration, err error) {
		t.Helper()
		clock = t0.Add(d)
		s.homeSilent(ctx, x, err)
		if x.Snapshot().Fingerprint != "sha256:old" || len(tokens) != 0 {
			t.Fatalf("followed at +%s after %v: %+v (asked %d)", d, err, x.Snapshot(), len(tokens))
		}
	}
	at(0, silent)
	at(HomeSilentAfter-time.Second, silent)
	for _, code := range []string{api.CodeUnauthenticated, api.CodeForbidden, api.CodeNotFound, api.CodeConflict, api.CodeUnavailable} {
		e := api.Errorf(code, "the home's own answer")
		e.HTTPStatus = 400
		at(HomeSilentAfter, e)
	}
	at(HomeSilentAfter+time.Second, silent)
	at(2*HomeSilentAfter, silent)
	if !s.homeCurrent(ctx, x, b) { // the home answered a beat
		t.Fatal("a current answer was not taken")
	}
	at(2*HomeSilentAfter+time.Second, silent)
	clock = t0.Add(3*HomeSilentAfter + time.Second)
	s.homeSilent(ctx, x, silent)
	if r := x.Snapshot(); r.Fingerprint != "sha256:new" || r.URL != succ.URL {
		t.Fatalf("after the home was silent for the window: %+v", r)
	}
}

// A home whose daemon is parked answers every beat parked, in its own
// words: the member asks the successor it named at once, and follows it
// when it serves at a higher epoch.
func TestParkedHomeIsFollowedAtOnce(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	succ := successor("sha256:new")
	ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 1, State: hometerm.Standby}
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 0, State: hometerm.Serving, Successor: &succ})
	s.heardHome(ctx, x, b)
	parked := api.Errorf(api.CodeParked, "this home does not serve now: it is frozen")
	parked.HTTPStatus = http.StatusServiceUnavailable

	// The successor does not serve yet (a move in progress): asked, not followed.
	s.homeSilent(ctx, x, parked)
	if len(tokens) != 1 || x.Snapshot().Fingerprint != "sha256:old" {
		t.Fatalf("a parked home with a standby successor: asked %d, %+v", len(tokens), x.Snapshot())
	}
	// Another refusal in the home's own words asks nothing.
	other := api.Errorf(api.CodeUnavailable, "the home's own answer")
	other.HTTPStatus = http.StatusServiceUnavailable
	s.homeSilent(ctx, x, other)
	if len(tokens) != 1 {
		t.Fatalf("an unavailable answer asked the successor (%d)", len(tokens))
	}
	// Promoted: followed at the next parked answer.
	ans.State = hometerm.Serving
	s.homeSilent(ctx, x, parked)
	if r := x.Snapshot(); r.Fingerprint != "sha256:new" || r.URL != succ.URL {
		t.Fatalf("a parked home's promoted successor was not followed: %+v", r)
	}
}

// A join being left refuses its own calls; that is not its home's silence,
// and its token does not go to the successor.
func TestALeftJoinDoesNotAskTheSuccessor(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	was := HomeSilentAfter
	HomeSilentAfter = 0
	t.Cleanup(func() { HomeSilentAfter = was })
	succ := successor("sha256:new")
	ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 1, State: hometerm.Serving}
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 0, State: hometerm.Frozen, Successor: &succ})
	s.heardHome(ctx, x, b)
	x.Mu.Lock()
	x.Recovery.Closing = true
	x.Mu.Unlock()
	err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/presence", api.PresenceRequest{Project: succProject}, nil)
	if err == nil {
		t.Fatal("a join being left called its home")
	}
	s.homeSilent(ctx, x, err)
	s.followIfAway(ctx, x)
	if len(tokens) != 0 || x.Snapshot().Fingerprint != "sha256:old" {
		t.Fatalf("a join being left asked the successor %d times: %+v", len(tokens), x.Snapshot())
	}
}

// The heartbeat follows a home that answers it is frozen, naming a
// successor that serves above it.
func TestHeartbeatFollowsAFrozenHome(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	s.RegisterBeatPart(AuthorityPart, BeatPart{Request: s.requestAuthority})
	succ := successor("sha256:new")
	ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 2, State: hometerm.Serving}
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	homeAt(ft, func(w http.ResponseWriter, r *http.Request) {
		presence(w, AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 1, State: hometerm.Frozen, Successor: &succ}, nil)
	})
	s.BeatRemote(ctx, x)
	if r := x.Snapshot(); r.Fingerprint != "sha256:new" || r.URL != succ.URL || x.Home().Epoch != 2 {
		t.Fatalf("after a frozen home's beat: %+v %+v", r, x.Home())
	}
}

// An epoch no registry row can hold is not taken from a home's answer.
func TestAnEpochOutOfRangeIsNotTaken(t *testing.T) {
	ctx := context.Background()
	s, x, _ := successorService(t)
	b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Epoch: math.MaxInt64 + 1, State: hometerm.Serving})
	s.heardHome(ctx, x, b)
	if h := x.Home(); h.Epoch != 0 || !h.Heard.IsZero() {
		t.Fatalf("after an answer out of range: %+v", h)
	}
}

// The headers stored for the home (a service token) go to a successor only
// at the home's own address and transport.
func TestStoredHeadersStayAtTheirAddress(t *testing.T) {
	for _, c := range []struct {
		name string
		host string
		want string
	}{
		{"another address", "potluck-mini2:7433", ""},
		{"the same address", "potluck-mini:7433", "svc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, _, _, ft := successorServiceOn(t)
			if err := WriteFileAtomic(s.headersPath(succProject), encodeHeaders(http.Header{"X-Service-Token": {"svc"}})); err != nil {
				t.Fatal(err)
			}
			succ := successor("sha256:new")
			succ.URL = "http://" + c.host
			var got []string
			ft.at[c.host] = "sha256:new"
			ft.homes["sha256:new"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = append(got, r.Header.Get("X-Service-Token"))
				presence(w, AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 1, State: hometerm.Serving}, nil)
			})
			if _, err := s.Follow(ctx, succProject, succ, "sha256:old", FollowOptions{}); err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 || got[0] != c.want {
				t.Fatalf("the successor got the service header %q, want %q", got, c.want)
			}
			if _, err := os.Stat(s.headersPath(succProject)); (err == nil) != (c.want != "") {
				t.Fatalf("the stored headers after a follow to %s: %v", c.name, err)
			}
		})
	}
}

// Within one join a save that read the home before a beat raised its epoch
// does not lower it, nor clears that the home is behind; a re-join (another
// joined time) starts again: its epoch, not behind, and no successor the
// earlier join's home named, even after a restart.
func TestRejoinStartsTheHomeAgain(t *testing.T) {
	ctx := context.Background()
	s, x, _ := successorService(t)
	succ := successor("sha256:new")
	b, _ := json.Marshal(AuthorityAnswer{Project: succProject, Epoch: 3, State: hometerm.Serving, Successor: &succ})
	s.heardHome(ctx, x, b)
	load := func() HomeView {
		t.Helper()
		after := &Service{d: s.d, remotes: map[string]*Remote{}}
		if err := after.Load(ctx); err != nil {
			t.Fatal(err)
		}
		return after.ByName(succProject).Home()
	}
	stale := &Remote{Token: "tok", HC: x.HC, R: x.Snapshot()}
	if err := s.Save(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if h := load(); h.Epoch != 3 || h.Successor == nil {
		t.Fatalf("after a stale save: %+v", h)
	}
	lower, _ := json.Marshal(AuthorityAnswer{Project: succProject, Epoch: 1, State: hometerm.Serving})
	s.homeCurrent(ctx, x, lower)
	if !x.Home().Behind {
		t.Fatal("the home is not behind")
	}
	if err := s.Save(ctx, stale); err != nil { // a save within the join keeps it behind
		t.Fatal(err)
	}
	if h := load(); !h.Behind {
		t.Fatalf("after a save within the join: %+v", h)
	}
	r := x.Snapshot()
	r.Joined = r.Joined.Add(time.Second)
	nu := &Remote{Token: "tok2", HC: x.HC, R: r}
	if err := s.Save(ctx, nu); err != nil {
		t.Fatal(err)
	}
	if h := load(); h.Epoch != 0 || h.Successor != nil || h.Behind {
		t.Fatalf("after a re-join: %+v", h)
	}
	other := successor("sha256:other")
	late, _ := json.Marshal(AuthorityAnswer{Project: succProject, Epoch: 5, State: hometerm.Serving, Successor: &other})
	s.heardHome(ctx, x, late) // the earlier join's beat, answered late
	if h := load(); h.Epoch != 0 || h.Successor != nil {
		t.Fatalf("after the earlier join's late answer: %+v", h)
	}
}

// A withdrawn successor whose removal did not save is removed with the
// next answer, so a restart does not bring it back.
func TestAWithdrawnSuccessorStaysWithdrawn(t *testing.T) {
	ctx := context.Background()
	s, x, _ := successorService(t)
	succ := successor("sha256:new")
	named, _ := json.Marshal(AuthorityAnswer{Project: succProject, Epoch: 1, State: hometerm.Serving, Successor: &succ})
	none, _ := json.Marshal(AuthorityAnswer{Project: succProject, Epoch: 1, State: hometerm.Serving})
	s.heardHome(ctx, x, named)
	db := s.d.Store.Registry()
	if _, err := db.Exec(`ALTER TABLE team_successors RENAME TO team_successors_away`); err != nil {
		t.Fatal(err)
	}
	s.heardHome(ctx, x, none) // its save fails
	if _, err := db.Exec(`ALTER TABLE team_successors_away RENAME TO team_successors`); err != nil {
		t.Fatal(err)
	}
	s.heardHome(ctx, x, none)
	after := &Service{d: s.d, remotes: map[string]*Remote{}}
	if err := after.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if h := after.ByName(succProject).Home(); h.Successor != nil {
		t.Fatalf("after a restart: %+v", h)
	}
}

// behindHome is a member whose home recorded epoch 2 answers at *epoch
// (its authority part), with the "other" part registered; paths are the
// calls the home got, applied the other part's answers.
type behindHome struct {
	s       *Service
	x       *Remote
	log     *bytes.Buffer
	ft      *fakeTransport
	ans     AuthorityAnswer
	header  string // HeaderTeam on its answers
	status  int    // its beat answer's status (0: 200)
	scope   int64  // its scope_version (0: none): a call that sends another is 409 rescope, as core's Authenticate answers
	paths   []string
	scopes  []string // each call's path and the scope_version it sent ("" for none)
	bodies  []api.PresenceRequest
	applied []string
	onOps   func() // runs as a GET /v0/team/ops arrives, before its answer
}

func newBehindHome(t *testing.T) *behindHome {
	t.Helper()
	ctx := context.Background()
	s, x, log, ft := successorServiceOn(t)
	b := &behindHome{s: s, x: x, log: log, ft: ft, ans: AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 1, State: hometerm.Serving}}
	s.RegisterBeatPart(AuthorityPart, BeatPart{Request: s.requestAuthority})
	s.RegisterBeatPart("other", BeatPart{Request: func(context.Context, Beat) (json.RawMessage, func(context.Context, json.RawMessage), error) {
		return json.RawMessage(`"mine"`), func(_ context.Context, raw json.RawMessage) { b.applied = append(b.applied, string(raw)) }, nil
	}})
	two, _ := json.Marshal(AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 2, State: hometerm.Serving})
	s.heardHome(ctx, x, two)
	homeAt(ft, func(w http.ResponseWriter, r *http.Request) {
		b.paths = append(b.paths, r.URL.Path)
		b.scopes = append(b.scopes, r.URL.Path+"="+api.GetHeader(r.Header, api.HeaderScopeVersion))
		if b.header != "" {
			api.SetHeader(w.Header(), api.HeaderTeam, b.header)
		}
		if v := api.GetHeader(r.Header, api.HeaderScopeVersion); b.scope != 0 && v != "" && v != strconv.FormatInt(b.scope, 10) {
			api.WriteJSON(w, http.StatusConflict, api.ErrorResponse{Error: api.Errorf(api.CodeRescope, "your access to this project changed")})
			return
		}
		switch r.URL.Path {
		case "/v0/team/presence":
			var req api.PresenceRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			b.bodies = append(b.bodies, req)
			if b.status != 0 {
				api.WriteJSON(w, b.status, api.ErrorResponse{Error: api.Errorf(api.CodeUnauthenticated, "no")})
				return
			}
			presence(w, b.ans, map[string]json.RawMessage{"other": json.RawMessage(`"x"`)})
		case "/v0/team/ops":
			if b.onOps != nil {
				b.onOps()
			}
			api.WriteJSON(w, http.StatusOK, api.MachineOpList{Ops: []api.MachineOp{{ID: "op-" + strconv.Itoa(len(b.paths)), Kind: "nothing"}}})
		default:
			api.WriteJSON(w, http.StatusOK, struct{}{})
		}
	})
	return b
}

// A home that answers a beat at an epoch below the one this machine
// recorded has lost its term: nothing of its answer is taken, the remote
// says so (and keeps saying so after a restart), no call but the beat and
// a leave goes to it, the beat carries nothing but the authority part, its
// ops poll takes nothing, and it is current again when it answers the
// recorded epoch.
func TestAHomeBehind(t *testing.T) {
	ctx := context.Background()
	b := newBehindHome(t)
	s, x := b.s, b.x
	heard := x.Home().Heard
	evil := successor("sha256:evil")
	b.ans.State, b.ans.Successor = hometerm.Frozen, &evil
	s.BeatRemote(ctx, x)
	h := x.Home()
	if h.Epoch != 2 || h.Successor != nil || !h.Heard.Equal(heard) || len(b.applied) != 0 || !h.Behind || h.BehindEpoch != 1 {
		t.Fatalf("after a home behind answered: %+v (parts applied %q)", h, b.applied)
	}
	if r := x.Snapshot(); !strings.Contains(r.LastError, "is behind: it answers epoch 1, this machine recorded 2") {
		t.Fatalf("the remote does not say its home is behind: %q", r.LastError)
	}
	if !strings.Contains(b.log.String(), "below the one this machine recorded") {
		t.Fatalf("not reported: %s", b.log.String())
	}
	if _, err := s.d.Store.Registry().Exec(`INSERT INTO team_ops_run (id, project, kind, state, created) VALUES ('took-1', ?, 'nothing', 'done', 0)`, succProject); err != nil {
		t.Fatal(err)
	}
	b.paths = nil
	s.BeatRemote(ctx, x)
	if last := b.bodies[len(b.bodies)-1]; len(last.Parts) != 0 || len(last.Ops) != 0 || len(last.Took) != 0 {
		t.Fatalf("a beat to a home behind carried more than the beat: %+v", last)
	}
	b.paths = nil
	if err := s.CallRemote(ctx, x, http.MethodGet, "/v0/projects/"+succProject, nil, nil); !api.IsCode(err, api.CodeUnavailable) || len(b.paths) != 0 {
		t.Fatalf("a call to a home behind: %v (it got %q)", err, b.paths)
	}
	if err := s.pollOps(ctx, x, succProject); !api.IsCode(err, api.CodeUnavailable) || len(b.paths) != 0 || opsRun(t, s) != 1 {
		t.Fatalf("an ops poll of a home behind: %v (it got %q, %d ops run)", err, b.paths, opsRun(t, s))
	}
	if err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/leave", struct{}{}, nil); err != nil || len(b.paths) != 1 {
		t.Fatalf("a leave to a home behind: %v (it got %q)", err, b.paths)
	}
	after := &Service{d: s.d, remotes: map[string]*Remote{}}
	if err := after.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if y := after.ByName(succProject); !y.Home().Behind || !strings.Contains(y.Snapshot().LastError, "is behind") {
		t.Fatalf("after a restart: %+v %q", y.Home(), y.Snapshot().LastError)
	}

	// Current again when it answers the recorded epoch.
	b.ans = AuthorityAnswer{Project: succProject, Machine: "mini", Epoch: 2, State: hometerm.Serving}
	s.BeatRemote(ctx, x) // only the beat went: the other part comes with the next
	s.BeatRemote(ctx, x)
	if h := x.Home(); h.Behind || len(b.applied) != 1 || x.Snapshot().LastError != "" {
		t.Fatalf("after the home answered its epoch: %+v %q (parts applied %q)", h, x.Snapshot().LastError, b.applied)
	}
	if err := s.pollOps(ctx, x, succProject); err != nil {
		t.Fatal(err)
	}
	s.ops.wg.Wait()
	if opsRun(t, s) != 2 {
		t.Fatal("the ops poll of a current home took nothing")
	}
}

// A call to a joined project's home carries this machine's scope_version,
// but a leave never does, nor a heartbeat once the home's epoch is
// recorded (Send): a home that lost its term, and so holds an older scope,
// refuses neither 409 rescope.
func TestScopeVersionNotOnALeaveNorOnABeatOnceAnEpochIsRecorded(t *testing.T) {
	ctx := context.Background()
	b := newBehindHome(t)
	s, x := b.s, b.x
	heldScope(x)
	x.Mu.Lock()
	x.home.Epoch = 0 // no epoch recorded yet: a join before its home's first term answer
	x.Mu.Unlock()
	b.ans.Epoch = 2
	s.BeatRemote(ctx, x)
	if h := x.Home(); h.Epoch != 2 {
		t.Fatalf("the home's epoch was not recorded: %+v", h)
	}
	s.BeatRemote(ctx, x)
	if err := s.CallRemote(ctx, x, http.MethodGet, "/v0/projects/"+succProject, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/leave", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	var sent []string // the calls above (a beat may bring a machine report too)
	for _, c := range b.scopes {
		if !strings.HasPrefix(c, "/v0/team/machine=") {
			sent = append(sent, c)
		}
	}
	want := []string{"/v0/team/presence=5", "/v0/team/presence=", "/v0/projects/" + succProject + "=5", "/v0/team/leave="}
	if !slices.Equal(sent, want) {
		t.Fatalf("scope_version sent: %q, want %q", sent, want)
	}
}

// A home that falls behind changes nothing of this machine's join with the
// answer that shows it, nor with any later one: no rescope, no role, no
// 401 counted.
func TestAHomeBehindRescopesNothing(t *testing.T) {
	ctx := context.Background()
	b := newBehindHome(t)
	s, x := b.s, b.x
	b.header = api.EncodeTeamHeader(api.TeamHeader{Project: api.ProjectRef{ID: succProject, Slug: "orchid"}, Role: "reader", ScopeVersion: 9,
		Cap: api.TeamCap{Project: succProject, Slug: "orchid", Role: "reader"}})
	s.BeatRemote(ctx, x)
	if r := x.Snapshot(); r.ScopeVersion != 0 || r.Role == "reader" || !x.Home().Behind {
		t.Fatalf("after the answer that shows the home behind: %+v %+v", r, x.Home())
	}
	b.status = http.StatusUnauthorized
	for range 3 {
		s.BeatRemote(ctx, x)
	}
	if r := x.Snapshot(); r.ScopeVersion != 0 || r.State != api.RemoteOK || len(x.Unauth) != 0 {
		t.Fatalf("after a home behind refused this machine: %+v (401s %d)", r, len(x.Unauth))
	}
}

// scopeHeader is a team header for succProject at scope_version v with role.
func scopeHeader(v int64, role string) string {
	return api.EncodeTeamHeader(api.TeamHeader{Project: api.ProjectRef{ID: succProject, Slug: "orchid"}, Role: role, ScopeVersion: v,
		Cap: api.TeamCap{Project: succProject, Slug: "orchid", Role: role}})
}

// heldScope makes x hold scope_version 5 as a writer, as after answers from
// its home.
func heldScope(x *Remote) {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	x.R.ScopeVersion, x.R.Role = 5, "writer"
}

// A home restored from an older backup, whose first answer would be a 409
// rescope to its older scope, changes nothing of this machine's join: the
// beat carries no scope_version once an epoch is recorded, so the answer is
// one whose authority part shows the home behind before anything of it is
// taken.
func TestAHomeBehindRescopesNothingOnItsFirstAnswer(t *testing.T) {
	ctx := context.Background()
	b := newBehindHome(t)
	s, x := b.s, b.x
	heldScope(x)
	b.ans.Epoch, b.scope, b.header = 1, 3, scopeHeader(3, "reader")
	s.BeatRemote(ctx, x)
	if r := x.Snapshot(); r.ScopeVersion != 5 || r.Role != "writer" || !x.Home().Behind {
		t.Fatalf("after the first answer of a home restored from an older backup: %+v %+v", r, x.Home())
	}
}

// A home behind that answers the recorded epoch again, at a scope_version
// other than the one this machine holds (put right from an older backup),
// is current again with its next beat, and its scope is taken after.
func TestAHomeBehindPutRightAtAnotherScope(t *testing.T) {
	ctx := context.Background()
	b := newBehindHome(t)
	s, x := b.s, b.x
	heldScope(x)
	b.ans.Epoch = 1
	s.BeatRemote(ctx, x)
	if !x.Home().Behind {
		t.Fatal("the home is not behind")
	}
	b.ans.Epoch, b.scope, b.header = 2, 7, scopeHeader(7, "reader")
	s.BeatRemote(ctx, x)
	if h := x.Home(); h.Behind {
		t.Fatalf("a home answering its recorded epoch at another scope is still behind: %+v %q", h, x.Snapshot().LastError)
	}
	s.BeatRemote(ctx, x)
	if r := x.Snapshot(); r.ScopeVersion != 7 || r.Role != "reader" {
		t.Fatalf("the current home's scope was not taken: %+v", r)
	}
	b.paths = nil
	if err := s.CallRemote(ctx, x, http.MethodGet, "/v0/projects/"+succProject, nil, nil); err != nil || len(b.paths) != 1 {
		t.Fatalf("a call to the home put right: %v (it got %q)", err, b.paths)
	}
}

// An ops poll in flight when its home falls behind, or when this machine
// follows another home, takes nothing from the answer that comes back.
func TestAnOpsPollInFlightTakesNothingFromAnOldHome(t *testing.T) {
	for _, c := range []string{"current", "behind", "repointed"} {
		t.Run(c, func(t *testing.T) {
			ctx := context.Background()
			b := newBehindHome(t)
			s, x := b.s, b.x
			b.ans.Epoch = 2
			ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 3, State: hometerm.Serving}
			var tokens []string
			b.ft.at["potluck-mini2:7433"] = "sha256:new"
			b.ft.homes["sha256:new"] = termHome(&ans, &tokens)
			b.onOps = func() {
				switch c {
				case "behind":
					b.ans.Epoch = 1
					s.BeatRemote(ctx, x)
					if !x.Home().Behind {
						t.Error("the home is not behind")
					}
				case "repointed":
					if _, err := s.Follow(ctx, succProject, successor("sha256:new"), "sha256:old", FollowOptions{}); err != nil {
						t.Error(err)
					}
				}
			}
			if err := s.pollOps(ctx, x, succProject); err != nil {
				t.Fatal(err)
			}
			s.ops.wg.Wait()
			if n, want := opsRun(t, s), map[string]int{"current": 1}[c]; n != want {
				t.Fatalf("ops taken: %d, want %d", n, want)
			}
		})
	}
}

// A forwarded call's live answer (an event stream) from a home this
// machine left, or that fell behind, ends.
func TestAForwardedStreamEndsWithItsHome(t *testing.T) {
	for _, c := range []string{"behind", "repointed"} {
		t.Run(c, func(t *testing.T) {
			ctx := context.Background()
			b := newBehindHome(t)
			s, x := b.s, b.x
			b.ans.Epoch = 2
			ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 3, State: hometerm.Serving}
			var tokens []string
			b.ft.at["potluck-mini2:7433"] = "sha256:new"
			b.ft.homes["sha256:new"] = termHome(&ans, &tokens)
			ended := make(chan struct{})
			home := b.ft.homes["sha256:old"]
			b.ft.homes["sha256:old"] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/events" {
					home.ServeHTTP(w, r)
					return
				}
				_, _ = w.Write([]byte("event 1\n"))
				<-r.Context().Done()
				close(ended)
			})
			req := httptest.NewRequest(http.MethodGet, "/v0/events?project="+succProject, nil)
			req = req.WithContext(core.WithSubject(req.Context(), &authz.Subject{}))
			req.Header.Set("Accept", "text/event-stream")
			resp, err := s.ForwardResp(req, x, "", true, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if line := make([]byte, 8); func() error { _, err := io.ReadFull(resp.Body, line); return err }() != nil {
				t.Fatal("no first event")
			}
			switch c {
			case "behind":
				b.ans.Epoch = 1
				s.BeatRemote(ctx, x)
			case "repointed":
				if _, err := s.Follow(ctx, succProject, successor("sha256:new"), "sha256:old", FollowOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-ended:
			case <-time.After(10 * time.Second):
				t.Fatal("the stream from the old home is still open")
			}
		})
	}
}

// The owner lowers the recorded epoch only to the one the home itself fell
// behind to: not when it is not behind, not to another home (another
// address, key or transport, each alone), not to an epoch it answers now
// that is not that one, not to a home that does not serve; a follow without
// ResetEpoch lowers nothing.
func TestResetEpoch(t *testing.T) {
	ctx := context.Background()
	b := newBehindHome(t)
	s, x := b.s, b.x
	same := Successor{Peer: hometerm.Peer{Via: api.ViaTailnet, URL: "http://potluck-mini:7433", Fingerprint: "sha256:old"}}
	reset := FollowOptions{ResetEpoch: true}
	refused := func(why string, succ Successor, opt FollowOptions) {
		t.Helper()
		if _, err := s.Follow(ctx, succProject, succ, "sha256:old", opt); !api.IsCode(err, api.CodeConflict) {
			t.Fatalf("%s: %v", why, err)
		}
		if h := x.Home(); h.Epoch != 2 {
			t.Fatalf("%s lowered the epoch: %+v", why, h)
		}
	}
	b.ans.Epoch = 0 // what it would fall behind to; it has not
	refused("a reset of a home that is not behind", same, reset)
	b.ans.Epoch = 1
	s.BeatRemote(ctx, x) // it answers 1
	if !x.Home().Behind {
		t.Fatal("the home is not behind")
	}
	refused("a follow without ResetEpoch", same, FollowOptions{})
	var tokens []string
	low := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 1, State: hometerm.Serving}
	b.ft.at["potluck-mini2:7433"] = "sha256:new"
	b.ft.homes["sha256:new"] = termHome(&low, &tokens)
	refused("a reset to another home", successor("sha256:new"), reset)
	// The home's own key at another address, another key at its address, its
	// address and key over another transport: each answers as the home would.
	b.ft.at["potluck-mini2:7433"] = "sha256:old"
	refused("a reset to the home's key at another address", Successor{Peer: hometerm.Peer{Via: api.ViaTailnet, URL: "http://potluck-mini2:7433", Fingerprint: "sha256:old"}}, reset)
	b.ft.at["potluck-mini:7433"], b.ft.homes["sha256:new"] = "sha256:new", b.ft.homes["sha256:old"]
	refused("a reset to another key at the home's address", Successor{Peer: hometerm.Peer{Via: api.ViaTailnet, URL: "http://potluck-mini:7433", Fingerprint: "sha256:new"}}, reset)
	b.ft.at["potluck-mini:7433"] = "sha256:old"
	s.d.AddTransport(&fakeTransport{via: api.ViaTunnel, at: b.ft.at, homes: b.ft.homes})
	refused("a reset to the home's address and key over another transport", Successor{Peer: hometerm.Peer{Via: api.ViaTunnel, URL: "http://potluck-mini:7433", Fingerprint: "sha256:old"}}, reset)
	b.ans.Epoch = 0
	refused("a reset to an epoch the home did not fall behind to", same, reset)
	b.ans.Epoch, b.ans.State = 1, hometerm.Standby
	refused("a reset to a home that does not serve", same, reset)
	b.ans.State = hometerm.Serving
	if _, err := s.Follow(ctx, succProject, same, "sha256:old", reset); err != nil {
		t.Fatal(err)
	}
	if h := x.Home(); h.Epoch != 1 || h.Behind || x.Snapshot().LastError != "" {
		t.Fatalf("after the owner took the home's epoch: %+v %q", h, x.Snapshot().LastError)
	}
	if !strings.Contains(b.log.String(), "lowered the recorded epoch") {
		t.Fatalf("the reset was not logged: %s", b.log.String())
	}
	b.paths = nil
	if err := s.CallRemote(ctx, x, http.MethodGet, "/v0/projects/"+succProject, nil, nil); err != nil || len(b.paths) != 1 {
		t.Fatalf("a call after the reset: %v (it got %q)", err, b.paths)
	}
	after := &Service{d: s.d, remotes: map[string]*Remote{}}
	if err := after.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if y := after.ByName(succProject); y.Home().Epoch != 1 || y.Home().Behind || y.Snapshot().LastError != "" {
		t.Fatalf("after the reset and a restart: %+v %q", y.Home(), y.Snapshot().LastError)
	}
}

// A follow of a join being left sends its token nowhere.
func TestFollowOfALeftJoin(t *testing.T) {
	ctx := context.Background()
	s, x, _, ft := successorServiceOn(t)
	ans := AuthorityAnswer{Project: succProject, Machine: "mini2", Epoch: 1, State: hometerm.Serving}
	var tokens []string
	ft.at["potluck-mini2:7433"] = "sha256:new"
	ft.homes["sha256:new"] = termHome(&ans, &tokens)
	x.Mu.Lock()
	x.Recovery.Closing = true
	x.Mu.Unlock()
	if _, err := s.Follow(ctx, succProject, successor("sha256:new"), "sha256:old", FollowOptions{}); !api.IsCode(err, api.CodeConflict) || len(tokens) != 0 {
		t.Fatalf("a follow of a join being left: %v (the successor was asked %d times)", err, len(tokens))
	}
}
