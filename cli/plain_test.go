package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

func TestPlainly(t *testing.T) {
	cause := errors.New("launchctl bootstrap: exit status 5")
	err := api.Plainly(fmt.Errorf("on: %w", api.Plainly(cause, "This Mac said no.")), "It didn't work.")
	if api.PlainForm(err) != "This Mac said no." || err.Error() != "on: "+cause.Error() || !errors.Is(err, cause) {
		t.Errorf("the closest words to the cause: %v %q", err, api.PlainForm(err))
	}
	var o, e bytes.Buffer
	a := &app{host: Host{Prefix: "potluck", Stdout: &o, Stderr: &e}}
	if a.report(fmt.Errorf("again: %w", err)); e.String() != "potluck: again: on: "+cause.Error()+"\n" {
		t.Errorf("a terminal sees the whole error: %s", e.String())
	}
	refused := api.Errorf(api.CodeConflict, "busy").WithHint("wait")
	if err := api.Plainly(refused, "Busy."); !api.IsCode(err, api.CodeConflict) || err.Error() != refused.Error() || refused.Plain != "" {
		t.Errorf("an API error keeps its code and words, and is not changed: %v %+v", err, refused)
	}
	if api.Plainly(nil, "x") != nil {
		t.Error("nil is no error")
	}
}

// Under --json the Mac app reads an error's plain form; a terminal sees only
// potluck's own words.
func TestPlainErrorOnStderr(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	run := func(args ...string) (string, int) {
		var o, e bytes.Buffer
		code := Run(context.Background(), args, strings.NewReader(""), &o, &e)
		return e.String(), code
	}
	const invite = "https://potluck.sh/join#v1.abc" // cut short
	errOut, code := run("--json", "host", "--yes", "--no-sudo", "--detach", invite)
	var got api.ErrorResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(errOut)), &got); err != nil || code == 0 || got.Error == nil || got.Error.Plain == "" || got.Error.Message == got.Error.Plain {
		t.Fatalf("exit %d, %v: %s", code, err, errOut)
	}
	if errOut, _ = run("host", "--yes", "--no-sudo", "--detach", invite); !strings.Contains(errOut, got.Error.Message) || strings.Contains(errOut, got.Error.Plain) {
		t.Errorf("a terminal: %s", errOut)
	}
	// What an older app pastes after -- is never a flag.
	if errOut, _ = run("--json", "host", "--yes", "--no-sudo", "--detach", "--", "--help"); !strings.Contains(errOut, `"error"`) {
		t.Errorf("a pasted flag: %s", errOut)
	}
}

// A pasted word that is no invite is not this Mac's fault.
func TestPlainNoSuchPool(t *testing.T) {
	m := onePoolMachine(t, refPin)
	useSwitch(t)
	_, e, code := m.run(t, "--json", "host", "--yes", "--no-sudo", "--detach", "--sizes", "c1m2", "hello")
	var got api.ErrorResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(e)), &got); err != nil || code == 0 || !strings.HasPrefix(got.Error.Plain, "That isn't a whole invite, or") {
		t.Errorf("exit %d, %v: %s", code, err, e)
	}
}
