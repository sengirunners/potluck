// Package agentrunners starts a coding agent vendor's self-hosted runner as
// a job in a fresh box: the generic agent runner job kind. A vendor's
// runner pulls its work from the vendor (it polls; nothing calls in), so a
// job is one box running one runner that serves one session and exits.
//
// What is generic lives here: checking the box has what the adapter needs,
// writing the job's files, and starting the adapter's entry point with the
// path of the renter's vendor key, which arrives as that one spawn's secret
// (the secrets door: grants, sealing, trusted machines only). What is a
// vendor's is an Adapter, which is data: its files, its secret's name and
// the commands it needs. Placement, the lease, following output and release
// are the caller's (potluck agent-runner), as for any box job.
//
// The package speaks to a box through Box, so the same start runs through
// the daemon (the CLI) and straight against a driver (the conformance
// suite's agent-runner group).
package agentrunners

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/potluck/protocol"
)

// Box is what a job needs from a box: exec (short commands, stdin as text)
// and spawn (a background process with secrets of its own and a time limit
// in seconds, 0 for the box's life).
type Box interface {
	Exec(ctx context.Context, argv []string, stdin string) (exit int, stdout, stderr string, err error)
	Spawn(ctx context.Context, argv []string, secrets map[string]string, timeoutS int, requestID string) (pid string, err error)
}

// File is one file of a job's directory. Mode is 0o700 (a script) or 0o600.
type File struct {
	Data string
	Mode int
}

// Adapter is one vendor's runner, as data.
type Adapter struct {
	// Name is what potluck agent-runner takes: [a-z][a-z0-9-]*.
	Name string
	// About is one line naming the vendor's runner.
	About string
	// Secret is the vendor key's file name in the job's secrets directory
	// ([A-Z][A-Z0-9_]*, the secrets door's rule; what a grant names).
	Secret string
	// Needs are commands the box must have on PATH (the image ships them).
	Needs []string
	// Files are the job directory's files for o. The one named "run" is the
	// entry point, started as: run <secret file> <job directory>.
	Files func(o Options) (map[string]File, error)
}

// Options are a job's settings an adapter turns into its runner's flags.
type Options struct {
	// Mode is the permission mode sessions start in, as the vendor names it
	// ("" pins none: the vendor's default).
	Mode string
	// Allow and Deny are tools the agent may use without asking, and may
	// never use, as the vendor names them (Claude Code: "Bash(git push:*)").
	Allow, Deny []string
	// Idle ends a runner no work reached in this long (0: never).
	Idle time.Duration
	// RetireAt is when the runner stops taking work and hands back what it
	// holds, before the job's time limit ends it (zero: never).
	RetireAt time.Time
}

// Entry is the name of the adapter file a job starts.
const Entry = "run"

var (
	adapterName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	secretName  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	fileName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	jobKey      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
)

// adapters is every adapter potluck agent-runner offers, by name.
var adapters = []Adapter{claude}

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
	case !secretName.MatchString(a.Secret):
		return fmt.Errorf("adapter %s: secret name %q: want [A-Z][A-Z0-9_]*", a.Name, a.Secret)
	case a.Files == nil:
		return fmt.Errorf("adapter %s: no files", a.Name)
	}
	for _, c := range a.Needs {
		if !commandName.MatchString(c) {
			return fmt.Errorf("adapter %s: needs %q: not a command name", a.Name, c)
		}
	}
	return nil
}

// CheckSecret reports what is wrong with a vendor key for a: empty, or with
// the adapter's name over the secrets door's 64 KiB.
func (a Adapter) CheckSecret(secret string) error {
	switch {
	case secret == "":
		return fmt.Errorf("no %s: the vendor key is empty", a.Secret)
	case len(a.Secret)+len(secret) > protocol.SecretsMax:
		return fmt.Errorf("the vendor key is over 64 KiB with its name %s", a.Secret)
	}
	return nil
}

// Check reports what is wrong with o: a tool is one line of at most 256
// bytes, never empty and never NUL; durations and times are not negative.
func (o Options) Check() error {
	for _, l := range [][]string{o.Allow, o.Deny} {
		for _, t := range l {
			if t == "" || len(t) > 256 || strings.ContainsAny(t, "\n\r\x00") || !utf8.ValidString(t) {
				return fmt.Errorf("tool %q: one line of 1 to 256 bytes", t)
			}
		}
	}
	if strings.ContainsAny(o.Mode, "\n\r\x00") || len(o.Mode) > 64 {
		return fmt.Errorf("mode %q: one short line", o.Mode)
	}
	if o.Idle < 0 {
		return fmt.Errorf("idle time %v is negative", o.Idle)
	}
	return nil
}

// Job is one runner to start in a box.
type Job struct {
	Adapter Adapter
	Options Options
	// Secret is the renter's vendor key, its bytes exactly. It goes to the
	// box only as the spawn's secret, never in argv, a file Potluck writes,
	// or the environment.
	Secret string
	// Key names the job: its directory and its spawn's request id
	// ([a-z0-9][a-z0-9-]*).
	Key string
	// TimeoutS is the runner's time limit in seconds (0: the box's life).
	TimeoutS int
}

// Dir is the job's directory, under the box's HOME.
func (j Job) Dir() string { return ".potluck/agent-runner/" + j.Key }

// Start checks the box has what the adapter needs, writes the job's files
// and starts the entry point in the background. It answers the process's
// pid; the caller follows its output and ends the box.
func Start(ctx context.Context, b Box, j Job) (pid string, err error) {
	if err := j.Adapter.Check(); err != nil {
		return "", err
	}
	if err := j.Options.Check(); err != nil {
		return "", err
	}
	if !jobKey.MatchString(j.Key) {
		return "", fmt.Errorf("job key %q: want [a-z0-9][a-z0-9-]*", j.Key)
	}
	if err := j.Adapter.CheckSecret(j.Secret); err != nil {
		return "", err
	}
	files, err := j.Adapter.Files(j.Options)
	if err != nil {
		return "", fmt.Errorf("adapter %s: %w", j.Adapter.Name, err)
	}
	if _, ok := files[Entry]; !ok {
		return "", fmt.Errorf("adapter %s: no %q file", j.Adapter.Name, Entry)
	}
	if err := needs(ctx, b, j.Adapter); err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for n, f := range files {
		switch {
		case !fileName.MatchString(n):
			return "", fmt.Errorf("adapter %s: file name %q: want [a-z][a-z0-9-]*", j.Adapter.Name, n)
		case f.Mode != 0o700 && f.Mode != 0o600:
			return "", fmt.Errorf("adapter %s: file %s: mode %o, want 700 or 600", j.Adapter.Name, n, f.Mode)
		case !utf8.ValidString(f.Data):
			return "", fmt.Errorf("adapter %s: file %s is not text", j.Adapter.Name, n)
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		exit, _, stderr, err := b.Exec(ctx, []string{"sh", "-c", writeFile, "potluck-agent-runner", j.Dir(), n, fmt.Sprintf("%o", files[n].Mode)}, files[n].Data)
		if err != nil {
			return "", fmt.Errorf("write the job's %s: %w", n, err)
		}
		if exit != 0 {
			return "", fmt.Errorf("write the job's %s: exit %d: %s", n, exit, oneLine(stderr))
		}
	}
	return b.Spawn(ctx, []string{"sh", "-c", launch, "potluck-agent-runner", j.Dir(), j.Adapter.Secret},
		map[string]string{j.Adapter.Secret: j.Secret}, j.TimeoutS, j.Key)
}

// needs refuses a box lacking a command the adapter needs, naming them.
func needs(ctx context.Context, b Box, a Adapter) error {
	missing, err := Missing(ctx, b, a.Needs...)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("the box has no %s on PATH: the %s adapter needs an image that ships it (%s)", strings.Join(missing, ", "), a.Name, a.About)
	}
	return nil
}

// Missing asks the box which of cmds are not commands on its PATH, and
// answers those (none: every one is there). Any box job that needs a
// command its image ships checks it so before starting (potluck agent-task
// too).
func Missing(ctx context.Context, b Box, cmds ...string) ([]string, error) {
	if len(cmds) == 0 {
		return nil, nil
	}
	exit, out, stderr, err := b.Exec(ctx, append([]string{"sh", "-c", probe, "potluck-probe"}, cmds...), "")
	if err != nil {
		return nil, fmt.Errorf("check the box for %s: %w", strings.Join(cmds, ", "), err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("check the box for %s: exit %d: %s", strings.Join(cmds, ", "), exit, oneLine(stderr))
	}
	return strings.Fields(out), nil
}

// probe prints each of its arguments that is not a command on PATH.
const probe = `for c; do command -v "$c" >/dev/null 2>&1 || printf '%s\n' "$c"; done`

// writeFile writes stdin to $1/$2 (under HOME unless absolute) with mode $3,
// in a directory only the box's user reads.
const writeFile = `case "$1" in /*) d=$1 ;; *) d=$HOME/$1 ;; esac
umask 077
mkdir -p "$d" && cat > "$d/$2.tmp" && chmod "$3" "$d/$2.tmp" && mv -f "$d/$2.tmp" "$d/$2"`

// launch starts the job's entry point with its secret file's path: $1 the
// job's directory, $2 the secret's name. A missing secret ends the job (125)
// before the runner starts.
const launch = `case "$1" in /*) d=$1 ;; *) d=$HOME/$1 ;; esac
s=` + protocol.ShSecretsDir + `
if [ -z "$s" ] || [ ! -r "$s/$2" ]; then echo "potluck: the job's $2 did not arrive in the box" >&2; exit 125; fi
cd "$d" || exit 125
exec "$d/run" "$s/$2" "$d"`

// ArgsFile is a file of arguments, one per line, that an adapter's script
// appends to "$@" with ReadArgs: no value is ever spliced into a script.
func ArgsFile(args []string) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteString(a)
		b.WriteByte('\n')
	}
	return b.String()
}

// ReadArgs appends each line of the file named next to "$@", in the
// script's own shell (a redirected loop, never a pipe or a function, whose
// "$@" would be another's): ReadArgs + `"$dir/args"`.
const ReadArgs = `while IFS= read -r potluck_a || [ -n "$potluck_a" ]; do set -- "$@" "$potluck_a"; done < `

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
