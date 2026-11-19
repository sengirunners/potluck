package cli

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Packing happens after acquire so the box can supply its base. A packing
// refusal must still release the acquired box for both one-shot commands.
func TestJobReleasesBoxWhenBundleTooLarge(t *testing.T) {
	m := onePoolMachine(t, refPin)
	repo := t.TempDir()
	data := make([]byte, 2<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "blob"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %s: %v", out, err)
		}
	}
	t.Chdir(repo)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fixture-token")
	for _, command := range []string{"run", "agent-task"} {
		t.Run(command, func(t *testing.T) {
			args := []string{"--json", command, "--image", "ref-image", "--max-mib", "1", "--timeout", "2m"}
			if command == "run" {
				args = append(args, "--", "true")
			} else {
				args = append(args, "--prompt-file", "-")
			}
			out, stderr, code := m.runIn(t, "do nothing", args...)
			var result struct {
				State, Error, Handle string
				Released             bool
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil || code == 0 || result.Handle == "" || result.State != "failed" || !result.Released || !strings.Contains(result.Error, "over 1.0 MiB") {
				t.Fatalf("exit %d, result %+v, parse %v\n%s\n%s", code, result, err, out, stderr)
			}
		})
	}
}
