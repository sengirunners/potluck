package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

// A message is printed as its producer wrote it: nothing rewrites a word
// in it, so a sentence that names another program by its name keeps it.
func TestReportPrintsMessagesAsWritten(t *testing.T) {
	var errw bytes.Buffer
	h := Host{Prefix: "supper pool", Stdout: io.Discard, Stderr: &errw,
		Client: func() (*client.Client, *config.Config, error) { return nil, nil, errors.New("no client") }}
	Report(h, &api.Error{Code: api.CodeUnavailable, Message: "zm's daemon (home ~/.zm) is the one potluck uses",
		Hint: "run zm daemon --detach, or potluck status"})
	want := "supper: zm's daemon (home ~/.zm) is the one potluck uses\n  run zm daemon --detach, or potluck status\n"
	if errw.String() != want {
		t.Fatalf("report:\n got %q\nwant %q", errw.String(), want)
	}
	errw.Reset()
	Report(h, errors.New("zm cannot read zm's file"))
	if got := errw.String(); got != "supper: zm cannot read zm's file\n" {
		t.Fatalf("plain error: %q", got)
	}

	// A line for people (Env.Say, what status prints with) as well.
	say := func(env Env) *cobra.Command {
		return &cobra.Command{Use: "say", RunE: func(*cobra.Command, []string) error {
			env.Say("using zm's daemon (home ~/.zm), zm-run-tart")
			return nil
		}}
	}
	var out bytes.Buffer
	if code := RunProgram(context.Background(), Program{Commands: []Command{say}}, []string{"say"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := out.String(); got != "using zm's daemon (home ~/.zm), zm-run-tart\n" {
		t.Fatalf("say: %q", got)
	}
}

// With potluck's program set (UseHome), kit's messages name potluck's
// commands in Potluck's words, and say so of a command potluck has none of
// instead of naming one that does not exist.
func TestKitNamesPotlucksCommands(t *testing.T) {
	if config.Program != "potluck" || config.Prefix != "" || config.HumanEnv != EnvHuman || config.HostEnv != "" {
		t.Fatalf("UseHome: program %q, prefix %q, human %q, host %q", config.Program, config.Prefix, config.HumanEnv, config.HostEnv)
	}
	if got := config.PotluckCommand("doctor"); got != "potluck doctor" {
		t.Errorf("PotluckCommand(doctor) = %q", got)
	}
	for _, w := range []string{"join", "leave", "invite", "remotes", "forget", "status", "daemon", "daemon stop", "daemon preflight",
		"host", "host stop", "doctor", "sandbox create", "sandbox exec", "sandbox rm", "run", "runners setup", "driver pin", "upgrade"} {
		if got := config.Command(w); got != "potluck "+w {
			t.Errorf("Command(%q) = %q", w, got)
		}
	}
	if got := config.Command("daemon", "--detach"); got != "potluck daemon --detach" {
		t.Errorf("Command(daemon, --detach) = %q", got)
	}
	for _, w := range []string{"tunnel setup", "tailnet up", "project why", "target ls", "token mint", "machine-class", "daemon update", "host share x"} {
		if !config.Missing(w) {
			t.Errorf("potluck has no %q, but Missing says it has", w)
		}
		if got, want := config.Command(w, "--x"), "the "+w+" command (potluck has none)"; got != want {
			t.Errorf("Command(%q) = %q, want %q", w, got, want)
		}
	}
	// A hidden old spelling is a command potluck has.
	if config.Missing("host share") {
		t.Error("host share (hidden) is one of potluck's")
	}
}
