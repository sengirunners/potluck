package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/agenttasks"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/roomwait"
)

// fakeClaude stands in for Claude Code in the box: it needs its token on
// descriptor 3, writes the prompt (its stdin) to a file and commits it,
// leaves one more file uncommitted, prints its argv, and exits 3 when the
// prompt says fail, 124 when it says exit124. A prompt saying nothing makes
// no change at all. One saying plant also puts a git repository (a broken
// box's) where the entry script writes the bundle.
const fakeClaude = `#!/bin/sh
[ "$CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR" = 3 ] && [ "$(cat <&3)" = "tok-sekrit-123" ] || { echo "fake claude: no descriptor token" >&2; exit 9; }
sh -c '[ -z "${CLAUDE_CODE_OAUTH_TOKEN+x}" ] && [ -z "${ANTHROPIC_API_KEY+x}" ] && [ -z "${CLAUDE_CODE_REMOTE+x}" ]' || exit 11
[ "$CLAUDE_CODE_DISABLE_BACKGROUND_TASKS" = 1 ] && [ "$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" = 1 ] || exit 10
prompt=$(cat)
echo "argv: $*"
case "$prompt" in "nothing verbose"*) i=0; while [ "$i" -lt 40 ]; do echo "line-$i"; i=$((i+1)); done ;; esac
case "$prompt" in nothing*) echo "done: nothing"; exit 0 ;; esac
printf '%s\n' "$prompt" > from-agent.txt
git add from-agent.txt && git commit -q -m "agent: from the prompt" || exit 8
echo leftover > left.txt
case "$prompt" in wait*) echo task-ready; sleep 30 ;; esac
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
	ErrorCode                         string             `json:"error_code"`
	SetupS                            *float64           `json:"setup_s"`
	Seconds                           float64            `json:"seconds"`
	Phases                            map[string]float64 `json:"phases"`
	StartDetails                      map[string]float64 `json:"start_details"`
	Handle, State, Error, Ref, Commit string
	Exit                              *int
	Commits                           int
	Kept, Released                    bool
	Last                              []string
}

// agent-task runs the agent in a box on the pushed commit with the
// credential through the box secrets door, and its commits (what it left
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

	t.Run("no target has structured code", func(t *testing.T) {
		o, e, code := m.run(t, "--json", "agent-task", "--size", "no-such-size", "--image", "ref-image", "--prompt-file", prompt, "--secret-file", tok, "--timeout", "2m")
		var r agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &r); err != nil || code != 1 || r.ErrorCode != "no_target" || r.Handle != "" || r.SetupS != nil {
			t.Fatalf("exit %d %+v %v\n%s", code, r, err, e)
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

	t.Run("credential grant refusal releases the box", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		o, e, code := m.run(t, "--json", "agent-task", "--image", "ref-image", "--prompt-file", prompt, "--secret-file", tok, "--timeout", "2m")
		var r agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &r); err != nil || code == 0 || r.State != "failed" || !r.Released || r.Exit != nil || !strings.Contains(r.Error, "box secrets door") {
			t.Fatalf("exit %d %+v %v\n%s", code, r, err, e)
		}
		noSecret(t, o, e)
	})
	m.ok(t, "secrets", "grant", "add", "--name", "CLAUDE_CODE_OAUTH_TOKEN", "--role", "owner", "--scope", "sandbox")

	t.Run("commits back, exit passes through", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		o, e, code := m.run(t, "--json", "agent-task", "claude", "--image", "ref-image", "--prompt-file", prompt, "--secret-file", tok, "--timeout", "2m")
		var r agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &r); err != nil || code != 3 || r.State != "exited" || r.Exit == nil || *r.Exit != 3 || !r.Released {
			t.Fatalf("exit %d %+v %v\n%s", code, r, err, e)
		}
		assertJobTiming(t, r.Seconds, r.SetupS, r.Phases)
		// start's parts: the probe and the prompt side by side, then the
		// credential and the spawn; together never more than start.
		for _, part := range []string{"probe", "prompt", "secrets", "spawn"} {
			if d, ok := r.StartDetails[part]; !ok || d <= 0 || d > r.Phases["start"] {
				t.Fatalf("start_details %v (start %v): %s", r.StartDetails, r.Phases["start"], part)
			}
		}
		if _, ok := r.StartDetails["proxy"]; ok || r.StartDetails["secrets"]+r.StartDetails["spawn"]+max(r.StartDetails["probe"], r.StartDetails["prompt"]) > r.Phases["start"]+0.01 {
			t.Fatalf("start_details %v over start %v", r.StartDetails, r.Phases["start"])
		}
		if !strings.Contains(e, "start ") || !strings.Contains(e, "(probe ") {
			t.Fatalf("the stderr line names no start parts:\n%s", e)
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

	t.Run("stdout file is not part of the dirty snapshot", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
		path := filepath.Join(repo, "captured.log")
		t.Cleanup(func() { _ = os.Remove(path) })
		o, e, code := m.runIn(t, "nothing to do\n", "--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m", "--dirty", "--include-untracked", "--stdout-file", path)
		var r agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &r); err != nil || code != 0 || r.Commit == "" {
			t.Fatalf("output file changed the snapshot: exit %d %+v %v\n%s", code, r, err, e)
		}
		if err := exec.Command("git", "-C", repo, "cat-file", "-e", r.Commit+":captured.log").Run(); err == nil {
			t.Fatal("stdout file was included in the resolved snapshot")
		}
	})

	t.Run("model effort and full stdout", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
		path := filepath.Join(t.TempDir(), "stdout")
		o, e, code := m.runIn(t, "nothing verbose\n", "--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m", "--model", "model with spaces; $(false)", "--effort", "high", "--stdout-file", path)
		if code != 0 {
			t.Fatalf("exit %d: %s %s", code, o, e)
		}
		b, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(b), "--model=model with spaces; $(false) --effort=high") || !strings.Contains(string(b), "done: nothing") || !strings.Contains(string(b), "line-0\n") || !strings.Contains(string(b), "line-39\n") {
			t.Fatalf("stdout %q: %v", b, err)
		}
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("stdout permissions: %v %v", fi, err)
		}
		noSecret(t, o, e, string(b))
	})

	t.Run("cooperative proxy", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("Python 3 required")
		}
		proxied := t.TempDir()
		check := `case "$HTTPS_PROXY" in http://127.0.0.1:*) ;; *) echo missing-proxy >&2; exit 90 ;; esac
test "$HTTPS_PROXY" = "$http_proxy" || exit 91
`
		if err := os.WriteFile(filepath.Join(proxied, "claude"), []byte(strings.Replace(fakeClaude, "#!/bin/sh\n", "#!/bin/sh\n"+check, 1)), 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", proxied+string(os.PathListSeparator)+sysPath)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
		o, e, code := m.runIn(t, "nothing proxy\n", "--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m", "--cooperative-proxy", "development", "--proxy-download-mbit", "12", "--proxy-allow-host", "mirror.example.com")
		if code != 0 {
			t.Fatalf("exit %d: %s %s", code, o, e)
		}
		var result struct {
			Proxy struct {
				Profile      string
				DownloadMbit int `json:"download_mbit"`
			} `json:"cooperative_proxy"`
		}
		if err := json.Unmarshal([]byte(o), &result); err != nil || result.Proxy.Profile != "development" || result.Proxy.DownloadMbit != 12 {
			t.Fatalf("proxy result: %s: %v", o, err)
		}
		if !strings.Contains(e, "cooperative proxy ready; clients can bypass it") {
			t.Fatalf("missing readiness receipt: %s", e)
		}
		noSecret(t, o, e)
	})

	t.Run("proxy options refuse before placement", func(t *testing.T) {
		for _, flags := range [][]string{{"--proxy-download-mbit", "12"}, {"--cooperative-proxy", "firewall"}, {"--cooperative-proxy", "development", "--proxy-allow-host", "127.0.0.1"}} {
			_, e, code := m.runIn(t, "nothing\n", append([]string{"agent-task", "--prompt-file", "-"}, flags...)...)
			if code == 0 || !strings.Contains(e, "proxy") {
				t.Fatalf("accepted %v: %s", flags, e)
			}
		}
	})

	t.Run("proxy startup failure never starts agent", func(t *testing.T) {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, "python3"), []byte("#!/bin/sh\nexit 125\n"), 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", broken+string(os.PathListSeparator)+fake+string(os.PathListSeparator)+sysPath)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
		o, e, code := m.runIn(t, "nothing proxy\n", "--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m", "--cooperative-proxy", "development")
		var result agentTaskResultJSON
		if err := json.Unmarshal([]byte(o), &result); err != nil || code != 125 || result.Exit == nil || *result.Exit != 125 || !result.Released || strings.Contains(e, "done: nothing") {
			t.Fatalf("startup failure: code %d %s %s (%v)", code, o, e, err)
		}
		noSecret(t, o, e)
	})

	t.Run("deps reach acquire", func(t *testing.T) {
		t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
		for name, content := range map[string]string{"package.json": "{}", "package-lock.json": `{"lockfileVersion":3,"packages":{"":{}}}`} {
			path := filepath.Join(repo, name)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
		}
		if err := os.WriteFile(filepath.Join(fake, "npm"), []byte("#!/bin/sh\nmkdir -p node_modules\n"), 0755); err != nil {
			t.Fatal(err)
		}
		o, e, code := m.runIn(t, "nothing to do\n", "--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", "2m", "--deps", "npm", "--deps-scripts", "off")
		if code != 0 || !strings.Contains(e, "deps npm at checkout: built") {
			t.Fatalf("deps: exit %d: %s %s", code, o, e)
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

	for _, stop := range []string{"deadline", "interrupt"} {
		t.Run("salvage on "+stop, func(t *testing.T) {
			t.Setenv("PATH", fake+string(os.PathListSeparator)+sysPath)
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-sekrit-123")
			t.Setenv(EnvHome, m.home)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out bytes.Buffer
			errw := &taskCancelOutput{}
			timeout := "1s"
			wantCode, wantState := 124, "timeout"
			if stop == "interrupt" {
				timeout = "2m"
				wantCode = 130
				wantState = "interrupted"
				errw.cancel = cancel
			}
			code := Run(ctx, []string{"--json", "agent-task", "--image", "ref-image", "--prompt-file", "-", "--timeout", timeout}, strings.NewReader("wait after writing changes\n"), &out, errw)
			var r agentTaskResultJSON
			if err := json.Unmarshal(out.Bytes(), &r); err != nil || code != wantCode || r.State != wantState || r.Commits != 2 || !r.Released || r.Ref == "" {
				t.Fatalf("exit %d result %+v: %v\n%s\n%s", code, r, err, out.String(), errw.String())
			}
			if got := gitIn("show", r.Ref+":left.txt"); got != "leftover" {
				t.Fatalf("salvaged %q", got)
			}
		})
	}

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

// Unsupported adapter options fail before a client or checkout is consulted.
func TestAgentTaskUnsupportedModel(t *testing.T) {
	a := agenttasks.Default()
	a.ModelFlag = ""
	r, err := agentTask(context.Background(), nil, agentTaskOptions{adapter: a, model: "chosen"})
	if err == nil || !strings.Contains(err.Error(), "--model is not supported") || r.Handle != "" {
		t.Fatalf("%+v: %v", r, err)
	}
}

func TestAgentTaskOutputRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openTaskOutput(path); err == nil {
		t.Fatal("existing output accepted")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "preserve" {
		t.Fatal("existing output overwritten")
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openTaskOutput(link); err == nil {
		t.Fatal("symlink accepted")
	}
	w, err := openTaskOutput(path + "-new")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("lost")); err != nil {
		t.Fatal("capture stopped output following")
	}
	if err := w.close(); err == nil {
		t.Fatal("write failure lost")
	}
}

// Cancels through the public command once followed output proves the agent ran.
type taskCancelOutput struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *taskCancelOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if w.cancel != nil && strings.Contains(w.String(), "task-ready") {
		w.cancel()
		w.cancel = nil
	}
	return n, err
}

func (w *taskCancelOutput) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

// agent-task waits for room as run does (room.go) only when --wait asks it
// to (default 0: a full pool fails at once): the task's acquire is asked
// again under one request id at the host's retry time, then places the box;
// a hard refusal never waits. Its help lists the flag on a line of its own,
// which callers read to learn the program has it.
func TestAgentTaskWaitsForRoom(t *testing.T) {
	var out, errw bytes.Buffer
	if code := RunProgram(context.Background(), Program{}, []string{"agent-task", "--help"}, strings.NewReader(""), &out, &errw); code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if !regexp.MustCompile(`(?m)^\s*--wait duration\b`).MatchString(out.String()) {
		t.Errorf("agent-task --help lists no --wait flag line:\n%s", out.String())
	}

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if b, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, b, err)
		}
	}
	slept := fakeRoomClock(t)

	// From the command line: no --wait is no wait; --wait 5m waits at the
	// host's retry time and asks again (a hard refusal then ends the run).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "tok-test")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Chdir(repo)
	cfg, err := config.LoadFrom(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	command := func(d *prepDaemon, flags ...string) error {
		c := client.WithHandler(d)
		c.Project, c.ProjectFrom = "pool", "flag"
		var stdout, stderr bytes.Buffer
		root := &cobra.Command{Use: "example", SilenceErrors: true, SilenceUsage: true}
		root.AddCommand(Commands(Host{Prefix: "example", Client: func() (*client.Client, *config.Config, error) { return c, cfg, nil },
			Stdin: strings.NewReader("fix it"), Stdout: &stdout, Stderr: &stderr})...)
		root.SetArgs(append([]string{"agent-task", "--prompt-file", "-", "--timeout", "5m"}, flags...))
		return root.ExecuteContext(context.Background())
	}
	forbidden := api.Errorf(api.CodeForbidden, "test acquire boundary")
	d := &prepDaemon{onlySmall: true, images: []string{agentPin}, verbs: []string{"spawn"}, caps: []string{protocol.CapBoxSecrets}, fail: map[int]*api.Error{1: noRoomOn("small"), 2: forbidden}}
	if err := command(d); err == nil || !strings.Contains(err.Error(), "no room on this host now") || len(*slept) != 0 || !slices.Equal(d.seen(), []string{"acquire small"}) {
		t.Errorf("no --wait: %v, asked %q, waited %v; want the no room at once", err, d.seen(), *slept)
	}
	d = &prepDaemon{onlySmall: true, images: []string{agentPin}, verbs: []string{"spawn"}, caps: []string{protocol.CapBoxSecrets}, fail: map[int]*api.Error{1: noRoomOn("small"), 2: forbidden}}
	if err := command(d, "--wait", "5m"); err == nil || err.Error() != "forbidden: test acquire boundary" || !slices.Equal(*slept, []time.Duration{4 * time.Minute}) ||
		!slices.Equal(d.seen(), []string{"acquire small", "acquire small"}) {
		t.Errorf("--wait 5m: %v, asked %q, waited %v; want one 4 min wait, then the second answer", err, d.seen(), *slept)
	}
	*slept = nil
	task := func(d *prepDaemon, room time.Duration) (agentTaskResult, error, string) {
		var said strings.Builder
		o := agentTaskOptions{adapter: agenttasks.Default(), prompt: []byte("fix it"), secret: "tok", secretName: agenttasks.Default().Secrets[0]}
		o.repo, o.timeout, o.room, o.started = repo, time.Minute, room, time.Now()
		o.out, o.errw, o.p = io.Discard, &said, "potluck"
		r, err := agentTask(context.Background(), client.WithHandler(d), o)
		return r, err, said.String()
	}

	d = &prepDaemon{onlySmall: true, fail: map[int]*api.Error{1: noRoomOn("small")}}
	if _, _, said := task(d, roomwait.Default); len(d.asks) < 2 || !slices.Equal(d.seen()[:2], []string{"acquire small", "acquire small"}) ||
		d.asks[1].RequestID != d.asks[0].RequestID || !slices.Equal(*slept, []time.Duration{4 * time.Minute}) ||
		!strings.HasPrefix(said, "potluck: small has no room: network busy (128/90 Mbit/s); asking again at ") {
		t.Fatalf("asked %q, waited %v, said %q; want one wait at the host's retry time, then the box under the same request id", d.seen(), *slept, said)
	}

	*slept = nil
	d = &prepDaemon{onlySmall: true, fail: map[int]*api.Error{1: noRoomOn("small")}}
	if _, err, _ := task(d, 0); !api.IsCode(err, api.CodeUnavailable) || len(*slept) != 0 || !slices.Equal(d.seen(), []string{"acquire small"}) {
		t.Errorf("--wait 0: %v, asked %q, waited %v; want it at once", err, d.seen(), *slept)
	}
	d = &prepDaemon{onlySmall: true, fail: map[int]*api.Error{1: api.Errorf(api.CodeForbidden, "potluck-driver-oci cannot isolate boxes now, so it makes no box")}}
	if _, err, _ := task(d, roomwait.Default); !api.IsCode(err, api.CodeForbidden) || len(*slept) != 0 || !slices.Equal(d.seen(), []string{"acquire small"}) {
		t.Errorf("hard refusal: %v, asked %q, waited %v; want it at once", err, d.seen(), *slept)
	}
}
