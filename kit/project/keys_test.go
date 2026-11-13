package project

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// noHooks runs the test as a binary where no module has registered a
// section, a key or extras (a Potluck-only daemon), and puts back what the
// test binary had when the test ends. The test may register its own.
func noHooks(t *testing.T) {
	t.Helper()
	sectionsMu.Lock()
	s, k := sections, keys
	sections, keys = map[string]Section{}, map[string]Section{}
	sectionsMu.Unlock()
	extrasMu.Lock()
	x := extras
	extras = nil
	extrasMu.Unlock()
	t.Cleanup(func() {
		sectionsMu.Lock()
		sections, keys = s, k
		sectionsMu.Unlock()
		extrasMu.Lock()
		extras = x
		extrasMu.Unlock()
	})
}

const fileHead = "# zm project config. Change it with `zm project`/`zm area`; hand edits need a daemon restart.\n"

// A project.toml as zm writes it: guest_issues and two trackers.
const zmToml = fileHead + `id = "01JAAAAAAAAAAAAAAAAAAAAAAA"
slug = "orchid"
name = "Orchid"
remotes = ["github.com/acme/orchid"]
guest_issues = true

[[areas]]
name = "api"
work = ["bd:api-*"]

[[trackers]]
adapter = "beads"
prefix = "bd"
area = "api"
config = { dir = "~/code/acme/.beads", mode = "dir" }
env = "inherit"

[[trackers]]
adapter = "native"
prefix = "issue"
`

// The same file as a build writes it that knows neither name: the same
// values, each table's keys sorted.
const zmTomlUnowned = fileHead + `id = "01JAAAAAAAAAAAAAAAAAAAAAAA"
slug = "orchid"
name = "Orchid"
remotes = ["github.com/acme/orchid"]
guest_issues = true

[[areas]]
name = "api"
work = ["bd:api-*"]

[[trackers]]
adapter = "beads"
area = "api"
config = { dir = "~/code/acme/.beads", mode = "dir" }
env = "inherit"
prefix = "bd"

[[trackers]]
adapter = "native"
prefix = "issue"
`

// A project.toml with top-level keys and tables of every decodable shape
// that nothing in this binary knows, in the order Encode writes them.
const futureToml = fileHead + `id = "01JAAAAAAAAAAAAAAAAAAAAAAA"
slug = "orchid"
name = "Orchid"
future_flag = true
future_list = ["a", 1, true, ["b"]]
future_n = 42
future_off = false
future_s = "x # \"y\""
"odd key" = { a = "z", b = 1 }

[[areas]]
name = "api"

[gadget]
on = true

[[widgets]]
name = "w1"
opts = { deep = { n = 1 } }

[[widgets]]
name = "w2"
`

// roundTrip parses src and checks it is written back as want, and that
// what was written is written back unchanged again.
func roundTrip(t *testing.T, src, want string) File {
	t.Helper()
	f, err := ParseFile(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out := f.Encode()
	if out != want {
		t.Fatalf("first write differs\n--- got\n%s--- want\n%s", out, want)
	}
	g, err := ParseFile(out)
	if err != nil {
		t.Fatalf("load of own write: %v", err)
	}
	if again := g.Encode(); again != out {
		t.Fatalf("second write differs\n--- got\n%s--- want\n%s", again, out)
	}
	return f
}

// A daemon with no hooks (Potluck alone) loads a file with guest_issues,
// [[trackers]] and names nobody knows, and writes every one of them back:
// byte for byte when the file is in Encode's order (top-level keys by name
// after the base's own, tables by name after [[areas]], a table's keys by
// name), and the same again on a second round trip.
func TestNoHooksKeepsEverything(t *testing.T) {
	noHooks(t)
	// zm's guest_issues and [[trackers]] are names like any other here:
	// kept, each table's keys by name.
	f := roundTrip(t, zmToml, zmTomlUnowned)
	if f.Key("guest_issues") != nil || f.Section("trackers") != nil || len(f.Keys) != 1 || len(f.Sections) != 1 {
		t.Fatalf("zm's names: %+v", f)
	}
	f = roundTrip(t, futureToml, futureToml)
	if f.Key("future_flag") != nil || f.Section("widgets") != nil {
		t.Fatalf("an unowned value is nobody's parsed value: %v %v", f.Key("future_flag"), f.Section("widgets"))
	}
	if len(f.Keys) != 6 || len(f.Sections) != 2 {
		t.Fatalf("kept %d keys, %d sections", len(f.Keys), len(f.Sections))
	}
	// A hand-written order, comments and spacing are not kept; the values are.
	hand := "future_n = 42 # why\nslug = \"orchid\"\n\"odd key\" = { b = 1, a = \"z\" }\nname = \"Orchid\"\nfuture_s = 'x # \"y\"'\n" +
		"future_off = false\nfuture_flag = true\nfuture_list = [\n  \"a\", 1, true, [\"b\"],\n]\nid = \"01JAAAAAAAAAAAAAAAAAAAAAAA\"\n" +
		"[[widgets]]\nopts = { deep = { n = 1 } }\nname = \"w1\"\n[gadget]\non = true\n[[widgets]]\nname = \"w2\"\n[[areas]]\nname = \"api\"\n"
	roundTrip(t, hand, futureToml)
	// The names the base reads itself are still checked.
	for _, bad := range []string{"slug = 1\n", "slug = \"x\"\n[[areas]]\nname = \"a\"\nnope = 1\n"} {
		if _, err := ParseFile(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// The same on a running registry: a daemon with no hooks starts on a file
// another build wrote (start rewrites every project.toml), keeps each
// name, and a later live change and a restart keep them too.
func TestNoHooksRegistryKeepsUnknownNames(t *testing.T) {
	noHooks(t)
	fx := newFixture(t)
	path := filepath.Join(fx.st.ProjectDir(fx.side.ID), FileName)
	src := fmt.Sprintf("id = %q\nslug = \"side\"\nname = \"side\"\nfuture_flag = true\nguest_issues = true\n\n"+
		"[[trackers]]\nadapter = \"beads\"\nprefix = \"bd\"\nconfig = { mode = \"new\" }\n\n[[widgets]]\nname = \"w1\"\n", fx.side.ID)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	want := fileHead + fmt.Sprintf("id = %q\nslug = \"side\"\nname = \"side\"\nfuture_flag = true\nguest_issues = true\n\n"+
		"[[trackers]]\nadapter = \"beads\"\nconfig = { mode = \"new\" }\nprefix = \"bd\"\n\n[[widgets]]\nname = \"w1\"\n", fx.side.ID)
	r := fx.reg
	for round := 1; round <= 2; round++ {
		var err error
		if r, err = Open(fx.ctx, fx.st, fx.reg.cfg); err != nil {
			t.Fatalf("start %d: %v", round, err)
		}
		if got, _ := os.ReadFile(path); string(got) != want {
			t.Fatalf("after start %d:\n%s", round, got)
		}
	}
	// The registry's answers carry only what a module adds: nothing here.
	if p, err := r.Get(fx.local.Scope, "side"); err != nil || p.GuestIssues || len(p.Trackers) != 0 {
		t.Fatalf("view: %+v %v", p, err)
	}
	if _, err := r.AddArea(fx.ctx, fx.local, "side", api.Area{Name: "live"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	for _, keep := range []string{"future_flag = true\n", "guest_issues = true\n", "[[trackers]]\nadapter = \"beads\"", "[[widgets]]\nname = \"w1\"", "name = \"live\""} {
		if !strings.Contains(string(got), keep) {
			t.Errorf("a live change lost %q:\n%s", keep, got)
		}
	}
}

// boolKey and trackerTables are a module's owners of guest_issues and
// [[trackers]], as zm registers them.
type boolKey struct{ name string }

func (k boolKey) Parse(where string, raw any) (any, error) {
	b, ok := raw.(bool)
	if !ok {
		return nil, fmt.Errorf("%s: %s must be true or false", where, k.name)
	}
	return b, nil
}
func (boolKey) Validate(any, File) error { return nil }
func (k boolKey) Encode(v any) string {
	if on, _ := v.(bool); on {
		return k.name + " = true\n"
	}
	return ""
}

type trackerTables struct{}

func (trackerTables) Parse(where string, raw any) (any, error) {
	ts, ok := raw.([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: trackers must be [[trackers]] tables", where)
	}
	var out []api.Tracker
	for _, t := range ts {
		tr := NewTOMLReader(where+" [[trackers]]", t)
		out = append(out, api.Tracker{Adapter: tr.Str("adapter"), Prefix: tr.Str("prefix"), Area: tr.Str("area"), Config: tr.StrMap("config"), Env: tr.Str("env")})
		if err := tr.Done(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (trackerTables) Validate(v any, f File) error {
	for _, t := range v.([]api.Tracker) {
		if t.Area != "" && !slicesHasArea(f.Areas, t.Area) {
			return api.Errorf(api.CodeBadRequest, "tracker %q names unknown area %q", t.Prefix, t.Area)
		}
	}
	return nil
}

func slicesHasArea(as []api.Area, name string) bool {
	for _, a := range as {
		if a.Name == name {
			return true
		}
	}
	return false
}

func (trackerTables) Encode(v any) string {
	var b strings.Builder
	for _, t := range v.([]api.Tracker) {
		fmt.Fprintf(&b, "\n[[trackers]]\nadapter = %s\nprefix = %s\n", TOMLString(t.Adapter), TOMLString(t.Prefix))
		if t.Area != "" {
			fmt.Fprintf(&b, "area = %s\n", TOMLString(t.Area))
		}
		if len(t.Config) > 0 {
			cfg := map[string]any{}
			for k, x := range t.Config {
				cfg[k] = x
			}
			fmt.Fprintf(&b, "config = %s\n", tomlValue(cfg))
		}
		if t.Env != "" {
			fmt.Fprintf(&b, "env = %s\n", TOMLString(t.Env))
		}
	}
	return b.String()
}

func moduleHooks(t *testing.T) {
	t.Helper()
	noHooks(t)
	RegisterKey("guest_issues", boolKey{"guest_issues"})
	RegisterSection("trackers", trackerTables{})
	RegisterExtras(Extras{
		Project: func(p *api.Project, f File, sc authz.Scope) {
			p.GuestIssues, _ = f.Key("guest_issues").(bool)
			if whole, _ := sc.Areas(p.ID); whole {
				p.Trackers, _ = f.Section("trackers").([]api.Tracker)
			}
		},
		Area: func(v *api.AreaView, f File, sc authz.Scope) {
			trs, _ := f.Section("trackers").([]api.Tracker)
			for _, tr := range trs {
				if tr.Area == v.Name && sc.WholeProject(v.Project.ID) {
					v.Trackers = append(v.Trackers, tr)
				}
			}
		},
		Update: func(f File, req api.UpdateProjectRequest, owner bool) (File, error) {
			if req.Trackers != nil && !owner {
				return f, LocalSettingsError("trackers")
			}
			if req.Trackers != nil {
				var v any
				if len(*req.Trackers) > 0 {
					v = append([]api.Tracker(nil), *req.Trackers...)
				}
				f = f.WithSection("trackers", v)
			}
			if req.GuestIssues != nil {
				var v any
				if *req.GuestIssues {
					v = true
				}
				f = f.WithKey("guest_issues", v)
			}
			return f, nil
		},
	})
}

// With a module's hooks registered the module gets its values, checks them,
// and writes them its own way.
func TestHooksOwnGuestIssuesAndTrackers(t *testing.T) {
	moduleHooks(t)
	f := roundTrip(t, zmToml, zmToml)
	trs, _ := f.Section("trackers").([]api.Tracker)
	if on, _ := f.Key("guest_issues").(bool); !on || len(trs) != 2 || trs[0].Prefix != "bd" || trs[0].Config["dir"] != "~/code/acme/.beads" {
		t.Fatalf("the module's values: %v %+v", f.Key("guest_issues"), trs)
	}
	// What a build without the module wrote (keys sorted) reads the same.
	if g := roundTrip(t, zmTomlUnowned, zmToml); !reflect.DeepEqual(g, f) {
		t.Fatalf("an unowned write parses differently:\n%+v\n%+v", g, f)
	}
	for _, bad := range []string{"slug = \"x\"\nguest_issues = 1\n", "slug = \"x\"\ntrackers = 1\n", "slug = \"x\"\n[guest_issues]\na = 1\n",
		"slug = \"x\"\n[[trackers]]\nadapter = \"a\"\nprefix = \"p\"\narea = \"gone\"\n", "slug = \"x\"\n[[trackers]]\nbogus = 1\n"} {
		if _, err := ParseFile(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestHooksRegistry(t *testing.T) {
	moduleHooks(t)
	fx := newFixture(t)
	ctx, r, id := fx.ctx, fx.reg, fx.orchid.ID
	path := filepath.Join(fx.st.ProjectDir(id), FileName)
	on := true
	bd := []api.Tracker{{Adapter: "beads", Prefix: "bd", Area: "sprout", Config: map[string]string{"mode": "new"}}}
	p, err := r.Update(ctx, fx.local, "orchid", api.UpdateProjectRequest{GuestIssues: &on, Trackers: &bd})
	if err != nil || !p.GuestIssues || !reflect.DeepEqual(p.Trackers, bd) {
		t.Fatalf("update through the module: %+v %v", p, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "guest_issues = true\n") != 1 || strings.Count(string(b), "[[trackers]]") != 1 {
		t.Fatalf("project.toml:\n%s", b)
	}
	if av, err := r.GetArea(fx.local.Scope, "orchid", "sprout"); err != nil || !reflect.DeepEqual(av.Trackers, bd) {
		t.Fatalf("area view: %+v %v", av, err)
	}
	// The module's validation runs on every change; a refused one changes nothing.
	gone := []api.Tracker{{Adapter: "beads", Prefix: "x", Area: "gone"}}
	if _, err := r.Update(ctx, fx.local, "orchid", api.UpdateProjectRequest{Trackers: &gone}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("invalid trackers: %v", err)
	}
	// Machine settings: the module refuses a caller who is not the owner on the socket.
	remote := *fx.local
	remote.Remote = true
	if _, err := r.Update(ctx, &remote, "orchid", api.UpdateProjectRequest{Trackers: &gone}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("remote admin set trackers: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(b) {
		t.Fatalf("a refused change wrote:\n%s", got)
	}
	// SetSection and SetKey are the module's own writes.
	if _, err := r.SetSection(ctx, id, "trackers", func(old any) (any, error) {
		return append(append([]api.Tracker(nil), old.([]api.Tracker)...), api.Tracker{Adapter: "native", Prefix: "issue"}), nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetKey(ctx, id, "guest_issues", func(old any) (any, error) {
		if old != true {
			t.Errorf("old key value %v", old)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetKey(ctx, id, "nobody", func(any) (any, error) { return 1, nil }); !api.IsCode(err, api.CodeInternal) {
		t.Fatalf("SetKey without an owner: %v", err)
	}
	if r.Key(id, "guest_issues") != nil || len(r.Section(id, "trackers").([]api.Tracker)) != 2 {
		t.Fatalf("after the module's writes: %v %v", r.Key(id, "guest_issues"), r.Section(id, "trackers"))
	}
	// A restart reads what was written.
	if _, err := r.SetKey(ctx, id, "guest_issues", func(any) (any, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(ctx, fx.st, fx.reg.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := r2.Get(fx.local.Scope, "orchid"); err != nil || !p.GuestIssues || len(p.Trackers) != 2 {
		t.Fatalf("after restart: %+v %v", p, err)
	}
}

func TestRegisterNames(t *testing.T) {
	noHooks(t)
	panics := func(what string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: no panic", what)
			}
		}()
		fn()
	}
	RegisterSection("trackers", trackerTables{})
	RegisterKey("guest_issues", boolKey{"guest_issues"})
	for _, name := range []string{"id", "slug", "name", "paths", "remotes", "areas", "trackers", "guest_issues", "a b", ""} {
		panics("section "+name, func() { RegisterSection(name, trackerTables{}) })
		panics("key "+name, func() { RegisterKey(name, boolKey{name}) })
	}
	panics("nil owner", func() { RegisterKey("free", nil) })
}

// A top-level name one slip from a name this binary reads is refused with
// the name it meant; any other unknown name is kept and listed.
func TestMisspeltNameRefused(t *testing.T) {
	moduleHooks(t)
	for bad, want := range map[string]string{
		"path = [\"~/a\"]": "paths", "PATHS = [\"~/a\"]": "paths", "Slug = \"y\"": "slug", "nam = \"y\"": "name", "remote = []": "remotes",
		"[[area]]\nname = \"a\"": "areas", "[[Areas]]\nname = \"a\"": "areas", "idd = \"x\"": "id",
		"Guest_Issues = true": "guest_issues", "guest_issue = true": "guest_issues", "[[tracker]]\nadapter = \"a\"": "trackers",
	} {
		_, err := ParseFile("slug = \"x\"\n" + bad + "\n")
		if err == nil || !strings.Contains(err.Error(), "did you mean \""+want+"\"?") {
			t.Errorf("%q: %v, want a hint at %s", bad, err, want)
		}
	}
	f, err := ParseFile("slug = \"x\"\nfuture_flag = true\npathways = 1\nguest_issues = true\n[[widgets]]\nname = \"w\"\n[[trackers]]\nadapter = \"a\"\nprefix = \"p\"\n")
	if err != nil || !reflect.DeepEqual(f.Unowned(), []string{"future_flag", "pathways", "widgets"}) {
		t.Fatalf("unowned %v %v", f.Unowned(), err)
	}
	// A module's names are only near misses while it is registered.
	noHooks(t)
	if _, err := ParseFile("slug = \"x\"\nwidget_on = true\n[[widget]]\nname = \"w\"\n"); err != nil {
		t.Fatalf("names near nothing this binary reads: %v", err)
	}
}

func TestRegistryListsUnowned(t *testing.T) {
	noHooks(t)
	fx := newFixture(t)
	path := filepath.Join(fx.st.ProjectDir(fx.side.ID), FileName)
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte("future_flag = true\n"+string(b)+"\n[[widgets]]\nname = \"w\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(fx.ctx, fx.st, fx.reg.cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"side": {"future_flag", "widgets"}}
	if got := r.Unowned(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Unowned %v", got)
	}
	if got := UnownedIn(fx.st.Home()); !reflect.DeepEqual(got, want) {
		t.Fatalf("UnownedIn %v", got)
	}
}

// A module that registers after the registry loaded the file (its value is
// still the decoded one) gets its own parsed value in SetKey and SetSection.
func TestSetOwnedParsesALateOwnersValue(t *testing.T) {
	noHooks(t)
	fx := newFixture(t)
	id := fx.side.ID
	path := filepath.Join(fx.st.ProjectDir(id), FileName)
	b, _ := os.ReadFile(path)
	src := strings.Replace(string(b), "name = \"side\"\n", "name = \"side\"\nlate_flag = true\n", 1) + "\n[[late]]\nadapter = \"a\"\nprefix = \"p\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(fx.ctx, fx.st, fx.reg.cfg)
	if err != nil {
		t.Fatal(err)
	}
	RegisterKey("late_flag", boolKey{"late_flag"})
	RegisterSection("late", trackerTables{})
	if _, err := r.SetKey(fx.ctx, id, "late_flag", func(old any) (any, error) {
		if old != true {
			t.Errorf("SetKey got %#v, want the parsed true", old)
		}
		return old, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetSection(fx.ctx, id, "late", func(old any) (any, error) {
		trs, ok := old.([]api.Tracker)
		if !ok || len(trs) != 1 || trs[0].Prefix != "p" {
			t.Errorf("SetSection got %#v, want the parsed trackers", old)
		}
		return old, nil
	}); err != nil {
		t.Fatal(err)
	}
	if r.Key(id, "late_flag") != true {
		t.Fatalf("after the write the value is the owner's: %#v", r.Key(id, "late_flag"))
	}
}

// A hook's Update changes its keys and sections only: what it does to the
// registry's own fields is dropped, and the owner check on repo rules holds.
func TestExtrasUpdateCannotTouchBaseFields(t *testing.T) {
	noHooks(t)
	RegisterKey("flag", boolKey{"flag"})
	RegisterExtras(Extras{Update: func(f File, req api.UpdateProjectRequest, owner bool) (File, error) {
		f.ID, f.Slug, f.Name = "01JZZZZZZZZZZZZZZZZZZZZZZZ", "stolen", "Stolen"
		f.Repos = api.RepoRules{Paths: []string{"/etc"}, Remotes: []string{"github.com/evil/x"}}
		f.Areas = append([]api.Area{{Name: "planted", RepoRules: api.RepoRules{Paths: []string{"/"}}}}, f.Areas...)
		return f.WithKey("flag", true), nil
	}})
	fx := newFixture(t)
	before, _ := fx.reg.File(fx.orchid.ID)
	tok, _ := fx.reg.Mint(fx.ctx, fx.local, api.MintTokenRequest{As: "carl", Project: "orchid", Role: "owner"})
	remote, _ := fx.reg.FromToken(fx.ctx, tok.Secret, true)
	name := "Orchid 2"
	for _, sub := range []*authz.Subject{remote, fx.local} {
		p, err := fx.reg.Update(fx.ctx, sub, "orchid", api.UpdateProjectRequest{Name: &name})
		if err != nil {
			t.Fatal(err)
		}
		after, _ := fx.reg.File(fx.orchid.ID)
		if p.ID != fx.orchid.ID || p.Slug != "orchid" || p.Name != name || after.ID != before.ID ||
			!reflect.DeepEqual(after.Repos, before.Repos) || !reflect.DeepEqual(after.Areas, before.Areas) || after.Key("flag") != true {
			t.Fatalf("a hook changed the registry's fields: %+v", after)
		}
	}
	if _, err := fx.reg.Update(fx.ctx, remote, "orchid", api.UpdateProjectRequest{Repos: &api.RepoRules{Paths: []string{"/x"}}}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("remote owner set repo rules past a hook: %v", err)
	}
}

// A key owner whose Encode forgets the newline does not swallow the next line.
type bareLine struct{}

func (bareLine) Parse(string, any) (any, error) { return true, nil }
func (bareLine) Validate(any, File) error       { return nil }
func (bareLine) Encode(any) string              { return "bare = true" }

func TestKeyEncodeWithoutNewline(t *testing.T) {
	noHooks(t)
	RegisterKey("bare", bareLine{})
	RegisterKey("flag", boolKey{"flag"})
	f, err := ParseFile("slug = \"x\"\nbare = true\nflag = true\nzeta = 1\n\n[[areas]]\nname = \"a\"\n")
	if err != nil {
		t.Fatal(err)
	}
	out := f.Encode()
	if !strings.Contains(out, "bare = true\nflag = true\nzeta = 1\n\n[[areas]]") {
		t.Fatalf("encode:\n%s", out)
	}
	if g, err := ParseFile(out); err != nil || g.Encode() != out {
		t.Fatalf("round trip: %v", err)
	}
}

// A hand edit made since the daemon loaded project.toml is never silently
// discarded by a live change (a module's section, project and area edits): the
// change is refused with a conflict naming the restart, the file keeps the
// edit, and after a reload (restart) the live change applies again.
func TestLiveRewriteKeepsHandEdit(t *testing.T) {
	moduleHooks(t)
	fx := newFixture(t)
	ctx, r, st, p := fx.ctx, fx.reg, fx.st, fx.side
	path := filepath.Join(st.ProjectDir(p.ID), FileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := string(b) + "\n[[areas]]\nname = \"byhand\"\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	add := func(prefix string) error {
		_, err := r.SetSection(ctx, p.ID, "trackers", func(old any) (any, error) {
			trs, _ := old.([]api.Tracker)
			return append(trs[:len(trs):len(trs)], api.Tracker{Adapter: "beads", Prefix: prefix, Config: map[string]string{"mode": "new"}}), nil
		})
		return err
	}
	if err := add("bd2"); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("tracker add over a hand edit: %v", err)
	}
	if _, err := r.AddArea(ctx, fx.local, p.ID, api.Area{Name: "live"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("area add over a hand edit: %v", err)
	}
	name := "Renamed"
	if _, err := r.Update(ctx, fx.local, p.ID, api.UpdateProjectRequest{Name: &name}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("project update over a hand edit: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != edited {
		t.Fatalf("hand edit lost:\n%s", got)
	}
	if v := r.Section(p.ID, "trackers"); v != nil {
		t.Fatalf("refused change applied in memory: %+v", v)
	}
	// A restart loads the edit; live changes then apply and keep it.
	r2, err := Open(ctx, st, fx.reg.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r = r2
	if err := add("bd2"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `name = "byhand"`) || !strings.Contains(string(got), `prefix = "bd2"`) {
		t.Fatalf("after restart:\n%s", got)
	}
	if err := add("bd3"); err != nil { // the daemon's own write is not a hand edit
		t.Fatal(err)
	}
}

// With no module taking them, PATCH's trackers and guest_issues are refused
// (not answered 200 and dropped), for the machine's owner and a remote admin
// alike; the file is untouched.
func TestUpdateWithoutAModuleRefusesModuleSettings(t *testing.T) {
	noHooks(t)
	fx := newFixture(t)
	path := filepath.Join(fx.st.ProjectDir(fx.orchid.ID), FileName)
	before, _ := os.ReadFile(path)
	tok, _ := fx.reg.Mint(fx.ctx, fx.local, api.MintTokenRequest{As: "carl", Project: "orchid", Role: "owner"})
	remote, _ := fx.reg.FromToken(fx.ctx, tok.Secret, true)
	on, name := true, "Renamed"
	trs := []api.Tracker{{Adapter: "beads", Prefix: "bd"}}
	for who, sub := range map[string]*authz.Subject{"owner": fx.local, "remote admin": remote} {
		for what, req := range map[string]api.UpdateProjectRequest{
			"trackers": {Trackers: &trs}, "guest_issues": {GuestIssues: &on}, "trackers, guest_issues": {Name: &name, Trackers: &trs, GuestIssues: &on}} {
			_, err := fx.reg.Update(fx.ctx, sub, "orchid", req)
			if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "no module for the project setting "+what+":") {
				t.Errorf("%s set %s: %v", who, what, err)
			}
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("a refused change wrote:\n%s", after)
	}
	if p, _ := fx.reg.Get(fx.local.Scope, "orchid"); p.Name != "Orchid" {
		t.Fatalf("a refused change applied its other fields: %+v", p)
	}
	// The registry's own fields still work.
	if _, err := fx.reg.Update(fx.ctx, fx.local, "orchid", api.UpdateProjectRequest{Name: &name}); err != nil {
		t.Fatal(err)
	}
}
