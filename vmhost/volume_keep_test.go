package vmhost

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tree lists dir's entries (relative paths; "@" after a link), without
// following links.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		r, _ := filepath.Rel(dir, p)
		if e.Type()&os.ModeSymlink != 0 {
			r += "@"
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// A release with seal and keep seals only the kept trees: everything else
// the builder wrote (a .git, other files, a kept path or a directory on the
// way that is a link or a file) is gone, no link is followed out of the
// volume, and what a kept tree holds (links too) stays as it was.
func TestSealKeepsOnlyKeep(t *testing.T) {
	b := newSealBox(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(outside, "node_modules", "keepme"), []byte("x"), 0o600)
	h := acquireVol(t, b.m, volArgs("k", "o", build("lay")))
	fill := `set -e; cd "$HOME/dep"
mkdir -p node_modules/a/.git node_modules/.bin .git/hooks ws/node_modules/b ws/src deep/er ws4
echo f > node_modules/a/f; echo g > node_modules/a/.git/config; ln -s ../a/f node_modules/.bin/x
echo '[core] hooksPath = x' > .git/config; echo h > .git/hooks/post-checkout
echo x > x.txt; echo y > .npmrc; echo z > ..odd; printf q > "$(printf 'a\nnode_modules')"
echo b > ws/node_modules/b/f; echo s > ws/src/i.js; echo t > ws/.gitattributes
ln -s ` + outside + ` ws2
echo file > ws3
ln -s ` + outside + `/node_modules ws4/node_modules
ln -s ../.. up
`
	if c, out := sh(t, b.m, h, fill); c != 0 {
		t.Fatalf("filling the build volume: %v %s", c, out)
	}
	keep := []string{"node_modules", "ws/node_modules", "ws2/node_modules", "ws3/node_modules", "ws4/node_modules", "absent/node_modules"}
	if res, err := call(t, b.m, "release", h, map[string]any{"seal": true, "keep": keep}); err != nil || res["released"] != true {
		t.Fatalf("release: %v %v", res, err)
	}
	if v, ok := b.st.loadVol(volID("t", "o", "lay")); !ok || !v.Sealed {
		t.Fatalf("not sealed: %+v %v", v, ok)
	}
	want := []string{"node_modules", "node_modules/.bin", "node_modules/.bin/x@", "node_modules/a", "node_modules/a/.git", "node_modules/a/.git/config", "node_modules/a/f",
		"ws", "ws/node_modules", "ws/node_modules/b", "ws/node_modules/b/f", "ws4"}
	if got := tree(t, b.dir("o", "lay")); !slices.Equal(got, want) {
		t.Errorf("the sealed volume holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := os.Stat(filepath.Join(outside, "node_modules", "keepme")); err != nil {
		t.Errorf("the prune followed a link out of the volume: %v", err)
	}
}

// keep is for a release with seal, and each path is a volume path; a prune
// that fails leaves no volume (never sealed).
func TestSealKeepRefusalsAndFailure(t *testing.T) {
	b := newSealBox(t)
	h := acquireVol(t, b.m, volArgs("k", "o", build("lay")))
	for _, a := range []map[string]any{
		{"keep": []string{"node_modules"}},
		{"seal": true, "keep": []string{}},
		{"seal": true, "keep": []string{"../x"}},
		{"seal": true, "keep": []string{"/abs"}},
		{"seal": true, "keep": []string{"a//b"}},
	} {
		if _, err := call(t, b.m, "release", h, a); code(err) != "bad_request" {
			t.Errorf("release %v: %v, want bad_request", a, err)
		}
	}
	if c, _ := sh(t, b.m, h, `mkdir -p "$HOME/dep/node_modules"`); c != 0 {
		t.Fatal("the box ended on a refused release")
	}
	_ = os.WriteFile(filepath.Join(filepath.Dir(b.fake.root), "keep-fail"), nil, 0o600)
	if _, err := call(t, b.m, "release", h, map[string]any{"seal": true, "keep": []string{"node_modules"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.st.loadVol(volID("t", "o", "lay")); ok {
		t.Error("a volume whose prune failed is still there")
	}
	if _, err := os.Stat(b.dir("o", "lay")); err == nil {
		t.Error("its directory is still there")
	}
}

// OCI: the prune is one helper container from the box's image with the
// volume its one mount, writable, no network, the binding's runtime and a
// read-only root, running sealCleanScript; its exit is CleanSeal's answer, and the
// helper is removed by the id the engine wrote.
func TestOCICleanSealHelper(t *testing.T) {
	for _, fail := range []bool{false, true} {
		o, dir := newFakeOCI(t, "none")
		cid := strings.Repeat("b", 64)
		exit := "0"
		if fail {
			exit = "1"
		}
		helper := `run) previous=;for arg;do if [ "$previous" = --cidfile ];then printf '%s\n' "` + cid + `" > "$arg";fi;previous=$arg;done; for arg; do printf '%s\n' "$arg"; done > "$d/runargs"; exit ` + exit + `;;`
		if err := os.WriteFile(o.bin, []byte(fakeOCICase(t, fakeOCI, helper)), 0o700); err != nil {
			t.Fatal(err)
		}
		err := o.CleanSeal("box-img", "lay1", []string{"node_modules", "ws/node_modules"})
		if fail != (err != nil) {
			t.Fatalf("fail %v: CleanSeal answered %v", fail, err)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "runargs"))
		args := string(raw)
		for _, w := range []string{"--network\nnone\n", "--runtime\n" + o.runtime + "\n", "--read-only\n", "--cap-drop\nALL\n", "--name\n" + ociKeepPrefix,
			"--mount\ntype=volume,source=lay1,target=/potluck-keep,volume-nocopy\nbox-img\n-c\n" + sealCleanScript + "\nsh\n/potluck-keep\nkeep\nnode_modules\nws/node_modules\nws\n"} {
			if !strings.Contains(args, w) {
				t.Errorf("the helper's run lacks %q:\n%s", w, args)
			}
		}
		if strings.Count(args, "--mount\n") != 1 {
			t.Errorf("the helper mounts more than the volume:\n%s", args)
		}
		if calls, _ := os.ReadFile(filepath.Join(dir, "calls")); !strings.Contains(string(calls), "rm -f "+cid) {
			t.Errorf("the helper was not removed: %s", calls)
		}
	}
}
