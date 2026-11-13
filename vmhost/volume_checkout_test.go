package vmhost

import (
	"path/filepath"
	"strings"
	"testing"
)

// zm-eakh.12.65.6: a box whose cow volume says checkout has its checkout in
// that volume's directory, beside the layer's node_modules, which the
// checkout leaves alone; its writes stay in the box; a box without one keeps
// guestDir/src.
func TestCheckoutIntoACOWVolume(t *testing.T) {
	_, repo, _ := fakeBinding(t, "linux-img")
	m := fakeVolumeBox(t)
	m["remote"] = repo.Remote
	co := func(name, path string) map[string]any {
		v := vol(name, path, "cow")
		v["checkout"] = true
		return v
	}
	for why, a := range map[string]map[string]any{
		"checkout on rw":    volArgs("bad1", "alice", map[string]any{"name": "deps", "path": "work", "mode": "rw", "checkout": true}),
		"checkout on ro":    volArgs("bad2", "alice", map[string]any{"name": "deps", "path": "work", "mode": "ro", "checkout": true}),
		"checkout on build": volArgs("bad3", "alice", map[string]any{"name": "deps", "path": "work", "mode": "build", "checkout": true}),
		"two checkouts":     volArgs("bad4", "alice", co("deps", "work"), co("deps2", "other")),
	} {
		if _, err := call(t, m, "acquire", "", a); code(err) != "bad_request" {
			t.Errorf("%s: %v, want bad_request", why, err)
		}
	}

	// The layer: node_modules and a package.json the commit does not hold.
	h := acquireVol(t, m, volArgs("fill", "alice", vol("deps", "work", "rw")))
	if c, _ := sh(t, m, h, `cd "$HOME/work" && mkdir -p node_modules/a && echo built > node_modules/a/index.js && echo '{}' > package.json && echo layer > probe.sh`); c != 0 {
		t.Fatal("filling the layer failed")
	}
	release(t, m, h)

	a := volArgs("co", "alice", co("deps", "work"))
	a["ref"], a["commit"] = "main", repo.Commit
	res, err := call(t, m, "acquire", "", a)
	if err != nil {
		t.Fatal(err)
	}
	b := res["handle"].(string)
	if vs, _ := res["volumes"].([]any); len(vs) != 1 || vs[0].(map[string]any)["checkout"] != true {
		t.Errorf("acquire's volumes: %v (want checkout true)", res["volumes"])
	}
	_, out := sh(t, m, b, `pwd -P; git rev-parse HEAD; cat node_modules/a/index.js package.json; git status --porcelain --untracked-files=no; [ -d /tmp/zm-run/src ] && echo has-src || echo no-src`)
	home, _ := filepath.EvalSymlinks(filepath.Join(m["state_dir"], "fakevms"))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], home) || !strings.HasSuffix(lines[0], "/home/work") || lines[1] != repo.Commit ||
		lines[2] != "built" || lines[3] != "{}" || lines[4] != "no-src" {
		t.Fatalf("exec's default cwd in a checkout box: %q (want the volume dir at %s, node_modules and the layer's package.json kept)", out, repo.Commit)
	}
	if c, _ := sh(t, m, b, `echo changed > node_modules/a/index.js`); c != 0 {
		t.Fatal("a write to node_modules in the checkout box failed")
	}
	if _, out := sh(t, m, b, `head -c 5 probe.sh`); out == "layer" {
		t.Error("the checkout left the layer's probe.sh: the commit's files must win")
	}
	release(t, m, b)
	h = acquireVol(t, m, volArgs("look", "alice", vol("deps", "work", "ro")))
	if _, out := sh(t, m, h, `cd "$HOME/work" && cat node_modules/a/index.js probe.sh && ls -A`); out != "built\nlayer\nnode_modules\npackage.json\nprobe.sh\n" {
		t.Errorf("the layer after a checkout box: %q (want it as it was)", out)
	}
	release(t, m, h)

	// A box without one: guestDir/src, as ever.
	a = map[string]any{"request_id": "plain", "ttl_s": 120, "actor": "someone", "ref": "main", "commit": repo.Commit}
	res, err = call(t, m, "acquire", "", a)
	if err != nil {
		t.Fatal(err)
	}
	p := res["handle"].(string)
	if _, out := sh(t, m, p, `pwd -P; git rev-parse HEAD`); !strings.HasSuffix(strings.Split(out, "\n")[0], "/guest/src") || !strings.Contains(out, repo.Commit) {
		t.Errorf("a box without a checkout volume: %q, want guestDir/src at the commit", out)
	}
	release(t, m, p)
}

// A .git the checkout volume holds is never trusted: the driver's checkout
// makes its own, so the layer's hooks never run and its config never
// reaches the box's checkout, which is exactly the commit.
func TestCheckoutNeverTrustsALayersGit(t *testing.T) {
	_, repo, _ := fakeBinding(t, "linux-img")
	m := fakeVolumeBox(t)
	m["remote"] = repo.Remote
	h := acquireVol(t, m, volArgs("fill", "alice", vol("deps", "work", "rw")))
	plant := `set -e; cd "$HOME/work"; mkdir -p node_modules; git init -q .; git config core.hooksPath .git/h; mkdir -p .git/h
printf '#!/bin/sh\necho hooked > "$HOME/work/hooked"\n' > .git/h/post-checkout; chmod +x .git/h/post-checkout`
	if c, out := sh(t, m, h, plant); c != 0 {
		t.Fatalf("planting a .git: %s", out)
	}
	release(t, m, h)
	v := vol("deps", "work", "cow")
	v["checkout"] = true
	a := volArgs("co", "alice", v)
	a["ref"], a["commit"] = "main", repo.Commit
	res, err := call(t, m, "acquire", "", a)
	if err != nil {
		t.Fatal(err)
	}
	b := res["handle"].(string)
	defer release(t, m, b)
	if _, out := sh(t, m, b, `git rev-parse HEAD; git config --get core.hooksPath; test -e hooked && echo hooked; true`); out != repo.Commit+"\n" {
		t.Errorf("the checkout over a layer's .git: %q, want only HEAD at %s", out, repo.Commit)
	}
}
