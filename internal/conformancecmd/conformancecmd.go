// Package conformancecmd is the conformance suite's program,
// potluck-conformance (built under its old name zm-run-conformance too, from
// cmd/zm-run-conformance). It checks a run-target driver against SPEC 20:
//
//	potluck-conformance <driver> [--kind db|run|box] [--target name] [--config k=v]... [--json]
//	    run: --ref <ref> --commit <sha> --script <name> [--repo <remote>] [--worktree <dir>] [--remote]
//	    db:  [--ref <ref>] [--commit <sha>] [--db-probe <command>] [--outside-config k=v]...
//	    box: [--ref <ref> --commit <sha> [--repo <remote>]] (the checkout check; without them a SKIP)
//	    [--reclaim-config k=v]... [--reclaim-in <duration>]
//	    [--ttl-short <seconds>] [--grace <duration>] [--run-wait <duration>] [--only <group,group>] [--strict]
//	    [--state-dir <dir>]... (box secrets: where the driver keeps its records, searched for a value)
//	potluck-conformance --print-probe
//
// <driver> is a path or a name ("scratch" finds potluck-driver-scratch, else zm-run-scratch, on PATH).
// Exit 0 = conformant (read any SKIP lines), 1 = a check failed, 2 = usage.
// Every run ends by listing its skipped checks by name (with --json, on
// stderr; stdout stays one JSON array). --strict fails the run (exit 1) when
// a network or expiry check was skipped, a group of them did not run, or (for
// a run or a box) --net-forbid or --net-peers was not given
// (conformance.StrictGroups); it also runs a box's no-call expiry check, which
// waits out --grace.
//
// It makes and ends real resources, so point it at a throwaway binding. The
// commit must hold the probe script (--print-probe prints it), which --script
// names as the driver resolves names. Pick an older pushed commit than the tip
// of --ref, and name the remote with --repo: the suite checks there that they
// differ, else the HEAD check is a SKIP (a driver ignoring commit would pass).
// --worktree is dirtied for one run and put back; without it the
// dirty-tree check is skipped. --remote says a run shares no filesystem with
// this command, which skips the heartbeat checks. --db-probe is a shell
// command given the url in $POTLUCK_CONFORMANCE_URL (and its old name
// $ZM_CONFORMANCE_URL), exiting 0 when the database is
// there (default: psql, when on PATH). --outside-config is laid over --config
// for one acquire that points the target at a host not in create_in; "{port}"
// in a value becomes a port this command listens on, to see nothing connects.
// --reclaim-config is laid over --config for the reclaim checks: a reclaim
// policy that says no once "{window}" closes, which the suite fills with
// HH:MM:SS-HH:MM:SS (local time) ending --reclaim-in after the acquire.
package conformancecmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/conformance"
)

type kv map[string]string

func (m kv) String() string { return fmt.Sprint(map[string]string(m)) }
func (m kv) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	m[k] = v
	return nil
}

const defaultProbe = `psql "$POTLUCK_CONFORMANCE_URL" -Atc 'select 1' >/dev/null 2>&1`

// shellProbe sees a database through a shell command. The url goes in the
// environment, never in argv, where any process could read it.
func shellProbe(command string) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, url string) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Env = append(os.Environ(), "POTLUCK_CONFORMANCE_URL="+url, "ZM_CONFORMANCE_URL="+url)
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &ee) && ee.ExitCode() != 127:
			return false, nil
		}
		return false, err
	}
}

// Main runs the program on argv (without the program name) and returns its
// exit code.
func Main(argv []string) int { return run(argv) }

// stdout and stderr are where results go; tests replace them.
var stdout, stderr io.Writer = os.Stdout, os.Stderr

func run(argv []string) int {
	fs := flag.NewFlagSet("potluck-conformance", flag.ContinueOnError)
	opt := conformance.Options{Config: map[string]string{}, OutsideConfig: map[string]string{}, ReclaimConfig: map[string]string{}}
	var asJSON, printProbe bool
	var dbProbe, only string
	fs.StringVar(&opt.Kind, "kind", "", "db, run or box (default: what describe answers; a driver serving more than one needs this)")
	fs.StringVar(&opt.Target, "target", "", "the binding name sent in config (default potluck-conformance)")
	fs.Var(kv(opt.Config), "config", "driver config key=value, sent with every request (repeatable)")
	fs.StringVar(&opt.Ref, "ref", "", "the ref to send (required for a run)")
	fs.StringVar(&opt.Commit, "commit", "", "the pushed commit to send: it holds the probe script (required for a run)")
	fs.StringVar(&opt.Repo, "repo", "", "the binding's remote: the suite asks the ref's tip there to prove the HEAD check (without it, a SKIP)")
	fs.StringVar(&opt.Script, "script", "", "the probe script's name, as the driver resolves names (required for a run)")
	fs.StringVar(&opt.Worktree, "worktree", "", "the checkout a driver's run starts from: dirtied for one run, then put back")
	fs.BoolVar(&opt.Remote, "remote", false, "a run shares no filesystem with this command: skip the heartbeat checks")
	fs.StringVar(&dbProbe, "db-probe", "", "shell command, url in $POTLUCK_CONFORMANCE_URL (also $ZM_CONFORMANCE_URL), exit 0 when the database is there (default psql)")
	fs.Var(kv(opt.OutsideConfig), "outside-config", `config key=value pointing at a host not in create_in; "{port}" is a port this command listens on (repeatable)`)
	fs.Var(kv(opt.ReclaimConfig), "reclaim-config", `config key=value whose reclaim policy says no once "{window}" (HH:MM:SS-HH:MM:SS) closes (repeatable)`)
	fs.DurationVar(&opt.ReclaimIn, "reclaim-in", 0, "how long the reclaim window stays open; must outlast an acquire (default 1m)")
	fs.IntVar(&opt.ShortTTL, "ttl-short", 0, "ttl_s of the expiry and renew checks (default 5)")
	fs.DurationVar(&opt.Grace, "grace", 0, "how far past expires_at a target may take to end a resource (default 30s)")
	fs.DurationVar(&opt.RunWait, "run-wait", 0, "how long a run may take to show up or finish (default 5m)")
	fs.Var((*addrs)(&opt.NetAllow), "net-allow", "host:port ([v6]:port) a run or box must reach, proving it has a network (repeatable)")
	fs.Var((*addrs)(&opt.NetForbid), "net-forbid", "host:port ([v6]:port) a run or box must not reach: a LAN host, this host's LAN address, a tailnet or metadata address; it must open from here, and loopback, unspecified and link-local addresses are SKIPs (repeatable)")
	fs.Var((*mailAddrs)(&opt.NetMail), "net-mail", "an outside mail server, host (on 25, 465 and 587) or host:port, a run or box must not open; a SKIP unless it opens from here (repeatable)")
	fs.Var((*names)(&opt.NetResolve), "net-resolve", "a DNS name (or name:port, opened where the guest has no getent; default 443) a run or box must resolve; default with --net-allow: its names, or github.com (repeatable)")
	fs.BoolVar(&opt.NetPeers, "net-peers", false, "run two runs or boxes at once and check neither reaches the other (IPv4 /24 and IPv6 link-local neighbours)")
	fs.IntVar(&opt.NetPeerPort, "net-peer-port", 0, "the port the listening run uses for --net-peers (default 47123)")
	fs.StringVar(&only, "only", "", "comma list of check groups: "+strings.Join(conformance.Groups, ", "))
	fs.BoolVar(&asJSON, "json", false, "print results as JSON")
	fs.Var((*dirs)(&opt.StateDirs), "state-dir", "a directory the driver keeps its records and logs in, searched for a box secret's value (repeatable; default: config state_dir and ~/.potluck-driver-<name>, where they exist)")
	fs.BoolVar(&opt.Strict, "strict", false, "fail (exit 1) when a network or expiry check was skipped or did not run, or a run or box lacks --net-forbid or --net-peers")
	fs.BoolVar(&printProbe, "print-probe", false, "print the probe script to commit, and exit")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: potluck-conformance <driver-path-or-name> [--kind db|run|box] [--config k=v]... [--ref r --commit sha --script name] [--json] [--strict]")
		fs.PrintDefaults()
	}
	// Accept flags before or after the driver argument.
	var target string
	for len(argv) > 0 {
		if err := fs.Parse(argv); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		if target != "" {
			fs.Usage()
			return 2
		}
		target, argv = fs.Arg(0), fs.Args()[1:]
	}
	if printProbe {
		_, _ = io.WriteString(stdout, conformance.ProbeScript) // the script's "date +%s" is no format
		return 0
	}
	if target == "" {
		fs.Usage()
		return 2
	}
	if only != "" {
		opt.Only = strings.Split(only, ",")
	}
	path, err := conformance.Find(target)
	if err != nil {
		fmt.Fprintln(stderr, "potluck-conformance:", err)
		return 2
	}
	switch _, perr := exec.LookPath("psql"); {
	case dbProbe != "":
		opt.DBExists = shellProbe(dbProbe)
	case perr == nil:
		opt.DBExists = shellProbe(defaultProbe)
	}
	// SIGTERM too: a cut-off run must still put --worktree back and end what it made.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if opt.Kind == "" {
		opt.Kind = conformance.Kind(ctx, path, opt) // so every call after carries config.kind
	}
	if err := opt.Validate(opt.Kind); err != nil {
		fmt.Fprintln(stderr, "potluck-conformance:", err)
		return 2
	}
	res := conformance.Run(ctx, path, opt)
	// The summary: on stdout in text mode, on stderr with --json, where
	// stdout stays one JSON array of results.
	sum := stdout
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(res)
		sum = stderr
	} else {
		fmt.Fprintf(stdout, "driver %s\n%s", path, conformance.Format(res))
	}
	if sk := conformance.SkippedResults(res); len(sk) > 0 {
		fmt.Fprintf(sum, "%d check(s) skipped: not proven\n", len(sk))
		for _, r := range sk {
			if r.Group != "" {
				fmt.Fprintf(sum, "  - %s (%s)\n", r.Name, r.Group)
			} else {
				fmt.Fprintf(sum, "  - %s\n", r.Name)
			}
		}
	}
	n := conformance.Failed(res)
	if n > 0 {
		fmt.Fprintf(sum, "%d check(s) failed\n", n)
	}
	if opt.Strict {
		for _, r := range res {
			if r.Name == conformance.StrictCheck && !r.OK {
				fmt.Fprintf(sum, "--strict failed: %s\n", r.Detail)
			}
		}
	}
	if n > 0 {
		return 1
	}
	return 0
}

// mailAddrs is a repeatable --net-mail flag: a host or host:port.
type mailAddrs []string

func (a *mailAddrs) String() string { return strings.Join(*a, ",") }

func (a *mailAddrs) Set(v string) error {
	as, err := conformance.MailAddrs(v)
	*a = append(*a, as...)
	return err
}

// names is a repeatable --net-resolve flag: a DNS name, or name:port.
type names []string

func (n *names) String() string { return strings.Join(*n, ",") }

func (n *names) Set(v string) error {
	if err := conformance.ResolveName(v); err != nil {
		return err
	}
	*n = append(*n, v)
	return nil
}

// dirs is a repeatable directory flag.
type dirs []string

func (d *dirs) String() string { return strings.Join(*d, ",") }

func (d *dirs) Set(v string) error {
	if st, err := os.Stat(v); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", v)
	}
	*d = append(*d, v)
	return nil
}

// addrs is a repeatable host:port flag.
type addrs []string

func (a *addrs) String() string { return strings.Join(*a, ",") }

func (a *addrs) Set(v string) error {
	if err := conformance.NetAddr(v); err != nil {
		return err
	}
	*a = append(*a, v)
	return nil
}
