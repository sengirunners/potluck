package vmhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/tartpin"
	"github.com/sengirunners/potluck/tartpin/tartpintest"
)

// staleTart is a file in a searched directory that has no execute bit.
const staleTart = "/usr/local/bin/tart"

func fakeTartSearch(onPath string, home string, files ...string) TartSearch {
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	return TartSearch{
		LookPath:   func(name string) (string, bool) { return onPath, onPath != "" },
		Executable: func(p string) bool { return have[p] && p != staleTart },
		Exists:     func(p string) bool { return have[p] },
		Home:       home,
	}
}

func TestFindTartOrder(t *testing.T) {
	const (
		home    = "/Users/o"
		app     = "/Applications/tart.app/Contents/MacOS/tart"
		homeApp = "/Users/o/Applications/tart.app/Contents/MacOS/tart"
		local   = "/Users/o/.local/bin/tart"
		brew    = "/opt/homebrew/bin/tart"
	)
	cases := []struct {
		name   string
		onPath string
		files  []string
		want   string
		hollow []string
	}{
		{name: "PATH wins over everything", onPath: "/p/tart", files: []string{app, local}, want: "/p/tart"},
		{name: "a bin directory before an app", files: []string{app, brew}, want: brew},
		{name: "the install guide's link", files: []string{local, brew}, want: local},
		{name: "Applications", files: []string{app, homeApp}, want: app},
		{name: "home Applications", files: []string{homeApp}, want: homeApp},
		{name: "a bundle with no program is hollow, a later whole one is used", files: []string{"/Applications/tart.app", homeApp},
			want: homeApp},
		{name: "only a hollow bundle", files: []string{"/Applications/tart.app"}, hollow: []string{"/Applications/tart.app"}},
		{name: "a file that cannot run does not hide a working app", files: []string{staleTart, app}, want: app},
		{name: "nothing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindTart(fakeTartSearch(c.onPath, home, c.files...))
			if got.Path != c.want {
				t.Fatalf("found %+v, want %q", got, c.want)
			}
			if c.hollow != nil && (len(got.Hollow) != 1 || got.Hollow[0] != c.hollow[0]) {
				t.Fatalf("hollow %v, want %v", got.Hollow, c.hollow)
			}
		})
	}
}

// With no home the home's directories are left out.
func TestFindTartWithoutHome(t *testing.T) {
	got := FindTart(fakeTartSearch("", "", "/Applications/tart.app/Contents/MacOS/tart"))
	if got.Path == "" {
		t.Fatalf("lost /Applications without a home: %+v", got)
	}
}

// A daemon the launchd job starts has no Homebrew or ~/.local/bin on its
// PATH and no bundled tart.app anywhere: a binding that names no Tart still
// runs the one the lender installed.
func TestConfigFindsTartOffPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	bin := filepath.Join(home, ".local", "bin", "tart")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := func(extra ...string) map[string]string {
		m := map[string]string{"state_dir": t.TempDir(), "image": "img", "remote": "https://example.com/r.git"}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	c, err := parseConfig(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.Tart != bin {
		t.Fatalf("tart = %q, want %q", c.Tart, bin)
	}
	if c, _ = parseConfig(base("tart", "/chosen/tart")); c.Tart != "/chosen/tart" {
		t.Fatalf("a named tart was replaced: %q", c.Tart)
	}
}

// The copy potluck tart install puts in ~/Applications is the one FindTart
// runs when nothing else is installed.
func TestFindTartFindsTheInstalledCopy(t *testing.T) {
	home := t.TempDir()
	p := tartpin.Place{AppsDir: filepath.Join(home, "Applications"), RecordFile: filepath.Join(home, "rec.json")}
	tarball, _, b := tartpintest.Release(t, "1.0.0", tartpintest.Whole())
	if _, err := p.Install(tarball, b); err != nil {
		t.Fatal(err)
	}
	inHome := func(path string) bool { return strings.HasPrefix(path, home+string(filepath.Separator)) }
	s := TartSearch{
		LookPath:   func(string) (string, bool) { return "", false },
		Executable: func(path string) bool { return inHome(path) && IsExecutableFile(path) },
		Exists:     func(path string) bool { _, err := os.Stat(path); return inHome(path) && err == nil },
		Home:       home,
	}
	if got := FindTart(s); got.Path != p.Program() {
		t.Fatalf("found %+v, want %s", got, p.Program())
	}
}
