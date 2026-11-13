package forkguard_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/internal/forkguard"
	"github.com/sengirunners/potluck/internal/workflowlint"
)

var own = []string{"self-hosted", "macos", "pool:c4m16"}

const workflow = `# Build the app.
name: app
on:
  push:
    branches: [main]
  pull_request:
permissions:
  contents: read
jobs:
  # The Mac build.
  native:
    name: Native build
    runs-on: macos-15   # Xcode 26
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v4
      - run: xcodebuild   # build
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: true
`

func TestApplyEditsOneValueInPlace(t *testing.T) {
	c, err := forkguard.Apply([]byte(workflow), "native", own)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := forkguard.Expression("macos-15", own)
	if !c.Changed || c.Line != 13 || c.Before != "macos-15" || c.After != want {
		t.Fatalf("%+v", c)
	}
	if got := string(c.Text); got != strings.Replace(workflow, "runs-on: macos-15   #", "runs-on: "+want+"   #", 1) {
		t.Fatalf("more than the one value changed:\n%s", got)
	}
	if p := workflowlint.Lint(string(c.Text)); len(p) > 0 {
		t.Fatalf("the generated workflow fails the lint: %v", p)
	}
	again, err := forkguard.Apply(c.Text, "native", own)
	if err != nil || again.Changed || string(again.Text) != string(c.Text) {
		t.Fatalf("second run changed something: %+v %v", again, err)
	}
	// The unguarded file fails the lint: the lint is what makes the change needed.
	if p := workflowlint.Lint(strings.Replace(workflow, "macos-15", "pool:c4m16", 1)); len(p) == 0 {
		t.Fatal("an unguarded self-hosted runs-on passed the lint")
	}
}

func TestApplyKeepsEachSpelling(t *testing.T) {
	want, _ := forkguard.Expression("macos-15", own)
	for _, tt := range []struct{ name, from, to string }{
		{"single-quoted", "runs-on: 'macos-15'", "runs-on: " + want},
		{"double-quoted", `runs-on: "macos-15"`, "runs-on: " + want},
		{"one-label flow list", "runs-on: [macos-15]", "runs-on: " + want},
		{"quoted key", `"runs-on": macos-15`, `"runs-on": ` + want},
		{"no comment", "runs-on: macos-15", "runs-on: " + want},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := strings.Replace(workflow, "runs-on: macos-15   # Xcode 26", tt.from, 1)
			c, err := forkguard.Apply([]byte(src), "native", own)
			if err != nil {
				t.Fatal(err)
			}
			if string(c.Text) != strings.Replace(src, tt.from, tt.to, 1) {
				t.Fatalf("got:\n%s", c.Text)
			}
		})
	}
	t.Run("CRLF", func(t *testing.T) {
		src := strings.ReplaceAll(workflow, "\n", "\r\n")
		c, err := forkguard.Apply([]byte(src), "native", own)
		if err != nil {
			t.Fatal(err)
		}
		if string(c.Text) != strings.Replace(src, "runs-on: macos-15 ", "runs-on: "+want+" ", 1) {
			t.Fatalf("line endings changed:\n%q", c.Text)
		}
		if p := workflowlint.Lint(string(c.Text)); len(p) > 0 {
			t.Fatal(p)
		}
	})
}

func TestApplyRefuses(t *testing.T) {
	guarded, _ := forkguard.Expression("macos-15", []string{"self-hosted", "macos", "other"})
	sub := func(from, to string) string { return strings.Replace(workflow, from, to, 1) }
	for _, tt := range []struct{ name, src, job, want string }{
		{"missing job", workflow, "nope", "no such job"},
		{"matrix", sub("macos-15   #", "${{ matrix.os }}   #"), "native", "matrix"},
		{"other expression", sub("macos-15   #", "${{ vars.RUNS_ON }}   #"), "native", "expression"},
		{"already guarded with other labels", sub("macos-15   #", guarded+"   #"), "native", "already fork-guarded"},
		{"routed guard", sub("macos-15   #", "${{ "+forkguard.Guard+" && 'macos-15' || needs.route.outputs.label || 'x' }}   #"), "native", "already fork-guarded"},
		{"reusable workflow", sub("    runs-on: macos-15   # Xcode 26\n", "    uses: o/r/.github/workflows/w.yml@v1\n"), "native", "reusable workflow"},
		{"no runs-on", sub("    runs-on: macos-15   # Xcode 26\n", ""), "native", "no runs-on"},
		{"runner group", sub("runs-on: macos-15   # Xcode 26", "runs-on:\n      group: macs\n      labels: [macos-15]"), "native", "runner group"},
		{"flow mapping", sub("runs-on: macos-15   # Xcode 26", "runs-on: {group: macs}"), "native", "runner group"},
		{"block list", sub("runs-on: macos-15   # Xcode 26", "runs-on:\n      - macos-15"), "native", "list of labels"},
		{"several labels", sub("runs-on: macos-15", "runs-on: [macos-15, arm64]"), "native", "list of labels"},
		{"anchor", sub("runs-on: macos-15", "runs-on: &mac macos-15"), "native", "anchor"},
		{"alias", sub("runs-on: macos-15", "runs-on: &mac macos-15") + "  other:\n    runs-on: *mac\n    steps:\n      - run: true\n", "other", "anchor or alias"},
		{"merge key", sub("  native:\n", "  native:\n    <<: {timeout-minutes: 5}\n"), "native", "merge key"},
		{"flow-style job", sub("  lint:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n", "  lint: {runs-on: ubuntu-latest, steps: [{run: 'true'}]}\n"), "lint", "block mapping"},
		{"block scalar", sub("runs-on: macos-15   # Xcode 26", "runs-on: >-\n      macos-15"), "native", "block scalar"},
		{"plain scalar over two lines", sub("runs-on: macos-15   # Xcode 26", "runs-on: macos-15\n      continued"), "native", "not a GitHub-hosted"},
		{"self-hosted today", sub("macos-15", "my-mac"), "native", "not a GitHub-hosted label"},
		{"hash inside the value", sub("macos-15   #", "macos-15#x   #"), "native", "not a GitHub-hosted label"},
		{"issue_comment trigger", sub("  pull_request:\n", "  pull_request:\n  issue_comment:\n"), "native", "issue_comment"},
		{"several documents", workflow + "---\nname: two\n", "native", "more than one YAML document"},
		{"bare CR", sub("  push:\n", "  push:\r  pull_request_target:\n"), "native", "carriage return"},
		{"U+2028", sub("  push:\n", "  push: "), "native", "line separator"},
		{"duplicate runs-on", sub("    timeout-minutes: 30\n", "    timeout-minutes: 30\n    runs-on: ubuntu-latest\n"), "native", "written twice"},
		{"duplicate job", workflow + "  native:\n    runs-on: ubuntu-latest\n", "native", "written twice"},
		{"pull_request_review trigger", sub("  pull_request:\n", "  pull_request:\n  pull_request_review:\n"), "native", "pull_request_review"},
		{"value on the next line", sub("runs-on: macos-15   # Xcode 26", "runs-on:\n      macos-15"), "native", "on one line"},
		{"complex key", sub("    runs-on: macos-15   # Xcode 26\n", "    ? runs-on\n    : macos-15\n"), "native", "on one line"},
		{"tag", sub("runs-on: macos-15", "runs-on: !!str macos-15"), "native", "tag"},
		{"true key with issue_comment", strings.Replace(sub("on:\n  push:\n    branches: [main]\n  pull_request:\n", "true:\n  issue_comment:\n"), "", "", 1), "native", "issue_comment"},
		{"On with issue_comment", sub("on:\n  push:\n    branches: [main]\n  pull_request:\n", "On: [issue_comment]\n"), "native", "issue_comment"},
		{"ON beside on", sub("permissions:\n", "ON: [issue_comment]\npermissions:\n"), "native", "written twice"},
		{"no on", sub("on:\n  push:\n    branches: [main]\n  pull_request:\n", ""), "native", "no `on` key"},
		{"Runs-On beside runs-on", sub("    timeout-minutes: 30\n", "    timeout-minutes: 30\n    Runs-On: ubuntu-latest\n"), "native", "written twice"},
		{"bad label", workflow, "native", "label"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			labels := own
			if tt.name == "bad label" {
				labels = []string{"self-hosted", "mac os"}
			}
			c, err := forkguard.Apply([]byte(tt.src), tt.job, labels)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want a refusal containing %q, got %v (%+v)", tt.want, err, c)
			}
			if tt.name == "missing job" && !errors.Is(err, forkguard.ErrNoJob) {
				t.Fatalf("missing job is not ErrNoJob: %v", err)
			}
		})
	}
}

func TestParse(t *testing.T) {
	e, err := forkguard.Expression("ubuntu-latest", own)
	if err != nil {
		t.Fatal(err)
	}
	g, ok := forkguard.Parse(e)
	if !ok || g.Fork != "ubuntu-latest" || !slices.Equal(g.Own, own) || !g.JSON || g.Routed != "" {
		t.Fatalf("%q read as %+v %v", e, g, ok)
	}
	g, ok = forkguard.Parse("${{ " + forkguard.Guard + " && 'ubuntu-latest' || needs.route.outputs.label || 'sengi' }}")
	if !ok || g.Routed != "needs.route.outputs.label" || !slices.Equal(g.Own, []string{"sengi"}) {
		t.Fatalf("routed form read as %+v %v", g, ok)
	}
	for _, bad := range []string{
		"macos-15",
		"${{ fromJSON(" + forkguard.Guard + " && '[\"my-mac\"]' || '[\"self-hosted\"]') }}",
		"${{ fromJSON(" + forkguard.Guard + " && '[\"macos-15\"]' || '[]') }}",
		"${{ fromJSON(" + forkguard.Guard + " && '[\"macos-15\"]' || '[\"\"]') }}",
		"${{ " + forkguard.Guard + " && 'macos-15' || '' }}",
		"${{ !" + forkguard.Guard + " && 'macos-15' || 'x' }}",
	} {
		if g, ok := forkguard.Parse(bad); ok {
			t.Errorf("%q read as guarded: %+v", bad, g)
		}
	}
	if _, err := forkguard.Expression("my-mac", own); err == nil {
		t.Error("a self-hosted fork label was accepted")
	}
}

func TestHolds(t *testing.T) {
	for _, tt := range []struct {
		events           []string
		reachable, holds bool
	}{
		{[]string{"push", "workflow_dispatch"}, false, true},
		{[]string{"push", "pull_request"}, true, true},
		{[]string{"pull_request_review"}, true, true},
		{[]string{"pull_request", "issue_comment"}, true, false},
		{[]string{"merge_group"}, true, false},
		{[]string{"workflow_call"}, true, false},
	} {
		if forkguard.Reachable(tt.events) != tt.reachable || forkguard.Holds(tt.events) != tt.holds {
			t.Errorf("%v: reachable %v holds %v", tt.events, forkguard.Reachable(tt.events), forkguard.Holds(tt.events))
		}
	}
}

// GitHub's keys are matched without regard to case (Key), so another spelling of on, jobs and runs-on is read, and
// the edit goes through as it does for the usual one.
func TestApplyReadsOtherSpellings(t *testing.T) {
	src := strings.NewReplacer("on:\n", "On:\n", "jobs:\n", "Jobs:\n", "runs-on: macos-15", "Runs-On: macos-15").Replace(workflow)
	c, err := forkguard.Apply([]byte(src), "native", own)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := forkguard.Expression("macos-15", own)
	if !c.Changed || c.After != want || string(c.Text) != strings.Replace(src, "Runs-On: macos-15", "Runs-On: "+want, 1) {
		t.Fatalf("%+v\n%s", c, c.Text)
	}
	if p := workflowlint.Lint(string(c.Text)); len(p) > 0 {
		t.Fatalf("the lint refuses the edited file: %v", p)
	}
}

func TestMacJobs(t *testing.T) {
	guarded, _ := forkguard.Expression("macos-26", own)
	src := "on: push\njobs:\n" +
		"  native:\n    runs-on: macos-15\n" +
		"  listed:\n    runs-on: [macos-latest]\n" +
		"  done:\n    runs-on: " + guarded + "\n" +
		"  web:\n    runs-on: ubuntu-latest\n" +
		"  lent:\n    runs-on: [self-hosted, macos]\n" +
		"  custom:\n    runs-on: macos-big\n" +
		"  none:\n    steps: []\n" +
		"  matrix:\n    runs-on: ${{ matrix.os }}\n"
	if got := forkguard.MacJobs([]byte(src)); !slices.Equal(got, []string{"native", "listed", "done"}) {
		t.Errorf("MacJobs = %v", got)
	}
	for _, bad := range []string{"", "jobs: [", "on: push\n", "jobs:\n  a:\n    runs-on: macos-15\n---\njobs: {}\n"} {
		if got := forkguard.MacJobs([]byte(bad)); got != nil {
			t.Errorf("MacJobs(%q) = %v", bad, got)
		}
	}
}
