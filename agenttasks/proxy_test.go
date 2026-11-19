package agenttasks_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/agenttasks"
	"github.com/sengirunners/potluck/taskproxy"
)

func proxyFiles(t *testing.T, dir string) {
	t.Helper()
	config, err := (taskproxy.Options{Profile: "development", DownloadMbit: 48}).Config()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"proxy.py": []byte(taskproxy.Script), "proxy.json": config, "prompt": []byte("task")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProxyCredentialBoundary(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 required")
	}
	home, dir, bin, secrets := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	proxyFiles(t, dir)
	// Inspect the actual helper's launch environment before running its code.
	stub := `#!/bin/sh
test -z "${CLAUDE_CODE_OAUTH_TOKEN+x}${ANTHROPIC_API_KEY+x}${ANTHROPIC_AUTH_TOKEN+x}${CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR+x}${CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR+x}${CLAUDE_CODE_REMOTE+x}" || exit 51
if ( : <&3 ) 2>/dev/null; then exit 52; fi
exec "$REAL_PYTHON" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range agenttasks.Default().Secrets {
		if err := os.WriteFile(filepath.Join(secrets, name), []byte("fake-token"), 0600); err != nil {
			t.Fatal(err)
		}
		a := agenttasks.Default()
		a.Binary = "sh"
		a.Argv = []string{"sh", "-c", `test "$(cat <&3)" = fake-token || exit 61
test -z "${CLAUDE_CODE_OAUTH_TOKEN+x}${ANTHROPIC_API_KEY+x}${ANTHROPIC_AUTH_TOKEN+x}" || exit 62
test "$HTTP_PROXY" = "$https_proxy" || exit 63
case "$HTTPS_PROXY" in http://127.0.0.1:*) ;; *) exit 64 ;; esac
echo descriptor-child-ok`}
		argv, err := a.CredentialCommand(name, "", "")
		if err != nil {
			t.Fatal(err)
		}
		argv, err = a.ProxyCommand(dir, argv)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "REAL_PYTHON=" + python, "POTLUCK_SECRETS=" + secrets,
			"CLAUDE_CODE_OAUTH_TOKEN=fake-token", "ANTHROPIC_API_KEY=fake-token", "ANTHROPIC_AUTH_TOKEN=fake-token", "CLAUDE_CODE_REMOTE=1",
			"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=3"}
		file, err := os.Open(filepath.Join(secrets, name))
		if err != nil {
			t.Fatal(err)
		}
		cmd.ExtraFiles = []*os.File{file}
		out, err := cmd.CombinedOutput()
		file.Close()
		cancel()
		if err != nil || !strings.Contains(string(out), "descriptor-child-ok") || strings.Contains(string(out), "fake-token") {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
}

func TestProxyEntryCleanup(t *testing.T) {
	for _, name := range []string{"python3", "timeout"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " required")
		}
	}
	for _, mode := range []string{"exit", "timeout", "TERM"} {
		t.Run(mode, func(t *testing.T) {
			home, dir, repo, secrets := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			proxyFiles(t, dir)
			env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "POTLUCK_SECRETS=" + secrets, "GIT_CONFIG_NOSYSTEM=1"}
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
				cmd.Env = env
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			git("commit", "--allow-empty", "-qm", "base")
			base := git("rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(secrets, "TOKEN"), []byte("fake"), 0600); err != nil {
				t.Fatal(err)
			}
			endpoint := filepath.Join(home, "endpoint")
			script := `printf '%s' "$HTTPS_PROXY" > "$1"; `
			limit := "5"
			if mode != "exit" {
				script += "sleep 30"
				if mode == "timeout" {
					limit = "0.5"
				}
			} else {
				script += "exit 0"
			}
			argv, err := agenttasks.Default().ProxyCommand(dir, []string{"sh", "-c", script, "agent", endpoint})
			if err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-c", agenttasks.Entry, "task", dir, repo, base, "TOKEN", limit}, argv...)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", args...)
			cmd.Env = env
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if mode == "TERM" {
				until := time.Now().Add(3 * time.Second)
				for {
					if _, err := os.Stat(endpoint); err == nil {
						break
					}
					if time.Now().After(until) {
						t.Fatal("agent did not start before cancellation")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err = cmd.Wait()
			out := output.Bytes()
			want := 0
			if mode != "exit" {
				want = 124
			}
			got := cmd.ProcessState.ExitCode()
			if got != want {
				t.Fatalf("exit %d want %d: %v\n%s", got, want, err, out)
			}
			value, err := os.ReadFile(endpoint)
			if err != nil {
				t.Fatalf("agent not started: %v\n%s", err, out)
			}
			u, err := url.Parse(string(value))
			if err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", u.Host, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				t.Fatal("proxy listener survived task completion")
			}
			status, err := os.ReadFile(filepath.Join(dir, agenttasks.OutDir, agenttasks.Status))
			if err != nil {
				t.Fatal(err)
			}
			wantState := "exited"
			if mode != "exit" {
				wantState = "timeout"
			}
			if strings.TrimSpace(string(status)) != wantState {
				t.Fatal(fmt.Sprintf("status %s", status))
			}
		})
	}
}
