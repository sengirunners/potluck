package runners_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/internal/forkguard"
	"github.com/sengirunners/potluck/internal/workflowlint"
	"github.com/sengirunners/potluck/runners"
	"gopkg.in/yaml.v3"
)

// The probe workflow is started by a push to its own branch only, holds no token permissions, runs on the
// labels given and passes the fork-safe lint.
func TestProbeWorkflow(t *testing.T) {
	labels := []string{"self-hosted", "macos", "pool:c4m16"}
	text, err := runners.ProbeWorkflow("potluck/probe-1", labels)
	if err != nil {
		t.Fatal(err)
	}
	if p := workflowlint.Lint(string(text)); len(p) > 0 {
		t.Fatalf("lint: %v\n%s", p, text)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(text, &doc); err != nil {
		t.Fatal(err)
	}
	if ev, err := forkguard.Triggers(doc.Content[0]); err != nil || !slices.Equal(ev, []string{"push"}) {
		t.Fatalf("triggers %v %v", ev, err)
	}
	var wf struct {
		On struct {
			Push struct{ Branches []string }
		} `yaml:"on"`
		Permissions map[string]string
		Jobs        map[string]struct {
			RunsOn  []string `yaml:"runs-on"`
			Timeout int      `yaml:"timeout-minutes"`
			Steps   []struct{ Run string }
		}
	}
	if err := yaml.Unmarshal(text, &wf); err != nil {
		t.Fatal(err)
	}
	j := wf.Jobs["probe"]
	if !slices.Equal(wf.On.Push.Branches, []string{"potluck/probe-1"}) || wf.Permissions == nil || len(wf.Permissions) != 0 ||
		len(wf.Jobs) != 1 || !slices.Equal(j.RunsOn, labels) || j.Timeout != runners.ProbeTimeoutMinutes {
		t.Fatalf("%+v\n%s", wf, text)
	}
	if len(j.Steps) != 1 || strings.Contains(j.Steps[0].Run, runners.ProbeMarker) {
		t.Fatalf("the step's own text holds the marker, so a log of the script would match it:\n%s", text)
	}
	for _, bad := range []struct {
		branch string
		labels []string
	}{{"potluck/probe-*", labels}, {"", labels}, {"b", nil}, {"b", []string{"self-hosted", "mac os"}}, {"b", []string{"x'y"}}} {
		if _, err := runners.ProbeWorkflow(bad.branch, bad.labels); err == nil {
			t.Errorf("%q %q accepted", bad.branch, bad.labels)
		}
	}
}

// The disk line comes from df -P: a df that wraps a long device name onto two lines (as GNU df does without -P)
// would leave awk the wrong line.
func TestProbeScriptReadsDiskWithPosixDF(t *testing.T) {
	text, err := runners.ProbeWorkflow("potluck/probe-1", []string{"self-hosted", "macos"})
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct{ Steps []struct{ Run string } }
	}
	if err := yaml.Unmarshal(text, &wf); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	df := "#!/bin/sh\ncase \"$1\" in\n-*P*) printf 'Filesystem Size Used Avail Capacity Mounted\\n/dev/disk3 100G 1G 99G 1%% /\\n' ;;\n*) printf 'Filesystem Size Used Avail Capacity Mounted\\n/dev/mapper/a-very-long-device-name\\n 100G 1G 99G 1%% /\\n' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "df"), []byte(df), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sw_vers", "xcodebuild"} { // the real ones may pop an installer dialog
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", wf.Jobs["probe"].Steps[0].Run)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "disk=99G free of 100G") {
		t.Fatalf("%v:\n%s", err, out)
	}
}
