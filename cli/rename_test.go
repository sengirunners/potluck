package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// kit's messages reach a potluck user in potluck's words: errors, hints
// and the human text built from them, never "zm" as a program or command.
func TestKitMessagesInPotluckWords(t *testing.T) {
	if config.Program != "potluck" || config.HumanEnv != "POTLUCK_HUMAN" || config.HostEnv != "" {
		t.Fatalf("UseHome: program %q, human %q, host %q", config.Program, config.HumanEnv, config.HostEnv)
	}
	var errw bytes.Buffer
	a := programApp(Program{}, nil, io.Discard, &errw)
	a.report(&api.Error{Code: api.CodeUnavailable, Message: "the zm daemon at /s/potluck.sock stopped or restarted during this request",
		Hint: "run it again once the daemon is back (zm status; zm daemon --detach starts it)"})
	want := "potluck: the potluck daemon at /s/potluck.sock stopped or restarted during this request\n" +
		"  run it again once the daemon is back (potluck status; potluck daemon --detach starts it)\n"
	if errw.String() != want {
		t.Fatalf("report:\n got %q\nwant %q", errw.String(), want)
	}
	errw.Reset()
	a.report(fmt.Errorf("no layer can lend this machine: run `zm host doctor` for what is missing"))
	if got := errw.String(); got != "potluck: no layer can lend this machine: run `potluck doctor` for what is missing\n" {
		t.Fatalf("plain error: %q", got)
	}
	var out bytes.Buffer
	fmt.Fprintf(words{&out}, "  - %s\n", "zm-run-tart: a box needs zm sandbox create (Zm-As, ZM_SECRETS, ~/.zm-run-tart)")
	if got := out.String(); got != "  - zm-run-tart: a box needs potluck sandbox create (Zm-As, ZM_SECRETS, ~/.zm-run-tart)\n" {
		t.Fatalf("words: %q", got)
	}
	// Every zm-only command named in kit's messages is in Verbs (as none),
	// so a message never names a potluck command that does not exist.
	for _, w := range []string{"tunnel setup", "tailnet up", "token mint", "project use", "invite ls", "machine-class"} {
		if v, ok := Verbs[w]; !ok || v != "" {
			t.Errorf("Verbs[%q] = %q, %v", w, v, ok)
		}
	}
	if got := config.Text("run zm tunnel setup --quick first"); strings.Contains(got, "zm") || strings.Contains(got, "potluck tunnel") {
		t.Errorf("a zm-only command: %q", got)
	}
}

// Every word Verbs maps a zm command to is a potluck command: a hint
// rewritten through it never names one that does not exist ("status" once
// became "potluck daemon status", which potluck has none of).
func TestVerbsNameRealCommands(t *testing.T) {
	root := programApp(Program{}, nil, io.Discard, io.Discard).root()
	for zm, words := range Verbs {
		if words == "" {
			continue
		}
		cmd := root
		for _, w := range strings.Fields(words) {
			if strings.HasPrefix(w, "-") {
				if cmd.Flags().Lookup(strings.TrimLeft(w, "-")) == nil {
					t.Errorf("Verbs[%q] = %q: potluck %s has no flag %s", zm, words, cmd.CommandPath(), w)
				}
				continue
			}
			var next *cobra.Command
			for _, c := range cmd.Commands() {
				if c.Name() == w || c.HasAlias(w) {
					next = c
				}
			}
			if next == nil {
				t.Errorf("Verbs[%q] = %q: %s has no command %q", zm, words, cmd.CommandPath(), w)
				break
			}
			cmd = next
		}
	}
}
