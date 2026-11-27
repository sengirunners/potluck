package cli

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
)

// A Mac half set up, its image half downloaded, then uninstalled: the plan
// changes nothing, and the removal takes the unfinished download with the
// home.
func TestUninstallTakesACutOffDownload(t *testing.T) {
	l := newLender(t)
	tart := filepath.Join(l.home, "tart")
	home := filepath.Join(l.home, "potluck-home")
	const image = "reg.example/mac@sha256:1"
	sum := md5.Sum([]byte(image))
	part := filepath.Join(tart, hostprobe.TartPullHome, "tmp", hex.EncodeToString(sum[:]))
	for _, d := range []string{home, part} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, s := range map[string]string{filepath.Join(home, "program"): "potluck\n", filepath.Join(part, "disk.img"): "half"} {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := hostprobe.StartTartShare(tart, home, []string{image}, time.Now()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHome, home)
	if out, code := l.uninstall("", "--dry-run"); code != 0 || !l.exists(part) || !strings.Contains(out, "delete the downloads a setup left unfinished") {
		t.Fatalf("plan: exit %d, download there %v\n%s", code, l.exists(part), out)
	}
	if out, code := l.uninstall("", "--yes"); code != 0 || l.exists(part) || l.exists(home) {
		t.Fatalf("exit %d, download there %v, home there %v\n%s", code, l.exists(part), l.exists(home), out)
	}
}

// One that cannot be deleted is said to stay, never as done, and the rest
// goes on.
func TestUninstallSaysADownloadItCouldNotDelete(t *testing.T) {
	l := newLender(t)
	home := filepath.Join(l.home, "potluck-home")
	tart := filepath.Join(l.home, "tart")
	part := filepath.Join(tart, hostprobe.TartPullHome, "tmp", "x")
	for _, d := range []string{home, part} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, s := range map[string]string{filepath.Join(home, "program"): "potluck\n", filepath.Join(tart, hostprobe.TartPullsFile): "{"} {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvHome, home)
	out, code := l.uninstall("", "--json", "--yes")
	if code != 0 || !l.exists(part) || l.exists(home) {
		t.Fatalf("exit %d, download there %v, home there %v\n%s", code, l.exists(part), l.exists(home), out)
	}
	if strings.Contains(out, "Deleted any download") || !strings.Contains(out, "A download a setup left unfinished, in ") {
		t.Errorf("%s", out)
	}
}
