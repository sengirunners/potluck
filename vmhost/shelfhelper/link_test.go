package shelfhelper_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/vmhost/shelfhelper"
)

// linkStep runs potluck_shelf_link with helper (or none) as the shelf's,
// and answers its output and status line.
func linkStep(t *testing.T, helper string) (string, time.Duration) {
	t.Helper()
	shelf := t.TempDir()
	if helper != "" {
		p := filepath.Join(shelf, "bin", shelfhelper.Name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(helper), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	out, err := exec.Command("sh", "-c", shelfhelper.LinkSh+`potluck_shelf_link "$1" /some/dir; echo "rc=$?"`, "sh", shelf).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out)), time.Since(start)
}

// The link step runs the shelf's helper with link DIR, Go naming off and
// stdin closed, and returns its status; with no helper it does nothing.
func TestLinkStep(t *testing.T) {
	if out, _ := linkStep(t, ""); out != "rc=0" {
		t.Errorf("no helper: %q", out)
	}
	out, _ := linkStep(t, "#!/bin/sh\necho \"$* go=$POTLUCK_DEPS_GO\"; cat; exit 3\n")
	if out != "link /some/dir go=off\nrc=3" {
		t.Errorf("helper: %q", out)
	}
}

// A helper that never ends is killed at LinkS seconds, with no timeout(1).
func TestLinkStepKillsAHungHelper(t *testing.T) {
	if testing.Short() {
		t.Skip("waits LinkS seconds")
	}
	out, took := linkStep(t, "#!/bin/sh\nexec sleep 300\n")
	if out == "rc=0" || !strings.HasPrefix(out, "rc=") || took > 30*time.Second {
		t.Errorf("hung helper: %q after %s", out, took)
	}
}
