package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The package's tests run as a program whose names are these; kit has none.
const testHomeEnv = "TEST_HOME"

var testNames = Names{HomeEnvs: []string{testHomeEnv}, HomeDirs: []string{".testhome"}, Sockets: []string{"test.sock"}}

func TestMain(m *testing.M) {
	Use(testNames)
	os.Exit(m.Run())
}

// wire sets n for one test and restores the names it found.
func wire(t *testing.T, n Names) {
	t.Helper()
	old := Wired()
	Use(n)
	t.Cleanup(func() { Use(old) })
}

// A renamed program: new names first, its old ones after.
var renamed = Names{
	HomeEnvs: []string{"NEW_HOME", "OLD_HOME"},
	HomeDirs: []string{".new", ".old"},
	Sockets:  []string{"new.sock", "old.sock"},
}

// The one home order (Names): the first variable set; two that differ warn
// and the first wins; else the first directory that exists; else the first.
func TestResolveHomeOrder(t *testing.T) {
	cwd, _ := os.Getwd()
	for _, tc := range []struct {
		name     string
		n        Names
		env      map[string]string
		user     string
		dirs     []string // directories that exist
		want     string
		from     string
		warn     bool
		errMatch string
	}{
		{name: "new variable", n: renamed, env: map[string]string{"NEW_HOME": "/a"}, user: "/u", want: "/a", from: "NEW_HOME"},
		{name: "old variable only", n: renamed, env: map[string]string{"OLD_HOME": "/b"}, user: "/u", want: "/b", from: "OLD_HOME"},
		{name: "both, same home", n: renamed, env: map[string]string{"NEW_HOME": "/a", "OLD_HOME": "/a/"}, user: "/u", want: "/a", from: "NEW_HOME"},
		{name: "both, different homes", n: renamed, env: map[string]string{"NEW_HOME": "/a", "OLD_HOME": "/b"}, user: "/u", want: "/a", from: "NEW_HOME", warn: true},
		{name: "new variable empty", n: renamed, env: map[string]string{"NEW_HOME": "", "OLD_HOME": "/b"}, user: "/u", want: "/b", from: "OLD_HOME"},
		{name: "variable beats a directory", n: renamed, env: map[string]string{"OLD_HOME": "/b"}, user: "/u", dirs: []string{"/u/.new"}, want: "/b", from: "OLD_HOME"},
		{name: "relative variable", n: renamed, env: map[string]string{"NEW_HOME": "rel"}, user: "/u", want: filepath.Join(cwd, "rel"), from: "NEW_HOME"},
		{name: "variable, no user home", n: renamed, env: map[string]string{"OLD_HOME": "/b"}, want: "/b", from: "OLD_HOME"},
		{name: "both directories", n: renamed, user: "/u", dirs: []string{"/u/.new", "/u/.old"}, want: "/u/.new"},
		{name: "old directory only", n: renamed, user: "/u", dirs: []string{"/u/.old"}, want: "/u/.old"},
		{name: "new directory only", n: renamed, user: "/u", dirs: []string{"/u/.new"}, want: "/u/.new"},
		{name: "neither directory: a new home", n: renamed, user: "/u", want: "/u/.new"},
		{name: "one name each", n: testNames, user: "/u", dirs: []string{"/u/.old"}, want: "/u/.testhome"},
		{name: "no user home", n: renamed, errMatch: "set NEW_HOME"},
		{name: "variables only, none set", n: Names{HomeEnvs: []string{"NEW_HOME"}}, user: "/u", errMatch: "set NEW_HOME"},
		{name: "no names", n: Names{}, env: map[string]string{"NEW_HOME": "/a"}, user: "/u", errMatch: ErrNoNames.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			isDir := func(p string) bool {
				for _, d := range tc.dirs {
					if p == d {
						return true
					}
				}
				return false
			}
			r, err := ResolveHome(tc.n, getenv, tc.user, isDir)
			if tc.errMatch != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errMatch) {
					t.Fatalf("got %+v %v, want an error with %q", r, err, tc.errMatch)
				}
				return
			}
			if err != nil || r.Dir != tc.want || r.From != tc.from || (r.Warning != "") != tc.warn {
				t.Fatalf("got %+v %v, want dir %q from %q warning %v", r, err, tc.want, tc.from, tc.warn)
			}
			if tc.warn && (!strings.Contains(r.Warning, "NEW_HOME") || !strings.Contains(r.Warning, "OLD_HOME") || !strings.Contains(r.Warning, "using NEW_HOME=/a")) {
				t.Fatalf("warning %q", r.Warning)
			}
		})
	}
}

// The client dials the first socket that exists, else the first; the
// daemon listens on the first. No socket names: NoSocketName.
func TestSocketOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      Names
		exists []string
		want   string
	}{
		{"neither", renamed, nil, "/h/new.sock"},
		{"old only", renamed, []string{"/h/old.sock"}, "/h/old.sock"},
		{"both", renamed, []string{"/h/old.sock", "/h/new.sock"}, "/h/new.sock"},
		{"new only", renamed, []string{"/h/new.sock"}, "/h/new.sock"},
		{"no names", Names{}, []string{"/h/old.sock"}, "/h/" + NoSocketName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exists := func(p string) bool {
				for _, e := range tc.exists {
					if p == e {
						return true
					}
				}
				return false
			}
			if got := SocketIn(tc.n, "/h", exists); got != tc.want {
				t.Fatalf("dial %q, want %q", got, tc.want)
			}
			listen := "/h/" + NoSocketName
			if len(tc.n.Sockets) > 0 {
				listen = "/h/" + tc.n.Sockets[0]
			}
			if got := ListenSocketIn(tc.n, "/h"); got != listen {
				t.Fatalf("listen %q, want %q", got, listen)
			}
		})
	}
}

// HomeDir and SocketPath read the real environment and disk in that order,
// and warn once when two variables name different homes.
func TestHomeDirOnDisk(t *testing.T) {
	wire(t, renamed)
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("NEW_HOME", "")
	t.Setenv("OLD_HOME", "")
	if h, err := HomeDir(); err != nil || h != filepath.Join(user, ".new") {
		t.Fatalf("fresh: %q %v", h, err)
	}
	old := filepath.Join(user, ".old")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, ".new"), nil, 0o600); err != nil { // a file is not a home
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || c.Home != old {
		t.Fatalf("old home kept: %+v %v", c, err)
	}
	if c.SocketPath() != filepath.Join(old, "new.sock") || c.ListenSocketPath() != filepath.Join(old, "new.sock") {
		t.Fatalf("no socket yet: %q %q", c.SocketPath(), c.ListenSocketPath())
	}
	if err := os.WriteFile(filepath.Join(old, "old.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if c.SocketPath() != filepath.Join(old, "old.sock") || c.ListenSocketPath() != filepath.Join(old, "new.sock") {
		t.Fatalf("old socket: dial %q listen %q", c.SocketPath(), c.ListenSocketPath())
	}

	var buf bytes.Buffer
	oldWarn := WarnTo
	WarnTo, warnOnce = &buf, sync.Once{}
	t.Cleanup(func() { WarnTo = oldWarn })
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("NEW_HOME", a)
	t.Setenv("OLD_HOME", b)
	for range 2 {
		if h, err := HomeDir(); err != nil || h != a {
			t.Fatalf("both set: %q %v", h, err)
		}
	}
	if got := buf.String(); strings.Count(got, "warning:") != 1 || !strings.Contains(got, "NEW_HOME and OLD_HOME") {
		t.Fatalf("warning %q", got)
	}
}

// Kit names no home of its own: a program that never called Use cannot
// load one from the environment.
func TestNoNames(t *testing.T) {
	wire(t, Names{})
	t.Setenv(testHomeEnv, t.TempDir())
	if _, err := Load(); !errors.Is(err, ErrNoNames) {
		t.Fatalf("Load without names: %v", err)
	}
	if HomeEnv() != "" {
		t.Fatalf("HomeEnv %q", HomeEnv())
	}
}
