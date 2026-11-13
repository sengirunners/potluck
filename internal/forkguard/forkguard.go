// Package forkguard is the one home of the fork-guarded runs-on rule: which runs-on values send a fork's pull
// request to a GitHub-hosted runner and the repository's own runs to self-hosted labels, which workflow triggers
// that guard holds on, and the edit that writes such a value into a workflow file (Apply). The fork-safe CI lint
// (internal/workflowlint), potluck ci doctor (runners) and potluck ci pr (cli) all ask here.
//
// A guarded value is one of:
//
//	${{ GUARD && '<hosted>' || '<label>' }}
//	${{ GUARD && '<hosted>' || needs.<job>.outputs.<name> || '<label>' }}
//	${{ fromJSON(GUARD && '["<hosted>"]' || '["<label>", ...]') }}
//
// GUARD is Guard, alone or in one pair of parentheses. GitHub's && binds tighter than ||, and both return an
// operand, not a boolean: when GUARD holds (a fork's pull request) the hosted label (or its one-element array) is
// the value and the rest is never read; when it does not, the own-run labels are. Anything else is not guarded.
package forkguard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Guard is true for a pull request whose head repository is not this one: a fork, or no head repository at all (a
// deleted fork), which counts as a fork. On push, schedule and dispatch events github.event.pull_request is null,
// so it is false and the own-run labels are taken.
const Guard = "github.event.pull_request && github.event.pull_request.head.repo.full_name != github.repository"

// The standard GitHub-hosted labels, exactly. A self-hosted runner may carry any other name, ubuntu-* included.
var hostedLabels = []string{
	"ubuntu-slim", "ubuntu-latest", "ubuntu-26.04", "ubuntu-24.04", "ubuntu-22.04",
	"ubuntu-26.04-arm", "ubuntu-24.04-arm", "ubuntu-22.04-arm",
	"windows-latest", "windows-2025", "windows-2022", "windows-11-arm",
	"macos-latest", "macos-26", "macos-15", "macos-26-intel", "macos-15-intel",
}

// Hosted reports whether label is one of GitHub's standard hosted runner labels. Larger (organisation-defined)
// hosted runners are not known here and count as not hosted.
func Hosted(label string) bool { return slices.Contains(hostedLabels, label) }

var (
	// Only people with write access (or the repository's own runs) can start these.
	trusted = map[string]bool{"push": true, "schedule": true, "workflow_dispatch": true}
	// The triggers whose payload carries github.event.pull_request: the only ones on which Guard can be true.
	prEvents = map[string]bool{"pull_request": true, "pull_request_review": true, "pull_request_review_comment": true}
)

// Reachable reports whether someone without write access can start a workflow with these triggers: any trigger
// but push, schedule and workflow_dispatch.
func Reachable(events []string) bool {
	return slices.ContainsFunc(events, func(e string) bool { return !trusted[e] })
}

// Holds reports whether Guard protects a workflow with these triggers: every trigger an outsider can start is one
// whose payload has github.event.pull_request. On any other (issue_comment, workflow_run, merge_group, ...) the
// guard is always false and an outsider's run takes the own-run labels.
func Holds(events []string) bool {
	return !slices.ContainsFunc(events, func(e string) bool { return !trusted[e] && !prEvents[e] })
}

// RunsOn is a guarded runs-on value, read.
type RunsOn struct {
	Fork   string   // the hosted label a fork's pull request runs on
	Own    []string // the labels the repository's own runs ask for; with Routed, its one fallback
	Routed string   // a needs.<job>.outputs.<name> taken before Own when set; then Own is only known at run time
	JSON   bool     // the fromJSON array form
}

var (
	guardAlt = `(?:` + regexp.QuoteMeta(Guard) + `|\(\s*` + regexp.QuoteMeta(Guard) + `\s*\))`
	stringRe = regexp.MustCompile(`^\$\{\{\s*` + guardAlt + `\s*&&\s*'([^']*)'\s*\|\|\s*` +
		`(?:(needs\.[A-Za-z_][\w-]*\.outputs\.[A-Za-z_][\w-]*)\s*\|\|\s*)?'([^']*)'\s*\}\}$`)
	jsonRe  = regexp.MustCompile(`^\$\{\{\s*(?i:fromjson)\(\s*` + guardAlt + `\s*&&\s*'([^']*)'\s*\|\|\s*'([^']*)'\s*\)\s*\}\}$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// Parse reads value, a runs-on scalar as YAML decodes it (quotes removed), as a guarded runs-on. ok is false for
// every other value, including a guard whose fork side is not a GitHub-hosted label.
func Parse(value string) (RunsOn, bool) {
	value = strings.TrimSpace(value)
	if m := stringRe.FindStringSubmatch(value); m != nil {
		if !Hosted(m[1]) || m[3] == "" {
			return RunsOn{}, false
		}
		return RunsOn{Fork: m[1], Own: []string{m[3]}, Routed: m[2]}, true
	}
	if m := jsonRe.FindStringSubmatch(value); m != nil {
		var fork, own []string
		if json.Unmarshal([]byte(m[1]), &fork) != nil || len(fork) != 1 || !Hosted(fork[0]) {
			return RunsOn{}, false
		}
		if json.Unmarshal([]byte(m[2]), &own) != nil || len(own) == 0 || slices.Contains(own, "") {
			return RunsOn{}, false
		}
		return RunsOn{Fork: fork[0], Own: own, JSON: true}, true
	}
	return RunsOn{}, false
}

// ValidLabel reports whether l is a runner label this package writes into a workflow: letters, digits, '.', '_',
// '-' and ':', starting with a letter or digit, so it needs no quoting in YAML or in an expression.
func ValidLabel(l string) bool { return labelRe.MatchString(l) }

// Expression is the guarded runs-on value Apply writes: the fromJSON array form, so the own runs can ask for
// several labels (the pool's label and an OS) while a fork's pull request keeps the one hosted label fork.
func Expression(fork string, own []string) (string, error) {
	if !Hosted(fork) {
		return "", fmt.Errorf("%q is not a GitHub-hosted runner label", fork)
	}
	if len(own) == 0 {
		return "", fmt.Errorf("no labels for the repository's own runs")
	}
	for _, l := range own {
		if !ValidLabel(l) {
			return "", fmt.Errorf("label %q: use letters, digits, '.', '_', '-' and ':'", l)
		}
	}
	f, _ := json.Marshal([]string{fork})
	o, _ := json.Marshal(own)
	return "${{ fromJSON(" + Guard + " && '" + string(f) + "' || '" + string(o) + "') }}", nil
}
