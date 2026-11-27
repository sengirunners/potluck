package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/release"
)

// -menubar puts the app's zip into the darwin tarballs and only those, and
// the release's own unpacking still installs only the programs.
func TestMenubarZipIsInDarwinTarballsOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two tarballs")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	zip := filepath.Join(t.TempDir(), "app.zip")
	if err := os.WriteFile(zip, []byte("PK fake zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	const version = "main-aaaaaaaaaaaa"
	if err := run(version, "unknown", out, []string{"darwin/arm64", "linux/amd64"}, zip, ""); err != nil {
		t.Fatal(err)
	}
	for goos, want := range map[string]bool{"darwin": true, "linux": false} {
		arch := map[string]string{"darwin": "arm64", "linux": "amd64"}[goos]
		name := release.Potluck.Tarball(version, goos, arch)
		names := tarNames(t, filepath.Join(out, name))
		prefix := name[:len(name)-len(".tar.gz")] + "/"
		if got := names[prefix+MenubarZip]; got != want {
			t.Errorf("%s holds %s: %v, want %v", name, MenubarZip, got, want)
		}
		if !names[prefix+"potluck"] {
			t.Errorf("%s holds no potluck", name)
		}
	}
	tb, err := os.ReadFile(filepath.Join(out, release.Potluck.Tarball(version, "darwin", "arm64")))
	if err != nil {
		t.Fatal(err)
	}
	progs, err := release.Potluck.Extract(tb, version, "darwin", "arm64", t.TempDir())
	if err != nil {
		t.Fatalf("a tarball with the app zip does not extract: %v", err)
	}
	for _, p := range progs {
		if p == MenubarZip {
			t.Errorf("Extract installed %s as a program", p)
		}
	}
	if err := run(version, "unknown", out, []string{"darwin/arm64"}, filepath.Join(out, "missing.zip"), ""); err == nil {
		t.Error("a -menubar zip that does not exist built a release")
	}
}

func tarNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
	}
}

// install.sh --menubar puts the app from the release into the
// chosen directory; without the flag nothing but the programs is installed.
func TestInstallMenubar(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the menu bar item is for macOS")
	}
	if testing.Short() {
		t.Skip("builds a release")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	tmp := t.TempDir()
	app := filepath.Join(tmp, "src", "Potluck.app", "Contents")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Info.plist"), []byte(menuPlist("sh.potluck.menu")), 0o644); err != nil {
		t.Fatal(err)
	}
	zip := filepath.Join(tmp, "Potluck-Menu.zip")
	if out, err := exec.Command("ditto", "-c", "-k", "--keepParent", filepath.Join(tmp, "src", "Potluck.app"), zip).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %v\n%s", err, out)
	}
	rel := filepath.Join(tmp, "release")
	const version = "main-aaaaaaaaaaaa"
	if err := run(version, "unknown", rel, []string{runtime.GOOS + "/" + runtime.GOARCH}, zip, ""); err != nil {
		t.Fatal(err)
	}
	seed, pub, _ := release.NewKey()
	key, _ := release.ParseSeed(seed)
	if _, err := release.Potluck.SignDir(rel, version, strings.Repeat("a", 40), time.Now(), key); err != nil {
		t.Fatal(err)
	}
	// The throwaway key is trusted through config update_keys, as in the end-to-end test.
	cfg, _ := json.Marshal(map[string]any{"update_keys": []string{pub}})
	potluckHome := func(name string) string {
		h := filepath.Join(tmp, name, "potluck")
		if err := os.MkdirAll(h, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h, "config.json"), cfg, 0o600); err != nil {
			t.Fatal(err)
		}
		return h
	}
	install := func(name string, args ...string) (string, string, string) {
		bin, apps := filepath.Join(tmp, name, "bin"), filepath.Join(tmp, name, "apps")
		c := exec.Command("sh", append([]string{filepath.Join(root, "install.sh"), "--from", rel}, args...)...)
		c.Env = []string{"HOME=" + filepath.Join(tmp, name, "home"), "POTLUCK_INSTALL_DIR=" + bin, "POTLUCK_MENUBAR_DIR=" + apps,
			"POTLUCK_HOME=" + potluckHome(name), "POTLUCK_NO_AUTOSTART=1", "POTLUCK_UPDATE_KEY=" + pub, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("install.sh %v: %v\n%s", args, err, out)
		}
		return string(out), bin, apps
	}
	out, bin, apps := install("with", "--menubar")
	if _, err := os.Stat(filepath.Join(apps, "Potluck.app", "Contents", "Info.plist")); err != nil {
		t.Errorf("--menubar did not install the app: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not Developer ID signed") {
		t.Errorf("an ad hoc build is not called unsigned:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(bin, "potluck")); err != nil {
		t.Errorf("potluck itself is not installed: %v", err)
	}
	// Again over that install: the installed potluck checks the release, then --menubar replaces the app.
	if err := os.WriteFile(filepath.Join(apps, "Potluck.app", "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := install("with", "--menubar"); !strings.Contains(out, "with the potluck already installed") {
		t.Errorf("the second install did not go through the installed potluck:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(apps, "Potluck.app", "stale")); err == nil {
		t.Error("--menubar over an installed app left the old copy's files")
	}
	if _, err := os.Stat(filepath.Join(apps, "Potluck.app", "Contents", "Info.plist")); err != nil {
		t.Errorf("--menubar over an installed app: %v", err)
	}
	_, bin, apps = install("without")
	if _, err := os.Stat(apps); err == nil {
		t.Errorf("without --menubar the app directory %s exists", apps)
	}
	if _, err := os.Stat(filepath.Join(bin, MenubarZip)); err == nil {
		t.Errorf("without --menubar %s was installed as a program", MenubarZip)
	}
	// A release without the app refuses --menubar, after installing potluck.
	bare := filepath.Join(tmp, "bare")
	if err := run(version, "unknown", bare, []string{runtime.GOOS + "/" + runtime.GOARCH}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := release.Potluck.SignDir(bare, version, strings.Repeat("a", 40), time.Now(), key); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", filepath.Join(root, "install.sh"), "--from", bare, "--menubar")
	c.Env = []string{"HOME=" + filepath.Join(tmp, "h3"), "POTLUCK_INSTALL_DIR=" + filepath.Join(tmp, "b3"), "POTLUCK_MENUBAR_DIR=" + filepath.Join(tmp, "a3"),
		"POTLUCK_HOME=" + potluckHome("three"), "POTLUCK_NO_AUTOSTART=1", "POTLUCK_UPDATE_KEY=" + pub, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if out, err := c.CombinedOutput(); err == nil || !strings.Contains(string(out), "has no menu bar app") {
		t.Errorf("a release without the app and --menubar: %v\n%s", err, out)
	}
}

// menuPlist is an Info.plist with a bundle id.
func menuPlist(id string) string {
	return `<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + id + `</string></dict></plist>`
}

// install.sh --menubar takes the app under its name of now or the one it had
// before, leaves one app of Potluck's, moves the replaced one to the Trash, and
// replaces nothing that is not Potluck's.
func TestInstallMenubarAcrossTheRename(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the menu bar item is for macOS")
	}
	if testing.Short() {
		t.Skip("builds releases")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	tmp := t.TempDir()
	const oldName, newName, version = "Potluck Menu.app", "Potluck.app", "main-aaaaaaaaaaaa"
	mkApp := func(path, id, mark string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(path, "Contents"), 0o755); err != nil {
			t.Fatal(err)
		}
		for f, b := range map[string]string{"Info.plist": menuPlist(id), "mark": mark} {
			if err := os.WriteFile(filepath.Join(path, "Contents", f), []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed, pub, _ := release.NewKey()
	key, _ := release.ParseSeed(seed)
	rels := map[string]string{}
	for i, name := range []string{oldName, newName} {
		src := filepath.Join(tmp, "src", string(rune('a'+i)))
		mkApp(filepath.Join(src, name), "sh.potluck.menu", "released "+name)
		zip := filepath.Join(src, "app.zip")
		if out, err := exec.Command("ditto", "-c", "-k", "--keepParent", filepath.Join(src, name), zip).CombinedOutput(); err != nil {
			t.Fatalf("ditto: %v\n%s", err, out)
		}
		rels[name] = filepath.Join(src, "release")
		if err := run(version, "unknown", rels[name], []string{runtime.GOOS + "/" + runtime.GOARCH}, zip, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := release.Potluck.SignDir(rels[name], version, strings.Repeat("a", 40), time.Now(), key); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := json.Marshal(map[string]any{"update_keys": []string{pub}})
	list := func(dir string) (names []string) {
		es, _ := os.ReadDir(dir)
		for _, e := range es {
			names = append(names, e.Name())
		}
		return names
	}
	for _, c := range []struct {
		name, rel string
		have      map[string]string // the apps there before: name to bundle id
		want      []string          // the app directory after
		trash     int
		fails     bool
	}{
		{"fresh, new name", newName, nil, []string{newName}, 0, false},
		{"fresh, old name", oldName, nil, []string{oldName}, 0, false},
		{"rename", newName, map[string]string{oldName: "sh.potluck.menu"}, []string{newName}, 1, false},
		{"back", oldName, map[string]string{newName: "sh.potluck.menu"}, []string{oldName}, 1, false},
		{"both there", newName, map[string]string{oldName: "sh.potluck.menu", newName: "sh.potluck.menu"}, []string{newName}, 2, false},
		{"another program's Potluck.app", newName, map[string]string{newName: "com.example.recipes"}, []string{newName}, 0, true},
		{"another program's Potluck Menu.app", newName, map[string]string{oldName: "com.example.recipes"}, []string{oldName, newName}, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			home, apps, phome := filepath.Join(dir, "home"), filepath.Join(dir, "apps"), filepath.Join(dir, "potluck")
			for _, d := range []string{home, apps, phome} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(phome, "config.json"), cfg, 0o600); err != nil {
				t.Fatal(err)
			}
			for name, id := range c.have {
				mkApp(filepath.Join(apps, name), id, "before")
			}
			x := exec.Command("sh", filepath.Join(root, "install.sh"), "--from", rels[c.rel], "--menubar")
			x.Env = []string{"HOME=" + home, "POTLUCK_INSTALL_DIR=" + filepath.Join(dir, "bin"), "POTLUCK_MENUBAR_DIR=" + apps, "POTLUCK_HOME=" + phome,
				"POTLUCK_NO_AUTOSTART=1", "POTLUCK_UPDATE_KEY=" + pub, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
			out, err := x.CombinedOutput()
			if (err != nil) != c.fails {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			if got := list(apps); strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("apps %q, want %q\n%s", got, c.want, out)
			}
			mark, _ := os.ReadFile(filepath.Join(apps, c.rel, "Contents", "mark"))
			if want := "released " + c.rel; !c.fails && string(mark) != want || c.fails && string(mark) != "before" {
				t.Errorf("%s holds %q", c.rel, mark)
			}
			trash := list(filepath.Join(home, ".Trash"))
			if len(trash) != c.trash {
				t.Errorf("trash %q, want %d", trash, c.trash)
			}
			for _, n := range trash {
				if b, _ := os.ReadFile(filepath.Join(home, ".Trash", n, "Contents", "mark")); string(b) != "before" {
					t.Errorf("trash %s holds %q", n, b)
				}
			}
		})
	}
}
