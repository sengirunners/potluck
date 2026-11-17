// Package agenttasks is what potluck agent-task needs to know of each coding
// agent CLI it runs once in a box, as data: an Adapter per agent. A task
// runs the agent non-interactively in the box's checkout of a pushed commit,
// with the renter's credential as that one spawn's secret, and leaves the
// commits it made in a git bundle the renter's client copies back.
//
// What is generic lives here too: the entry script the box runs (Entry) and
// the names of what it leaves (OutDir, Bundle, NoChanges). Placement, the
// push, the lease, following output, copying the bundle back and fetching it
// are the caller's (cli, agent-task), on top of potluck run's box job.
//
// This is not agentrunners: a vendor's self-hosted runner polls the vendor
// for sessions; an agent task is one prompt, run here and now, whose result
// is commits.
package agenttasks

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
)

// Adapter is one agent CLI, as data.
type Adapter struct {
	// Name is what potluck agent-task takes: [a-z][a-z0-9-]*.
	Name string
	// About is one line naming the agent.
	About string
	// Binary is the command the box must have on PATH (the image ships it).
	Binary string
	// Template is the box template whose image ships Binary
	// (computeapi.BoxTemplates).
	Template string
	// Secrets are the credential names the agent reads from its environment,
	// the default first ([A-Z][A-Z0-9_]*, the secrets door's rule). A task
	// sends one of them; the box exports it under that name for the agent
	// alone.
	Secrets []string
	// Argv runs the agent once, non-interactively, in the checkout. The
	// prompt arrives on its stdin, never in argv.
	Argv []string
	// Login is the command a person runs on their own machine to get a
	// credential (shown when none is given).
	Login string
}

var (
	adapterName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	secretName  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// claude is Claude Code in print mode: edits accepted, the listed tools
// allowed without asking, no MCP servers but none.
var claude = Adapter{
	Name:     "claude",
	About:    "Claude Code (claude -p)",
	Binary:   "claude",
	Template: "agent",
	Secrets:  []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
	Argv: []string{"claude", "-p", "--permission-mode", "acceptEdits",
		"--allowedTools", "Bash", "Read", "Edit", "Write", "Glob", "Grep", "--strict-mcp-config"},
	Login: "claude setup-token",
}

// adapters is every adapter potluck agent-task offers, the default first.
var adapters = []Adapter{claude}

// Default is the adapter a task runs when none is named.
func Default() Adapter { return adapters[0] }

// Lookup finds an adapter by name.
func Lookup(name string) (Adapter, bool) {
	i := slices.IndexFunc(adapters, func(a Adapter) bool { return a.Name == name })
	if i < 0 {
		return Adapter{}, false
	}
	return adapters[i], true
}

// Names lists the adapters' names.
func Names() []string {
	var n []string
	for _, a := range adapters {
		n = append(n, a.Name)
	}
	return n
}

// Check reports what is wrong with an adapter's own fields.
func (a Adapter) Check() error {
	switch {
	case !adapterName.MatchString(a.Name):
		return fmt.Errorf("adapter name %q: want [a-z][a-z0-9-]*", a.Name)
	case !commandName.MatchString(a.Binary):
		return fmt.Errorf("adapter %s: binary %q: not a command name", a.Name, a.Binary)
	case !slices.Contains(computeapi.BoxTemplates(), a.Template):
		return fmt.Errorf("adapter %s: template %q: not one of %v", a.Name, a.Template, computeapi.BoxTemplates())
	case len(a.Secrets) == 0:
		return fmt.Errorf("adapter %s: no credential names", a.Name)
	case len(a.Argv) == 0 || a.Argv[0] != a.Binary:
		return fmt.Errorf("adapter %s: argv must start with its binary %s", a.Name, a.Binary)
	case a.Login == "":
		return fmt.Errorf("adapter %s: no login command", a.Name)
	}
	for _, s := range a.Secrets {
		if !secretName.MatchString(s) {
			return fmt.Errorf("adapter %s: credential name %q: want [A-Z][A-Z0-9_]*", a.Name, s)
		}
	}
	return nil
}

// Accepts says whether name is one of a's credential names.
func (a Adapter) Accepts(name string) bool { return slices.Contains(a.Secrets, name) }

// CheckSecret reports what is wrong with a credential sent as name: empty,
// or with its name over the secrets door's 64 KiB.
func (a Adapter) CheckSecret(name, value string) error {
	switch {
	case !a.Accepts(name):
		return fmt.Errorf("%s takes a credential named one of %v, not %s", a.Name, a.Secrets, name)
	case value == "":
		return fmt.Errorf("the %s credential is empty", name)
	case len(name)+len(value) > protocol.SecretsMax:
		return fmt.Errorf("the %s credential is over 64 KiB with its name", name)
	}
	return nil
}

// What a task leaves in its directory in the box: OutDir, holding either
// Bundle (the commits since the pushed commit) or NoChanges (it made none).
const (
	OutDir    = "out"
	Bundle    = "task.bundle"
	NoChanges = "none"
	// BundleRef is the ref the bundle names: the box's HEAD at the end.
	BundleRef = "HEAD"
	// UncommittedMessage is the message of the commit that carries what the
	// agent left uncommitted.
	UncommittedMessage = "potluck agent-task: changes the agent left uncommitted"
	// ExitNotStarted is Entry's exit code when it could not start the agent
	// (its out directory is then missing or empty). Entry writes the number
	// itself; TestEntryNotStarted runs it to keep the two the same.
	ExitNotStarted = 125
)

// Entry is the box's side of a task, run as
//
//	sh -c Entry potluck-agent-task <dir> <checkout> <base> <secret name> <argv...>
//
// <dir> is the task's directory (holding the file prompt) and <checkout>
// the pushed checkout, each absolute or under HOME; <base> is the pushed
// commit. It exports the credential from its file under the command's
// secrets directory, runs argv in the checkout with the prompt on stdin,
// commits what the agent left uncommitted as one more commit, and writes
// the commits since base as a bundle into <dir>/out (or out/none when there
// are none). It exits with the agent's exit code; ExitNotStarted when it
// could not start the agent.
const Entry = `case "$1" in /*) d=$1 ;; *) d=$HOME/$1 ;; esac
case "$2" in /*) co=$2 ;; *) co=$HOME/$2 ;; esac
base=$3 name=$4
shift 4
s=` + protocol.ShSecretsDir + `
if [ -z "$s" ] || [ ! -r "$s/$name" ]; then echo "potluck: the task's $name did not arrive in the box" >&2; exit 125; fi
cd "$co" || exit 125
out=$d/` + OutDir + `
rm -rf "$out" && mkdir -p "$out" || exit 125
git config user.name >/dev/null 2>&1 || git config user.name "potluck agent-task"
git config user.email >/dev/null 2>&1 || git config user.email "agent-task@potluck.invalid"
v=$(cat "$s/$name") || exit 125
export "$name=$v"
v=
"$@" < "$d/prompt"
code=$?
unset "$name"
if [ -n "$(git status --porcelain)" ]; then
  git add -A && git commit -q --no-verify -m "` + UncommittedMessage + `" || echo "potluck: could not commit what the agent left uncommitted" >&2
fi
if [ "$(git rev-parse HEAD)" = "$base" ]; then
  : > "$out/` + NoChanges + `"
elif ! git bundle create "$out/` + Bundle + `" "$base..` + BundleRef + `"; then
  rm -f "$out/` + Bundle + `"
  echo "potluck: could not bundle the agent's commits" >&2
fi
exit "$code"`
