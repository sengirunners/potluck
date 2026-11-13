package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/hostprobe"
)

// doctor on a machine where no layer is ready exits 1 and says so; its lines
// carry no trailing spaces (a layer with no version, CLI or runtime).
func TestDoctorNothingReady(t *testing.T) {
	old := detectHost
	t.Cleanup(func() { detectHost = old })
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", Arch: "arm64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100,
			Layers: []hostprobe.Layer{{Name: "tart", Problems: []string{"tart is not installed"}}, {Name: "oci"}}}
	}
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(EnvNoAutostart, "1")
	for _, args := range [][]string{{"doctor"}, {"doctor", "--json"}} {
		var o, e bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &o, &e)
		if code != 1 {
			t.Fatalf("%v: exit %d\nstdout:\n%s\nstderr:\n%s", args, code, o.String(), e.String())
		}
		if args[len(args)-1] == "--json" {
			if e.Len() != 0 || !strings.Contains(o.String(), `"layers"`) {
				t.Fatalf("doctor --json: stdout:\n%s\nstderr:\n%s", o.String(), e.String())
			}
			continue
		}
		if !strings.Contains(o.String(), "  tart   not ready\n") || !strings.Contains(e.String(), "cannot share sandboxes yet") {
			t.Fatalf("doctor: stdout:\n%s\nstderr:\n%s", o.String(), e.String())
		}
		for _, l := range strings.Split(o.String(), "\n") {
			if strings.TrimRight(l, " ") != l {
				t.Fatalf("trailing space: %q", l)
			}
		}
	}
}

// sandbox cp's argument errors say the sandbox comes first, with no daemon.
func TestSandboxCpArgs(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(EnvNoAutostart, "1")
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"sandbox", "cp", "./data", "mini-c2m4@mini/01J"}, 2},
		{[]string{"sandbox", "cp", "./data", "mini-c2m4@mini/01J", ":work/"}, 1},
		{[]string{"sandbox", "cp", "mini-c2m4@mini/01J", "./a", "./b"}, 1},
	} {
		var o, e bytes.Buffer
		if code := Run(context.Background(), tc.args, strings.NewReader(""), &o, &e); code != tc.code || !strings.Contains(e.String(), "takes the sandbox first") {
			t.Errorf("%v: exit %d\n%s", tc.args, code, e.String())
		}
	}
}

// doctor names the project.toml settings nothing in this build reads.
func TestDoctorListsUnownedProjectSettings(t *testing.T) {
	old := detectHost
	t.Cleanup(func() { detectHost = old })
	detectHost = func(context.Context, string) hostprobe.Report {
		return hostprobe.Report{OS: "darwin", Arch: "arm64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, Layers: []hostprobe.Layer{{Name: "oci"}}}
	}
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvNoAutostart, "1")
	dir := filepath.Join(home, "projects", "01JAAAAAAAAAAAAAAAAAAAAAAA")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := "id = \"01JAAAAAAAAAAAAAAAAAAAAAAA\"\nslug = \"orchid\"\nname = \"Orchid\"\nfuture_flag = true\n\n[[widgets]]\nname = \"w\"\n"
	if err := os.WriteFile(filepath.Join(dir, "project.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	var o, e bytes.Buffer
	Run(context.Background(), []string{"doctor"}, strings.NewReader(""), &o, &e)
	if !strings.Contains(o.String(), "project orchid: project.toml has settings nothing in this build reads") || !strings.Contains(o.String(), ": future_flag, widgets\n") {
		t.Fatalf("doctor:\n%s\n%s", o.String(), e.String())
	}
	o.Reset()
	Run(context.Background(), []string{"doctor", "--json"}, strings.NewReader(""), &o, &e)
	if !strings.Contains(o.String(), `"unowned_project_settings"`) || !strings.Contains(o.String(), `"future_flag"`) {
		t.Fatalf("doctor --json:\n%s", o.String())
	}
}
