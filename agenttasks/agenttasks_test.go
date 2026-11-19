package agenttasks_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/agenttasks"
)

// Every adapter offered is well formed, and the default is one of them.
func TestAdapters(t *testing.T) {
	for _, n := range agenttasks.Names() {
		a, ok := agenttasks.Lookup(n)
		if !ok {
			t.Fatalf("%s listed but not found", n)
		}
		if err := a.Check(); err != nil {
			t.Error(err)
		}
	}
	if _, ok := agenttasks.Lookup(agenttasks.Default().Name); !ok {
		t.Fatal("the default adapter is not offered")
	}
	if _, ok := agenttasks.Lookup("nope"); ok {
		t.Fatal("found an adapter that is not offered")
	}
}

// A credential is one of the adapter's names, not empty, within 64 KiB;
// the refusal never shows the value.
func TestCheckSecret(t *testing.T) {
	a := agenttasks.Default()
	if err := a.CheckSecret(a.Secrets[0], "tok"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, value string }{
		{"OTHER_KEY", "tok-value"},
		{a.Secrets[0], ""},
		{a.Secrets[0], strings.Repeat("v", 64<<10)},
	} {
		err := a.CheckSecret(c.name, c.value)
		if err == nil {
			t.Fatalf("%s: accepted", c.name)
		}
		if c.value != "" && strings.Contains(err.Error(), c.value[:3]) {
			t.Fatalf("the refusal shows the value: %v", err)
		}
	}
}

// Entry exits ExitNotStarted, with the agent never run, when the credential
// did not arrive or the checkout is not there.
func TestEntryNotStarted(t *testing.T) {
	home := t.TempDir()
	secrets := t.TempDir()
	if err := os.WriteFile(filepath.Join(secrets, "TOKEN"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(home, "ran")
	for _, c := range []struct {
		what    string
		secrets string
	}{{"no secrets directory", ""}, {"no checkout", secrets}} {
		cmd := exec.Command("sh", "-c", agenttasks.Entry, "potluck-agent-task", "task", "missing-checkout", "0000", "TOKEN", "60", "touch", ran)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "POTLUCK_SECRETS=" + c.secrets}
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != agenttasks.ExitNotStarted {
			t.Errorf("%s: %v, want exit %d", c.what, err, agenttasks.ExitNotStarted)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Fatalf("%s: the agent ran", c.what)
		}
	}
}

// Check refuses an adapter whose argv does not start with its binary, or
// whose credential name breaks the secrets door's rule.
func TestCheckRefuses(t *testing.T) {
	a := agenttasks.Default()
	b := a
	b.Argv = append([]string{"sh"}, a.Argv...)
	c := a
	c.Secrets = []string{"lower"}
	d := a
	d.Template = "nope"
	for _, x := range []agenttasks.Adapter{b, c, d} {
		if err := x.Check(); err == nil {
			t.Errorf("accepted %+v", x)
		}
	}
}

func TestCommandOptions(t *testing.T) {
	a := agenttasks.Default()
	model := "x; $(touch never) ' --help"
	argv, err := a.Command(model, "high")
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"--model=" + model, "--effort=high", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"} {
		if !slices.Contains(argv, arg) {
			t.Fatalf("missing %q in %q", arg, argv)
		}
	}
	if argv[0] != "env" || slices.Contains(a.Argv, "--effort=high") {
		t.Fatalf("argv mutated or env missing: %q", argv)
	}
	a.ModelFlag = ""
	if _, err := a.Command("chosen", ""); err == nil || !strings.Contains(err.Error(), "--model is not supported") {
		t.Fatalf("unsupported model: %v", err)
	}
	a.EffortFlag = ""
	if _, err := a.Command("", "high"); err == nil {
		t.Fatal("unsupported effort accepted")
	}
}

func TestCheckOptionsAndEnvironment(t *testing.T) {
	for _, change := range []func(*agenttasks.Adapter){
		func(a *agenttasks.Adapter) { a.ModelFlag = "--model;bad" },
		func(a *agenttasks.Adapter) { a.Env = map[string]string{"BAD=NAME": "1"} },
		func(a *agenttasks.Adapter) { a.Env = map[string]string{a.Secrets[0]: "credential"} },
		func(a *agenttasks.Adapter) { a.Env = map[string]string{"SETTING": "nul\x00"} },
	} {
		a := agenttasks.Default()
		change(&a)
		if err := a.Check(); err == nil {
			t.Fatalf("accepted invalid adapter: %+v", a)
		}
	}
}

// Entry saves work only after stopping the agent and its ordinary descendants.
func TestEntrySalvagesStoppedTask(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("GNU timeout is required by the agent image")
	}
	for _, why := range []string{"deadline", "TERM", "own124", "exited-with-child"} {
		t.Run(why, func(t *testing.T) {
			home, secrets := t.TempDir(), t.TempDir()
			repo, task := filepath.Join(home, "repo"), filepath.Join(home, "task")
			for _, dir := range []string{repo, task} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			git := func(args ...string) string {
				t.Helper()
				c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
				out, err := c.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s: %v", args, out, err)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			git("commit", "--allow-empty", "-qm", "base")
			base := git("rev-parse", "HEAD")
			for path, data := range map[string]string{filepath.Join(secrets, "TOKEN"): "secret", filepath.Join(task, "prompt"): "task"} {
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			sentinel := filepath.Join(home, "descendant-wrote")
			ready := filepath.Join(home, "ready")
			script := `echo saved > changed
sh -c 'trap "" TERM; sleep 1; echo escaped > "$1"' child "$1" &
echo ready > "$2"
sleep 30`
			wantCode, wantState, limit := 124, "timeout", "0.2"
			if why == "TERM" {
				limit = "30"
			}
			if why == "own124" {
				script = "echo saved > changed; exit 124"
				wantState = "exited"
				limit = "30"
			}
			if why == "exited-with-child" {
				script = strings.Replace(script, "sleep 30", "exit 0", 1)
				wantState = "exited"
				wantCode = 0
				limit = "30"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", agenttasks.Entry, "task", task, repo, base, "TOKEN", limit, "sh", "-c", script, "agent", sentinel, ready)
			cmd.Env = append(os.Environ(), "HOME="+home, "POTLUCK_SECRETS="+secrets)
			var out strings.Builder
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			if why == "TERM" {
				until := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(ready); err == nil {
						break
					}
					if time.Now().After(until) {
						t.Fatal("agent never started")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err := <-done
			got := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatal(err)
				}
				got = ee.ExitCode()
			}
			if got != wantCode {
				t.Fatalf("exit %d want %d: %v\n%s", got, wantCode, err, out.String())
			}
			status, err := os.ReadFile(filepath.Join(task, agenttasks.OutDir, agenttasks.Status))
			if err != nil || strings.TrimSpace(string(status)) != wantState {
				t.Fatalf("status %q: %v", status, err)
			}
			bundle := filepath.Join(task, agenttasks.OutDir, agenttasks.Bundle)
			git("bundle", "verify", bundle)
			git("fetch", "--quiet", bundle, "HEAD:refs/test/result")
			if got := git("show", "refs/test/result:changed"); got != "saved" {
				t.Fatalf("saved change %q", got)
			}
			if why != "own124" {
				time.Sleep(1200 * time.Millisecond)
				if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
					t.Fatalf("descendant wrote after collection: %v", err)
				}
			}
		})
	}
}
