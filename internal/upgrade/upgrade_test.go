package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/release"
)

// The list every build carries must parse: a malformed or empty one would
// make that build refuse every upgrade. The release workflow runs this on
// the commit it releases, before it builds.
func TestReleaseKeys(t *testing.T) {
	if _, err := ReleaseKeys(); err != nil {
		t.Fatal(err)
	}
}

// install.sh checks a first install's signature with its own copy of the
// list: it must be release_keys.txt's.
func TestInstallScriptCarriesTheReleaseKeys(t *testing.T) {
	b, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^RELEASE_KEYS="([^"]*)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("install.sh sets no RELEASE_KEYS")
	}
	want, err := ReleaseKeys()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(m[1])); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("install.sh RELEASE_KEYS = %q, internal/upgrade/release_keys.txt has %q", got, want)
	}
}

// withReleaseKeys makes the built-in list text for one test.
func withReleaseKeys(t *testing.T, text string) {
	t.Helper()
	old := releaseKeysFile
	releaseKeysFile = text
	t.Cleanup(func() { releaseKeysFile = old })
}

func TestKeys(t *testing.T) {
	_, a, _ := release.NewKey()
	_, b, _ := release.NewKey()
	withReleaseKeys(t, "# the channel\n"+a+"\n\n")
	got, err := Keys([]string{"", b, " ", a, " " + b})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != a+","+b {
		t.Fatalf("Keys = %v, want [%s %s]", got, a, b)
	}
}

// A list that is empty, malformed or repeats a key refuses: no upgrade
// trusts part of a list. So does a malformed config update_keys entry, even
// after a good key.
func TestKeysRefuseABadList(t *testing.T) {
	_, a, _ := release.NewKey()
	for name, text := range map[string]string{
		"empty":         "",
		"comments only": "# nothing here\n\n",
		"not a key":     a + "\nssh-ed25519 AAAA\n",
		"short key":     "ed25519:AAAA\n",
		"repeated":      a + "\n" + a + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			withReleaseKeys(t, text)
			if keys, err := Keys(nil); err == nil || !strings.Contains(err.Error(), "release_keys.txt") {
				t.Fatalf("Keys = %v, %v; want a refusal naming release_keys.txt", keys, err)
			}
		})
	}
	withReleaseKeys(t, a+"\n")
	if keys, err := Keys([]string{a, "ed25519:not-base64!"}); err == nil || !strings.Contains(err.Error(), "update_keys") {
		t.Fatalf("Keys = %v, %v; want a refusal naming update_keys", keys, err)
	}
}

// During a rotation a build trusts the current key and the next: a release
// signed by either installs, and one signed by any other key is refused.
func TestRotationTrustsBothKeys(t *testing.T) {
	oldSeed, oldPub, _ := release.NewKey()
	newSeed, newPub, _ := release.NewKey()
	otherSeed, _, _ := release.NewKey()
	withReleaseKeys(t, oldPub+"\n"+newPub+"\n")
	keys, err := Keys(nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, seed := range map[string]string{"current key": oldSeed, "next key": newSeed} {
		dir := t.TempDir()
		writeRelease(t, dir, "main-1eacf23e722f", time.Now(), seed)
		if _, err := Stage(Dir(dir), keys, "linux", "amd64", t.TempDir()); err != nil {
			t.Errorf("a release signed with the %s: %v", name, err)
		}
	}
	dir := t.TempDir()
	writeRelease(t, dir, "main-1eacf23e722f", time.Now(), otherSeed)
	if _, err := Stage(Dir(dir), keys, "linux", "amd64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "refusing the release") {
		t.Fatalf("a release signed with a third key: %v, want a refusal", err)
	}
}

const commit = "1eacf23e722f2df10a113480c8496e29607ba005"

// release writes one signed release for linux/amd64 into dir.
func writeRelease(t *testing.T, dir, version string, created time.Time, seed string) {
	writeReleaseOf(t, dir, version, "", created, seed)
}

// writeReleaseOf is writeRelease with extra in each program's body.
func writeReleaseOf(t *testing.T, dir, version, extra string, created time.Time, seed string) {
	t.Helper()
	prefix := strings.TrimSuffix(release.Potluck.Tarball(version, "linux", "amd64"), ".tar.gz")
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755})
	for _, n := range []string{"potluck", "potluck-driver-local"} {
		body := n + " " + version + extra
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/" + n, Mode: 0o755, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	name := prefix + ".tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b.Bytes())
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	key, err := release.ParseSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := release.Potluck.SignDir(dir, version, commit, created, key); err != nil {
		t.Fatal(err)
	}
}

func TestStageFromDir(t *testing.T) {
	seed, pub, _ := release.NewKey()
	dir := t.TempDir()
	writeRelease(t, dir, "main-1eacf23e722f", time.Now(), seed)

	if _, err := Stage(Dir(dir), nil, "linux", "amd64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no trusted release key") {
		t.Fatalf("staged with no keys: %v", err)
	}
	st, err := Stage(Dir(dir), []string{pub}, "linux", "amd64", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Manifest.Version != "main-1eacf23e722f" || strings.Join(st.Names, ",") != "potluck,potluck-driver-local" {
		t.Fatalf("staged %+v", st)
	}
	if _, err := Stage(Dir(dir), []string{pub}, "darwin", "arm64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no build for darwin/arm64") {
		t.Fatalf("staged a platform the release lacks: %v", err)
	}

	// Signed by another key: refused, nothing written.
	_, other, _ := release.NewKey()
	stage := t.TempDir()
	if _, err := Stage(Dir(dir), []string{other}, "linux", "amd64", stage); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("a manifest signed by an untrusted key staged: %v", err)
	}
	if ents, _ := os.ReadDir(stage); len(ents) != 0 {
		t.Fatalf("a refused release wrote %d files", len(ents))
	}

	// A tampered tarball (a well-formed one with other programs, and
	// SHA256SUMS rewritten to match, as a download host could): the signed
	// manifest refuses it.
	evil := t.TempDir()
	writeReleaseOf(t, evil, "main-1eacf23e722f", " evil", time.Now(), seed)
	name := release.Potluck.Tarball("main-1eacf23e722f", "linux", "amd64")
	for _, f := range []string{name, "SHA256SUMS"} {
		b, _ := os.ReadFile(filepath.Join(evil, f))
		_ = os.WriteFile(filepath.Join(dir, f), b, 0o644)
	}
	if _, err := Stage(Dir(dir), []string{pub}, "linux", "amd64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "does not match the signed manifest") {
		t.Fatalf("a tampered tarball staged: %v", err)
	}
}

func TestOlderRefusesARollback(t *testing.T) {
	now := time.Now().UTC()
	m := release.Manifest{Version: "main-old", Created: now.Add(-time.Hour)}
	st := release.State{Version: "main-new", Created: now}
	if Older(m, st, true) == "" {
		t.Fatal("an older release may replace a newer one")
	}
	if Older(m, st, false) != "" || Older(release.Manifest{Version: "main-new", Created: now}, st, true) != "" ||
		Older(release.Manifest{Version: "main-newer", Created: now.Add(time.Hour)}, st, true) != "" {
		t.Fatal("refused a release that may be installed")
	}
}

func TestGitHubSource(t *testing.T) {
	seed, pub, _ := release.NewKey()
	dir := t.TempDir()
	writeRelease(t, dir, "main-1eacf23e722f", time.Now(), seed)
	var auth []string
	private := true
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if private && r.Header.Get("Authorization") != "Bearer tok" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/repos/sengirunners/potluck/releases" {
			asset := func(n string) map[string]string { return map[string]string{"name": n, "url": srv.URL + "/assets/" + n} }
			files := []map[string]string{asset("manifest.json"), asset("manifest.json.sig"), asset(release.Potluck.Tarball("main-1eacf23e722f", "linux", "amd64"))}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"tag_name": "potluck-main-ffffffffffff", "draft": true, "assets": files},
				{"tag_name": "potluck-main-eeeeeeeeeeee", "prerelease": true, "assets": files},
				{"tag_name": "zm-main-dddddddddddd", "assets": files},
				{"tag_name": "potluck-main-cccccccccccc", "assets": []any{}},
				{"tag_name": "potluck-main-1eacf23e722f", "assets": files},
			})
			return
		}
		if n, ok := strings.CutPrefix(r.URL.Path, "/assets/"); ok && r.Header.Get("Accept") == "application/octet-stream" {
			http.ServeFile(w, r, filepath.Join(dir, n))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	g := NewGitHub(func(string) string { return "" })
	g.API = srv.URL
	if _, err := Stage(g, []string{pub}, "linux", "amd64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "answered 404 with no token") {
		t.Fatalf("a private repository without a token: %v", err)
	}
	g = NewGitHub(func(k string) string { return map[string]string{"GITHUB_TOKEN": "tok"}[k] })
	g.API = srv.URL
	st, err := Stage(g, []string{pub}, "linux", "amd64", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Manifest.Version != "main-1eacf23e722f" || g.String() != "github.com/sengirunners/potluck release potluck-main-1eacf23e722f" {
		t.Fatalf("staged %s from %s", st.Manifest.Version, g)
	}
	g = NewGitHub(func(k string) string { return map[string]string{"GH_TOKEN": "bad", "GITHUB_TOKEN": "tok"}[k] })
	g.API = srv.URL
	if _, err := Stage(g, []string{pub}, "linux", "amd64", t.TempDir()); err == nil || !strings.Contains(err.Error(), "cannot read the repository") {
		t.Fatalf("GH_TOKEN first: %v", err)
	}
	if auth[len(auth)-1] != "Bearer bad" {
		t.Fatalf("sent %q", auth[len(auth)-1])
	}
}
