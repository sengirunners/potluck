package project

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/store"
)

func TestTOMLRoundTrip(t *testing.T) {
	f := File{ID: "01JAAAAAAAAAAAAAAAAAAAAAAA", Slug: "orchid", Name: `on "Call"`,
		Repos: api.RepoRules{Paths: []string{"~/code/Orchid"}, Remotes: []string{"github.com/acme/Orchid"}},
		Areas: []api.Area{{Name: "sprout", RepoRules: api.RepoRules{Paths: []string{"~/code/Orchid/sprout"}}, Work: []string{"bd:sprout-*"}, Labels: []string{"sprout"}}},
	}
	got, err := ParseFile(f.Encode())
	if err != nil {
		t.Fatalf("%v\n%s", err, f.Encode())
	}
	if !reflect.DeepEqual(got, f) {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, f)
	}
	hand := `
# comment
id = "01JAAAAAAAAAAAAAAAAAAAAAAA"
slug = 'side'   # trailing
name = "Side"
paths = [
  "~/a",   # first
  "~/b",
]
[[areas]]
name = "x"
`
	g, err := ParseFile(hand)
	if err != nil || len(g.Repos.Paths) != 2 || g.Areas[0].Name != "x" {
		t.Fatalf("%+v %v", g, err)
	}
	for _, bad := range []string{`slug = "x"` + "\nname = 1", `slug = "x"` + "\n[a.b]", `slug = "none"`, "slug = \"x\"\n[[areas]]\nname = \"a\"\n[[areas]]\nname = \"a\""} {
		if _, err := ParseFile(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	rf, err := ParseRepoFile(RepoFile{Project: "orchid", Area: "sprout"}.Encode())
	if err != nil || rf.Project != "orchid" || rf.Area != "sprout" {
		t.Fatal(rf, err)
	}
}

func TestNormalizeRemoteAndGlob(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/Orchid.git":          "github.com/acme/Orchid",
		"git@GitHub.com:acme/Orchid.git":              "github.com/acme/Orchid",
		"ssh://git@github.com:22/acme/Orchid":         "github.com/acme/Orchid",
		"https://user:pw@gitlab.example.com/g/sub/r/": "gitlab.example.com/g/sub/r",
		"github.com/acme/Orchid":                      "github.com/acme/Orchid",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if !Glob("bd:sprout-*", "bd:sprout-3x") || Glob("bd:sprout-*", "bd:billing-1") || !Glob("gh:o/*#*", "gh:o/r#7") || !Glob("x", "x") {
		t.Fatal("glob")
	}
}

type fixture struct {
	ctx            context.Context
	reg            *Registry
	st             *store.Store
	local          *authz.Subject
	orchid, side   api.Project
	repo, sideRepo string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "judy", Machine: "mini"}
	reg, err := Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{ctx: ctx, reg: reg, st: st, local: reg.Local("")}
	src := t.TempDir()
	f.repo, f.sideRepo = filepath.Join(src, "Orchid"), filepath.Join(src, "side")
	mkRepo(t, f.repo, "git@github.com:acme/Orchid.git")
	mkRepo(t, f.sideRepo, "")
	f.orchid, err = reg.Create(ctx, f.local, api.NewProjectRequest{Slug: "orchid", Name: "Orchid",
		RepoRules: api.RepoRules{Remotes: []string{"https://github.com/acme/Orchid"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"sprout", "billing"} {
		if _, err := reg.AddArea(ctx, f.local, "orchid", api.Area{Name: a, RepoRules: api.RepoRules{Paths: []string{filepath.Join(f.repo, a)}}, Work: []string{"bd:" + a + "-*"}}); err != nil {
			t.Fatal(err)
		}
	}
	f.side, err = reg.Create(ctx, f.local, api.NewProjectRequest{Slug: "side", RepoRules: api.RepoRules{Paths: []string{f.sideRepo}}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mkRepo(t *testing.T, dir, origin string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n"
	if origin != "" {
		cfg += "[remote \"origin\"]\n\turl = " + origin + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"sprout/pkg", "billing", "docs"} {
		_ = os.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
}

func TestRegistryScopedViews(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	// Reopen: everything survives (project.toml + registry row).
	reg2, err := Open(ctx, f.st, &config.Config{Home: f.st.Home(), Human: "judy", Machine: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	if ps := reg2.List(authz.All()); len(ps) != 2 || len(ps[0].Areas) != 2 {
		t.Fatalf("reopen: %+v", ps)
	}
	tok, err := f.reg.Mint(ctx, f.local, api.MintTokenRequest{As: "victor", Project: "orchid", Area: "sprout", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tok.Token.ID, tok.Secret) || tok.Token.Area != "sprout" {
		t.Fatalf("%+v", tok)
	}
	v, err := f.reg.FromToken(ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	ps := f.reg.List(v.Scope)
	if len(ps) != 1 || ps[0].Slug != "orchid" || len(ps[0].Areas) != 1 || ps[0].Areas[0].Name != "sprout" ||
		len(ps[0].Remotes) != 0 || ps[0].Role != "guest" {
		t.Fatalf("guest view: %+v", ps)
	}
	_, hidden := f.reg.Get(v.Scope, "side")
	_, missing := f.reg.Get(v.Scope, "nope")
	if hidden == nil || strings.Replace(hidden.Error(), "side", "X", 1) != strings.Replace(missing.Error(), "nope", "X", 1) {
		t.Fatalf("hidden %v missing %v", hidden, missing)
	}
	if _, err := f.reg.Grant(ctx, v, api.GrantRequest{Identity: "eve", Role: "owner", Project: "orchid", Area: "sprout"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("guest granted: %v", err)
	}
	if _, err := f.reg.Grant(ctx, v, api.GrantRequest{Identity: "eve", Role: "owner", Project: "side"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("guest granted in hidden project: %v", err)
	}
	if _, err := f.reg.Grants(ctx, v, "side"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("guest listed hidden grants: %v", err)
	}
	gs, err := f.reg.Grants(ctx, f.local, "orchid")
	if err != nil || len(gs) != 2 {
		t.Fatalf("grants %+v %v", gs, err)
	}
	// Revoke the grant: the token keeps authenticating but sees nothing.
	if _, err := f.reg.Revoke(ctx, f.local, api.GrantRequest{Identity: "victor", Project: "orchid", Area: "sprout"}); err != nil {
		t.Fatal(err)
	}
	v, _ = f.reg.FromToken(ctx, tok.Secret, true)
	if len(f.reg.List(v.Scope)) != 0 {
		t.Fatal("revoked grant still visible")
	}
	if _, err := f.reg.RevokeToken(ctx, tok.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.FromToken(ctx, tok.Secret, true); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("revoked token: %v", err)
	}
	if _, err := f.reg.FromToken(ctx, "zm_nope", true); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatal(err)
	}
	if _, err := f.reg.Revoke(ctx, f.local, api.GrantRequest{Identity: "judy", Project: "orchid"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("owner self-revoke: %v", err)
	}
	// Tailscale identities.
	if _, err := f.reg.AddIdentity(ctx, api.NewIdentityRequest{Name: "carl", TailscaleLogin: "carl@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Grant(ctx, f.local, api.GrantRequest{Identity: "carl", Role: "member", Project: "side"}); err != nil {
		t.Fatal(err)
	}
	c, err := f.reg.FromTailscale(ctx, "carl@example.com")
	if err != nil || !c.Can(authz.Operate, authz.Object{Project: f.side.ID}) || c.Can(authz.Read, authz.Object{Project: f.orchid.ID}) || !c.Remote {
		t.Fatalf("carl %+v %v", c, err)
	}
	if _, err := f.reg.FromTailscale(ctx, "mallory@example.com"); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatal(err)
	}
}

func TestResolveOrder(t *testing.T) {
	f := newFixture(t)
	sc := authz.All()
	rule := func(in ResolveInput) string {
		r := f.reg.Resolve(sc, in)
		if r.Project == nil {
			return "ERR"
		}
		return r.Rule + ":" + r.Project.Slug + areaSuffix(r.Area)
	}
	cases := []struct {
		in   ResolveInput
		want string
	}{
		{ResolveInput{Explicit: "side", ExplicitFrom: RuleFlag, Cwd: f.repo}, "flag:side"},
		{ResolveInput{Explicit: "orchid/sprout", ExplicitFrom: RuleEnv}, "env:orchid/sprout"},
		{ResolveInput{Explicit: "nope", Default: "side"}, "ERR"},                                                         // an explicit miss never falls through
		{ResolveInput{Cwd: filepath.Join(f.repo, "sprout", "pkg"), SessionProject: f.side.ID}, "cwd-path:orchid/sprout"}, // cwd beats session
		{ResolveInput{Cwd: filepath.Join(f.repo, "docs")}, "cwd-remote:orchid"},
		{ResolveInput{Cwd: f.sideRepo}, "cwd-path:side"},
		{ResolveInput{Cwd: t.TempDir(), SessionProject: f.side.ID}, "session:side"},
		{ResolveInput{Cwd: t.TempDir(), SessionProject: api.ProjectNone, Default: "orchid"}, "default_project:orchid"},
		{ResolveInput{Cwd: t.TempDir()}, "ERR"},
	}
	for i, c := range cases {
		if got := rule(c.in); got != c.want {
			t.Errorf("#%d: got %s, want %s (%+v)", i, got, c.want, f.reg.Resolve(sc, c.in).Steps)
		}
	}
	// .zm/project.toml in the repo beats path and remote rules.
	if _, err := WriteRepoFile(f.repo, RepoFile{Project: "side"}); err != nil {
		t.Fatal(err)
	}
	if got := rule(ResolveInput{Cwd: filepath.Join(f.repo, "docs")}); got != "cwd-file:side" {
		t.Fatal(got)
	}
	_ = os.WriteFile(filepath.Join(f.repo, RepoFileDir, FileName), []byte(`project = "gone"`), 0o644)
	if got := rule(ResolveInput{Cwd: f.repo, SessionProject: f.side.ID}); got != "ERR" {
		t.Fatal("a repo file naming a missing project must not fall through:", got)
	}
	// A guest of orchid/sprout resolves orchid (to its one area) and never sees side.
	tok, _ := f.reg.Mint(f.ctx, f.local, api.MintTokenRequest{As: "victor", Project: "orchid", Area: "sprout", Role: "guest"})
	v, _ := f.reg.FromToken(f.ctx, tok.Secret, true)
	if r := f.reg.Resolve(v.Scope, ResolveInput{Explicit: "orchid"}); r.Project == nil || r.Area != "sprout" {
		t.Fatalf("%+v", r)
	}
	if r := f.reg.Resolve(v.Scope, ResolveInput{Explicit: "side"}); r.Project != nil || r.Steps[0].Result != "no project side" {
		t.Fatalf("%+v", r)
	}
	if r := f.reg.Resolve(v.Scope, ResolveInput{Explicit: "orchid/billing"}); r.Project != nil || r.Steps[0].Result != "no area billing in orchid" {
		t.Fatalf("%+v", r)
	}
}

func TestOriginURLWorktree(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	mkRepo(t, main, "https://github.com/o/r.git")
	wtGit := filepath.Join(main, ".git", "worktrees", "wt")
	_ = os.MkdirAll(wtGit, 0o755)
	_ = os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644)
	wt := filepath.Join(dir, "wt")
	_ = os.MkdirAll(wt, 0o755)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644)
	if got := NormalizeRemote(OriginURL(GitRoot(filepath.Join(wt)))); got != "github.com/o/r" {
		t.Fatal(got)
	}
}

func TestExport(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "x")
	res, err := f.reg.Export(f.ctx, f.local, "orchid", out)
	if err != nil || len(res.Files) != 2 {
		t.Fatal(res, err)
	}
	b, _ := os.ReadFile(res.Files[1])
	if pf, err := ParseFile(string(b)); err != nil || pf.Slug != "orchid" || len(pf.Areas) != 2 {
		t.Fatal(pf, err)
	}
	if _, err := f.reg.Export(f.ctx, f.local, "orchid", out); !api.IsCode(err, api.CodeConflict) {
		t.Fatal("export overwrote", err)
	}
}

// Dogfood: "~/code/Orchid*" was accepted and silently never matched.
func TestPathGlobRules(t *testing.T) {
	f := newFixture(t)
	base := filepath.Dir(f.repo) // .../Orchid and .../side live here
	for _, d := range []string{"Orchid-clean/src", "Orchid-7712", "other"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g, err := f.reg.Create(f.ctx, f.local, api.NewProjectRequest{Slug: "globbed",
		RepoRules: api.RepoRules{Paths: []string{filepath.Join(base, "Orchid-*")}}})
	if err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		filepath.Join(base, "Orchid-clean", "src"): "cwd-path:globbed",
		filepath.Join(base, "Orchid-7712"):         "cwd-path:globbed",
		filepath.Join(base, "other"):               "ERR",
		base:                                       "ERR",
	} {
		r := f.reg.Resolve(authz.All(), ResolveInput{Cwd: dir})
		got := "ERR"
		if r.Project != nil {
			got = r.Rule + ":" + r.Project.Slug
		}
		if got != want {
			t.Errorf("%s: got %s, want %s", dir, got, want)
		}
	}
	// A longer plain path still beats the glob (side's area paths are plain).
	if n, ok := PathMatch(ExpandPath(filepath.Join(base, "Orchid-clean", "src")), filepath.Join(base, "Orchid*")); !ok || n != len(ExpandPath(filepath.Join(base, "Orchid-clean"))) {
		t.Fatalf("PathMatch length %d %v", n, ok)
	}
	roots := ExpandRoots([]string{filepath.Join(base, "Orchid-*"), filepath.Join(base, "missing-*")})
	if len(roots) != 2 {
		t.Fatalf("ExpandRoots: %v", roots)
	}
	_ = g
}

// Dogfood: changing an area's rules needed a hand edit of project.toml and a
// daemon restart; `zm project show orchid/sprout` answered "no project".
func TestAreaSetAppliesLive(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	labels, newDir := []string{"sprout", " "}, filepath.Join(f.repo, "docs")
	v, err := f.reg.UpdateArea(ctx, f.local, "orchid", "Sprout", api.UpdateAreaRequest{Labels: &labels, Paths: &[]string{newDir}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "sprout" || !reflect.DeepEqual(v.Labels, []string{"sprout"}) || !reflect.DeepEqual(v.Work, []string{"bd:sprout-*"}) ||
		!reflect.DeepEqual(v.Paths, []string{newDir}) || v.Role != "owner" {
		t.Fatalf("view %+v", v)
	}
	// Live: the registry's file and resolution see it on the next call.
	if pf, _ := f.reg.File(f.orchid.ID); !reflect.DeepEqual(pf.Areas[0].Labels, []string{"sprout"}) {
		t.Fatalf("labels not applied: %+v", pf.Areas)
	}
	if r := f.reg.Resolve(authz.All(), ResolveInput{Cwd: newDir}); r.Project == nil || r.Area != "sprout" || r.Rule != RuleCwdPath {
		t.Fatalf("paths not applied: %+v", r)
	}
	// Cleared with an empty list; persisted to project.toml (a restart keeps it).
	if _, err := f.reg.UpdateArea(ctx, f.local, "orchid", "sprout", api.UpdateAreaRequest{Work: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	reg2, err := Open(ctx, f.st, &config.Config{Home: f.st.Home(), Human: "judy", Machine: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := reg2.GetArea(authz.All(), "orchid", "sprout"); err != nil || len(v.Work) != 0 || !reflect.DeepEqual(v.Labels, []string{"sprout"}) {
		t.Fatalf("after reopen %+v %v", v, err)
	} else if b, _ := json.Marshal(v); !strings.Contains(string(b), `"work":[]`) || !strings.Contains(string(b), `"remotes":[]`) {
		// Review: a cleared list answered "work": null (absent) while others were arrays.
		t.Fatalf("cleared list is not an array: %s", b)
	}
	// Area add and project edits are live too.
	if _, err := f.reg.AddArea(ctx, f.local, "orchid", api.Area{Name: "docs", Labels: []string{"docs"}}); err != nil {
		t.Fatal(err)
	}
	if !f.reg.HasArea(f.orchid.ID, "docs") {
		t.Fatal("added area not applied")
	}
	if _, err := f.reg.Update(ctx, f.local, "side", api.UpdateProjectRequest{Repos: &api.RepoRules{Paths: []string{newDir}}}); err != nil {
		t.Fatal(err)
	}
	if r := f.reg.Resolve(authz.All(), ResolveInput{Cwd: f.sideRepo}); r.Project != nil && r.Project.Slug == "side" && r.Rule == RuleCwdPath {
		t.Fatalf("old project path still resolves: %+v", r)
	}

	// A guest of orchid/sprout sees its area; a hidden area or project
	// answers exactly like a missing one.
	tok, _ := f.reg.Mint(ctx, f.local, api.MintTokenRequest{As: "victor", Project: "orchid", Area: "sprout", Role: "guest"})
	g, _ := f.reg.FromToken(ctx, tok.Secret, true)
	if gv, err := f.reg.GetArea(g.Scope, "orchid", "sprout"); err != nil || gv.Role != "guest" {
		t.Fatalf("guest view %+v %v", gv, err)
	}
	same := func(what string, hidden, missing error, h, m string) {
		t.Helper()
		if hidden == nil || missing == nil || strings.ReplaceAll(hidden.Error(), h, "X") != strings.ReplaceAll(missing.Error(), m, "X") {
			t.Errorf("%s: hidden %v, missing %v", what, hidden, missing)
		}
	}
	_, hid := f.reg.GetArea(g.Scope, "orchid", "billing")
	_, mis := f.reg.GetArea(g.Scope, "orchid", "nope")
	same("get hidden area", hid, mis, "billing", "nope")
	_, hid = f.reg.GetArea(g.Scope, "side", "x")
	_, mis = f.reg.GetArea(g.Scope, "nope", "x")
	same("get hidden project", hid, mis, "side", "nope")
	_, hid = f.reg.UpdateArea(ctx, g, "orchid", "billing", api.UpdateAreaRequest{Labels: &labels})
	_, mis = f.reg.UpdateArea(ctx, g, "orchid", "nope", api.UpdateAreaRequest{Labels: &labels})
	same("set hidden area", hid, mis, "billing", "nope")
	if _, err := f.reg.UpdateArea(ctx, g, "orchid", "sprout", api.UpdateAreaRequest{Work: &[]string{"*"}}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("guest widened its own area: %v", err)
	}
	// An owner token from the network may change globs and labels, never paths.
	ot, _ := f.reg.Mint(ctx, f.local, api.MintTokenRequest{As: "carl", Project: "orchid", Role: "owner"})
	o, _ := f.reg.FromToken(ctx, ot.Secret, true)
	if _, err := f.reg.UpdateArea(ctx, o, "orchid", "billing", api.UpdateAreaRequest{Remotes: &[]string{"github.com/a/b"}}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("remote owner set remotes: %v", err)
	}
	if _, err := f.reg.UpdateArea(ctx, o, "orchid", "billing", api.UpdateAreaRequest{Labels: &[]string{"billing"}}); err != nil {
		t.Fatal(err)
	}
}

// zm join takes a code or a project name: anything code-shaped (or no
// slug) is a code, so a typo never starts a GitHub join.
func TestLooksLikeJoinCode(t *testing.T) {
	for _, s := range []string{"zm-mini:euuarzri7ecyaxyq:ABCD-EFGH", "zm-nonsense", "nonsense:code", "ABCD-EFGH", "abcd-efgh",
		"NotASlug", "has space", "none", strings.Repeat("a", 64)} {
		if !LooksLikeJoinCode(s) {
			t.Errorf("%q taken for a project name", s)
		}
	}
	for _, s := range []string{"orchid", "nonsense", "web-app", "a.b_c", "abcd-efghi"} {
		if LooksLikeJoinCode(s) {
			t.Errorf("project %q taken for a code", s)
		}
	}
}
