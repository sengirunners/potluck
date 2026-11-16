package issuers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// fakeRunTargets stands in for run targets: the end hook the module
// subscribes, and the door (every box trusted and live; what it writes).
type fakeRunTargets struct {
	rt.Service
	mu     sync.Mutex
	onEnd  []rt.EndFunc
	writes []string
	class  string            // every box's machine class (default trusted)
	vals   map[string]string // handle name -> the last value written
	lease  time.Time         // every box's lease end (zero: none)
}

func (f *fakeRunTargets) setClass(c string) { f.mu.Lock(); f.class = c; f.mu.Unlock() }

func (f *fakeRunTargets) OnEnd(fn rt.EndFunc) {
	f.mu.Lock()
	f.onEnd = append(f.onEnd, fn)
	f.mu.Unlock()
}
func (f *fakeRunTargets) BoxSecret(_ context.Context, project, handle, name string, v secrets.Value) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, handle+" "+name)
	if f.vals == nil {
		f.vals = map[string]string{}
	}
	f.vals[handle+" "+name] = v.Reveal()
	return nil
}

func (f *fakeRunTargets) value(handle, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vals[handle+" "+name]
}

func (f *fakeRunTargets) BoxExpires(context.Context, string, string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lease, nil
}
func (f *fakeRunTargets) BoxClass(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.class != "" {
		return f.class, nil
	}
	return ClassTrusted, nil
}
func (f *fakeRunTargets) BoxSwapSafe(context.Context, string, string) error     { return nil }
func (f *fakeRunTargets) BoxLive(context.Context, string, string) (bool, error) { return true, nil }

// moduleDeps is a daemon's Deps with project orchid, run targets faked, and
// config.json's secrets.issuers set to issuers.
func moduleDeps(t *testing.T, iss map[string]config.SecretsIssuer) (*core.Deps, *fakeRunTargets, string) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "judy", Machine: "mini"}
	cfg.File.Secrets = config.Secrets{Issuers: iss}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	ref, _ := reg.Lookup("orchid")
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rts := &fakeRunTargets{}
	d.Provide(rt.Name, rt.Service(rts))
	d.Provide(rt.DoorName, rt.Door(rts))
	return d, rts, ref.ID
}

// With no github-app issuer, or two, the module is idle: From answers nil,
// it hooks no box end, and Start and Close do nothing.
func TestModuleIdle(t *testing.T) {
	e := newEnv(t, appCeiling)
	one := e.cfg.Secrets.Issuers["gh"]
	for name, iss := range map[string]map[string]config.SecretsIssuer{
		"none": nil,
		"two":  {"gh": one, "gh-two": one},
	} {
		d, rts, _ := moduleDeps(t, iss)
		m := NewModule(d)
		if From(d) != nil || grants.IssuerOf(d, computeapi.SecretKindGitHubToken) != nil {
			t.Errorf("%s: an issuer is running", name)
		}
		if len(rts.onEnd) != 0 {
			t.Errorf("%s: hooked box ends", name)
		}
		if err := m.Start(context.Background()); err != nil {
			t.Errorf("%s: Start: %v", name, err)
		}
		if err := m.Close(); err != nil {
			t.Errorf("%s: Close: %v", name, err)
		}
	}
	if calls, _, _, _ := e.gh.snapshot(); len(calls) != 0 {
		t.Errorf("an idle module called GitHub: %v", calls)
	}
}

// With one issuer: From is the module, Issue waits for Start (a caller
// giving up first gets unavailable and nothing is minted), then issues
// through the door, and a box's end (run targets' OnEnd) revokes its token.
func TestModuleIssuesAfterStartAndRevokesAtEnd(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, appCeiling)
	e.clk.mu.Lock()
	e.clk.now = time.Now() // the daemon's issuer runs on the real clock
	e.clk.mu.Unlock()
	d, rts, proj := moduleDeps(t, e.cfg.Secrets.Issuers)
	m := NewModule(d)
	t.Cleanup(func() { m.Close() })
	if From(d) == nil || len(rts.onEnd) != 1 {
		t.Fatalf("one issuer: From %v, %d end hooks", From(d), len(rts.onEnd))
	}

	box := Box{Project: proj, ID: "box:small/1"}
	jr := jobGrantRequest(t, d, proj, box)

	early, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err := m.Issue(early, jr)
	cancel()
	if codeOf(err) != api.CodeUnavailable {
		t.Fatalf("Issue before Start: %v, want unavailable", err)
	}
	if _, mints, _, _ := e.gh.snapshot(); len(mints) != 0 {
		t.Fatalf("minted before Start: %v", mints)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := m.Issue(wait, jr)
	if err != nil || out.Repo != "acme/app" || out.Permissions["contents"] != "write" {
		t.Fatalf("Issue after Start: %+v %v", out, err)
	}
	rts.mu.Lock()
	writes := strings.Join(rts.writes, ";")
	rts.mu.Unlock()
	if writes != "box:small/1 GH_TOKEN" || e.gh.liveCount() != 1 {
		t.Fatalf("delivered %q, %d live tokens", writes, e.gh.liveCount())
	}

	// Another box's end revokes nothing; this box's end revokes its token.
	rts.onEnd[0](ctx, proj, computeapi.RunTargetHandle{Handle: "box:small/2", State: "released"})
	if e.gh.liveCount() != 1 {
		t.Fatal("another box's end revoked this box's token")
	}
	rts.onEnd[0](ctx, proj, computeapi.RunTargetHandle{Handle: box.ID, State: "released"})
	if _, _, minted, revoked := e.gh.snapshot(); e.gh.liveCount() != 0 || len(revoked) != 1 || revoked[0] != minted[0] {
		t.Fatalf("after the box's end: %d live, revoked %v of %v", e.gh.liveCount(), revoked, minted)
	}
}

// jobGrantRequest adds a job grant of GH_TOKEN for mainJob through the
// grants routes, and answers the request it gives mainJob for box.
func jobGrantRequest(t *testing.T, d *core.Deps, proj string, box Box) Request {
	t.Helper()
	r := core.NewRouter()
	grants.New(d).Routes(r)
	b, _ := json.Marshal(computeapi.AddSecretGrantRequest{
		To:   computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/lander.yml", Ref: "refs/heads/main", Events: []string{"push"}}},
		Kind: computeapi.SecretKindGitHubToken, Name: "GH_TOKEN", Scopes: []string{"box"},
		Ceiling: json.RawMessage(`{"repos":["acme/app"],"permissions":{"contents":"write"}}`)})
	req := httptest.NewRequest(http.MethodPost, "/v0/secret_grants", bytes.NewReader(b))
	c := core.WithResolveInput(core.WithSubject(req.Context(), d.Projects.Local("")), project.ResolveInput{Explicit: "orchid", ExplicitFrom: project.RuleFlag})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req.WithContext(c))
	if rec.Code != http.StatusOK {
		t.Fatalf("add a job grant: %d %s", rec.Code, rec.Body)
	}
	gs, err := grants.JobGrants(context.Background(), d, proj, "", "acme/app")
	if err != nil || len(gs) != 1 || !grants.JobMatches(gs[0], *mainJob()) {
		t.Fatalf("the job grants: %+v %v", gs, err)
	}
	jr, err := JobRequest(gs[0], *mainJob(), box)
	if err != nil {
		t.Fatal(err)
	}
	return jr
}
