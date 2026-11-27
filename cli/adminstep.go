package cli

import (
	"cmp"
	"errors"
	"strings"
)

// adminStep is a root step a command did not run itself because it was
// given --no-sudo: why it is needed, in plain words for the person, and the
// program and arguments to run as root. A Mac app runs it through macOS's
// administrator password dialog. Argv[0] is an absolute path; nothing here
// passes Argv through a shell. The root half it names checks again, as
// root, everything it relies on (docs/softnet.md).
type adminStep struct {
	Why  string   `json:"why"`
	Argv []string `json:"argv"`
	// detail is what the step does in a terminal's words (what it puts
	// where, and why it needs root); "" when Why says it.
	detail string
}

// helperWhy is an admin step's reason, as the Mac app's password dialog
// says it: to is what the step does, as "install the network helper".
func helperWhy(to string) string {
	return "macOS will ask for this Mac's password to " + to + "."
}

// command is the step as one line for a terminal, each word quoted for a
// POSIX shell.
func (s adminStep) command() string {
	q := make([]string, len(s.Argv))
	for i, w := range s.Argv {
		q[i] = shellQuote(w)
	}
	return strings.Join(q, " ")
}

// adminStepLeft is what a command's user half returns, under --no-sudo,
// in place of running its root step.
type adminStepLeft struct{ step adminStep }

func (e *adminStepLeft) Error() string {
	return "the admin step was not run (--no-sudo); run as root: " + e.step.command()
}

// adminStepCmd runs a command with a root step that --no-sudo hands back.
// Under --json the people-words go to stderr and stdout carries only the
// answer: {"admin_step": {...}} when the root step is left, {} when the
// command finished. Without --json a left step is said as the command to run
// as root, then what to run after it (then). Either way a left step exits 0; any other error is
// the command's.
func (a *app) adminStepCmd(then string, run func() error) error {
	err := a.wordsAside(run)
	var left *adminStepLeft
	switch {
	case errors.As(err, &left) && a.asJSON():
		return a.printJSON(map[string]adminStep{"admin_step": left.step})
	case errors.As(err, &left):
		a.say("--no-sudo: the admin step was not run. %s\nRun this as root, then %s:\n  %s", cmp.Or(left.step.detail, left.step.Why), then, left.step.command())
		return nil
	case err != nil:
		return err
	case a.asJSON():
		return a.printJSON(struct{}{})
	}
	return nil
}

// wordsAside runs run with the people-words on stderr under --json, so
// stdout carries only the answer the caller prints after it.
func (a *app) wordsAside(run func() error) error {
	if !a.asJSON() {
		return run()
	}
	out := a.host.Stdout
	a.host.Stdout = a.host.Stderr
	defer func() { a.host.Stdout = out }()
	return run()
}
