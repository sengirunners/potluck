package agenttasks_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
		cmd := exec.Command("sh", "-c", agenttasks.Entry, "potluck-agent-task", "task", "missing-checkout", "0000", "TOKEN", "touch", ran)
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
