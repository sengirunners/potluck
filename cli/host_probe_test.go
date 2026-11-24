package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/menubar"
)

// host status --probe on a Mac with nothing on it: an empty home, no
// daemon. It answers the Mac's checks first, never starts the daemon and
// writes nothing into the home; plain status does not read the Mac.
func TestHostStatusProbeOnAnEmptyHome(t *testing.T) {
	e := newJobEnv(t, "darwin")
	oldGOOS, oldParent, oldProbe := setupGOOS, menuParent, probeMac
	t.Cleanup(func() { setupGOOS, menuParent, probeMac = oldGOOS, oldParent, oldProbe })
	setupGOOS, menuParent = "darwin", func() string { return "" }
	probed := 0
	probeMac = func(context.Context) hostprobe.MacParts {
		probed++
		return hostprobe.MacParts{
			Report:  hostprobe.Report{OS: "darwin", Arch: "arm64", MacOS: "26.1", TartDir: filepath.Join(e.home, ".tart"), TartFreeGB: 200},
			HV:      true,
			Tart:    hostprobe.Layer{Name: "tart", Problems: []string{"tart is not installed"}},
			Softnet: hostprobe.Layer{Name: "tart", SoftnetPath: "/opt/homebrew/bin/softnet", SoftnetVersion: "0.25.0"},
			NeedGB:  60,
		}
	}
	status := func(args ...string) menubar.Setup {
		t.Helper()
		var o, er bytes.Buffer
		if code := Run(context.Background(), append([]string{"--json", "host", "status"}, args...), strings.NewReader(""), &o, &er); code != 0 {
			t.Fatalf("host status %v: exit %d\n%s", args, code, er.String())
		}
		var st menubar.Status
		if err := json.Unmarshal(o.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st.Setup
	}
	ids := func(s menubar.Setup) []string {
		var out []string
		for _, c := range s.Checks {
			out = append(out, c.ID)
		}
		return out
	}

	s := status("--probe")
	if got := strings.Join(ids(s), " "); !strings.HasPrefix(got, "mac potluck tart softnet disk daemon setup menu updates") {
		t.Errorf("checks: %s", got)
	}
	if s.NextID != "tart" || s.Checks[2].Do == nil || s.Checks[2].Do.URL != hostprobe.TartReleases {
		t.Errorf("next %q (%s), tart %+v", s.Next, s.NextID, s.Checks[2])
	}
	if probed != 1 {
		t.Errorf("probed %d times", probed)
	}
	if left, _ := os.ReadDir(e.home); len(left) != 0 {
		t.Errorf("host status --probe wrote into the home: %v", left)
	}

	// The app carries a release and this program is not the installed one:
	// installing it comes first.
	install, rel := t.TempDir(), filepath.Join(t.TempDir(), "release")
	t.Setenv("POTLUCK_INSTALL_DIR", install)
	install, _ = filepath.EvalSymlinks(install) // the install directory as upgrade compares it
	t.Setenv(EnvBundledRelease, rel)
	s = status("--probe")
	p := s.Checks[1]
	if p.ID != "potluck" || p.State != menubar.Todo || s.NextID != "potluck" || p.Do == nil ||
		strings.Join(p.Do.Args, " ") != "upgrade --release "+rel+" --dir "+install || !strings.HasPrefix(s.Next, "potluck upgrade --release ") {
		t.Errorf("bundled: next %q (%s), %+v %+v", s.Next, s.NextID, p, p.Do)
	}

	// Plain status, as the menu asks every 15 s: no Mac checks, nothing run.
	s = status()
	if got := ids(s); got[0] != "potluck" || strings.Contains(strings.Join(got, " "), "mac") {
		t.Errorf("plain status: %v", got)
	}
	if probed != 2 {
		t.Errorf("plain status probed the Mac (%d)", probed)
	}
}
