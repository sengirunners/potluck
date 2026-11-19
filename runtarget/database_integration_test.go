package runtarget_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/database"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// A protocol-only driver makes no VM or database. Its incorrect answers exercise
// the daemon's trust boundary independently of any driver's implementation.
func TestDatabaseProtocolDriver(t *testing.T) {
	if os.Getenv("POTLUCK_DATABASE_TEST_DRIVER") != "1" {
		return
	}
	var req protocol.Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(2)
	}
	log, err := os.OpenFile(req.Config["log"], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	_ = json.NewEncoder(log).Encode(req)
	_ = log.Close()
	if delegate := req.Config["delegate"]; delegate != "" {
		// Delegate real volume sealing, checkout and exec to the reference driver.
		// Only database boot is simulated; its lifecycle has separate OCI tests.
		var args map[string]json.RawMessage
		_ = json.Unmarshal(req.Args, &args)
		var image string
		_ = json.Unmarshal(args["image"], &image)
		if image == "snapshot" {
			args["image"], _ = json.Marshal("ref-image")
		}
		db := args["database"]
		delete(args, "database")
		req.Args, _ = json.Marshal(args)
		input, _ := json.Marshal(req)
		cmd := exec.Command(delegate)
		cmd.Stdin = bytes.NewReader(input)
		output, err := cmd.Output()
		if err != nil {
			os.Exit(3)
		}
		var response map[string]any
		if json.Unmarshal(output, &response) != nil {
			os.Exit(4)
		}
		if result, ok := response["result"].(map[string]any); ok {
			if req.Verb == "describe" {
				caps, _ := result["capabilities"].([]any)
				result["capabilities"] = append(caps, protocol.CapDatabase)
			}
			if req.Verb == "acquire" && image == "snapshot" {
				result["image_digest"] = "sha256:" + strings.Repeat("a", 64)
			}
			if req.Verb == "acquire" && db != nil {
				var identity any
				_ = json.Unmarshal(db, &identity)
				result["database"] = identity
			}
		}
		_ = json.NewEncoder(os.Stdout).Encode(response)
		os.Exit(0)
	}
	var a struct {
		RequestID string             `json:"request_id"`
		Image     string             `json:"image"`
		Database  *protocol.Database `json:"database"`
	}
	_ = json.Unmarshal(req.Args, &a)
	var result any
	switch req.Verb {
	case "describe":
		caps := []string{protocol.CapDatabase}
		if req.Config["mode"] == "unsupported" {
			caps = nil
		}
		result = map[string]any{"potluck": "potluck/1", "name": "database-test", "kind": req.Config["kind"], "version": "1", "verbs": []string{"describe", "acquire", "status", "release", "list", "exec", "port"}, "available": true, "isolation": "ephemeral", "credentials": false, "cost": map[string]any{"unit": "s", "amount": 0}, "reports_spend": false, "ttl_max_s": 3600, "capabilities": caps, "size": map[string]any{"name": "test", "os": "linux", "cpu": 1, "memory_mb": 512, "disk_gb": 1}}
	case "acquire":
		if req.Config["mode"] == "prepare-failed" {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": map[string]any{"code": "unavailable", "message": "snapshot preparation failed"}})
			os.Exit(0)
		}
		image := a.Image
		if a.Database != nil {
			image = a.Database.Image
		}
		digest := strings.Split(image, "@")
		imageDigest := ""
		if len(digest) == 2 {
			imageDigest = digest[1]
		}
		if req.Config["mode"] == "wrong-image" {
			imageDigest = "sha256:" + strings.Repeat("0", 64)
		}
		if req.Config["mode"] == "wrong-database" && a.Database != nil {
			a.Database.Project = "another-project"
		}
		if req.Config["mode"] == "missing-database" {
			a.Database = nil
		}
		result = map[string]any{"handle": "box:" + req.Config["target"] + "/box1", "ttl_s": 120, "expires_at": time.Now().Add(120 * time.Second).UTC(), "image_digest": imageDigest, "database": a.Database, "size": "test"}
	case "release":
		result = map[string]any{"released": true}
	case "list":
		result = map[string]any{"handles": []any{}}
	default:
		result = map[string]any{"state": "ready"}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "result": result})
	os.Exit(0)
}

type databaseDaemonFixture struct {
	dm            *server.Daemon
	owner, member *client.Client
	project, log  string
	recipe        database.Recipe
	svc           *runtarget.Service
}

func newDatabaseDaemonFixture(t *testing.T, mode string) *databaseDaemonFixture {
	t.Helper()
	home := t.TempDir()
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
	f := &databaseDaemonFixture{dm: dm, log: filepath.Join(home, "driver.log")}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(home, "database-driver")
	// Quote the executable as shell data; Go test paths may contain spaces.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	if err := os.WriteFile(driver, []byte("#!/bin/sh\nPOTLUCK_DATABASE_TEST_DRIVER=1 exec "+quote(exe)+" -test.run '^TestDatabaseProtocolDriver$'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			f.svc = s
			s.SetLookPath(func(string) (string, error) { return driver, nil })
		}
	}
	ctx := context.Background()
	f.owner = client.WithHandler(dm.SocketHandler())
	f.owner.Project, f.owner.ProjectFrom = "acme", "flag"
	p, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	f.project = p.ID
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	f.member = client.WithHandler(dm.Handler)
	f.member.Token, f.member.Project, f.member.ProjectFrom = tok.Secret, "acme", "flag"
	manifest, err := os.ReadFile("../database/testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	f.recipe = database.Recipe{Name: "example-db", Image: "snapshot@sha256:" + strings.Repeat("a", 64), Manifest: string(manifest)}
	f.setRecipes(t, []database.Recipe{f.recipe})
	_, err = computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "dbbox", Driver: "database-test", Kind: "box", Isolation: "ephemeral", Images: []string{refPin, f.recipe.Image}, Config: map[string]string{"mode": mode, "log": f.log}}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *databaseDaemonFixture) setRecipes(t *testing.T, recipes []database.Recipe) {
	t.Helper()
	_, err := f.dm.Deps.Projects.SetSection(context.Background(), f.project, "databases", func(any) (any, error) { return recipes, nil })
	if err != nil {
		t.Fatal(err)
	}
}
func (f *databaseDaemonFixture) requests(t *testing.T, verb string) []protocol.Request {
	t.Helper()
	b, _ := os.ReadFile(f.log)
	var out []protocol.Request
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r protocol.Request
		if json.Unmarshal([]byte(line), &r) == nil && r.Verb == verb {
			out = append(out, r)
		}
	}
	return out
}

func TestDatabaseDaemonRecipeIdentityAndReplay(t *testing.T) {
	f := newDatabaseDaemonFixture(t, "")
	ctx := context.Background()
	req := computeapi.AcquireRunTargetRequest{RequestID: "first", Database: "example-db"}
	first, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", req)
	if err != nil {
		t.Fatal(err)
	}
	calls := f.requests(t, "acquire")
	if len(calls) != 1 {
		t.Fatalf("acquire calls %d", len(calls))
	}
	var a struct {
		Image    string             `json:"image"`
		Database *protocol.Database `json:"database"`
	}
	if err := json.Unmarshal(calls[0].Args, &a); err != nil {
		t.Fatal(err)
	}
	want := protocol.Database{Project: f.project, Name: f.recipe.Name, Image: f.recipe.Image, Manifest: f.recipe.Manifest}
	if a.Database == nil || *a.Database != want || a.Image != "snapshot" {
		t.Fatalf("driver received %+v, want authority-resolved %+v", a, want)
	}
	edited := f.recipe
	edited.Image = "snapshot@sha256:" + strings.Repeat("b", 64)
	f.setRecipes(t, []database.Recipe{edited})
	again, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", req)
	if err != nil || again.Handle != first.Handle || again.ImageDigest != first.ImageDigest {
		t.Fatalf("replay after recipe edit: %+v %v; first %+v", again, err, first)
	}
	for _, call := range f.requests(t, "acquire") {
		var replay struct {
			Database *protocol.Database `json:"database"`
		}
		if err := json.Unmarshal(call.Args, &replay); err != nil {
			t.Fatal(err)
		}
		if replay.Database == nil || *replay.Database != want {
			t.Errorf("replay changed effective recipe: %+v", replay.Database)
		}
	}
	req.Database = "other"
	if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", req); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("changed recipe: %v", err)
	}
	req.Database = ""
	if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", req); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("removed recipe: %v", err)
	}
}

func TestDatabaseDaemonRefusesBeforeAcquire(t *testing.T) {
	f := newDatabaseDaemonFixture(t, "")
	ctx := context.Background()
	for name, req := range map[string]computeapi.AcquireRunTargetRequest{"unknown": {Database: "missing"}, "persist": {Database: "example-db", Persist: true}, "database on base builder": {Database: "example-db", Base: &computeapi.BoxBase{Repo: testBaseRepo, Fill: true}}, "image": {Database: "example-db", Image: refPin}, "template": {Database: "example-db", Template: "agent"}, "overlap": {Database: "example-db", Deps: []computeapi.BoxDeps{npmDeps(".potluck-database/data")}}} {
		t.Run(name, func(t *testing.T) {
			req.RequestID = name
			if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", req); !api.IsCode(err, api.CodeBadRequest) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// A member cannot inject an authority-resolved object into the public API.
	forged := map[string]any{"request_id": "forged", "database": protocol.Database{Project: f.project, Name: "example-db", Image: f.recipe.Image, Manifest: f.recipe.Manifest}}
	if err := f.member.Do(ctx, "POST", "/v0/run_targets/dbbox/acquire", forged, nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("forged policy: %v", err)
	}
	unauth := client.WithHandler(f.dm.Handler)
	unauth.Project, unauth.ProjectFrom = "acme", "flag"
	if _, err := computeclient.AcquireRunTarget(ctx, unauth, "dbbox", computeapi.AcquireRunTargetRequest{RequestID: "unauth", Database: "example-db"}); err == nil {
		t.Error("unauthenticated acquire succeeded")
	}
	f.member.Project = "elsewhere"
	if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", computeapi.AcquireRunTargetRequest{RequestID: "foreign", Database: "example-db"}); err == nil {
		t.Error("foreign project acquire succeeded")
	}
	if calls := f.requests(t, "acquire"); len(calls) != 0 {
		t.Fatalf("refusals reached driver: %+v", calls)
	}
	for name, mut := range map[string]func(*database.Recipe){"mutable image": func(r *database.Recipe) { r.Image = "snapshot:latest" }, "bad manifest": func(r *database.Recipe) { r.Manifest = "{}" }} {
		r := f.recipe
		mut(&r)
		_, err := f.dm.Deps.Projects.SetSection(ctx, f.project, "databases", func(any) (any, error) { return []database.Recipe{r}, nil })
		if err == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

// A binding that lends no pin of the recipe's image refuses before it records
// the request, marked so placement asks the next binding with the same id.
func TestDatabaseDaemonRefusesAnUnlentImage(t *testing.T) {
	f := newDatabaseDaemonFixture(t, "")
	ctx := context.Background()
	other := "snapshot@sha256:" + strings.Repeat("c", 64) // the same name, another digest
	_, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "nolend", Driver: "database-test", Kind: "box", Isolation: "ephemeral", Images: []string{refPin, other}, Config: map[string]string{"log": f.log}}})
	if err != nil {
		t.Fatal(err)
	}
	req := computeapi.AcquireRunTargetRequest{RequestID: "walk", Database: "example-db"}
	_, err = computeclient.AcquireRunTarget(ctx, f.member, "nolend", req)
	if reason, ok := computeapi.RefusedBeforeRecord(err); !ok || reason != computeapi.RefusedImage {
		t.Fatalf("unlent image: %v (reason %q)", err, reason)
	}
	if calls := f.requests(t, "acquire"); len(calls) != 0 {
		t.Fatalf("refusal reached driver: %+v", calls)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", req); err != nil {
		t.Fatalf("the same request id on a binding that lends it: %v", err)
	}
}

// A binding another machine serves lends what its host advertises: one whose
// advert pins other images refuses before it records anything, marked so
// placement asks the next binding (the host's own refusal would be "driver").
func TestDatabaseRefusesAnUnlentImageOnALentBinding(t *testing.T) {
	rp := newRelayPairConfigured(t, nil, map[string]string{"database": "1"})
	rp.svc.SetHostClass(api.MachineTrusted)
	ctx := context.Background()
	manifest, err := os.ReadFile("../database/testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	recipe := database.Recipe{Name: "example-db", Image: "snapshot@sha256:" + strings.Repeat("a", 64), Manifest: string(manifest)}
	if err := rp.svc.SetDatabaseRecipesForTest(ctx, rp.project, []database.Recipe{recipe}); err != nil {
		t.Fatal(err)
	}
	_, err = computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "walk", Database: "example-db"})
	if reason, ok := computeapi.RefusedBeforeRecord(err); !ok || reason != computeapi.RefusedImage {
		t.Fatalf("unlent image on %s: %v (reason %q)", lent, err, reason)
	}
	if hs, err := computeclient.RunTargetHandles(ctx, rp.owner, lent, true); err != nil || len(hs) != 0 {
		t.Fatalf("a refusal before the record left handles %+v %v", hs, err)
	}
	// A host whose advert pins the recipe's image, or names so many pins that
	// the advert's cap may have cut it, is asked; one pinning others is not.
	p, err := rp.owner.GetProject(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	full := make([]string, 16)
	for i := range full {
		full[i] = fmt.Sprintf("other%d@sha256:%s", i, strings.Repeat("d", 64))
	}
	for name, images := range map[string][]string{"lends it": {refPin, recipe.Image}, "at the advert cap": full, "other pins": {refPin}} {
		ads := rp.svc.HostAdverts(ctx, rp.project)
		ads[0].Describe.Images = images
		if err := rp.svc.Beat(ctx, p, "other", ads); err != nil {
			t.Fatal(err)
		}
		_, err := computeclient.AcquireRunTarget(ctx, rp.owner, "sandbox@other", computeapi.AcquireRunTargetRequest{RequestID: "pinned-" + strings.ReplaceAll(name, " ", "-"), Database: "example-db"})
		reason, _ := computeapi.RefusedBeforeRecord(err)
		if unlent := name == "other pins"; unlent != (reason == computeapi.RefusedImage) {
			t.Errorf("%s: %v (reason %q), want refused as unlent: %v", name, err, reason, unlent)
		}
	}
}

func TestDatabaseDaemonDriverFailures(t *testing.T) {
	for _, mode := range []string{"unsupported", "wrong-image", "wrong-database", "missing-database", "prepare-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newDatabaseDaemonFixture(t, mode)
			ctx := context.Background()
			h, err := computeclient.AcquireRunTarget(ctx, f.member, "dbbox", computeapi.AcquireRunTargetRequest{RequestID: "failure", Database: "example-db"})
			if err == nil || h.Handle != "" {
				t.Fatalf("unusable database returned %+v %v", h, err)
			}
			acquire, release := len(f.requests(t, "acquire")), len(f.requests(t, "release"))
			switch mode {
			case "unsupported":
				if acquire != 0 {
					t.Errorf("unsupported driver acquired %d boxes", acquire)
				}
			case "prepare-failed":
				if !strings.Contains(err.Error(), "snapshot preparation failed") {
					t.Errorf("lost prepare failure: %v", err)
				}
			default:
				reason := "database provenance"
				if mode == "wrong-image" {
					reason = "booted image"
				}
				if !strings.Contains(err.Error(), reason) {
					t.Errorf("wrong refusal reason: %v", err)
				}
				if acquire != 1 || release != 1 {
					t.Errorf("invalid answer: acquires=%d releases=%d error=%v", acquire, release, err)
				}
			}
			hs, listErr := computeclient.RunTargetHandles(ctx, f.member, "dbbox", false)
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, h := range hs {
				if h.State == "ready" || h.State == "running" {
					t.Errorf("failed acquisition left usable handle %+v", h)
				}
			}
		})
	}
}

func TestDatabaseHostRelayProjectScope(t *testing.T) {
	f := newDatabaseDaemonFixture(t, "")
	ctx := context.Background()
	_, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{f.project}, Target: computeapi.RunTarget{Name: "hostdb", Driver: "database-test", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"log": f.log}}})
	if err != nil {
		t.Fatal(err)
	}
	d := protocol.Database{Project: "foreign-project", Name: f.recipe.Name, Image: f.recipe.Image, Manifest: f.recipe.Manifest}
	args := func() json.RawMessage {
		b, _ := json.Marshal(map[string]any{"request_id": "relay", "actor": "ana", "ttl_s": 120, "image": d.Image, "database": d})
		return b
	}
	if _, err := f.svc.HostCall(ctx, f.project, "hostdb", "acquire", "", args()); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("foreign database accepted: %v", err)
	}
	if len(f.requests(t, "acquire")) != 0 {
		t.Fatal("foreign database reached host driver")
	}
	d.Project = f.project
	if _, err := f.svc.HostCall(ctx, f.project, "hostdb", "acquire", "", args()); err != nil {
		t.Fatalf("trusted project-scoped relay: %v", err)
	}
	calls := f.requests(t, "acquire")
	if len(calls) != 1 {
		t.Fatalf("relay acquire count %d", len(calls))
	}
	var got struct {
		Database protocol.Database `json:"database"`
	}
	if err := json.Unmarshal(calls[0].Args, &got); err != nil {
		t.Fatal(err)
	}
	if got.Database != d {
		t.Fatalf("relay changed provenance: %s", fmt.Sprint(got.Database))
	}
}

func TestDatabaseComposesWithBaseAndDependencies(t *testing.T) {
	f := newDatabaseDaemonFixture(t, "")
	ctx := context.Background()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	driver, err := refdriver.Install(dir, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	runtarget.SetDepsRecipe(t, "npm", testRecipe)
	_, err = computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "layers", Driver: "database-test", Kind: "box", Isolation: "ephemeral", Images: []string{refPin, f.recipe.Image}, Config: map[string]string{"log": f.log, "delegate": driver, "net_off": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	fillBase(t, f.member, "layers", "base-fill")
	request := baseReq("with-database", false)
	request.Database = "example-db"
	request.Deps = []computeapi.BoxDeps{npmDeps("app")}
	h, err := computeclient.AcquireRunTarget(ctx, f.member, "layers", request)
	if err != nil {
		t.Fatal(err)
	}
	defer computeclient.ReleaseRunTarget(ctx, f.member, "layers", idOf(h.Handle))
	if h.Base == nil || !h.Base.Hit || len(h.Deps) != 1 {
		t.Fatalf("composition omitted base/deps: %+v", h)
	}
	check := func(key string) computeapi.RunTargetHandle {
		request.RequestID = key
		h, err := computeclient.AcquireRunTarget(ctx, f.member, "layers", request)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	again := check("repeat-database")
	defer computeclient.ReleaseRunTarget(ctx, f.member, "layers", idOf(again.Handle))
	builders := 0
	for _, call := range f.requests(t, "acquire") {
		var a struct {
			RequestID string                 `json:"request_id"`
			Image     string                 `json:"image"`
			Database  *protocol.Database     `json:"database"`
			Volumes   []computeapi.BoxVolume `json:"volumes"`
		}
		_ = json.Unmarshal(call.Args, &a)
		if strings.Contains(a.RequestID, "deps-build-") {
			builders++
			if a.Image != "snapshot" || a.Database != nil {
				t.Fatalf("dependency builder must use DB image without starting DB: %+v", a)
			}
		}
		if a.Database != nil && a.Image != "snapshot" {
			t.Fatalf("database task used wrong image: %+v", a)
		}
	}
	if builders != 1 {
		t.Fatalf("repeat database task rebuilt cached dependencies: builders=%d", builders)
	}
	// Replay after the owner's image changes still uses the original layer key
	// and recipe. No builder for the changed image may enter this request.
	updated := f.recipe
	updated.Image = "different@sha256:" + strings.Repeat("b", 64)
	f.setRecipes(t, []database.Recipe{updated})
	replay := check("with-database")
	if replay.Handle != h.Handle || replay.ImageDigest != h.ImageDigest {
		t.Fatalf("replay lost frozen image: %+v", replay)
	}
	for _, path := range []string{protocol.DatabaseHome, protocol.DatabaseHome + "/data"} {
		bad := request
		bad.RequestID = "overlap"
		bad.Deps = []computeapi.BoxDeps{npmDeps(path)}
		if _, err := computeclient.AcquireRunTarget(ctx, f.member, "layers", bad); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("overlapping dependency path %s: %v", path, err)
		}
		bad.Deps = nil
		bad.Volumes = []computeapi.BoxVolume{{Name: "shared", Path: path}}
		if _, err := computeclient.AcquireRunTarget(ctx, f.member, "layers", bad); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("overlapping volume path %s: %v", path, err)
		}
	}
}
