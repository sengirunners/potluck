package runtarget

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sengirunners/potluck/computeapi"
)

const collectLock = `{"lockfileVersion":3,"packages":{"":{"workspaces":["packages/*"]},"packages/a":{},"node_modules/a":{"link":true},"node_modules/x/node_modules/y":{}}}`

// What a caller collects is exactly what the daemon's kinds table takes: the
// daemon accepts it, and any file the table does not name is not collected.
func TestCollectDepsFilesIsWhatTheDaemonTakes(t *testing.T) {
	fsys := fstest.MapFS{
		"package-lock.json":       {Data: []byte(collectLock)},
		"package.json":            {Data: []byte(`{}`)},
		"packages/a/package.json": {Data: []byte(`{"name":"a"}`)},
		"packages/b/package.json": {Data: []byte(`{"name":"b"}`)},
		"index.js":                {Data: []byte(`x`)},
	}
	files, err := CollectDepsFiles("npm", fsys, computeapi.DepsFilesMax, computeapi.DepsBytesMax)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("%d files: %+v", len(files), files)
	}
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	if _, err := depsLayers(computeapi.AcquireRunTargetRequest{Deps: []computeapi.BoxDeps{{Kind: "npm", Path: "work", Files: files}}}, digest); err != nil {
		t.Fatalf("the daemon refuses what a caller collects: %v", err)
	}
}

func TestCollectDepsFilesRefusals(t *testing.T) {
	base := func() fstest.MapFS {
		return fstest.MapFS{
			"package-lock.json":       {Data: []byte(collectLock)},
			"package.json":            {Data: []byte(`{}`)},
			"packages/a/package.json": {Data: []byte(`{}`)},
		}
	}
	for _, c := range []struct {
		name         string
		edit         func(fstest.MapFS)
		kind         string
		files, bytes int
		want         string
	}{
		{"unknown kind", func(fstest.MapFS) {}, "gem", 10, 1 << 20, `no dependency layer kind "gem"`},
		{"no lockfile", func(m fstest.MapFS) { delete(m, "package-lock.json") }, "npm", 10, 1 << 20, "package-lock.json"},
		{"missing workspace", func(m fstest.MapFS) { delete(m, "packages/a/package.json") }, "npm", 10, 1 << 20, "packages/a/package.json"},
		{"a directory", func(m fstest.MapFS) { m["package.json"] = &fstest.MapFile{Mode: 0o20000000755} }, "npm", 10, 1 << 20, "regular file"},
		{"too many files", func(fstest.MapFS) {}, "npm", 2, 1 << 20, "files"},
		{"too many bytes", func(fstest.MapFS) {}, "npm", 10, len(collectLock) + 1, "MiB"},
		{"path out of the project", func(m fstest.MapFS) {
			m["package-lock.json"] = &fstest.MapFile{Data: []byte(`{"lockfileVersion":3,"packages":{"":{},"../x":{}}}`)}
		}, "npm", 10, 1 << 20, "outside the project"},
	} {
		m := base()
		c.edit(m)
		if _, err := CollectDepsFiles(c.kind, m, c.files, c.bytes); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
