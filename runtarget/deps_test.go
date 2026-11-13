package runtarget_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// testRecipe stands in for npm: a lockfile naming "fail" leaves a partial
// tree and exits 3; any other is copied into node_modules with the scripts
// setting, after a pause long enough for other asks to meet the build.
func testRecipe(scripts bool) string {
	return fmt.Sprintf(`if grep -q '"fail"' package-lock.json; then mkdir -p node_modules && echo partial > node_modules/partial; echo boom >&2; exit 3; fi
sleep 0.5
mkdir -p node_modules && cp package-lock.json node_modules/.lock && echo %v > node_modules/.scripts
`, scripts)
}

// lock is a v3 lockfile with one workspace, packages/a; name marks it.
func lock(name string) []byte {
	return []byte(`{"name":"` + name + `","lockfileVersion":3,"packages":{"":{"workspaces":["packages/a"]},"packages/a":{"version":"1.0.0"},"node_modules/a":{"resolved":"packages/a","link":true}}}`)
}

func npmDeps(name string) computeapi.BoxDeps {
	return computeapi.BoxDeps{Kind: "npm", Path: "work", Files: []computeapi.DepsFile{
		{Path: "package-lock.json", Data: lock(name)},
		{Path: "package.json", Data: []byte(`{"name":"` + name + `","workspaces":["packages/a"]}`)},
		{Path: "packages/a/package.json", Data: []byte(`{"name":"a","version":"1.0.0"}`)},
	}}
}

type depsFix struct {
	ana, bo *client.Client
	owner   *client.Client
	log     string
	state   string
}

// newDepsFix is a daemon with binding "deps" (the reference driver, every
// request logged) and two members, ana and bo.
func newDepsFix(t *testing.T) *depsFix {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	f := &depsFix{state: filepath.Join(fix, "state"), log: filepath.Join(fix, "requests.log")}
	ref, err := refdriver.Install(fix, f.state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := refdriver.KillAll(f.state); err != nil {
			t.Errorf("reference driver cleanup: %v", err)
		}
	})
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
	wrap := filepath.Join(fix, "zm-run-logged")
	script := "#!/bin/sh\nin=$(cat)\nout=$(printf '%s' \"$in\" | " + ref + ")\n" +
		"printf '{\"in\":%s,\"out\":%s}\\n' \"$in\" \"$(printf '%s\\n' \"$out\" | tail -n 1)\" >> " + f.log + "\n" +
		"printf '%s\\n' \"$out\"\n"
	if err := os.WriteFile(wrap, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return wrap, nil })
			s.SetDepsPoll(100 * time.Millisecond)
		}
	}
	runtarget.SetDepsRecipe(t, "npm", testRecipe)
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	mint := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	f.ana, f.bo, f.owner = mint("ana"), mint("bo"), owner
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "deps", Driver: "ref", Kind: "box",
		Isolation: "ephemeral", Images: []string{refPin}, Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	return f
}

// requests are the driver requests of verb logged so far; answered says
// only those the driver did (ok true).
func (f *depsFix) requests(t *testing.T, verb string, answered ...bool) []map[string]json.RawMessage {
	t.Helper()
	b, _ := os.ReadFile(f.log)
	var out []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec struct {
			In struct {
				Verb string                     `json:"verb"`
				ID   string                     `json:"id"`
				Args map[string]json.RawMessage `json:"args"`
			} `json:"in"`
			Out struct {
				OK bool `json:"ok"`
			} `json:"out"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || len(answered) > 0 && !rec.Out.OK {
			continue
		}
		if req := rec.In; req.Verb == verb {
			if req.Args == nil {
				req.Args = map[string]json.RawMessage{}
			}
			req.Args["_id"], _ = json.Marshal(req.ID)
			out = append(out, req.Args)
		}
	}
	return out
}

// builders are the answered acquires holding a volume in mode build.
func (f *depsFix) builders(t *testing.T) []map[string]json.RawMessage {
	var out []map[string]json.RawMessage
	for _, a := range f.requests(t, "acquire", true) {
		var vols []computeapi.BoxVolume
		_ = json.Unmarshal(a["volumes"], &vols)
		for _, v := range vols {
			if v.Mode == "build" {
				out = append(out, a)
			}
		}
	}
	return out
}

func boxExec(t *testing.T, c *client.Client, h computeapi.RunTargetHandle, script string) computeapi.RunTargetExec {
	t.Helper()
	x, err := computeclient.ExecRunTarget(context.Background(), c, "deps", idOf(h.Handle), computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// One builder for many asks: four concurrent acquires of one layer start one
// build between them; every box mounts the sealed layer; the next acquire is
// a hit. The builder gets nothing of the caller's but the image: no
// volumes of theirs, no network setting, no ref, and its execs no secrets.
func TestDepsOneBuilderForConcurrentAsks(t *testing.T) {
	f := newDepsFix(t)
	ctx := context.Background()
	deps := npmDeps("app")
	var wg sync.WaitGroup
	hs := make([]computeapi.RunTargetHandle, 4)
	errs := make([]error, 4)
	for i := range hs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := computeapi.AcquireRunTargetRequest{RequestID: fmt.Sprintf("c%d", i), TTLS: 120, Net: computeapi.NetOn, Deps: []computeapi.BoxDeps{deps}}
			if i == 1 { // a volume of the caller's: never the builder's
				req.Volumes = []computeapi.BoxVolume{{Name: "cache", Path: "cache", Mode: computeapi.VolumeRW}}
			}
			hs[i], errs[i] = computeclient.AcquireRunTarget(ctx, f.ana, "deps", req)
		}()
	}
	wg.Wait()
	built := 0
	for i, h := range hs {
		if errs[i] != nil {
			t.Fatalf("acquire %d: %v", i, errs[i])
		}
		if len(h.Deps) != 1 || len(h.Deps[0].Key) != 64 || h.Deps[0].Hit == h.Deps[0].Built {
			t.Fatalf("acquire %d's deps: %+v", i, h.Deps)
		}
		if h.Deps[0].Built {
			built++
		}
		if x := boxExec(t, f.ana, h, "cat work/node_modules/.lock && cat work/node_modules/.scripts"); x.Exit != 0 || !strings.Contains(x.Stdout, `"app"`) || !strings.Contains(x.Stdout, "true") {
			t.Errorf("box %d's layer: %+v", i, x)
		}
	}
	if b := f.builders(t); len(b) != 1 || built != 1 {
		t.Fatalf("builders: %d (answers saying built: %d), want one", len(b), built)
	}
	b := f.builders(t)[0]
	for k := range b {
		switch k {
		case "request_id", "actor", "ttl_s", "image", "volumes", "volume_owner", "volume_project", "_id":
		default:
			t.Errorf("the builder's acquire carries %s=%s: nothing of the caller's but who and the image", k, b[k])
		}
	}
	var vols []computeapi.BoxVolume
	_ = json.Unmarshal(b["volumes"], &vols)
	if len(vols) != 1 || !strings.HasPrefix(vols[0].Name, "deps-npm-") || len(vols[0].Name) != len("deps-npm-")+52 || vols[0].Path != "work" {
		t.Errorf("the builder's volumes: %+v", vols)
	}
	for _, x := range f.requests(t, "exec") {
		if _, ok := x["secrets"]; ok {
			t.Errorf("an exec carried secrets: %s", x["secrets"])
		}
	}
	var callerOwner string
	for _, a := range f.requests(t, "acquire") {
		var rid string
		_ = json.Unmarshal(a["request_id"], &rid)
		if strings.HasSuffix(rid, "-c0") {
			callerOwner = string(a["volume_owner"])
		}
	}
	if callerOwner == "" || string(b["volume_owner"]) != callerOwner {
		t.Errorf("the builder's volume_owner %s, the caller's %s: want the same", b["volume_owner"], callerOwner)
	}
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "again", Deps: []computeapi.BoxDeps{deps}})
	if err != nil || len(h.Deps) != 1 || !h.Deps[0].Hit {
		t.Fatalf("a later acquire: %+v %v, want a hit", h.Deps, err)
	}
	// A box writing its view of the layer changes nothing for the next box.
	if x := boxExec(t, f.ana, h, "echo mine > work/node_modules/.lock"); x.Exit != 0 {
		t.Fatalf("writing the cow view: %+v", x)
	}
	h2, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "again2", Deps: []computeapi.BoxDeps{deps}})
	if err != nil {
		t.Fatal(err)
	}
	if x := boxExec(t, f.ana, h2, "cat work/node_modules/.lock"); !strings.Contains(x.Stdout, `"app"`) {
		t.Errorf("the next box's layer after another box wrote its view: %q", x.Stdout)
	}
	// Another owner's acquire of the same files builds that owner's own layer.
	hb, err := computeclient.AcquireRunTarget(ctx, f.bo, "deps", computeapi.AcquireRunTargetRequest{RequestID: "b1", Deps: []computeapi.BoxDeps{deps}})
	if err != nil || len(hb.Deps) != 1 || !hb.Deps[0].Built || hb.Deps[0].Key != h.Deps[0].Key {
		t.Errorf("bo's acquire of ana's layer: %+v %v, want bo's own build of the same key", hb.Deps, err)
	}
}

// A failed build leaves no layer, partial or not: the acquire is refused
// saying why, and the next ask builds again.
func TestDepsFailedBuildLeavesNoLayer(t *testing.T) {
	f := newDepsFix(t)
	ctx := context.Background()
	deps := npmDeps("fail")
	for i := range 2 {
		_, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: fmt.Sprintf("f%d", i), Deps: []computeapi.BoxDeps{deps}})
		if err == nil || !strings.Contains(err.Error(), "exited 3") || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("try %d: %v, want the build's failure", i, err)
		}
		if b := f.builders(t); len(b) != i+1 {
			t.Fatalf("try %d: %d builders, want %d (a failed build leaves nothing to find)", i, len(b), i+1)
		}
	}
	if dirs, _ := filepath.Glob(filepath.Join(f.state, "vol-*")); len(dirs) != 0 {
		t.Errorf("the driver keeps %v after failed builds", dirs)
	}
	for _, r := range f.requests(t, "release") {
		if _, ok := r["seal"]; ok {
			t.Errorf("a failed build's release asked seal: %v", r)
		}
	}
}

// What only the daemon may say: a caller naming a deps- volume or asking
// sealed is refused, as are deps that are not exactly a kind's files.
func TestDepsRefusals(t *testing.T) {
	f := newDepsFix(t)
	ctx := context.Background()
	good := npmDeps("app")
	extra := npmDeps("app")
	extra.Files = append(extra.Files, computeapi.DepsFile{Path: "src/index.js", Data: []byte("x")})
	missing := npmDeps("app")
	missing.Files = missing.Files[:2]
	dotdot := npmDeps("app")
	dotdot.Files[2].Path = "packages/../a/package.json"
	outside := npmDeps("app")
	outside.Files[0].Data = []byte(`{"lockfileVersion":3,"packages":{"":{},"../evil":{}}}`)
	v1 := npmDeps("app")
	v1.Files[0].Data = []byte(`{"lockfileVersion":1,"dependencies":{}}`)
	scripts := npmDeps("app")
	scripts.Scripts = "maybe"
	other := npmDeps("web")
	other.Path = "web"
	abs := npmDeps("app")
	abs.Path = "/work"
	wait := -1
	for name, req := range map[string]computeapi.AcquireRunTargetRequest{
		"a deps- volume":       {Volumes: []computeapi.BoxVolume{{Name: "deps-npm-0123456789abcdef0123456789abcdef0123456789abcdef0123", Path: "work", Mode: "cow"}}},
		"a sealed volume":      {Volumes: []computeapi.BoxVolume{{Name: "cache", Path: "work", Mode: "cow", Sealed: true}}},
		"a checkout volume":    {Volumes: []computeapi.BoxVolume{{Name: "cache", Path: "work", Mode: "cow", Checkout: true}}},
		"a ref, two layers":    {Ref: "main", Deps: []computeapi.BoxDeps{good, other}},
		"a ref, no wait":       {Ref: "main", Deps: []computeapi.BoxDeps{good}, DepsWaitS: new(int)},
		"a file not an input":  {Deps: []computeapi.BoxDeps{extra}},
		"a workspace missing":  {Deps: []computeapi.BoxDeps{missing}},
		"a .. path":            {Deps: []computeapi.BoxDeps{dotdot}},
		"a package outside":    {Deps: []computeapi.BoxDeps{outside}},
		"lockfile version 1":   {Deps: []computeapi.BoxDeps{v1}},
		"scripts maybe":        {Deps: []computeapi.BoxDeps{scripts}},
		"an absolute path":     {Deps: []computeapi.BoxDeps{abs}},
		"no such kind":         {Deps: []computeapi.BoxDeps{{Kind: "go", Path: "work"}}},
		"with persist":         {Deps: []computeapi.BoxDeps{good}, Persist: true},
		"over a caller volume": {Deps: []computeapi.BoxDeps{good}, Volumes: []computeapi.BoxVolume{{Name: "c", Path: "work/x"}}},
		"a negative wait":      {Deps: []computeapi.BoxDeps{good}, DepsWaitS: &wait},
		"a wait without deps":  {DepsWaitS: new(int)},
	} {
		req.RequestID = "r"
		if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", req); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v, want bad_request", name, err)
		}
	}
	if b := f.requests(t, "acquire"); len(b) != 0 {
		t.Errorf("refused acquires reached the driver: %v", b)
	}
	big := npmDeps("app")
	big.Files[1].Data = make([]byte, computeapi.DepsBytesMax+1)
	if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "big", Deps: []computeapi.BoxDeps{big}}); !api.IsCode(err, api.CodeTooLarge) {
		t.Errorf("over 16 MiB of files: %v, want too_large", err)
	}
}

// With deps_wait_s 0, an acquire meeting another box's build takes a box
// without the layer at once, and says so.
func TestDepsWaitZeroHandsBackAPlainBox(t *testing.T) {
	f := newDepsFix(t)
	ctx := context.Background()
	slow := npmDeps("slow")
	runtarget.SetDepsRecipe(t, "npm", func(bool) string { return "sleep 3; mkdir -p node_modules\n" })
	done := make(chan error, 1)
	go func() {
		_, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "builder", Deps: []computeapi.BoxDeps{slow}})
		done <- err
	}()
	for by := time.Now().Add(20 * time.Second); len(f.builders(t)) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(by) {
			t.Fatal("no build started")
		}
	}
	// The builder is the daemon's: its holder cannot exec in it, write the
	// layer before it is sealed, or release it.
	// The driver may have answered before the daemon recorded the handle
	// (the row is acquiring until then): wait for it.
	var hs []computeapi.RunTargetHandle
	var builder string
	for by := time.Now().Add(20 * time.Second); builder == ""; time.Sleep(50 * time.Millisecond) {
		var err error
		if hs, err = computeclient.RunTargetHandles(ctx, f.ana, "deps", true); err != nil {
			t.Fatal(err)
		}
		for _, h := range hs {
			if strings.Contains(h.RequestID, "deps-build-") && h.Handle != "" {
				builder = idOf(h.Handle)
			}
		}
		if builder == "" && time.Now().After(by) {
			t.Fatalf("no builder among ana's handles: %+v", hs)
		}
	}
	if _, err := computeclient.ExecRunTarget(ctx, f.ana, "deps", builder, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "echo evil > work/x"}}); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("the holder's exec in the builder: %v, want not_found", err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, f.ana, "deps", builder); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("the holder's release of the builder: %v, want not_found", err)
	}
	for _, h := range hs {
		if i := strings.Index(h.RequestID, "deps-build-"); i >= 0 {
			if _, err := computeclient.CancelRunTargetAcquire(ctx, f.ana, "deps", h.RequestID[i:], ""); !api.IsCode(err, api.CodeNotFound) {
				t.Errorf("the holder's cancel of the builder's acquire: %v, want not_found", err)
			}
			if _, err := computeclient.RunTargetAcquireProgress(ctx, f.ana, "deps", h.RequestID[i:], ""); !api.IsCode(err, api.CodeNotFound) {
				t.Errorf("the holder's progress of the builder's acquire: %v, want not_found", err)
			}
		}
	}
	zero := 0
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "plain", DepsWaitS: &zero, Deps: []computeapi.BoxDeps{slow}})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Deps) != 1 || h.Deps[0].Hit || h.Deps[0].Built || len(h.Volumes) != 0 {
		t.Errorf("deps_wait_s 0 during a build: deps %+v volumes %+v, want a plain box saying so", h.Deps, h.Volumes)
	}
	if err := <-done; err != nil {
		t.Fatalf("the building acquire: %v", err)
	}
	if b := f.builders(t); len(b) != 1 {
		t.Errorf("%d builders, want one", len(b))
	}
	// Asked again with the same request_id once the layer is there, it
	// answers the same box without the layer.
	again, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "plain", DepsWaitS: &zero, Deps: []computeapi.BoxDeps{slow}})
	if err != nil || again.Handle != h.Handle {
		t.Errorf("the same request again: %s %v, want %s", again.Handle, err, h.Handle)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "deps-build-0"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("a caller's request_id deps-build-0: %v, want bad_request", err)
	}
}

// Layers are for trusted hosts: an acquire with deps on a public host's
// binding is refused before anything is sent to it.
func TestDepsRefusedOnAPublicHost(t *testing.T) {
	rp := newRelayPair(t)
	_, err := computeclient.AcquireRunTarget(context.Background(), rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "p1", Deps: []computeapi.BoxDeps{npmDeps("app")}})
	if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "public host") {
		t.Errorf("deps on a public host: %v, want forbidden", err)
	}
}

// With a ref, the box's checkout goes into its layer: the driver is asked
// for the layer's volume with checkout (the builder never is), and the box
// holds the commit's files beside the layer's node_modules in that
// directory, its default working directory. Without a ref nothing is
// asked to be a checkout.
func TestDepsCheckoutIntoTheLayer(t *testing.T) {
	f := newDepsFix(t)
	ctx := context.Background()
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "co", Ref: "main", Deps: []computeapi.BoxDeps{npmDeps("app")}})
	if err != nil || len(h.Deps) != 1 || !h.Deps[0].Built {
		t.Fatalf("acquire with a ref: %+v %v", h.Deps, err)
	}
	if x := boxExec(t, f.ana, h, `case $(pwd) in */work) ;; *) echo "cwd $(pwd)"; exit 1 ;; esac; test -f probe.sh && cat node_modules/.lock`); x.Exit != 0 || !strings.Contains(x.Stdout, `"app"`) {
		t.Fatalf("the box's checkout and layer: %+v", x)
	}
	plain, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "plain", Deps: []computeapi.BoxDeps{npmDeps("app")}})
	if err != nil || len(plain.Deps) != 1 || !plain.Deps[0].Hit {
		t.Fatalf("acquire without a ref: %+v %v", plain.Deps, err)
	}
	checkouts := map[string]bool{}
	for _, a := range f.requests(t, "acquire", true) {
		var rid string
		var vols []computeapi.BoxVolume
		_ = json.Unmarshal(a["request_id"], &rid)
		_ = json.Unmarshal(a["volumes"], &vols)
		for _, v := range vols {
			if v.Checkout {
				checkouts[rid[strings.Index(rid, "-")+1:]] = true
				if v.Mode != computeapi.VolumeCOW || !v.Sealed || v.Path != "work" {
					t.Errorf("the checkout volume: %+v", v)
				}
			}
		}
	}
	if len(checkouts) != 1 || !checkouts["co"] {
		t.Errorf("acquires asking a checkout volume: %v, want only the box with a ref", checkouts)
	}
}

// A layer holds only its kind's trees: the builder's release keeps the
// root's node_modules and each workspace's, and whatever else the recipe or
// an install script wrote there (a .git, the files sent, a workspace's
// other files) never reaches a box.
func TestDepsLayerKeepsOnlyNodeModules(t *testing.T) {
	f := newDepsFix(t)
	runtarget.SetDepsRecipe(t, "npm", func(bool) string {
		return `mkdir -p node_modules/x .git/hooks packages/a/node_modules/w packages/a/src
echo m > node_modules/x/f; echo w > packages/a/node_modules/w/f; echo s > packages/a/src/i.js
printf '[core]\n\thooksPath = .git/hooks\n' > .git/config; echo planted > x.txt
`
	})
	ctx := context.Background()
	h, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "k", Deps: []computeapi.BoxDeps{npmDeps("app")}})
	if err != nil || len(h.Deps) != 1 || !h.Deps[0].Built {
		t.Fatalf("acquire: %+v %v", h.Deps, err)
	}
	const want = "work\nwork/node_modules\nwork/node_modules/x\nwork/node_modules/x/f\nwork/packages\nwork/packages/a\nwork/packages/a/node_modules\nwork/packages/a/node_modules/w\nwork/packages/a/node_modules/w/f\n"
	if x := boxExec(t, f.ana, h, "find work | LC_ALL=C sort"); x.Exit != 0 || x.Stdout != want {
		t.Errorf("the box's layer holds\n%s\nwant\n%s", x.Stdout, want)
	}
	var keep []string
	for _, r := range f.requests(t, "release") {
		if _, ok := r["seal"]; ok {
			_ = json.Unmarshal(r["keep"], &keep)
		}
	}
	if strings.Join(keep, " ") != "node_modules packages/a/node_modules" {
		t.Errorf("the builder's release kept %v", keep)
	}
}

// A workspace whose node_modules a driver could not keep (a name a volume
// path cannot hold) is refused before any build, and a binding whose driver
// keeps everything it seals (no volumes-keep) builds no layers.
func TestDepsRefusedWhenTheLayerCannotKeepOnlyItsTrees(t *testing.T) {
	f := newDepsFix(t)
	ctx := context.Background()
	spaced := computeapi.BoxDeps{Kind: "npm", Path: "work", Files: []computeapi.DepsFile{
		{Path: "package-lock.json", Data: []byte(`{"lockfileVersion":3,"packages":{"":{},"my app":{"version":"1.0.0"}}}`)},
		{Path: "package.json", Data: []byte(`{}`)},
		{Path: "my app/package.json", Data: []byte(`{}`)},
	}}
	if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "deps", computeapi.AcquireRunTargetRequest{RequestID: "sp", Deps: []computeapi.BoxDeps{spaced}}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "my app/node_modules") {
		t.Errorf("a workspace with a space: %v, want bad_request naming it", err)
	}
	if b := f.builders(t); len(b) != 0 {
		t.Errorf("%d builders for a refused layer", len(b))
	}
	if _, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "nokeep", Driver: "ref", Kind: "box",
		Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"keep": "0"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, f.ana, "nokeep", computeapi.AcquireRunTargetRequest{RequestID: "nk", Deps: []computeapi.BoxDeps{npmDeps("app")}}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "volumes-keep") {
		t.Errorf("a driver without volumes-keep: %v, want bad_request naming it", err)
	}
}
