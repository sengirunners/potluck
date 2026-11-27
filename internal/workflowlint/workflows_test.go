package workflowlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/internal/forkguard"
)

func TestRepoWorkflowsAreForkSafe(t *testing.T) {
	files, err := filepath.Glob("../../.github/workflows/*.y*ml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found (%v)", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range Lint(string(b)) {
			t.Errorf("%s: %s", filepath.Base(f), p)
		}
	}
}

// j is potluck ci pr's form (forkguard.Expression).
var j = must(forkguard.Expression("macos-15", []string{"self-hosted", "macos", "pool:c4m16"}))

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

const g = "${{ " + forkguard.Guard + " && 'ubuntu-latest' || 'sengi-standard-4-ubuntu-2404' }}"

func wf(on, perms, runsOn string) string {
	return "name: x\non:\n" + on + perms + "jobs:\n  a:\n    runs-on: " + runsOn + "\n    steps:\n      - run: true\n"
}

const ro = "permissions:\n  contents: read\n"

func TestLintCatchesEachRule(t *testing.T) {
	pr := "  pull_request:\n"
	cases := []struct {
		name, src, want string // want: substring of a problem; empty means must pass
	}{
		{"guarded self-hosted on a PR", wf(pr, ro, g), ""},
		{"hosted on a PR", wf(pr, ro, "ubuntu-latest"), ""},
		{"push and dispatch may be self-hosted", wf("  push:\n  workflow_dispatch:\n", ro, "sengi-standard-4-ubuntu-2404"), ""},
		{"inline list with a PR, guarded", "name: x\non: [push, pull_request]\n" + ro + "jobs:\n  a:\n    runs-on: " + g + "\n", ""},
		{"empty permissions", wf("  push:\n", "permissions: {}\n", "ubuntu-latest"), ""},

		{"unconditional sengi on a PR", wf(pr, ro, "sengi-standard-4-ubuntu-2404"), "not a GitHub-hosted label"},
		{"self-hosted on a PR", wf(pr, ro, "self-hosted"), "not a GitHub-hosted label"},
		{"vars label on a PR", wf(pr, ro, "${{ vars.CI_RUNNER }}"), "not a GitHub-hosted label"},
		{"self-hosted named like ubuntu", wf(pr, ro, "ubuntu-24.04-sengi"), "not a GitHub-hosted label"},
		{"guard with the labels swapped", wf(pr, ro, "${{ "+forkguard.Guard+" && 'sengi-standard-4-ubuntu-2404' || 'ubuntu-latest' }}"), "not a GitHub-hosted label"},
		{"wrong guard (equality)", wf(pr, ro, "${{ github.event.pull_request.head.repo.full_name == github.repository && 'ubuntu-latest' || 'sengi' }}"), "not a GitHub-hosted label"},
		{"flow list runs-on", wf(pr, ro, "[self-hosted, linux]"), "not a GitHub-hosted label"},
		{"other fork-reachable trigger", wf("  issue_comment:\n", ro, "sengi-standard-4-ubuntu-2404"), "not a GitHub-hosted label"},
		{"reusable workflow from a PR", "name: x\non:\n  pull_request:\n" + ro + "jobs:\n  a:\n    uses: o/r/.github/workflows/w.yml@main\n", "reusable workflow"},
		{"second job unguarded", wf(pr, ro, g) + "  b:\n    runs-on: sengi-standard-2-ubuntu-2404\n", "job b"},

		{"pull_request_target", wf("  pull_request_target:\n", ro, "ubuntu-latest"), "pull_request_target"},
		{"pull_request_target inline", "name: x\non: pull_request_target\n" + ro + "jobs:\n  a:\n    runs-on: ubuntu-latest\n", "pull_request_target"},

		{"pull_request_target in other letters", wf("  Pull_Request_Target:\n", ro, "ubuntu-latest"), "pull_request_target"},

		{"no permissions", wf(pr, "", g), "no top-level permissions"},
		{"write at top level", wf("  workflow_dispatch:\n", "permissions:\n  contents: write\n", "ubuntu-latest"), "want read or none"},
		{"write-all", wf("  workflow_dispatch:\n", "permissions: write-all\n", "ubuntu-latest"), "write-all"},
		{"write on a job a fork can reach", wf(pr, ro, g) + "    permissions:\n      contents: write\n", "want read or none"},

		{"guard on issue_comment", wf("  issue_comment:\n", ro, g), "not a GitHub-hosted label"},
		{"guard on workflow_run", wf("  workflow_run:\n", ro, g), "not a GitHub-hosted label"},
		{"guard on merge_group", wf("  merge_group:\n", ro, g), "not a GitHub-hosted label"},
		{"guard with a PR and a merge_group", "name: x\non: [pull_request, merge_group]\n" + ro + "jobs:\n  a:\n    runs-on: " + g + "\n", "not a GitHub-hosted label"},
		{"hosted label on merge_group", wf("  merge_group:\n", ro, "ubuntu-latest"), ""},
		{"guard on pull_request_review", wf("  pull_request_review:\n", ro, g), ""},
		{"trailing text after the guard", wf(pr, ro, g[:len(g)-3]+" }}${{ 'sengi' }}"), "not a GitHub-hosted label"},
		{"unquoted fallback", wf(pr, ro, "${{ "+forkguard.Guard+" && 'ubuntu-latest' || vars.CI }}"), "not a GitHub-hosted label"},
		{"guard, routed label, literal fallback", wf(pr, ro, "${{ "+forkguard.Guard+" && 'ubuntu-latest' || needs.route.outputs.label || 'sengi-standard-4-ubuntu-2404' }}"), ""},
		{"routed label without a literal fallback", wf(pr, ro, "${{ "+forkguard.Guard+" && 'ubuntu-latest' || needs.route.outputs.label }}"), "not a GitHub-hosted label"},
		{"vars before the literal fallback", wf(pr, ro, "${{ "+forkguard.Guard+" && 'ubuntu-latest' || vars.CI || 'sengi' }}"), "not a GitHub-hosted label"},
		{"routed label with an operator", wf(pr, ro, "${{ "+forkguard.Guard+" && 'ubuntu-latest' || needs.route.outputs.label && 'x' || 'sengi' }}"), "not a GitHub-hosted label"},
		{"routed label unguarded", wf(pr, ro, "${{ needs.route.outputs.label || 'sengi' }}"), "not a GitHub-hosted label"},
		{"routed label on issue_comment", wf("  issue_comment:\n", ro, "${{ "+forkguard.Guard+" && 'ubuntu-latest' || needs.route.outputs.label || 'sengi' }}"), "not a GitHub-hosted label"},
		{"fromJSON array form", wf(pr, ro, j), ""},
		{"fromJSON array form, single-quoted YAML", wf(pr, ro, "'"+strings.ReplaceAll(j, "'", "''")+"'"), ""},
		{"fromJSON with the guard in parentheses", wf(pr, ro, "${{ fromJSON(("+forkguard.Guard+") && '[\"macos-15\"]' || '[\"self-hosted\",\"macos\",\"pool:c4m16\"]') }}"), ""},
		{"fromJSON on issue_comment", wf("  issue_comment:\n", ro, j), "not a GitHub-hosted label"},
		{"fromJSON with the arrays swapped", wf(pr, ro, "${{ fromJSON("+forkguard.Guard+" && '[\"self-hosted\",\"macos\"]' || '[\"macos-15\"]') }}"), "not a GitHub-hosted label"},
		{"fromJSON fork side with a self-hosted label too", wf(pr, ro, "${{ fromJSON("+forkguard.Guard+" && '[\"macos-15\",\"self-hosted\"]' || '[\"self-hosted\"]') }}"), "not a GitHub-hosted label"},
		{"fromJSON of a variable", wf(pr, ro, "${{ fromJSON(vars.RUNS_ON) }}"), "not a GitHub-hosted label"},
		{"fromJSON with the wrong guard", wf(pr, ro, "${{ fromJSON(github.event.pull_request && '[\"macos-15\"]' || '[\"self-hosted\"]') }}"), "not a GitHub-hosted label"},
		{"fromJSON own side not an array", wf(pr, ro, "${{ fromJSON("+forkguard.Guard+" && '[\"macos-15\"]' || 'self-hosted') }}"), "not a GitHub-hosted label"},
		{"fromJSON with trailing text", wf(pr, ro, j+"${{ 'x' }}"), "not a GitHub-hosted label"},
		{"bare CR hides a trigger", "name: x\non:\n  push:\r  pull_request:\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "line separator"},
		{"U+2028", "name: x\non:\n  push:\u2028  pull_request:\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "line separator"},
		{"U+0085", "name: x\non:\n  push:\u0085  pull_request:\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "line separator"},
		{"CRLF file passes", strings.ReplaceAll(wf(pr, ro, g), "\n", "\r\n"), ""},
		{"quoted write on a PR job", wf(pr, ro, g) + "    permissions:\n      contents: 'write'\n", "want read or none"},
		{"flow map on a PR job", wf(pr, ro, g) + "    permissions: { contents: write }\n", "not read"},
		{"quoted write-all on a PR job", wf(pr, ro, g) + "    permissions: \"write-all\"\n", "write-all"},
		{"quoted keys write", wf(pr, ro, g) + "    permissions:\n      'contents': \"write\"\n", "want read or none"},
		{"flow map on a dispatch job", wf("  workflow_dispatch:\n", ro, "x") + "    permissions: { contents: write }\n", "not read"},
		{"job write on a dispatch workflow is fine", wf("  workflow_dispatch:\n", ro, "x") + "    permissions:\n      contents: write\n", ""},
		{"empty top-level permissions", wf(pr, "permissions:\n", g), "permissions is empty"},
		{"quoted read", wf(pr, "permissions:\n  contents: 'read'\n", g), ""},
		{"leading document marker", "---\n" + wf(pr, ro, g), ""},
		{"with: mode: write is not a permission", wf(pr, ro, g) + "      - uses: x/y@v1\n        with:\n          mode: write\n", ""},
		{"flow-map on", "name: x\non: {pull_request: {}}\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "flow-map"},
		{"On with issue_comment", "name: x\nOn: [issue_comment]\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "not a GitHub-hosted label"},
		{"Jobs and Runs-On", "name: x\non: [pull_request]\n" + ro + "Jobs:\n  a:\n    Runs-On: sengi\n", "not a GitHub-hosted label"},
		{"ON beside on", "name: x\non: [push]\nON: [issue_comment]\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "duplicate key"},
		{"true beside on", "name: x\non: [push]\ntrue: [issue_comment]\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "duplicate key"},
		{"a colon in a plain scalar", wf("  push:\n", ro, "ubuntu-latest") + "      - run: echo (out: fine)\n", "not YAML"},
		{"no on", "name: x\n" + ro + "jobs:\n  a:\n    runs-on: sengi\n", "no `on` key"},
		{"tab indentation", "name: x\non:\n\tpull_request:\n" + ro + "jobs:\n", "tab"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Lint(c.src)
			if c.want == "" {
				if len(got) > 0 {
					t.Fatalf("want pass, got %v", got)
				}
				return
			}
			for _, p := range got {
				if strings.Contains(p, c.want) {
					return
				}
			}
			t.Fatalf("want a problem containing %q, got %v", c.want, got)
		})
	}
}
