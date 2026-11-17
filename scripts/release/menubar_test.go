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
	if err := run(version, "unknown", out, []string{"darwin/arm64", "linux/amd64"}, zip); err != nil {
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
	if err := run(version, "unknown", out, []string{"darwin/arm64"}, filepath.Join(out, "missing.zip")); err == nil {
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

// install.sh --menubar puts Potluck Menu.app from the release into the
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
	app := filepath.Join(tmp, "src", "Potluck Menu.app", "Contents")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "Info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	zip := filepath.Join(tmp, "Potluck-Menu.zip")
	if out, err := exec.Command("ditto", "-c", "-k", "--keepParent", filepath.Join(tmp, "src", "Potluck Menu.app"), zip).CombinedOutput(); err != nil {
		t.Fatalf("ditto: %v\n%s", err, out)
	}
	rel := filepath.Join(tmp, "release")
	const version = "main-aaaaaaaaaaaa"
	if err := run(version, "unknown", rel, []string{runtime.GOOS + "/" + runtime.GOARCH}, zip); err != nil {
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
		c := exec.Command("sh", append([]string{filepath.Join(root, "install.sh"), "--from", rel, "--trust-dir"}, args...)...)
		c.Env = []string{"HOME=" + filepath.Join(tmp, name, "home"), "POTLUCK_INSTALL_DIR=" + bin, "POTLUCK_MENUBAR_DIR=" + apps,
			"POTLUCK_HOME=" + potluckHome(name), "POTLUCK_NO_AUTOSTART=1", "POTLUCK_UPDATE_KEY=" + pub, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("install.sh %v: %v\n%s", args, err, out)
		}
		return string(out), bin, apps
	}
	out, bin, apps := install("with", "--menubar")
	if _, err := os.Stat(filepath.Join(apps, "Potluck Menu.app", "Contents", "Info.plist")); err != nil {
		t.Errorf("--menubar did not install the app: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not Developer ID signed") {
		t.Errorf("an ad hoc build is not called unsigned:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(bin, "potluck")); err != nil {
		t.Errorf("potluck itself is not installed: %v", err)
	}
	// Again over that install: the installed potluck checks the release, then --menubar replaces the app.
	if err := os.WriteFile(filepath.Join(apps, "Potluck Menu.app", "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := install("with", "--menubar"); !strings.Contains(out, "with the potluck already installed") {
		t.Errorf("the second install did not go through the installed potluck:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(apps, "Potluck Menu.app", "stale")); err == nil {
		t.Error("--menubar over an installed app left the old copy's files")
	}
	if _, err := os.Stat(filepath.Join(apps, "Potluck Menu.app", "Contents", "Info.plist")); err != nil {
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
	if err := run(version, "unknown", bare, []string{runtime.GOOS + "/" + runtime.GOARCH}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := release.Potluck.SignDir(bare, version, strings.Repeat("a", 40), time.Now(), key); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", filepath.Join(root, "install.sh"), "--from", bare, "--trust-dir", "--menubar")
	c.Env = []string{"HOME=" + filepath.Join(tmp, "h3"), "POTLUCK_INSTALL_DIR=" + filepath.Join(tmp, "b3"), "POTLUCK_MENUBAR_DIR=" + filepath.Join(tmp, "a3"),
		"POTLUCK_HOME=" + potluckHome("three"), "POTLUCK_NO_AUTOSTART=1", "POTLUCK_UPDATE_KEY=" + pub, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if out, err := c.CombinedOutput(); err == nil || !strings.Contains(string(out), "has no Potluck Menu.app") {
		t.Errorf("a release without the app and --menubar: %v\n%s", err, out)
	}
}
