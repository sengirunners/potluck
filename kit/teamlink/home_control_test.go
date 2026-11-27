package teamlink_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/hometerm"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/wiring"
)

// ---- a fake move engine ----

type engineCall struct {
	method string
	by     api.MoveActor
	req    any
}

// fakeEngine records each call and answers what the test set.
type fakeEngine struct {
	mu        sync.Mutex
	calls     []engineCall
	status    api.AuthorityStatus
	cands     api.CandidateList
	preflight api.HomePreflight
	move      api.HomeMove
	err       error
}

func (f *fakeEngine) record(method string, by api.MoveActor, req any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, engineCall{method, by, req})
	return f.err
}

func (f *fakeEngine) Calls() []engineCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]engineCall(nil), f.calls...)
}

// Presses is every call but Status (a read each status asks).
func (f *fakeEngine) Presses() []engineCall {
	var out []engineCall
	for _, c := range f.Calls() {
		if c.method != "Status" {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeEngine) Status(context.Context) (api.AuthorityStatus, error) {
	return f.status, f.record("Status", api.MoveActor{}, nil)
}
func (f *fakeEngine) Candidates(context.Context) (api.CandidateList, error) {
	return f.cands, f.record("Candidates", api.MoveActor{}, nil)
}
func (f *fakeEngine) Preflight(_ context.Context, q api.MoveRequest) (api.HomePreflight, error) {
	return f.preflight, f.record("Preflight", api.MoveActor{}, q)
}
func (f *fakeEngine) SetStandby(_ context.Context, by api.MoveActor, q api.StandbyRequest) (api.AuthorityStatus, error) {
	return f.status, f.record("SetStandby", by, q)
}
func (f *fakeEngine) ReleaseStandby(_ context.Context, by api.MoveActor, q api.StandbyRequest) (api.AuthorityStatus, error) {
	return f.status, f.record("ReleaseStandby", by, q)
}
func (f *fakeEngine) StartMove(_ context.Context, by api.MoveActor, q api.MoveRequest) (api.MoveAnswer, error) {
	m := f.move
	return api.MoveAnswer{Preflight: f.preflight, Move: &m}, f.record("StartMove", by, q)
}
func (f *fakeEngine) AbortMove(_ context.Context, by api.MoveActor, q api.MoveControl) (api.HomeMove, error) {
	return f.move, f.record("AbortMove", by, q)
}
func (f *fakeEngine) ResumeMove(_ context.Context, by api.MoveActor, q api.MoveControl) (api.HomeMove, error) {
	return f.move, f.record("ResumeMove", by, q)
}
func (f *fakeEngine) StartPromote(_ context.Context, by api.MoveActor, q api.PromoteRequest) (api.HomeMove, error) {
	return f.move, f.record("StartPromote", by, q)
}
func (f *fakeEngine) Retire(_ context.Context, by api.MoveActor, q api.RetireRequest) (api.HomeMove, error) {
	return f.move, f.record("Retire", by, q)
}

// ---- daemons ----

// hqDaemon is a KIT daemon on tn whose teamlink has engine e (nil: none).
type hqDaemon struct {
	c    *client.Client
	dm   *server.Daemon
	cfg  *config.Config
	stop func()
}

func newHQDaemon(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, e teamlink.HomeEngine) *hqDaemon {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = machine+"-human", machine
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wiring.Kit)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		switch s := m.(type) {
		case *tailnet.Service:
			s.NewNode = tn.NodeFunc(id)
		case *teamlink.Service:
			if e != nil {
				s.Hooks.Home = e
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Errorf("daemon %s did not stop", machine)
			}
		})
	}
	t.Cleanup(func() {
		stop()
		os.RemoveAll(home)
	})
	c := client.WithHandler(dm.SocketHandler())
	if err := client.NewUnix(cfg.SocketPath()).WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if st, err := c.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("%s: tailnet up: %+v %v", machine, st, err)
	}
	return &hqDaemon{c: c, dm: dm, cfg: cfg, stop: stop}
}

// joinAs joins m to project (an area of it, when area is set) as identity as
// with role, by an invite from hq.
func joinAs(t *testing.T, hq, m *client.Client, project, area, as, role string) {
	t.Helper()
	ctx := context.Background()
	inv, err := hq.Invite(ctx, api.InviteRequest{Project: project, Area: area, As: as, Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	if role != api.RoleMember {
		if _, err := hq.Grant(ctx, api.GrantRequest{Identity: as, Role: role, Project: project, Area: area}); err != nil {
			t.Fatal(err)
		}
	}
}

func teamProject(t *testing.T, c *client.Client, slug string) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: slug}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ProjectTeam(ctx, slug); err != nil {
		t.Fatal(err)
	}
}

// netCall calls dm's network handler as rm with token (either may be empty).
func netCall(dm *server.Daemon, method, path, token string, rm *core.Remote, body string) (int, error) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "100.64.0.1:7433" // a Tailscale address of this node (core.OwnTailnetHost)
	req.Header.Set("Content-Type", "application/json")
	api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
	if token != "" {
		req.Header.Set(api.HeaderAuthorization, "Bearer "+token)
	}
	if rm != nil {
		req = req.WithContext(core.WithRemote(req.Context(), *rm))
	}
	w := httptest.NewRecorder()
	dm.Handler.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		return w.Code, nil
	}
	var e struct {
		Error *api.Error `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error == nil {
		return w.Code, errors.New(w.Body.String())
	}
	return w.Code, e.Error
}

// sockCall calls dm on this machine's socket with body as it is.
func sockCall(dm *server.Daemon, method, path, body string) (int, error) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	api.SetHeader(req.Header, api.HeaderProtocol, api.WireProtocol)
	w := httptest.NewRecorder()
	dm.SocketHandler().ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		return w.Code, nil
	}
	var e struct {
		Error *api.Error `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Error == nil {
		return w.Code, errors.New(w.Body.String())
	}
	return w.Code, e.Error
}

// refusedNotOwner fails unless err is the owner check's refusal.
func refusedNotOwner(t *testing.T, who string, err error) {
	t.Helper()
	if !api.IsCode(err, api.CodeForbidden) || api.HQRefusalReason(err) != api.ReasonNotOwner {
		t.Errorf("%s: %v, want forbidden with reason not_owner", who, err)
	}
}

func shownAt(machine string, epoch uint64) api.Shown { return api.NewShown(machine, epoch) }

// pressBodies are a whole request for each press route on HQ "mini" at
// epoch 0.
func pressBodies() map[string]any {
	sh := shownAt("mini", 0)
	return map[string]any{
		api.StandbyPath:        api.StandbyRequest{Machine: "laptop", Shown: sh},
		api.StandbyReleasePath: api.StandbyRequest{Machine: "laptop", Shown: sh},
		api.MovePath:           api.MoveRequest{To: "laptop", PlanID: "p1", Shown: sh},
		api.MoveAbortPath:      api.MoveControl{MoveID: "mv1"},
		api.MoveResumePath:     api.MoveControl{MoveID: "mv1"},
		api.PromotePath:        api.PromoteRequest{Machine: "mini", Shown: shownAt("old", 0), OldHomeIsDown: true},
		api.RetirePath:         api.RetireRequest{Machine: "mini", Shown: shownAt("new", 1)},
	}
}

// ---- tests ----

// With no engine, HQ's status is what its term says and every press,
// whatever its body, answers no_engine.
func TestHQWithNoEngine(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, nil)
	teamProject(t, hq.c, "orchid")
	st, err := hq.c.Authority(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ServedBy != (api.ServedBy{Machine: "mini", State: api.HQServing}) || st.Machine != "mini" || st.State != api.HQServing ||
		st.Epoch != 0 || !st.CanManage || st.Standby.State != api.StandbyNone || st.Move != nil || st.Silent != nil {
		t.Fatalf("status with no engine: %+v", st)
	}
	if _, err := hq.c.AuthorityCandidates(ctx); api.HQRefusalReason(err) != api.ReasonNoEngine || !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("candidates with no engine: %v", err)
	}
	for path, body := range pressBodies() {
		for _, b := range []any{body, json.RawMessage(`{}`)} {
			if err := hq.c.Do(ctx, http.MethodPost, path, b, nil); api.HQRefusalReason(err) != api.ReasonNoEngine || !api.IsCode(err, api.CodeUnavailable) {
				t.Errorf("POST %s %v with no engine: %v, want no_engine", path, b, err)
			}
		}
	}
	// Who pressed comes first: a non-owner hears not_owner, never that
	// there is no engine.
	minted, err := hq.c.MintToken(ctx, api.MintTokenRequest{As: "helper", Project: "orchid", Role: api.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	tok := client.WithHandler(hq.dm.SocketHandler())
	tok.Token = minted.Secret
	for path, body := range pressBodies() {
		refusedNotOwner(t, "a non-owner's POST "+path+" with no engine", tok.Do(ctx, http.MethodPost, path, body, nil))
	}
	_, err = tok.AuthorityCandidates(ctx)
	refusedNotOwner(t, "a non-owner's candidates with no engine", err)
}

// Every press Checks its request before anything acts on it: an empty
// object or no body at all is a bad_request, and the engine is never asked.
func TestHQPressWithNothingIsABadRequest(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	teamProject(t, hq.c, "orchid")
	for path := range pressBodies() {
		for _, b := range []any{json.RawMessage(`{}`), nil} {
			if err := hq.c.Do(ctx, http.MethodPost, path, b, nil); !api.IsCode(err, api.CodeBadRequest) {
				t.Errorf("POST %s with %v: %v, want bad_request", path, b, err)
			}
		}
	}
	// A move's check_only needs only its target, and a promote without
	// force needs the owner's tick.
	if err := hq.c.Do(ctx, http.MethodPost, api.MovePath, api.MoveRequest{CheckOnly: true}, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("check_only with no target: %v", err)
	}
	if err := hq.c.Do(ctx, http.MethodPost, api.PromotePath, api.PromoteRequest{Machine: "mini", Shown: shownAt("old", 0)}, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("promote without old_home_is_down: %v", err)
	}
	// A body is one JSON value: a second one, or anything after it, is a
	// bad_request too.
	for path, b := range pressBodies() {
		one, _ := json.Marshal(b)
		for _, raw := range []string{string(one) + string(one), string(one) + " x", string(one) + "}"} {
			if _, err := sockCall(hq.dm, http.MethodPost, path, raw); !api.IsCode(err, api.CodeBadRequest) {
				t.Errorf("POST %s %s: %v, want bad_request", path, raw, err)
			}
		}
	}
	if got := e.Calls(); len(got) != 0 {
		t.Fatalf("the engine was asked: %+v", got)
	}
	// A whole request reaches it (promote and retire are a parked machine's:
	// TestHQTakeoverAndRetireRefusedWhileServing).
	n := 0
	for path, b := range pressBodies() {
		if path == api.PromotePath || path == api.RetirePath {
			continue
		}
		n++
		if err := hq.c.Do(ctx, http.MethodPost, path, b, nil); err != nil {
			t.Errorf("POST %s: %v", path, err)
		}
	}
	if got := e.Calls(); len(got) != n {
		t.Fatalf("the engine was asked %d times: %+v", len(got), got)
	}
}

// A takeover or a retire on a serving HQ, even one naming that HQ itself
// as the owner's own press, is refused stale and never reaches the engine:
// they belong to a parked standby or a fenced old HQ.
func TestHQTakeoverAndRetireRefusedWhileServing(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	teamProject(t, hq.c, "orchid")
	for name, press := range map[string]func() error{
		"promote --force": func() error {
			_, err := hq.c.PromoteStandby(ctx, api.PromoteRequest{Machine: "mini", Shown: shownAt("mini", 0), Force: true})
			return err
		},
		"promote": func() error {
			_, err := hq.c.PromoteStandby(ctx, api.PromoteRequest{Machine: "mini", Shown: shownAt("mini", 0), OldHomeIsDown: true})
			return err
		},
		"retire": func() error {
			_, err := hq.c.RetireHQ(ctx, api.RetireRequest{Machine: "mini", Shown: shownAt("mini", 0)})
			return err
		},
	} {
		if err := press(); !api.IsCode(err, api.CodeConflict) || api.HQRefusalReason(err) != api.ReasonStale {
			t.Errorf("%s on the serving HQ: %v, want conflict with reason stale", name, err)
		}
	}
	if got := e.Calls(); len(got) != 0 {
		t.Fatalf("a takeover or retire of a serving HQ reached the engine: %+v", got)
	}
}

// Only this machine's owner on its socket, or a member machine whose
// identity owns every team project here by its own grants, presses. An
// owner of one project, of an area, a token on the socket, a minted token,
// a box's token and a tailnet login are refused, and the engine never
// asked; a takeover is a person's own press, never a session's.
func TestHQPressedBy(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	laptop := newHQDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"}, nil)
	studio := newHQDaemon(t, tn, "studio", tailnettest.Identity{NodeID: "nSTUDIO", NodeKey: "nodekey:studio", Login: "bo@example.com", Host: "studio"}, nil)
	den := newHQDaemon(t, tn, "den", tailnettest.Identity{NodeID: "nDEN", NodeKey: "nodekey:den", Login: "cy@example.com", Host: "den"}, nil)
	teamProject(t, hq.c, "orchid")
	teamProject(t, hq.c, "fern")
	if _, err := hq.c.CreateProject(ctx, api.NewProjectRequest{Slug: "solo"}); err != nil { // not a team project: never counted
		t.Fatal(err)
	}
	if _, err := hq.c.AddArea(ctx, "orchid", api.AddAreaRequest{Name: "team"}); err != nil {
		t.Fatal(err)
	}
	joinAs(t, hq.c, laptop.c, "orchid", "", "ana", api.RoleOwner)
	if _, err := hq.c.Grant(ctx, api.GrantRequest{Identity: "ana", Role: api.RoleOwner, Project: "fern"}); err != nil {
		t.Fatal(err)
	}
	joinAs(t, hq.c, studio.c, "orchid", "", "bo", api.RoleOwner)
	if _, err := hq.c.Grant(ctx, api.GrantRequest{Identity: "bo", Role: api.RoleMember, Project: "fern"}); err != nil {
		t.Fatal(err)
	}
	joinAs(t, hq.c, den.c, "orchid", "team", "cy", api.RoleOwner) // owner of an area only
	standby := api.StandbyRequest{Machine: "laptop", Shown: shownAt("mini", 0)}
	asked := func() int { return len(e.Presses()) }

	// The owner of both, from their own machine.
	if _, err := laptop.c.SetStandby(ctx, standby); err != nil {
		t.Fatalf("an owner of every team project, from a member machine: %v", err)
	}
	if c := e.Presses(); len(c) != 1 || c[0].method != "SetStandby" || c[0].by.Identity != "ana" || c[0].by.Machine != "laptop" || c[0].by.At.IsZero() {
		t.Fatalf("the engine was asked %+v", c)
	}
	st, err := laptop.c.Authority(ctx)
	if err != nil || !st.CanManage || st.Machine != "mini" || st.ServedBy.Machine != "mini" {
		t.Fatalf("status through an owner's machine: %+v %v", st, err)
	}
	// This machine's owner on its socket.
	if _, err := hq.c.SetStandby(ctx, standby); err != nil {
		t.Fatalf("HQ's owner on its socket: %v", err)
	}
	if c := e.Presses(); len(c) != 2 || c[1].by.Machine != "mini" || c[1].by.Identity == "" {
		t.Fatalf("the engine was asked %+v", c)
	}

	n := asked()
	// An owner of one project, a member of the other: the refusal names it
	// and its owners.
	_, err = studio.c.SetStandby(ctx, standby)
	refusedNotOwner(t, "an owner of one team project", err)
	if !strings.Contains(err.Error(), "fern (owners: ") || strings.Contains(err.Error(), "orchid") {
		t.Errorf("the refusal does not name fern and its owners alone: %v", err)
	}
	if st, err := studio.c.Authority(ctx); err != nil || st.CanManage {
		t.Errorf("status through a non-owner's machine: %+v %v", st, err)
	}
	// An owner of an area: it owns no whole project, and fern, which it
	// cannot see, is counted, not named.
	_, err = den.c.SetStandby(ctx, standby)
	refusedNotOwner(t, "an owner of an area", err)
	if !strings.Contains(err.Error(), "orchid (owners: ") || strings.Contains(err.Error(), "fern") || !strings.Contains(err.Error(), "1 more you are not a member of") {
		t.Errorf("the area owner's refusal: %v", err)
	}
	_, err = den.c.AuthorityCandidates(ctx)
	refusedNotOwner(t, "an area owner's candidates", err)

	// A token on HQ's own socket narrows: refused.
	minted, err := hq.c.MintToken(ctx, api.MintTokenRequest{As: "helper", Project: "orchid", Role: api.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"orchid", "fern"} {
		if _, err := hq.c.Grant(ctx, api.GrantRequest{Identity: "helper", Role: api.RoleOwner, Project: p}); err != nil {
			t.Fatal(err)
		}
	}
	tok := client.WithHandler(hq.dm.SocketHandler())
	tok.Token = minted.Secret
	_, err = tok.SetStandby(ctx, standby)
	refusedNotOwner(t, "a token on the socket", err)
	body, _ := json.Marshal(standby)
	// A minted token of an owner of both, over the tailnet.
	_, err = netCall(hq.dm, http.MethodPost, api.StandbyPath, minted.Secret, &core.Remote{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop"}, string(body))
	refusedNotOwner(t, "a minted token", err)
	// A box's token, over the tunnel from its key.
	key, err := tunnel.NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	reg := hq.dm.Deps.Projects // a box key is never on the wire
	box, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "helper", Project: "orchid", Role: api.RoleOwner, BoxKey: tunnel.BoxKeyID(key)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = netCall(hq.dm, http.MethodPost, api.StandbyPath, box.Secret, &core.Remote{Via: core.ViaTunnel, NodeID: tunnel.BoxKeyID(key)}, string(body))
	refusedNotOwner(t, "a box's token", err)
	// A tailnet login of an owner of both.
	if _, err := hq.c.AddIdentity(ctx, api.NewIdentityRequest{Name: "lu", TailscaleLogin: "lu@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"orchid", "fern"} {
		if _, err := hq.c.Grant(ctx, api.GrantRequest{Identity: "lu", Role: api.RoleOwner, Project: p}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = netCall(hq.dm, http.MethodGet, api.CandidatesPath, "", &core.Remote{NodeID: "nLU", NodeKey: "nodekey:lu", Login: "lu@example.com"}, "")
	refusedNotOwner(t, "a tailnet login", err)
	if _, err := netCall(hq.dm, http.MethodGet, api.AuthorityPath, "", &core.Remote{NodeID: "nLU", NodeKey: "nodekey:lu", Login: "lu@example.com"}, ""); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("a tailnet login's status: %v, want forbidden", err)
	}
	if got := asked(); got != n {
		t.Fatalf("a refused press reached the engine: %+v", e.Presses()[n:])
	}

	// A person's own presses (standby, release, move, its check, promote,
	// retire): a session or the machine's address on the socket, or a
	// session forwarded from an owner's own machine, is no person.
	sess := client.WithHandler(hq.dm.SocketHandler())
	sess.As = api.SessionAddress("mini", "abcd2345")
	mach := client.WithHandler(hq.dm.SocketHandler())
	mach.As = "machine:mini"
	lsess := client.WithHandler(laptop.dm.SocketHandler())
	lsess.As = api.SessionAddress("laptop", "abcd2345")
	presses := map[string]func(*client.Client) error{
		"standby": func(c *client.Client) error { _, err := c.SetStandby(ctx, standby); return err },
		"release": func(c *client.Client) error { _, err := c.ReleaseStandby(ctx, standby); return err },
		"move": func(c *client.Client) error {
			_, err := c.MoveHQ(ctx, api.MoveRequest{To: "laptop", PlanID: "p1", Shown: shownAt("mini", 0)})
			return err
		},
		"check-only move": func(c *client.Client) error {
			_, err := c.MoveHQ(ctx, api.MoveRequest{To: "laptop", CheckOnly: true})
			return err
		},
		"promote": func(c *client.Client) error {
			_, err := c.PromoteStandby(ctx, api.PromoteRequest{Machine: "mini", Shown: shownAt("old", 0), OldHomeIsDown: true})
			return err
		},
		"retire": func(c *client.Client) error {
			_, err := c.RetireHQ(ctx, api.RetireRequest{Machine: "mini", Shown: shownAt("new", 1)})
			return err
		},
	}
	for who, c := range map[string]*client.Client{"a session": sess, "the machine": mach, "a session on an owner's machine": lsess} {
		for name, press := range presses {
			refusedNotOwner(t, who+"'s "+name, press(c))
		}
	}
	if got := asked(); got != n {
		t.Fatalf("a session's press reached the engine: %+v", e.Presses()[n:])
	}
	// Abort and resume back out of a stopped move: a session may press
	// them, on HQ's socket and from an owner's machine.
	if _, err := sess.AbortMove(ctx, api.MoveControl{MoveID: "mv1"}); err != nil {
		t.Fatalf("a session's abort: %v", err)
	}
	if _, err := lsess.ResumeMove(ctx, api.MoveControl{MoveID: "mv1"}); err != nil {
		t.Fatalf("a session's resume from an owner's machine: %v", err)
	}
	if c := e.Presses(); len(c) != n+2 || c[n+1].by.Identity != "ana" || c[n+1].by.Machine != "laptop" {
		t.Fatalf("the engine was asked %+v", c[n:])
	}
}

// A press whose screen is out of date is refused stale before the engine
// is asked: another HQ or epoch than this serving home's, or a takeover or
// retire naming another machine.
func TestHQStalePress(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	teamProject(t, hq.c, "orchid")
	for name, press := range map[string]func() error{
		"another epoch": func() error {
			_, err := hq.c.SetStandby(ctx, api.StandbyRequest{Machine: "laptop", Shown: shownAt("mini", 1)})
			return err
		},
		"another HQ": func() error {
			_, err := hq.c.ReleaseStandby(ctx, api.StandbyRequest{Machine: "laptop", Shown: shownAt("echo", 0)})
			return err
		},
		"a move from another epoch": func() error {
			_, err := hq.c.MoveHQ(ctx, api.MoveRequest{To: "laptop", PlanID: "p1", Shown: shownAt("mini", 2)})
			return err
		},
		"a takeover of another machine": func() error {
			_, err := hq.c.PromoteStandby(ctx, api.PromoteRequest{Machine: "delta", Shown: shownAt("old", 0), OldHomeIsDown: true})
			return err
		},
		"a retire of another machine": func() error {
			_, err := hq.c.RetireHQ(ctx, api.RetireRequest{Machine: "delta", Shown: shownAt("new", 1)})
			return err
		},
	} {
		if err := press(); !api.IsCode(err, api.CodeConflict) || api.HQRefusalReason(err) != api.ReasonStale {
			t.Errorf("%s: %v, want conflict with reason stale", name, err)
		}
	}
	if got := e.Calls(); len(got) != 0 {
		t.Fatalf("a stale press reached the engine: %+v", got)
	}
	// A check-only move shows no screen yet: it is never stale.
	if _, err := hq.c.MoveHQ(ctx, api.MoveRequest{To: "laptop", CheckOnly: true}); err != nil {
		t.Fatal(err)
	}
	if c := e.Calls(); len(c) != 1 || c[0].method != "Preflight" {
		t.Fatalf("check-only asked %+v", c)
	}
}

// The free text an engine answers (Reason.Text, PreflightCheck.Text and
// Fix, MoveStep.Detail, HomeMove.Failure) leaves only as a fixed phrase: a
// path, a URL or query, an address, a host, a key or token, or anything
// outside printable ASCII is withheld; plain sentences with machine names,
// clock times and whole numbers pass.
func TestHQFreeTextIsFixedPhrases(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	dirty := []string{
		"cannot open /home/pat/.potluck/registry.db",
		"GET https://bucket.example/set?X-Amz-Signature=abc failed",
		"the set is at bucket?X-Amz-Signature",
		`C:\Users\me\home`,
		"token potluck_" + strings.Repeat("a", 52) + " refused",
		"old token zm_" + strings.Repeat("a", 52),
		"ana@example.com is offline",
		"two\nlines",
		strings.Repeat("long ", 70),
		"reach 10.0.0.5 first",
		"HQ answers on mini:7433",
		"HQ answers on port 1:7433",
		"fe80::1 is HQ",
		"HQ is hq.example.com now",
		"key AKIAIOSFODNN7EXAMPLE",
		"secret wJalrXUtnFEMIK7MDENGbPxRfiCYEXAMPLEKEY",
		"key sk-ant-api03-abc",
		"key AGE-SECRET-KEY-1QQQ",
		"github ghp_abc",
		"home\u2215pat",
		"a \u202ereversed word",
		"zero\u200bwidth",
		"caf\u00e9 is down",
		"path%2Fetc",
		"a=b",
		"1.5 GB free",
		"key 550e8400-e29b-41d4-a716-446655440000",
		"key ab12-cd34-ef56-gh78-ij90",
		"key ab12_cd34_ef56_gh78_ij90",
	}
	clean := []string{
		"delta is 3 minutes behind HQ at 14:05",
		"50% done: 3 of 6 machines follow, at 9:05:30",
		"the standby's copy needs 10 GB; delta has 2 GB free.",
		"is HQ down? Check it first",
		"set AWS_SECRET_ACCESS_KEY_2024 on delta first",
	}
	reasons := func(text string) []api.Reason { return []api.Reason{{Code: api.ReasonBehind, Text: text}} }
	move := func(text string) *api.HomeMove {
		return &api.HomeMove{ID: "mv1", Kind: api.MoveKindSwap, State: api.MoveFailed, From: "mini", To: "delta", Failure: text,
			Steps:  []api.MoveStep{{ID: api.StepVerify, State: api.StepFailed, Detail: text}},
			Follow: &api.FollowProgress{Stragglers: []api.Straggler{{Machine: "x", Reason: api.Reason{Code: api.ReasonOffline, Text: text}}}}}
	}
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	teamProject(t, hq.c, "orchid")
	for _, text := range append(dirty, clean...) {
		want := text
		if !slices.Contains(clean, text) {
			want = "the engine's text was withheld: it was not a fixed phrase"
		}
		e.mu.Lock()
		e.status = api.AuthorityStatus{
			Standby: api.StandbyStatus{Machine: "delta", State: api.StandbyReady, Reasons: reasons(text)},
			Setup:   &api.StandbySetup{Machine: "charlie", Step: api.SetupCopying, Failure: &api.Reason{Code: api.ReasonNoDisk, Text: text}},
			Move:    move(text), LastMove: move(text),
		}
		e.cands = api.CandidateList{Candidates: []api.Candidate{{Machine: "delta", Reasons: reasons(text)}}}
		e.preflight = api.HomePreflight{ID: "p1", Checks: []api.PreflightCheck{{Code: api.CheckStandbyDisk, Text: text, Fix: text}}}
		e.move = *move(text)
		e.mu.Unlock()
		st, err := hq.c.Authority(ctx)
		if err != nil {
			t.Fatal(err)
		}
		texts := map[string]string{
			"standby reason": st.Standby.Reasons[0].Text, "setup failure": st.Setup.Failure.Text,
		}
		for name, m := range map[string]*api.HomeMove{"move": st.Move, "last move": st.LastMove} {
			texts[name+" failure"], texts[name+" step detail"], texts[name+" straggler"] = m.Failure, m.Steps[0].Detail, m.Follow.Stragglers[0].Reason.Text
		}
		cl, err := hq.c.AuthorityCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		texts["candidate reason"] = cl.Candidates[0].Reasons[0].Text
		ma, err := hq.c.MoveHQ(ctx, api.MoveRequest{To: "delta", CheckOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		texts["check text"], texts["check fix"] = ma.Preflight.Checks[0].Text, ma.Preflight.Checks[0].Fix
		ma, err = hq.c.MoveHQ(ctx, api.MoveRequest{To: "delta", PlanID: "p1", Shown: shownAt("mini", 0)})
		if err != nil {
			t.Fatal(err)
		}
		texts["started move failure"], texts["started check"] = ma.Move.Failure, ma.Preflight.Checks[0].Text
		hm, err := hq.c.AbortMove(ctx, api.MoveControl{MoveID: "mv1"})
		if err != nil {
			t.Fatal(err)
		}
		texts["aborted move detail"] = hm.Steps[0].Detail
		for name, got := range texts {
			if got != want {
				t.Errorf("%q: %s answered %q, want %q", text, name, got, want)
			}
		}
	}
}

// An engine's error is answered with fixed words chosen by its code and
// reason: its message, hint and data (a path, a URL, a token) never leave;
// a reason outside api.Reasons and a code outside the routes' are dropped.
func TestHQEngineErrorsAreFixedWords(t *testing.T) {
	tn := tailnettest.New()
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	teamProject(t, hq.c, "orchid")
	tok := "potluck_" + strings.Repeat("a", 52)
	answer := func() (int, string) {
		code, err := sockCall(hq.dm, http.MethodPost, api.MoveAbortPath, `{"move_id":"mv1"}`)
		b, _ := json.Marshal(err)
		return code, string(b)
	}
	leaks := func(raw string) bool {
		return strings.Contains(raw, "/") || strings.Contains(raw, "potluck_") || strings.Contains(raw, "example") || strings.Contains(raw, "14:05")
	}
	for _, reason := range api.Reasons {
		e.err = &api.Error{Code: api.CodeConflict, Message: "move mv1 ended at 14:05 in /home/pat/.potluck/moves/mv1.log", Hint: "see https://example.com/x",
			Data: json.RawMessage(`{"reason":"` + reason + `","home":"/home/pat/.potluck","token":"` + tok + `"}`)}
		_, raw := answer()
		var got api.Error
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		if got.Code != api.CodeConflict || string(got.Data) != `{"reason":"`+reason+`"}` || got.Message == "" || leaks(raw) {
			t.Errorf("an engine refusal with reason %s: %s", reason, raw)
		}
	}
	for name, c := range map[string]struct {
		err  error
		code string
	}{
		"an unknown reason": {&api.Error{Code: api.CodeConflict, Message: "fine", Data: json.RawMessage(`{"reason":"/etc/passwd"}`)}, api.CodeConflict},
		"an unknown code":   {&api.Error{Code: "see /etc/passwd", Message: "fine"}, api.CodeInternal},
		"a plain error":     {errors.New("open /var/lib/x: permission denied " + tok), api.CodeInternal},
		"a fixed refusal":   {api.RefuseHQ(api.CodeForbidden, api.ReasonNotOwner, "move mv1 ended at 14:05"), api.CodeForbidden},
	} {
		e.err = c.err
		_, raw := answer()
		var got api.Error
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		wantData := ""
		if name == "a fixed refusal" {
			wantData = `{"reason":"not_owner"}`
		}
		if got.Code != c.code || string(got.Data) != wantData || leaks(raw) || strings.Contains(raw, "fine") {
			t.Errorf("%s: %s", name, raw)
		}
	}
}

// Every code, id, name and address an engine answers leaves only when it
// is what its field holds: a code from its vocabulary, a ULID or short id, a
// machine or identity name, a project slug, a secret's plain name, a host
// with a port. Anything else, a path or a token in any of them, is
// "unknown"; a clean answer passes as it is.
func TestHQCodesAndNamesAreChecked(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	teamProject(t, hq.c, "orchid")
	at := time.Date(2026, 11, 21, 14, 5, 0, 0, time.UTC)
	mv := func(v func(string) string) api.HomeMove {
		return api.HomeMove{ID: v("01JCXZ5V7Q8W9E0R1T2Y3M4N5P"), Kind: v(api.MoveKindSwap), State: v(api.MoveRunning), From: v("mini"), To: v("delta"),
			Steps:  []api.MoveStep{{ID: v(api.StepFreeze), State: v(api.StepDone)}},
			Follow: &api.FollowProgress{Stragglers: []api.Straggler{{Machine: v("charlie"), Reason: api.Reason{Code: v(api.ReasonOffline)}}}},
			By:     api.MoveActor{Identity: v("ana"), Machine: v("laptop"), At: at}, Started: at}
	}
	answers := func(v func(string) string) {
		e.mu.Lock()
		defer e.mu.Unlock()
		m, last := mv(v), mv(v)
		last.ID = v("mv1")
		e.status = api.AuthorityStatus{Machine: v("mini"), State: v(api.HQServing),
			Standby: api.StandbyStatus{Machine: v("delta"), State: v(api.StandbyReady), Reasons: []api.Reason{{Code: v(api.ReasonBehind)}}},
			Setup:   &api.StandbySetup{Machine: v("charlie"), Step: v(api.SetupCopying), Failure: &api.Reason{Code: v(api.ReasonNoDisk)}},
			Move:    &m, LastMove: &last,
			FormerHQ: &api.FormerHQ{Machine: v("old-hq")}}
		e.cands = api.CandidateList{Preview: api.StandbyPreview{Projects: 1, Secrets: []string{v("ANTHROPIC_API_KEY")}},
			Candidates: []api.Candidate{{Machine: v("delta"), Reasons: []api.Reason{{Code: v(api.ReasonNoDisk)}}}}}
		e.preflight = api.HomePreflight{ID: v("p1"), From: v("mini"), To: v("delta"), Checks: []api.PreflightCheck{{Code: v(api.CheckStandbyDisk), OK: true}},
			Projects: []string{v("orchid")}, PublicAddress: &api.PublicAddress{Address: v("hq.example.com:443"), Members: 2},
			Unseen: []api.UnseenMember{{Machine: v("den")}}}
		e.move = m
	}
	// What leaves, each field's value in one list (the status's Machine and
	// State are this serving daemon's own, and not the engine's).
	values := func() []string {
		st, err := hq.c.Authority(ctx)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := hq.c.AuthorityCandidates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ma, err := hq.c.MoveHQ(ctx, api.MoveRequest{To: "delta", CheckOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		hm, err := hq.c.AbortMove(ctx, api.MoveControl{MoveID: "mv1"})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		ofMove := func(m api.HomeMove) {
			out = append(out, m.ID, m.Kind, m.State, m.From, m.To, m.Steps[0].ID, m.Steps[0].State, m.Follow.Stragglers[0].Machine,
				m.Follow.Stragglers[0].Reason.Code, m.By.Identity, m.By.Machine)
		}
		out = append(out, st.Standby.Machine, st.Standby.State, st.Standby.Reasons[0].Code, st.Setup.Machine, st.Setup.Step, st.Setup.Failure.Code, st.FormerHQ.Machine)
		ofMove(*st.Move)
		ofMove(*st.LastMove)
		ofMove(hm)
		out = append(out, cl.Preview.Secrets[0], cl.Candidates[0].Machine, cl.Candidates[0].Reasons[0].Code)
		p := ma.Preflight
		out = append(out, p.ID, p.From, p.To, p.Checks[0].Code, p.Projects[0], p.PublicAddress.Address, p.Unseen[0].Machine)
		return out
	}
	answers(func(s string) string { return s })
	cleanValues := values()
	if slices.Contains(cleanValues, api.UnknownCode) {
		t.Fatalf("a clean answer was changed: %q", cleanValues)
	}
	for _, bad := range []string{"/home/pat/.potluck", "potluck_" + strings.Repeat("a", 52), "AKIAIOSFODNN7EXAMPLE", "mini dev", "hq.example.com/x?sig=1"} {
		answers(func(string) string { return bad })
		for i, got := range values() {
			if got != api.UnknownCode {
				t.Errorf("%q: field %d (clean %q) answered %q, want %q", bad, i, cleanValues[i], got, api.UnknownCode)
			}
		}
	}
	// A move's or plan's id: a ULID or a short word, never a token, a key,
	// a UUID or a hex digest.
	for _, id := range []string{"potluck_mv1", "zm_mv1", "550e8400-e29b-41d4-a716-446655440000", strings.Repeat("a1b2c3d4e5", 4), "akiaiosfodnn7example"} {
		answers(func(s string) string {
			if s == "01JCXZ5V7Q8W9E0R1T2Y3M4N5P" || s == "p1" {
				return id
			}
			return s
		})
		got := values()
		if got[7] != api.UnknownCode || got[29] != api.UnknownCode || got[43] != api.UnknownCode || got[18] != "mv1" {
			t.Errorf("id %q answered as move %q, started move %q, plan %q (last move %q)", id, got[7], got[29], got[43], got[18])
		}
	}
	// A machine's name: letters, digits and '-' only, so no host name or
	// address leaves as one; an identity's name may hold a '.'.
	for _, name := range []string{"delta.tail1234.ts.net", "10.0.0.5", "delta_4"} {
		answers(func(s string) string {
			if s == "delta" || s == "laptop" || s == "ana" {
				return name
			}
			return s
		})
		got := values()
		for _, i := range []int{0, 11, 17, 41, 45} { // standby, move to, move by, candidate, preflight to
			if got[i] != api.UnknownCode {
				t.Errorf("machine %q: field %d (clean %q) answered %q", name, i, cleanValues[i], got[i])
			}
		}
		if got[16] != name {
			t.Errorf("identity %q answered %q", name, got[16])
		}
	}
	// The public address: a host and port only.
	for _, addr := range []string{"user:pw@hq.example.com", "https://hq.example.com", "hq.example.com:99999", "hq.example.com?x=1"} {
		answers(func(s string) string {
			if s == "hq.example.com:443" {
				return addr
			}
			return s
		})
		ma, err := hq.c.MoveHQ(ctx, api.MoveRequest{To: "delta", CheckOnly: true})
		if err != nil || ma.Preflight.PublicAddress.Address != api.UnknownCode {
			t.Errorf("public address %q answered %+v %v", addr, ma.Preflight.PublicAddress, err)
		}
	}
}

// On a member's socket the HQ routes go to the home of its joined team
// projects; when that home does not answer, the status is filled from what
// the member recorded of it and says HQ is silent.
func TestHQThroughAMember(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	e := &fakeEngine{status: api.AuthorityStatus{Standby: api.StandbyStatus{State: api.StandbyNone}}}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	laptop := newHQDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"}, nil)
	teamProject(t, hq.c, "orchid")
	joinAs(t, hq.c, laptop.c, "orchid", "", "ana", api.RoleOwner)
	if _, err := laptop.c.MoveHQ(ctx, api.MoveRequest{To: "laptop", PlanID: "p1", Shown: shownAt("mini", 0)}); err != nil {
		t.Fatal(err)
	}
	if c := e.Presses(); len(c) != 1 || c[0].method != "StartMove" || c[0].by.Machine != "laptop" {
		t.Fatalf("the engine was asked %+v", c)
	}

	// A second home: an unnamed call is refused, a named one goes there.
	other := newHQDaemon(t, tn, "den", tailnettest.Identity{NodeID: "nDEN", NodeKey: "nodekey:den", Login: "cy@example.com", Host: "den"}, nil)
	teamProject(t, other.c, "fern")
	joinAs(t, other.c, laptop.c, "fern", "", "ana", api.RoleOwner)
	if _, err := laptop.c.Authority(ctx); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "fern") || !strings.Contains(err.Error(), "orchid") {
		t.Fatalf("an unnamed status with two homes: %v", err)
	}
	named := *laptop.c
	named.Project = "fern"
	if st, err := named.Authority(ctx); err != nil || st.Machine != "den" || st.ServedBy.Machine != "den" {
		t.Fatalf("fern's HQ: %+v %v", st, err)
	}
	named.Project = "nowhere"
	if _, err := named.Authority(ctx); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a project this machine does not have: %v", err)
	}

	// orchid's HQ goes away.
	named.Project = "orchid"
	if st, err := named.Authority(ctx); err != nil || st.Machine != "mini" || st.Silent != nil {
		t.Fatalf("orchid's HQ: %+v %v", st, err)
	}
	hq.stop()
	st, err := named.Authority(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Silent == nil || st.Silent.StandbyProbe != api.ProbeUnknown || st.Machine != "mini" || st.State != api.HQServing ||
		st.Epoch != 0 || st.CanManage || st.Counts != nil || st.Move != nil || st.ServedBy.Machine != "laptop" || st.Standby.State != api.StandbyNone {
		t.Fatalf("a silent HQ's status: %+v", st)
	}
	if _, err := named.SetStandby(ctx, api.StandbyRequest{Machine: "laptop", Shown: shownAt("mini", 0)}); err == nil {
		t.Fatal("a press on a silent HQ succeeded")
	}
}

// parkedHome is a parked daemon over a copy of src's home registry (src
// stopped first), or over none when src is nil, on machine for human with
// term, with e its parked routes' engine (nil: none). It holds the token
// files each of holds joined with, as a standby holds its member
// machine's.
func parkedHome(t *testing.T, src *hqDaemon, machine, human string, term hometerm.Term, e teamlink.HomeEngine, holds ...*hqDaemon) *server.Daemon {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = human, machine
	if src != nil {
		src.stop()
		for _, suffix := range []string{"", "-wal"} {
			b, err := os.ReadFile(src.cfg.RegistryPath() + suffix)
			if errors.Is(err, os.ErrNotExist) && suffix != "" {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.RegistryPath()+suffix, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, h := range holds {
		files, err := filepath.Glob(filepath.Join(h.cfg.Home, teamlink.DirRemotes, "*.token"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s holds no token: %v", h.cfg.Machine, err)
		}
		if err := os.MkdirAll(filepath.Join(home, teamlink.DirRemotes), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, teamlink.DirRemotes, fmt.Sprintf("%d-%s", i, filepath.Base(f))), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := hometerm.Create(home, term); err != nil {
		t.Fatal(err)
	}
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wiring.Kit, server.ParkedListeners(func(d *core.Deps) ([]core.Module, error) {
		if e != nil {
			teamlink.ProvideParkedRoutes(d, e)
		}
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	if !dm.Deps.Parked {
		t.Fatalf("%s's daemon is not parked", machine)
	}
	return dm
}

// editRegistry runs q on the registry.db of dm's home (a parked daemon's,
// which has it closed).
func editRegistry(t *testing.T, dm *server.Daemon, q string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenFile(ctx, dm.Deps.Config.RegistryPath(), 1, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res, err := db.ExecContext(ctx, q); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("%s changed nothing", q)
	}
}

// treeOf is every file under dir with its bytes, and every directory.
func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			out[path] = "dir"
		case d.Type().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = string(b)
		default:
			out[path] = d.Type().String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// members is HQ echo with team project orchid, which delta joined as
// its owner ana and charlie as a plain member bo.
type members struct{ hq, ana, bo *hqDaemon }

func homeAndMembers(t *testing.T) members {
	t.Helper()
	tn := tailnettest.New()
	hq := newHQDaemon(t, tn, "echo", tailnettest.Identity{NodeID: "nECHO", NodeKey: "nodekey:echo", Login: "judy@example.com", Host: "echo"}, nil)
	ana := newHQDaemon(t, tn, "delta", tailnettest.Identity{NodeID: "nDELTA", NodeKey: "nodekey:delta", Login: "ana@example.com", Host: "delta"}, nil)
	bo := newHQDaemon(t, tn, "charlie", tailnettest.Identity{NodeID: "nCHARLIE", NodeKey: "nodekey:charlie", Login: "bo@example.com", Host: "charlie"}, nil)
	teamProject(t, hq.c, "orchid")
	joinAs(t, hq.c, ana.c, "orchid", "", "ana", api.RoleOwner)
	joinAs(t, hq.c, bo.c, "orchid", "", "bo", api.RoleMember)
	return members{hq, ana, bo}
}

// homeWithMembers is homeAndMembers' HQ.
func homeWithMembers(t *testing.T) *hqDaemon {
	t.Helper()
	return homeAndMembers(t).hq
}

// A parked machine answers its status and the presses that belong to it
// (abort, resume, promote, retire) on its socket, through the engine its
// wiring provided, to this machine's owner as the home's registry.db names
// them: on a standby, its restored copy of HQ's, where the owner is the
// identity HQ issued this machine's tokens to, and must own every team
// project. A plain member's standby, an unreadable copy, a session's
// takeover and a token are refused; from the network, and with no routes
// provided, it answers parked.
func TestParkedHQRoutes(t *testing.T) {
	ctx := context.Background()
	pred := hometerm.Peer{Via: core.ViaTunnel, URL: "http://127.0.0.1:1", Fingerprint: "fp-old"}
	standby, err := hometerm.NewStandby(pred, 3)
	if err != nil {
		t.Fatal(err)
	}
	m := homeAndMembers(t)
	e := &fakeEngine{status: api.AuthorityStatus{Machine: "echo", State: api.HQServing, Epoch: 3, Standby: api.StandbyStatus{Machine: "delta", State: api.StandbyReady}}}
	// ana's machine delta holds the standby, and the tokens it joined
	// with; its own human is named otherwise there, and is ana in HQ's
	// registry.
	dm := parkedHome(t, m.hq, "delta", "ana-at-home", standby, e, m.ana)
	home := dm.Deps.Config.Home
	before := treeOf(t, home)
	c := client.WithHandler(dm.SocketHandler())
	st, err := c.Authority(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ServedBy != (api.ServedBy{Machine: "delta", State: api.HQStandby}) || st.Machine != "echo" || st.Epoch != 3 || !st.CanManage {
		t.Fatalf("the parked standby's status: %+v", st)
	}
	for _, b := range []any{json.RawMessage(`{}`), nil} {
		if err := c.Do(ctx, http.MethodPost, api.PromotePath, b, nil); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("an empty promote on a parked machine: %v", err)
		}
	}
	promote := api.PromoteRequest{Machine: "delta", Shown: shownAt("echo", 3), OldHomeIsDown: true}
	if _, err := c.PromoteStandby(ctx, api.PromoteRequest{Machine: "charlie", Shown: shownAt("echo", 3), OldHomeIsDown: true}); api.HQRefusalReason(err) != api.ReasonStale {
		t.Errorf("a takeover of another machine: %v", err)
	}
	// A standby is no fenced old HQ: retire is stale here.
	if _, err := c.RetireHQ(ctx, api.RetireRequest{Machine: "delta", Shown: shownAt("echo", 3)}); !api.IsCode(err, api.CodeConflict) || api.HQRefusalReason(err) != api.ReasonStale {
		t.Errorf("a retire of a standby: %v", err)
	}
	sess := client.WithHandler(dm.SocketHandler())
	sess.As = api.SessionAddress("delta", "abcd2345")
	_, err = sess.PromoteStandby(ctx, promote)
	refusedNotOwner(t, "a session's takeover on a parked machine", err)
	tok := client.WithHandler(dm.SocketHandler())
	tok.Token = "potluck_" + strings.Repeat("a", 52)
	_, err = tok.PromoteStandby(ctx, promote)
	refusedNotOwner(t, "a token on a parked machine's socket", err)
	if got := e.Presses(); len(got) != 0 {
		t.Fatalf("a refused press reached the engine: %+v", got)
	}
	// The status and the presses so far read the home's registry and wrote
	// nothing in the home: not even SQLite's -wal or -shm. A read-only open
	// in place leaves them; a read-write one makes them and removes them on
	// close, so abort and resume run while the home is read only.
	if _, err := c.PromoteStandby(ctx, promote); err != nil {
		t.Fatal(err)
	}
	if after := treeOf(t, home); !maps.Equal(before, after) {
		for p := range after {
			if _, ok := before[p]; !ok {
				t.Errorf("a parked press made %s", p)
			}
		}
		t.Fatal("a parked press changed the home")
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	for _, p := range []string{api.MoveAbortPath, api.MoveResumePath} {
		if err := c.Do(ctx, http.MethodPost, p, api.MoveControl{MoveID: "mv1"}, nil); err != nil {
			t.Errorf("POST %s on a parked machine whose home is read only: %v", p, err)
		}
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// A session backs a stopped move out on a parked machine too.
	if _, err := sess.AbortMove(ctx, api.MoveControl{MoveID: "mv1"}); err != nil {
		t.Errorf("a session's abort on a parked machine: %v", err)
	}
	calls := e.Presses()
	if len(calls) != 4 || calls[0].method != "StartPromote" || calls[0].by.Identity != "ana" || calls[0].by.Machine != "delta" ||
		calls[1].method != "AbortMove" || calls[2].method != "ResumeMove" || calls[3].method != "AbortMove" {
		t.Fatalf("the engine was asked %+v", calls)
	}
	// Not a parked machine's: answered parked, as is the network.
	if _, err := c.SetStandby(ctx, api.StandbyRequest{Machine: "delta", Shown: shownAt("echo", 3)}); !api.IsCode(err, api.CodeParked) {
		t.Errorf("a standby press on a parked machine: %v", err)
	}
	body, _ := json.Marshal(promote)
	if code, err := netCall(dm, http.MethodPost, api.PromotePath, "", &core.Remote{NodeID: "nX", NodeKey: "nodekey:x"}, string(body)); code != http.StatusServiceUnavailable || !api.IsCode(err, api.CodeParked) {
		t.Errorf("a takeover from the network: %d %v", code, err)
	}
	if code, _ := netCall(dm, http.MethodGet, api.AuthorityPath, "", &core.Remote{NodeID: "nX", NodeKey: "nodekey:x"}, ""); code != http.StatusServiceUnavailable {
		t.Errorf("a parked status from the network: %d", code)
	}
	if got := e.Presses(); len(got) != 4 {
		t.Fatalf("the engine was asked from off the socket: %+v", got[4:])
	}

	// With no routes provided, the parked machine answers them parked.
	bare := parkedHome(t, nil, "delta", "ana", standby, nil)
	bc := client.WithHandler(bare.SocketHandler())
	if _, err := bc.PromoteStandby(ctx, promote); !api.IsCode(err, api.CodeParked) {
		t.Errorf("a takeover on a parked machine with no engine: %v", err)
	}
	if _, err := bc.Authority(ctx); !api.IsCode(err, api.CodeParked) {
		t.Errorf("a status on a parked machine with no engine: %v", err)
	}
}

// On a standby the owner is the one identity whose live node-bound tokens
// it holds, by HQ's restored registry, and must own every team project
// there. A plain member's tokens (under the owner's machine name too),
// tokens of two identities (on one node too), of one identity on two
// nodes, revoked or expired tokens, no token, and no readable copy are
// each refused not_owner, and the engine never asked: being this machine's
// owner, or naming it as an owner's, is not enough.
func TestParkedStandbyNeedsAnOwnerOfEveryTeamProject(t *testing.T) {
	ctx := context.Background()
	pred := hometerm.Peer{Via: core.ViaTunnel, URL: "http://127.0.0.1:1", Fingerprint: "fp-old"}
	standby, err := hometerm.NewStandby(pred, 3)
	if err != nil {
		t.Fatal(err)
	}
	presses := func(c *client.Client, machine string) map[string]error {
		out := map[string]error{}
		_, out["promote"] = c.PromoteStandby(ctx, api.PromoteRequest{Machine: machine, Shown: shownAt("echo", 3), OldHomeIsDown: true})
		_, out["promote --force"] = c.PromoteStandby(ctx, api.PromoteRequest{Machine: machine, Shown: shownAt("echo", 3), Force: true})
		_, out["retire"] = c.RetireHQ(ctx, api.RetireRequest{Machine: machine, Shown: shownAt("echo", 4)})
		_, out["abort"] = c.AbortMove(ctx, api.MoveControl{MoveID: "mv1"})
		_, out["resume"] = c.ResumeMove(ctx, api.MoveControl{MoveID: "mv1"})
		return out
	}
	e := &fakeEngine{status: api.AuthorityStatus{Machine: "echo", State: api.HQServing, Epoch: 3, Standby: api.StandbyStatus{Machine: "charlie", State: api.StandbyReady}}}
	m := homeAndMembers(t)
	const (
		ana = `(SELECT id FROM identities WHERE name = 'ana')`
		bo  = `(SELECT id FROM identities WHERE name = 'bo')`
	)
	refused := func(name string, dm *server.Daemon, machine string) {
		t.Helper()
		for press, err := range presses(client.WithHandler(dm.SocketHandler()), machine) {
			refusedNotOwner(t, name+": "+press, err)
		}
	}

	// bo, a plain member, holds the standby on charlie.
	member := parkedHome(t, m.hq, "charlie", "bo", standby, e, m.bo)
	refused("a plain member's standby", member, "charlie")
	if st, err := client.WithHandler(member.SocketHandler()).Authority(ctx); err != nil || st.CanManage {
		t.Errorf("a plain member's standby's status: %+v %v", st, err)
	}
	// The same, its machine named as the owner's (POTLUCK_MACHINE).
	refused("a plain member's standby named as the owner's machine", parkedHome(t, m.hq, "delta", "ana", standby, e, m.bo), "delta")
	// It holds no token at all.
	refused("a standby that holds no token", parkedHome(t, m.hq, "delta", "ana", standby, e), "delta")
	// The tokens of the owner and of a member (the owner's read first).
	refused("a standby holding two identities' tokens", parkedHome(t, m.hq, "delta", "ana", standby, e, m.ana, m.bo), "delta")
	// ... bound to one node.
	one := parkedHome(t, m.hq, "delta", "ana", standby, e, m.ana, m.bo)
	editRegistry(t, one, `UPDATE tokens SET node = (SELECT node FROM tokens WHERE identity = `+ana+` AND node != '' LIMIT 1) WHERE identity = `+bo+` AND node != ''`)
	refused("a standby holding two identities' tokens on one node", one, "delta")
	// The owner's tokens, bound to two nodes.
	two := parkedHome(t, m.hq, "delta", "ana", standby, e, m.ana, m.bo)
	editRegistry(t, two, `UPDATE tokens SET identity = `+ana+` WHERE identity = `+bo+` AND node != ''`)
	refused("a standby holding one identity's tokens of two nodes", two, "delta")
	// The owner's tokens, revoked or expired.
	revoked := parkedHome(t, m.hq, "delta", "ana", standby, e, m.ana)
	editRegistry(t, revoked, `UPDATE tokens SET revoked = 1 WHERE identity = `+ana+` AND node != ''`)
	refused("a standby holding a revoked token", revoked, "delta")
	expired := parkedHome(t, m.hq, "delta", "ana", standby, e, m.ana)
	editRegistry(t, expired, `UPDATE tokens SET expires = 1 WHERE identity = `+ana+` AND node != ''`)
	refused("a standby holding an expired token", expired, "delta")
	// No copy, or one that is no database.
	refused("a standby with no copy", parkedHome(t, nil, "charlie", "bo", standby, e, m.bo), "charlie")
	src := homeWithMembers(t)
	src.stop()
	if err := os.WriteFile(src.cfg.RegistryPath(), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(src.cfg.RegistryPath() + "-wal")
	refused("a standby whose copy is no database", parkedHome(t, src, "delta", "ana", standby, e, m.ana), "delta")
	if got := e.Presses(); len(got) != 0 {
		t.Fatalf("a refused press reached the engine: %+v", got)
	}

	// The owner's own tokens alone pass, as the machine HQ bound them to.
	ok := &fakeEngine{status: e.status}
	if _, err := client.WithHandler(parkedHome(t, m.hq, "delta", "ana", standby, ok, m.ana).SocketHandler()).
		PromoteStandby(ctx, api.PromoteRequest{Machine: "delta", Shown: shownAt("echo", 3), OldHomeIsDown: true}); err != nil {
		t.Fatalf("the owner's standby: %v", err)
	}
	if c := ok.Presses(); len(c) != 1 || c[0].by.Identity != "ana" || c[0].by.Machine != "delta" {
		t.Fatalf("the engine was asked %+v", c)
	}
}

// On a fenced or frozen old HQ the owner is the home's own (its registry
// marks them local) and must own every team project: they retire, take
// their data back and back a move out; a home with no team project refuses
// everyone.
func TestParkedOldHQ(t *testing.T) {
	ctx := context.Background()
	succ := hometerm.Peer{Via: core.ViaTunnel, URL: "http://127.0.0.1:1", Fingerprint: "fp-new"}
	fenced, err := hometerm.Initial().Fence(succ, 4)
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeEngine{status: api.AuthorityStatus{Machine: "delta", State: api.HQServing, Epoch: 4, Standby: api.StandbyStatus{State: api.StandbyNone}}}
	src := homeWithMembers(t)
	old := parkedHome(t, src, "echo", "echo-human", fenced, e)
	c := client.WithHandler(old.SocketHandler())
	if _, err := c.RetireHQ(ctx, api.RetireRequest{Machine: "echo", Shown: shownAt("delta", 4)}); err != nil {
		t.Fatalf("the old HQ's owner retires it: %v", err)
	}
	if _, err := c.PromoteStandby(ctx, api.PromoteRequest{Machine: "echo", Shown: shownAt("delta", 4), OldHomeIsDown: true}); err != nil {
		t.Fatalf("the old HQ's owner takes its data back: %v", err)
	}
	if calls := e.Presses(); len(calls) != 2 || calls[0].method != "Retire" || calls[0].by.Identity != "echo-human" || calls[1].method != "StartPromote" {
		t.Fatalf("the engine was asked %+v", calls)
	}
	// Another human on a copy of the same home is not its owner.
	other := client.WithHandler(parkedHome(t, src, "echo", "mallory", fenced, e).SocketHandler())
	_, err = other.RetireHQ(ctx, api.RetireRequest{Machine: "echo", Shown: shownAt("delta", 4)})
	refusedNotOwner(t, "another human on the old HQ", err)
	if got := e.Presses(); len(got) != 2 {
		t.Fatalf("a refused press reached the engine: %+v", got[2:])
	}

	// A frozen home that holds no team project: no one presses.
	frozen, err := hometerm.Initial().Freeze()
	if err != nil {
		t.Fatal(err)
	}
	tn := tailnettest.New()
	solo := newHQDaemon(t, tn, "den", tailnettest.Identity{NodeID: "nDEN", NodeKey: "nodekey:den", Login: "cy@example.com", Host: "den"}, nil)
	if _, err := solo.c.CreateProject(ctx, api.NewProjectRequest{Slug: "solo"}); err != nil {
		t.Fatal(err)
	}
	n := len(e.Presses())
	sc := client.WithHandler(parkedHome(t, solo, "den", "den-human", frozen, e).SocketHandler())
	_, err = sc.AbortMove(ctx, api.MoveControl{MoveID: "mv1"})
	refusedNotOwner(t, "a home with no team project", err)
	if got := e.Presses(); len(got) != n {
		t.Fatalf("a refused press reached the engine: %+v", got[n:])
	}
}

// The codes zm's HQ puts in a standby setup's failure (its Setup* codes)
// pass through the status as sent; not_opted_in and term_no_copy also pass
// as a standby's reason and keep their reason on a refused press. A code
// outside the vocabulary answers as unknown. The codes are zm's literals, so
// the test does not read them from the list it checks.
func TestHQSetupReasonCodesPassThrough(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	e := &fakeEngine{}
	hq := newHQDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, e)
	teamProject(t, hq.c, "orchid")
	answer := func(code string) (failure, standby string) {
		e.mu.Lock()
		e.status = api.AuthorityStatus{
			Standby: api.StandbyStatus{Machine: "delta", State: api.StandbyCopying, Reasons: []api.Reason{{Code: code, Text: "a fixed phrase"}}},
			Setup:   &api.StandbySetup{Machine: "delta", Step: api.SetupAsking, Failure: &api.Reason{Code: code, Text: "a fixed phrase"}},
		}
		e.mu.Unlock()
		st, err := hq.c.Authority(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st.Setup.Failure.Code, st.Standby.Reasons[0].Code
	}
	setup := []string{"is_authority", "not_serving", "switch_busy", "unreachable", "file_system", "no_disk", "no_tokens",
		"no_set", "build", "check", "not_owner", "identity", "sealed", "stop", "not_opted_in", "not_linked",
		"unsupported_os", "no_tool", "term_no_copy", "interrupted", "parked_start", "wrong_key", "no_answer", "op_ended",
		"released"}
	// Also a candidate's or standby's reason.
	reasons := []string{"no_disk", "not_owner", "not_opted_in", "term_no_copy"}
	for _, code := range setup {
		f, s := answer(code)
		if f != code {
			t.Errorf("setup failure %q answered as %q", code, f)
		}
		want := "unknown"
		if slices.Contains(reasons, code) {
			want = code
		}
		if s != want {
			t.Errorf("standby reason %q answered as %q, want %q", code, s, want)
		}
	}
	for _, code := range []string{"not_a_reason", "NOT_OPTED_IN", "no_engine", ""} {
		if f, _ := answer(code); f != "unknown" {
			t.Errorf("setup failure %q answered as %q, want unknown", code, f)
		}
	}
	for _, code := range []string{"not_opted_in", "term_no_copy"} {
		e.err = &api.Error{Code: api.CodeConflict, Message: "anything", Data: json.RawMessage(`{"reason":"` + code + `"}`)}
		_, err := sockCall(hq.dm, http.MethodPost, api.MoveAbortPath, `{"move_id":"mv1"}`)
		var got *api.Error
		if !errors.As(err, &got) || api.HQRefusalReason(got) != code || strings.Contains(got.Message, "log says why") || got.Hint == "" {
			t.Errorf("a press refused with %q answered %+v", code, err)
		}
	}
}
