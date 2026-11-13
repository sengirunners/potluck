package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const commit = "1eacf23e722f2df10a113480c8496e29607ba005"

func tarball(t *testing.T, prefix string, entries map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755})
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/" + name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	for _, h := range extra {
		_ = tw.WriteHeader(h)
	}
	tw.Close()
	gz.Close()
	return b.Bytes()
}

func signed(t *testing.T, files map[string][]byte) (body []byte, sig string, pub string, priv ed25519.PrivateKey) {
	t.Helper()
	p, k, _ := ed25519.GenerateKey(nil)
	var sums []string
	for name, data := range files {
		s := sha256.Sum256(data)
		sums = append(sums, hex.EncodeToString(s[:])+"  "+name)
	}
	m, err := FromSums("main-1eacf23e722f", commit, time.Now(), []byte(strings.Join(sums, "\n")+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	body, sig, err = Sign(m, k)
	if err != nil {
		t.Fatal(err)
	}
	return body, sig, FormatKey(p), k
}

func TestSignVerifyAndTamper(t *testing.T) {
	name := Tarball("main-1eacf23e722f", "linux", "amd64")
	tb := tarball(t, strings.TrimSuffix(name, ".tar.gz"), map[string]string{"zm": "bin"})
	body, sig, pub, _ := signed(t, map[string][]byte{name: tb})
	m, err := Verify(body, sig, []string{pub})
	if err != nil || m.Commit != commit {
		t.Fatalf("verify: %+v %v", m, err)
	}
	if err := m.CheckFile(name, tb); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckFile(name, append(tb, 0)); err == nil {
		t.Fatal("a changed tarball passed")
	}
	if err := m.CheckFile(Tarball("main-1eacf23e722f", "plan9", "386"), tb); err == nil {
		t.Fatal("a file the manifest does not name passed")
	}
	other, _, _ := ed25519.GenerateKey(nil)
	for what, c := range map[string]struct {
		body []byte
		sig  string
		keys []string
	}{
		"changed body":  {bytes.Replace(body, []byte(commit), []byte(strings.Repeat("a", 40)), 1), sig, []string{pub}},
		"other key":     {body, sig, []string{FormatKey(other)}},
		"no keys":       {body, sig, nil},
		"bad signature": {body, "AAAA", []string{pub}},
		"bad key":       {body, sig, []string{"ssh-ed25519 AAAA"}},
	} {
		if _, err := Verify(c.body, c.sig, c.keys); err == nil {
			t.Errorf("%s verified", what)
		}
	}
	// A rotated key list verifies with either key.
	if _, err := Verify(body, sig, []string{FormatKey(other), pub}); err != nil {
		t.Fatalf("second key: %v", err)
	}
}

func TestManifestShapeChecks(t *testing.T) {
	_, k, _ := ed25519.GenerateKey(nil)
	good := Manifest{Schema: Schema, Version: "main-abc", Commit: commit, Created: time.Now(), Files: map[string]string{"zm_main-abc_linux_amd64.tar.gz": strings.Repeat("0", 64)}}
	if _, _, err := Sign(good, k); err != nil {
		t.Fatal(err)
	}
	for what, mut := range map[string]func(*Manifest){
		"schema":       func(m *Manifest) { m.Schema = "zm-release/2" },
		"short commit": func(m *Manifest) { m.Commit = "1eacf23" },
		"version":      func(m *Manifest) { m.Version = "../x" },
		"no time":      func(m *Manifest) { m.Created = time.Time{} },
		"path in name": func(m *Manifest) {
			m.Files = map[string]string{"../zm_main-abc_linux_amd64.tar.gz": strings.Repeat("0", 64)}
		},
		"other version": func(m *Manifest) {
			m.Files = map[string]string{"zm_main-zzz_linux_amd64.tar.gz": strings.Repeat("0", 64)}
		},
		"bad sum":  func(m *Manifest) { m.Files = map[string]string{"zm_main-abc_linux_amd64.tar.gz": "xyz"} },
		"no files": func(m *Manifest) { m.Files = nil },
	} {
		m := good
		mut(&m)
		if _, _, err := Sign(m, k); err == nil {
			t.Errorf("%s: signed a bad manifest", what)
		}
	}
}

func TestExtractTakesOnlyProgramsUnderItsPrefix(t *testing.T) {
	prefix := "zm_v1_linux_amd64"
	dir := t.TempDir()
	got, err := Extract(tarball(t, prefix, map[string]string{"zm": "a", "bd-zm": "b", "zm-work-conformance": "c", "README.md": "d"}), "v1", "linux", "amd64", dir)
	if err != nil || len(got) != 3 {
		t.Fatalf("extract: %v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err == nil {
		t.Fatal("docs were installed")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "zm")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("zm mode: %v", fi)
	}
	for what, tb := range map[string][]byte{
		"other prefix": tarball(t, "zm_v2_linux_amd64", map[string]string{"zm": "a"}),
		"escape":       tarball(t, prefix, map[string]string{"zm": "a"}, &tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/../evil", Size: 0}),
		"nested":       tarball(t, prefix, map[string]string{"zm": "a"}, &tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/x/zm", Size: 0}),
		"symlink":      tarball(t, prefix, map[string]string{"zm": "a"}, &tar.Header{Typeflag: tar.TypeSymlink, Name: prefix + "/bd-zm", Linkname: "/bin/sh"}),
		"no zm":        tarball(t, prefix, map[string]string{"bd-zm": "a"}),
	} {
		if _, err := Extract(tb, "v1", "linux", "amd64", t.TempDir()); err == nil {
			t.Errorf("%s: extracted", what)
		}
	}
}

func TestStateKeepsWhatTheRolloutReads(t *testing.T) {
	home := t.TempDir()
	if _, ok := ReadState(home); ok {
		t.Fatal("state in a new home")
	}
	// A failed first install is kept, though nothing was installed.
	var st State
	st.MarkFailed(Failure{Version: "main-aaaaaaaaaaaa", At: time.Now().UTC(), Result: "rolled_back"})
	if err := WriteState(home, st); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadState(home)
	if _, failed := got.FailedHere("main-aaaaaaaaaaaa"); ok || !failed {
		t.Fatalf("installed=%v failed=%v", ok, failed)
	}
	m := Manifest{Version: "main-1eacf23e722f", Commit: commit, Created: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	inst := time.Now().Add(-Soak - time.Minute)
	st = got.Installing(m, inst)
	if err := WriteState(home, st); err != nil {
		t.Fatal(err)
	}
	got, ok = ReadState(home)
	if !ok || got.Commit != commit || !got.Created.Equal(m.Created) || got.Installed.IsZero() || len(got.Failed) != 1 {
		t.Fatalf("state: %+v %v", got, ok)
	}
	// Stable: the release runs, Soak since the install, no restart since.
	if !got.StableNow(m.Version, commit, inst.Add(-time.Second), time.Now()) {
		t.Fatal("not stable after the soak")
	}
	if got.StableNow(m.Version, commit, inst.Add(time.Second), time.Now()) || got.StableNow(m.Version, commit, inst.Add(-time.Second), inst.Add(Soak/2)) ||
		got.StableNow(m.Version, "080d8123aaaa", inst.Add(-time.Second), time.Now()) ||
		got.StableNow("v0.0.0-20261112000000-1eacf23e722f+dirty", commit[:12], inst.Add(-time.Second), time.Now()) {
		t.Fatal("stable after a restart, before the soak, for another build, or for a source build at its commit")
	}
	for i := 0; i < keep+5; i++ {
		got.MarkStable(fmt.Sprintf("main-%012d", i))
	}
	if len(got.Stable) != keep || got.Stable[0] != fmt.Sprintf("main-%012d", keep+4) || got.MarkStable(got.Stable[3]) {
		t.Fatalf("stable list %v", got.Stable)
	}
	got.MarkFailed(Failure{Version: "main-aaaaaaaaaaaa", Result: "failed"})
	if f, _ := got.FailedHere("main-aaaaaaaaaaaa"); len(got.Failed) != 1 || f.Result != "failed" {
		t.Fatalf("failed list %+v", got.Failed)
	}
	if !SameCommit(commit, commit[:7]) || SameCommit(commit, commit[:6]) || SameCommit("", "") || SameCommit(commit, "080d8123") || SameCommit("unknown", "unknown") {
		t.Fatal("SameCommit")
	}
}

func TestHoldSwitch(t *testing.T) {
	p := HoldPath(t.TempDir())
	if _, held, err := ReadHold(p); held || err != nil {
		t.Fatalf("held with no file: %v", err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil { // touch still holds
		t.Fatal(err)
	}
	if h, held, err := ReadHold(p); !held || err != nil || h.Reason != "" || h.Since.IsZero() {
		t.Fatalf("touched: %+v %v %v", h, held, err)
	}
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := WriteHold(p, Hold{Reason: "investigating host-c", Since: since}); err != nil {
		t.Fatal(err)
	}
	if h, held, err := ReadHold(p); !held || err != nil || h.Reason != "investigating host-c" || !h.Since.Equal(since) {
		t.Fatalf("written: %+v %v %v", h, held, err)
	}
}

// A state file that exists but does not parse (an empty one after a power
// loss included) is an error for the updater, never a fresh home: that
// would forget the failed installs and retry them.
func TestLoadStateRefusesCorrupt(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	if _, ok, err := LoadState(home); ok || err != nil {
		t.Fatalf("missing state: ok=%v err=%v, want a fresh home", ok, err)
	}
	for _, body := range []string{"", `{"version":"main-1`} {
		if err := os.WriteFile(StatePath(home), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadState(home); err == nil {
			t.Fatalf("state %q: no error", body)
		}
	}
	st := State{Version: "main-2", Commit: "abcdef1", Created: time.Now()}
	st.MarkFailed(Failure{Version: "main-3", Result: "rolled_back"})
	if err := WriteState(home, st); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadState(home)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if _, failed := got.FailedHere("main-3"); !failed {
		t.Fatal("failed install lost across a rewrite")
	}
}

// A release may carry the programs under their new names (potluck,
// potluck-driver-*, potluck-conformance, potluck-e2b): an updater of this
// build installs them beside the old ones. It still needs zm.
func TestExtractTakesNewProgramNames(t *testing.T) {
	prefix := "zm_v1_linux_amd64"
	dir := t.TempDir()
	files := map[string]string{"zm": "a", "zm-run-tart": "b", "potluck": "c", "potluck-driver-tart": "d",
		"potluck-conformance": "e", "potluck-e2b": "f", "potluck.md": "g", "potluckx": "h"}
	got, err := Extract(tarball(t, prefix, files), "v1", "linux", "amd64", dir)
	if err != nil || len(got) != 6 {
		t.Fatalf("extract: %v %v", got, err)
	}
	for _, n := range []string{"potluck", "potluck-driver-tart", "potluck-conformance", "potluck-e2b", "zm-run-tart"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s not installed: %v", n, err)
		}
	}
	if _, err := Extract(tarball(t, prefix, map[string]string{"potluck": "a"}), "v1", "linux", "amd64", t.TempDir()); err == nil {
		t.Fatal("a release with no zm extracted")
	}
}

// Runs is the one comparison of a running build with the installed release:
// a build made from source at the release's commit (vcs.revision, 12 hex
// digits; a modified tree's "+dirty" version) is not the release, and a
// restart would not run it.
func TestRuns(t *testing.T) {
	const full = "1eacf23e722f2df10a113480c8496e29607ba005"
	st := State{Version: "main-1eacf23e722f", Commit: full, Created: time.Now()}
	for _, tt := range []struct {
		installed       bool
		version, commit string
		want            Running
	}{
		{true, "v0.0.0-dev", "unknown", RunsDev},
		{true, "v0.0.0-20261112000000-1eacf23e722f", full[:12], RunsSource},
		{true, "v0.0.0-20261112000000-1eacf23e722f+dirty", full[:12], RunsSource},
		{true, "main-1eacf23e722f+dirty", full, RunsSource},
		{false, "main-1eacf23e722f", full, RunsUninstalled},
		{true, "main-1eacf23e722f", full, RunsInstalled},
		{true, "main-0000000older", "0000000000000000000000000000000000000000", RunsOther},
		{true, "main-1eacf23e722f-rebuilt", full, RunsOther},
	} {
		if got := st.Runs(tt.installed, tt.version, tt.commit); got != tt.want {
			t.Errorf("Runs(%v, %q, %q) = %d, want %d", tt.installed, tt.version, tt.commit, got, tt.want)
		}
		m := Manifest{Version: st.Version, Commit: st.Commit}
		if got := m.RunBy(tt.version, tt.commit); got != (tt.want == RunsInstalled || tt.want == RunsUninstalled && tt.version == st.Version) {
			t.Errorf("RunBy(%q, %q) = %v", tt.version, tt.commit, got)
		}
	}
}
