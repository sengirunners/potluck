package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/agenttasks"
)

// fakeClaude stands in for Claude Code in the box: it needs its token in
// the environment, writes the prompt (its stdin) to a file and commits it,
// leaves one more file uncommitted, prints its argv, and exits 3 when the
// prompt says fail, 124 when it says exit124. A prompt saying nothing makes
// no change at all. One saying plant also puts a git repository (a broken
// box's) where the entry script writes the bundle.
const fakeClaude = `#!/bin/sh
[ "$CLAUDE_CODE_OAUTH_TOKEN" = "tok-sekrit-123" ] || { echo "fake claude: no token" >&2; exit 9; }
prompt=$(cat)
echo "argv: $*"
case "$prompt" in nothing*) echo "done: nothing"; exit 0 ;; esac
printf '%s\n' "$prompt" > from-agent.txt
git add from-agent.txt && git commit -q -m "agent: from the prompt" || exit 8
echo leftover > left.txt
case "$prompt" in plant*)
  for o in "$HOME"/.potluck/agent-task/*/out; do
    git init -q "$o/task.bundle" && git -c user.name=box -c user.email=box@example.com -C "$o/task.bundle" commit -q --allow-empty -m planted || exit 7
  done ;;
esac
echo "done: $prompt"
case "$prompt" in fail*) exit 3 ;; exit124*) exit 124 ;; esac
exit 0
`

// fakeGit stands in for git in a broken box: its bundle is not one. It
// passes every other call (and every call outside the box's command, which
// has no secrets directory) to the git at %s.
const fakeGit = `#!/bin/sh
if [ -n "$POTLUCK_SECRETS" ] && [ "$1" = bundle ] && [ "$2" = create ]; then echo "not a bundle" > "$3"; exit 0; fi
exec %s "$@"
`

// agentTaskResultJSON is what the tests read of agent-task --json.
type agentTaskResultJSON struct {
	Handle, State, Error, Ref, Commit string
	Exit                              *int
	Commits                           int
	Kept, Released                    bool
	Last                              []string
}

// agent-task runs the agent in a box on the pushed commit with the
// credential as its spawn's secret, and its commits (what it left
// uncommitted as one more) come back as a ref in the checkout; its exit
// code is the command's; the credential shows in no argv or output. With no
// credential nothing is placed; a box without the agent's command is
// refused before the agent is started.
func TestAgentTask(t *testing.T) {
	sysPath := os.Getenv("PATH")
	bare := plainPath(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	m := onePoolMachine(t, refPin, agentPin)
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn("init", "-q")
	gitIn("add", ".")
	gitIn("commit", "-q", "-m", "fixture")
	base := gitIn("rev-parse", "HEAD")
	t.Chdir(repo)

	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok-sekrit-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(t.TempDir(), "prompt")
	if err := os.WriteFile(prompt, []byte("fail after writing this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noSecret := func(t *testing.T, outs ...string) {
		t.Helper()
		for _, s := range outs {
			if strings.Contains(s, "sekrit") {
				t.Fatalf("the credential shows:\n%s", s)
			}
		}
	}

	t.Run("no credential", func(t *testing.T) {
		_, e, code := m.run(t, "agent-task", "--prompt-file", prompt, "--timeout", "2m")
		if code == 0 || !strings.Contains(e, "claude setup-token") || !strings.Contains(e, "nothing was placed") {
			t.Fatalf("exit %d\n%s", code, e)
		}
		if o := m.ok(t, "--json", "sandbox", "ls"); strings.Contains(o, "solo-") {
			t.Fatalf("a sandbox was made:\n%s", o)
		}
	})

	// The machine lends an image of the agent template's name, which the
	// reference driver refuses by name (it boots ref-image alone): the
	// refusal shows the task asked for the template, with no --image.
	t.Run("asks for the agent template", func(t *testing.T) {
		_, e, code := m.run(t, "agent-task", "--prompt-file", prompt, "--secret-file", tok, "--timeout", "2m")
		if code == 0 || !strings.Contains(e, `image "ghcr.io/sengirunners/potluck-box-agent" is not one of the binding's images`) {
			t.Fatalf("exit %d\n%s", code, e)
		}
	})

	t.Run("no agent in the box", func(t *testing.T) {
		t.Setenv("PATH", bare)
		o, e, code := m.run(t, "--json", "agent-task", "--image", "ref-image", "--prompt-file", prompt, "--secret-file", tok, "--timeout", "2m")
		var r agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &r); err != nil || code == 0 || r.State != "failed" || !r.Released ||
			!strings.Contains(r.Error, "the sandbox has no claude on PATH") {
			t.Fatalf("exit %d %+v %v\n%s", code, r, err, e)
		}
		noSecret(t, o, e)
	})

	t.Run("commits back, exit passes through", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		o, e, code := m.run(t, "--json", "agent-task", "claude", "--image", "ref-image", "--prompt-file", prompt, "--secret-file", tok, "--timeout", "2m")
		var r agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &r); err != nil || code != 3 || r.State != "exited" || r.Exit == nil || *r.Exit != 3 || !r.Released {
			t.Fatalf("exit %d %+v %v\n%s", code, r, err, e)
		}
		noSecret(t, o, e)
		if !strings.HasPrefix(r.Ref, AgentTaskRefs) || r.Commits != 2 || r.Commit != base {
			t.Fatalf("result %+v\n%s", r, e)
		}
		if !slices.Contains(r.Last, "done: fail after writing this") {
			t.Fatalf("last lines %q", r.Last)
		}
		for _, l := range r.Last {
			if strings.HasPrefix(l, "argv: ") && (strings.Contains(l, "sekrit") || !strings.Contains(l, "--strict-mcp-config")) {
				t.Fatalf("the agent's argv: %s", l)
			}
		}
		if got := gitIn("log", "--format=%s", base+".."+r.Ref); got != agenttasks.UncommittedMessage+"\nagent: from the prompt" {
			t.Fatalf("commits at %s:\n%s", r.Ref, got)
		}
		if got := gitIn("show", r.Ref+":from-agent.txt"); got != "fail after writing this" {
			t.Fatalf("from-agent.txt: %q", got)
		}
		if got := gitIn("show", r.Ref+":left.txt"); got != "leftover" {
			t.Fatalf("left.txt: %q", got)
		}
		// Nothing was checked out: the working tree and HEAD are as they were.
		if gitIn("rev-parse", "HEAD") != base || gitIn("status", "--porcelain") != "" {
			t.Fatal("the checkout changed")
		}
	})

	t.Run("no changes", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
		_, e, code := m.runIn(t, "nothing to do\n", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m")
		if code != 0 || !strings.Contains(e, "the agent made no changes") {
			t.Fatalf("exit %d\n%s", code, e)
		}
		noSecret(t, e)
	})

	// An agent that exits 124 itself, well before --timeout, is not the
	// driver's timeout: its commits come back and 124 is the exit code.
	t.Run("the agent's own 124", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
		o, e, code := m.runIn(t, "exit124 on purpose\n", "--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m")
		var r agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &r); err != nil || code != 124 || r.State != "exited" || r.Commits != 2 || !r.Released {
			t.Fatalf("exit %d %+v %v\n%s", code, r, err, e)
		}
	})

	// A broken box sends back a git repository, or a file that is no
	// bundle, where the bundle goes: refused before any fetch, no ref is
	// written, and the box is kept with the commands to copy its work out
	// and to end it.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	junk := t.TempDir()
	if err := os.WriteFile(filepath.Join(junk, "git"), []byte(fmt.Sprintf(fakeGit, realGit)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "claude"), []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ what, path, prompt, refusal string }{
		{"a repository", fake, "plant a repository\n", "not a file: refused"},
		{"not a bundle", junk, "write a junk bundle\n", "is not one git takes"},
	} {
		t.Run(c.what, func(t *testing.T) {
			t.Setenv("PATH", c.path+string(os.PathListSeparator)+sysPath)
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
			refsBefore := gitIn("for-each-ref", AgentTaskRefs)
			o, e, code := m.runIn(t, c.prompt, "--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m")
			var r agentTaskResultJSON
			if err := json.Unmarshal([]byte(o), &r); err != nil || code != 1 || r.Ref != "" || !r.Kept || r.Released ||
				!strings.Contains(r.Error, c.refusal) {
				t.Fatalf("exit %d %+v %v\n%s", code, r, err, e)
			}
			sid := sandboxID(r.Handle)
			for _, hint := range []string{"potluck sandbox cp " + sid + " :.potluck/agent-task/", "potluck sandbox rm " + sid} {
				if !strings.Contains(r.Error, hint) {
					t.Fatalf("no %q in %q", hint, r.Error)
				}
			}
			if got := gitIn("for-each-ref", AgentTaskRefs); got != refsBefore {
				t.Fatalf("refs written:\n%s", got)
			}
			m.ok(t, "sandbox", "rm", sid)
		})
	}
}

// plainPath is a PATH with the system's tools and git but no claude.
func plainPath(t *testing.T) string {
	t.Helper()
	dirs := []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	for _, c := range []string{"git", "sh"} {
		p, err := exec.LookPath(c)
		if err != nil {
			t.Skipf("no %s", c)
		}
		if d := filepath.Dir(p); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, "claude")); err == nil {
			t.Skipf("%s holds a claude: no PATH without one", d)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}
