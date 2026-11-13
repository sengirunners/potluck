package runners

// The probe of potluck ci doctor --probe (docs/ci-doctor.md): a generated
// one-job workflow that a throwaway branch carries alone, so the push starts
// it and nothing else. Its job reports the machine that took it.

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/sengirunners/potluck/internal/forkguard"
	"gopkg.in/yaml.v3"
)

// ProbeWorkflowPath is where the probe's workflow sits on its branch.
const ProbeWorkflowPath = ".github/workflows/potluck-probe.yml"

// ProbeMarker starts each line the probe job prints about its machine. The job's
// script never holds it whole, so a log's copy of the script never matches it.
const ProbeMarker = "potluck-probe: "

// ProbeTimeoutMinutes is the probe job's timeout-minutes: what it does takes seconds.
const ProbeTimeoutMinutes = 10

// probeScript prints the OS, the macOS version, Xcode and free disk, each as one marked line. df -P keeps a
// long device name from wrapping its line, which would leave awk the wrong one.
const probeScript = `say() { printf '%s-probe: %s\n' potluck "$1"; }
say "os=$(uname -s) $(uname -r) $(uname -m)"
if command -v sw_vers >/dev/null 2>&1; then say "macos=$(sw_vers -productVersion)"; fi
if command -v xcodebuild >/dev/null 2>&1; then say "xcode=$(xcodebuild -version 2>/dev/null | head -n 1)"; else say "xcode=none"; fi
say "disk=$(df -Ph "${GITHUB_WORKSPACE:-.}" | awk 'NR==2 {print $4 " free of " $2}')"
`

// ProbeWorkflow is the probe's workflow file: it runs on a push to branch
// only, with no token permissions and no checkout, on a runner with labels.
func ProbeWorkflow(branch string, labels []string) ([]byte, error) {
	if branch == "" || strings.ContainsAny(branch, "*?[]!+ \t\n'\"\\") {
		return nil, fmt.Errorf("probe branch %q: use a plain branch name", branch)
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("no labels for the probe job")
	}
	for _, l := range labels {
		if !forkguard.ValidLabel(l) {
			return nil, fmt.Errorf("label %q: use letters, digits, '.', '_', '-' and ':'", l)
		}
	}
	type step struct {
		Name  string `yaml:"name"`
		Shell string `yaml:"shell"`
		Run   string `yaml:"run"`
	}
	type job struct {
		RunsOn  []string `yaml:"runs-on,flow"`
		Timeout int      `yaml:"timeout-minutes"`
		Steps   []step   `yaml:"steps"`
	}
	type push struct {
		Branches []string `yaml:"branches,flow"`
	}
	type workflow struct {
		Name        string            `yaml:"name"`
		On          map[string]push   `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]job    `yaml:"jobs"`
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2) // the fork-safe lint (internal/workflowlint) reads two-space blocks
	err := enc.Encode(workflow{
		Name:        "potluck probe",
		On:          map[string]push{"push": {Branches: []string{branch}}},
		Permissions: map[string]string{},
		Jobs: map[string]job{"probe": {RunsOn: labels, Timeout: ProbeTimeoutMinutes, Steps: []step{{
			Name: "Report the machine that took this job", Shell: "sh", Run: probeScript,
		}}}},
	})
	if err == nil {
		err = enc.Close()
	}
	return b.Bytes(), err
}
