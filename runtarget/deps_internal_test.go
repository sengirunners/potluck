package runtarget

import (
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
)

// A layer's key, so its name, changes with everything its build depends
// on: the image digest, the mount path, the scripts setting and any byte or
// path of any input file. The same request keys the same, whatever order
// its files come in.
func TestDepsKey(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	base := func() computeapi.BoxDeps {
		return computeapi.BoxDeps{Kind: "npm", Path: "work", Files: []computeapi.DepsFile{
			{Path: "package-lock.json", Data: []byte(`{"lockfileVersion":3,"packages":{"":{},"packages/a":{}}}`)},
			{Path: "package.json", Data: []byte(`{"name":"x"}`)},
			{Path: "packages/a/package.json", Data: []byte(`{"name":"a"}`)},
		}}
	}
	key := func(d computeapi.BoxDeps, digest string) string {
		t.Helper()
		ls, err := depsLayers(computeapi.AcquireRunTargetRequest{Deps: []computeapi.BoxDeps{d}}, digest)
		if err != nil {
			t.Fatal(err)
		}
		if n := ls[0].name(); len(n) != 61 || n[:9] != "deps-npm-" || n[9:] != ls[0].key[:52] {
			t.Fatalf("name %q for key %s", n, ls[0].key)
		}
		return ls[0].key
	}
	k := key(base(), digest)
	if k2 := key(base(), digest); k2 != k {
		t.Fatalf("the same request keyed %s and %s", k, k2)
	}
	swapped := base()
	swapped.Files[0], swapped.Files[2] = swapped.Files[2], swapped.Files[0]
	if key(swapped, digest) != k {
		t.Error("the files' order changed the key")
	}
	changed := map[string]string{"image digest": key(base(), "sha256:0000000000000000000000000000000000000000000000000000000000000002")}
	p := base()
	p.Path = "app"
	changed["path"] = key(p, digest)
	sc := base()
	sc.Scripts = computeapi.DepsScriptsOff
	changed["scripts off"] = key(sc, digest)
	for i := range base().Files {
		b := base()
		b.Files[i].Data = append(b.Files[i].Data[:len(b.Files[i].Data)-1:len(b.Files[i].Data)-1], ' ', '}')
		changed["a byte of "+b.Files[i].Path] = key(b, digest)
	}
	seen := map[string]string{k: "the base"}
	for what, ck := range changed {
		if other, dup := seen[ck]; dup {
			t.Errorf("%s keys the same as %s", what, other)
		}
		seen[ck] = what
	}
	if npmRecipe(true) == npmRecipe(false) {
		t.Error("scripts off runs the same recipe")
	}
}

// A member's proxy waits for an acquire with deps as long as it may take:
// deps_wait_s and two builds a layer more than a plain acquire; any other
// call, or a body that is not an acquire's, waits as before.
func TestForwardedDepsAnswerWithin(t *testing.T) {
	const acq = "/v0/run_targets/sandbox@lender/acquire"
	plain := runTargetAnswerWithin("POST", acq, []byte(`{"request_id":"r"}`))
	if got := runTargetAnswerWithin("POST", acq, []byte(`{"deps":[]}`)); got != plain {
		t.Errorf("empty deps: %v, want %v", got, plain)
	}
	one := runTargetAnswerWithin("POST", acq, []byte(`{"deps":[{}],"deps_wait_s":0}`))
	build := time.Duration(depsBuildTTLS) * time.Second
	if one < plain+2*build {
		t.Errorf("one layer, no wait: %v, want at least %v", one, plain+2*build)
	}
	if got := runTargetAnswerWithin("POST", acq, []byte(`{"deps":[{}]}`)); got != one+computeapi.DepsWaitDefaultS*time.Second {
		t.Errorf("one layer, the default wait: %v, want %v", got, one+computeapi.DepsWaitDefaultS*time.Second)
	}
	if got := runTargetAnswerWithin("POST", acq, []byte(`{"deps":[{},{},{},{},{},{}],"deps_wait_s":99999}`)); got != runTargetAnswerWithin("POST", acq, []byte(`{"deps":[{},{},{},{}],"deps_wait_s":3600}`)) {
		t.Errorf("past the limits: %v, want the limits' wait", got)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v0/run_targets/sandbox@lender/handles/x/exec", `{"deps":[{}]}`},
		{"GET", acq, `{"deps":[{}]}`},
		{"POST", acq, `not json`},
	} {
		n, _ := forwardedDeps(c.method, c.path, []byte(c.body))
		if n != 0 {
			t.Errorf("%s %s %s: %d layers, want none", c.method, c.path, c.body, n)
		}
	}
}
