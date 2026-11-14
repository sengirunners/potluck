package runtarget

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sengirunners/potluck/computeapi"
)

// zm-eakh.12.65.26: an .npmrc is read as syntax, and only resolution keys
// pass, rewritten to their key=value lines; any other key is refused by
// name, its value never in the message.
func TestNpmrcCanon(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"legacy-peer-deps=true\n", "legacy-peer-deps=true\n"},
		{"# a comment: //r/:_authToken=x\n; another\n\n  legacy-peer-deps = true  \r\nomit[]=dev\nomit=optional\n", "legacy-peer-deps=true\nomit[]=dev\nomit=optional\n"},
		{"install-strategy=hoisted\nengine-strict=false\nsave-exact=true\ninstall-links=false\nstrict-peer-deps=false\ninclude=peer", "install-strategy=hoisted\nengine-strict=false\nsave-exact=true\ninstall-links=false\nstrict-peer-deps=false\ninclude=peer\n"},
		{"# only comments\n\n", ""},
	} {
		got, err := npmrcCanon([]byte(c.in))
		if err != nil || string(got) != c.want {
			t.Errorf("%q: %q %v, want %q", c.in, got, err, c.want)
		}
	}
	const secret = "s3cr3t-T0KEN"
	for _, c := range []struct{ in, want string }{
		{"//registry.npmjs.org/:_authToken=" + secret, "key (not shown"},
		{"//user:" + secret + "@host/:_auth=x", "key (not shown"},
		{"_auth=" + secret, `key "_auth" is not one`},
		{"_authToken=" + secret, `key "_authToken" is not one`},
		{"registry=https://evil.example/" + secret, `key "registry" is not one`},
		{"@scope:registry=https://evil.example/" + secret, `key "@scope:registry" is not one`},
		{"legacy-peer-deps=true\nemail=" + secret, `line 2: key "email" is not one`},
		{"script-shell=/bin/" + secret, `key "script-shell" is not one`},
		{secret + " x=y", "(not shown"},
		{secret, "line 1 is not key=value"},
		{"[section]", "line 1 is not key=value"},
		{"=true", "line 1 is not key=value"},
		{"legacy-peer-deps=${" + secret + "}", "key legacy-peer-deps takes true, false"},
		{"legacy-peer-deps=\"true\"", "key legacy-peer-deps takes true, false"},
		{"legacy-peer-deps[]=true", "key legacy-peer-deps is not a list"},
		{"omit=" + secret, "key omit takes dev, optional, peer"},
	} {
		_, err := npmrcCanon([]byte(c.in))
		switch {
		case err == nil:
			t.Errorf("%q: taken, want refused", c.in)
		case !strings.Contains(err.Error(), c.want):
			t.Errorf("%q: %v, want %q", c.in, err, c.want)
		case strings.Contains(err.Error(), secret):
			t.Errorf("%q: the refusal shows the value: %v", c.in, err)
		}
	}
}

// An .npmrc beside the lockfile is collected, sent as its key=value lines
// only, and part of the layer's key: a resolution key changes it, a comment
// does not, and one with no key keys as a project with none. One with a
// credential is refused before it is sent, and by the daemon.
func TestNpmrcIsALayerInput(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	project := func(npmrc string) fstest.MapFS {
		m := fstest.MapFS{
			"package-lock.json":       {Data: []byte(collectLock)},
			"package.json":            {Data: []byte(`{}`)},
			"packages/a/package.json": {Data: []byte(`{}`)},
		}
		if npmrc != "-" {
			m[".npmrc"] = &fstest.MapFile{Data: []byte(npmrc)}
		}
		return m
	}
	key := func(files []computeapi.DepsFile) string {
		t.Helper()
		ls, err := depsLayers(computeapi.AcquireRunTargetRequest{Deps: []computeapi.BoxDeps{{Kind: "npm", Path: "work", Files: files}}}, digest)
		if err != nil {
			t.Fatalf("the daemon refuses: %v", err)
		}
		return ls[0].key
	}
	collect := func(npmrc string) []computeapi.DepsFile {
		t.Helper()
		files, err := CollectDepsFiles("npm", project(npmrc), computeapi.DepsFilesMax, computeapi.DepsBytesMax)
		if err != nil {
			t.Fatal(err)
		}
		return files
	}

	files := collect("# onCall's\nlegacy-peer-deps=true\n")
	var sent string
	for _, f := range files {
		if f.Path == ".npmrc" {
			sent = string(f.Data)
		}
	}
	if sent != "legacy-peer-deps=true\n" {
		t.Fatalf("the .npmrc sent: %q, want its key=value line only", sent)
	}
	none, legacy := key(collect("-")), key(files)
	if none == legacy {
		t.Error("legacy-peer-deps=true keys the same as no .npmrc")
	}
	if key(collect("legacy-peer-deps=true")) != legacy {
		t.Error("a comment or a final newline changed the key")
	}
	if key(collect("# nothing\n")) != none {
		t.Error("an .npmrc with no key keys differently from none")
	}
	if key(collect("legacy-peer-deps=false\n")) == legacy {
		t.Error("another value keys the same")
	}
	// The daemon rewrites what a caller sent as it came, as the collector does.
	raw := append(collect("-"), computeapi.DepsFile{Path: ".npmrc", Data: []byte("; raw\nlegacy-peer-deps = true\n")})
	if key(raw) != legacy {
		t.Error("the daemon keys a raw .npmrc differently from the collected one")
	}

	const secret = "npm_S3CRET"
	if _, err := CollectDepsFiles("npm", project("legacy-peer-deps=true\n//registry.npmjs.org/:_authToken="+secret+"\n"), computeapi.DepsFilesMax, computeapi.DepsBytesMax); err == nil || !strings.Contains(err.Error(), "line 2: key (not shown") || strings.Contains(err.Error(), secret) {
		t.Errorf("collecting an .npmrc with a token: %v, want refused by key, the token not shown", err)
	}
	bad := append(collect("-"), computeapi.DepsFile{Path: ".npmrc", Data: []byte("registry=https://evil.example/\n")})
	if _, err := depsLayers(computeapi.AcquireRunTargetRequest{Deps: []computeapi.BoxDeps{{Kind: "npm", Path: "work", Files: bad}}}, digest); err == nil || !strings.Contains(err.Error(), `"registry"`) {
		t.Errorf("the daemon given an .npmrc naming a registry: %v, want refused", err)
	}
	nested := append(collect("-"), computeapi.DepsFile{Path: "packages/a/.npmrc", Data: []byte("legacy-peer-deps=true\n")})
	if _, err := depsLayers(computeapi.AcquireRunTargetRequest{Deps: []computeapi.BoxDeps{{Kind: "npm", Path: "work", Files: nested}}}, digest); err == nil {
		t.Error("the daemon took a workspace's .npmrc: only the project's, beside the lockfile")
	}
}
