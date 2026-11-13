package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The agent image is built FROM the default (so it keeps the runner and
// every tool a box needs) and pins its CLIs by version, never latest.
func TestAgentImageDockerfile(t *testing.T) {
	b, err := os.ReadFile("images/agent/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	d := string(b)
	for _, want := range []string{"ARG BASE=", "FROM ${BASE}", "USER runner", "test -x /home/runner/run.sh",
		"@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}", "@openai/codex@${CODEX_VERSION}"} {
		if !strings.Contains(d, want) {
			t.Errorf("the agent Dockerfile lacks %q", want)
		}
	}
	for _, v := range []string{"CLAUDE_CODE_VERSION", "CODEX_VERSION", "NODE_VERSION", "GH_VERSION"} {
		if !regexp.MustCompile(`(?m)^ARG ` + v + `=[0-9]+\.[0-9]+\.[0-9]+\s*$`).MatchString(d) {
			t.Errorf("the agent Dockerfile does not pin %s to an exact version", v)
		}
	}
	if strings.Contains(d, "@latest") {
		t.Errorf("the agent Dockerfile installs a @latest")
	}
}
