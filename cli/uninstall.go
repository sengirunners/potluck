package cli

// potluck uninstall: take Potluck off this machine. It undoes, in an order
// that never strands a later step, what install.sh and the first commands put
// here: what the machine shares, the pools it joined, the managed Softnet (the
// one root step), the daemon, the daily update job, the line install.sh added
// to a shell startup file, the host override file, the home and the programs.
// It says first what it will do and asks; it never starts a daemon; every
// step is safe to run again. What it cannot remove for the person (Tart's
// images, which may be theirs, a menu bar app's login item) it names at the
// end (docs/lending.md).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
)

// InstalledPrograms are the programs a release puts in the install directory
// (scripts/release builds them; a test holds the two lists equal), each kept
// beside a <name>.prev after an upgrade.
var InstalledPrograms = []string{"potluck", "potluck-driver-local", "potluck-driver-tart", "potluck-conformance", "potluck-e2b"}

// pathMarker is the comment line install.sh writes above the line that puts
// its install directory on PATH.
const pathMarker = "# Potluck (install.sh): put potluck on PATH"

// pathStartupFiles are the files, below the user's home, install.sh may have
// added that line to (its path_setup).
var pathStartupFiles = []string{".zshrc", ".bashrc", ".bash_profile", ".bash_login", ".profile", filepath.Join(".config", "fish", "conf.d", "potluck.fish")}

// uninstallStep is one thing uninstall does: what it says in the plan, and
// how it does it.
type uninstallStep struct {
	plan string
	do   func(ctx context.Context) error
}

type uninstallFlags struct{ yes, dryRun, keepHome, force, noSudo bool }

func (a *app) uninstallCmd() *cobra.Command {
	var f uninstallFlags
	cmd := &cobra.Command{
		Use:   "uninstall [--yes] [--keep-home] [--force] [--dry-run] [--no-sudo]",
		Short: "Remove Potluck from this machine: stop sharing, leave its pools, stop the daemon, remove its files",
		Long: `Uninstall takes Potluck off this machine. It prints what it will do and asks
before it does anything (--dry-run only prints; --yes does not ask). In this
order:

  1. stops a setup run (host) still going on, then stops sharing every size
     this machine lends (refused while a sandbox runs on one, unless --force);
  2. leaves every pool this machine joined, and asks that pool's machine to
     revoke this one's token;
  3. on a Mac, removes the managed Softnet (the one root step; it asks for
     your Mac password);
  4. stops the daemon;
  5. removes the Softnet download softnet install --no-sudo kept, the daily
     update job, the host override file and the lines install.sh added to a
     shell startup file;
  6. removes the home (default ~/.potluck): your identity, the pools this
     machine runs and everything they hold, so the pools end for their
     members. --keep-home keeps it;
  7. removes the programs install.sh put in the directory this potluck runs
     from (potluck, its drivers, potluck-conformance, potluck-e2b and their
     .prev copies), this one last.

A step that fails stops the run and says so; run it again to go on, since
every step is safe to repeat. It never starts a daemon: with none running,
nothing is unshared and no pool is left by step 1 and 2, and the pools this machine joined are not told
(their owners can revoke its token). It does not touch Tart or Tart's images
(they may be yours), Tailscale, or a menu bar app; it names what is left.

--no-sudo runs no sudo: at the root step (3) it stops, with the steps before
it done, and prints the root command; run that as root, then uninstall
again to go on. With --json the answer is {"done": [...], "admin_step":
{"why": ..., "argv": [...]}, "left": [...], "not_removed": [...]}: the steps
done, the root step when one is left, the steps left (that one first) and
what uninstall does not remove; the words for people go to stderr. A step
that fails adds "error" and is the first of "left" (the exit is not 0); when
--force would let it go on, "force" says what that does and the arguments.
With --json --dry-run, "left" is the plan, "pools_run" the pools this machine
runs, "warning" says what is lost for good, and "remove" is the question to
ask and the arguments that run the plan.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.uninstall(cmd.Context(), f) },
	}
	cmd.Flags().BoolVar(&f.yes, "yes", false, "do not ask")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print what it would do and change nothing")
	cmd.Flags().BoolVar(&f.keepHome, "keep-home", false, "keep the home: your identity and the pools this machine runs")
	cmd.Flags().BoolVar(&f.noSudo, "no-sudo", false, "do not run sudo: stop at the root step and print it (with --json, as admin_step) for you or an app to run as root")
	cmd.Flags().BoolVar(&f.force, "force", false, "stop sharing a size even while sandboxes run on one joined pool's machine (they go on until their lease ends)")
	return cmd
}

// uninstallAnswer is uninstall's --json answer: the plans of the steps done,
// the root step when --no-sudo left one, the plans of the steps left (that
// one first), and what uninstall does not remove. A step that failed leaves
// Error set and is the first of Left (it may be partly done), with Force set
// when --force would let it go on. A dry run sets Remove: Left is the plan.
type uninstallAnswer struct {
	Done       []string   `json:"done"`
	AdminStep  *adminStep `json:"admin_step,omitempty"`
	Left       []string   `json:"left"`
	NotRemoved []string   `json:"not_removed"`
	Error      string     `json:"error,omitempty"`
	// PoolsRun are the pools this machine runs (empty also when they could
	// not be read; Warning then says so).
	PoolsRun []string `json:"pools_run"`
	// Warning is what removing the home loses for good ("" when the plan
	// keeps the home or there is none).
	Warning string           `json:"warning,omitempty"`
	Remove  *uninstallAction `json:"remove,omitempty"`
	Force   *uninstallAction `json:"force,omitempty"`
}

// uninstallAction is what an app may run after the person said yes to
// Confirm (Detail says more): potluck's arguments.
type uninstallAction struct {
	Confirm string   `json:"confirm"`
	Detail  string   `json:"detail,omitempty"`
	Args    []string `json:"args"`
}

// action is the run an app may start with these flags, its question first.
func (f uninstallFlags) action(confirm, detail string) *uninstallAction {
	args := []string{"--json", "uninstall", "--yes", "--no-sudo"}
	if f.keepHome {
		args = append(args, "--keep-home")
	}
	if f.force {
		args = append(args, "--force")
	}
	return &uninstallAction{Confirm: confirm, Detail: strings.TrimSpace(detail), Args: args}
}

// uninstallPlan is what uninstall does on this machine, in order, and what
// the person should know of it.
type uninstallPlan struct {
	steps    []uninstallStep
	left     []string // not removed, as people-words
	poolsRun []string
	warning  string
	home     string   // the home, when there is one
	stopsRun bool     // the first step stops a host run going on
	running  bool     // its daemon answered
	joined   []string // the pools it joined, when running
	// joinedNow reads those again (nil: not running).
	joinedNow func(ctx context.Context) ([]string, error)
	// notes are what the step running could not do though it goes on, as
	// people-words: they go with it in the answer's done.
	notes *[]string
}

func (a *app) uninstall(ctx context.Context, f uninstallFlags) error {
	if err := a.notWhileAttached(a.cmd("uninstall")); err != nil {
		return err
	}
	var ans uninstallAnswer
	err := a.wordsAside(func() (err error) { ans, err = a.runUninstall(ctx, f); return err })
	switch {
	case !a.asJSON():
		return err
	case err != nil && ans.Error == "":
		return err // nothing was done
	case err != nil:
		if perr := a.printJSON(ans); perr != nil {
			return perr
		}
		return err
	}
	return a.printJSON(ans)
}

// runUninstall plans and runs uninstall's steps, saying each for people.
func (a *app) runUninstall(ctx context.Context, f uninstallFlags) (uninstallAnswer, error) {
	ans := uninstallAnswer{Done: []string{}, Left: []string{}, NotRemoved: []string{}, PoolsRun: []string{}}
	cfg, err := a.loadClient()
	if err != nil {
		return ans, err
	}
	p, err := a.planUninstall(ctx, cfg, f)
	if err != nil {
		return ans, err
	}
	steps, left := p.steps, p.left
	ans.NotRemoved = append(ans.NotRemoved, left...)
	ans.PoolsRun, ans.Warning = append(ans.PoolsRun, p.poolsRun...), p.warning
	plans := func(steps []uninstallStep) []string {
		out := []string{}
		for _, s := range steps {
			out = append(out, s.plan)
		}
		return out
	}
	if len(steps) == 0 {
		a.say("nothing of potluck's is on this machine that %s removes", a.cmd("uninstall"))
	} else {
		a.say("%s will:", a.cmd("uninstall"))
		for i, s := range steps {
			a.say("  %d. %s", i+1, s.plan)
		}
	}
	if p.warning != "" {
		a.say("%s (--keep-home keeps this.)", p.warning)
	}
	if f.dryRun {
		a.sayLeftOver(left)
		ans.Left = plans(steps)
		if len(steps) > 0 {
			ans.Remove = f.action("Remove Potluck from this machine?", p.warning)
		}
		return ans, nil
	}
	if len(steps) > 0 && !f.yes {
		if err := a.confirmUninstall(); err != nil {
			return ans, err
		}
	}
	release := func() {}
	defer func() { release() }()
	for i, s := range steps {
		var err error
		if i == 0 && !p.stopsRun {
			release, err = holdHost(ctx, p)
		}
		if err == nil {
			err = s.do(ctx)
		}
		if err == nil && i == 0 && p.stopsRun {
			release, err = holdHost(ctx, p)
		}
		var root *adminStepLeft
		if errors.As(err, &root) {
			ans.AdminStep, ans.Left = &root.step, plans(steps[i:])
			a.say("--no-sudo: stopped at step %d, the root step, with the steps before it done. %s\nRun this as root, then run %s again to finish:\n  %s",
				i+1, root.step.Why, a.cmd("uninstall --no-sudo"), root.step.command())
			return ans, nil
		}
		if err != nil {
			var busy *needsForce
			if errors.As(err, &busy) && !f.force {
				forced := f
				forced.force = true
				ans.Force = forced.action("Remove Potluck though sandboxes run, or may run, on this machine?", "They go on until their lease ends, out of reach of the members who made them.")
			}
			err = fmt.Errorf("step %d (%s) did not finish: %w\nnothing after it was done; fix that and run %s again (each step is safe to repeat)", i+1, s.plan, err, a.cmd("uninstall"))
			ans.Left, ans.Error = plans(steps[i:]), err.Error()
			return ans, err
		}
		done := s.plan
		if n := *p.notes; len(n) > 0 {
			done += " (but: " + strings.Join(n, "; ") + ")"
		}
		ans.Done, *p.notes = append(ans.Done, done), nil
	}
	a.sayLeftOver(left)
	return ans, nil
}

// holdHost keeps every other host run off the plan's home until release: one
// started during the removal would share again, or write the home again.
func holdHost(ctx context.Context, p uninstallPlan) (release func(), err error) {
	if p.home == "" {
		return func() {}, nil
	}
	lock, err := takeLock(filepath.Join(p.home, hostLockName))
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = errors.New("another host run started meanwhile")
		}
		return func() {}, err
	}
	// This holds the lock and is no host run: what a killed one left here
	// must not name another process to stopHostRun.
	for _, n := range []string{hostPIDName, hostBgPIDName} {
		_ = os.Remove(filepath.Join(p.home, n))
	}
	if !p.running && homeLockHeld(p.home) {
		lock.Close()
		return func() {}, errors.New("the host run started the daemon meanwhile")
	}
	// A host run may have joined a pool since: one the plan does not leave.
	if p.joinedNow != nil {
		now, err := p.joinedNow(ctx)
		if err == nil {
			if i := slices.IndexFunc(now, func(s string) bool { return !slices.Contains(p.joined, s) }); i >= 0 {
				err = fmt.Errorf("a host run joined %s meanwhile, which this plan does not leave", now[i])
			}
		}
		if err != nil {
			lock.Close()
			return func() {}, err
		}
	}
	return func() { lock.Close() }, nil
}

// confirmUninstall reads "yes" from stdin.
func (a *app) confirmUninstall() error { return a.confirmYes("Type yes to do this: ", "removed") }

// confirmYes asks prompt on stderr and reads the answer from stdin: nil for
// yes, else an error saying nothing was done (a word like "removed").
func (a *app) confirmYes(prompt, done string) error {
	fmt.Fprint(a.host.Stderr, prompt)
	line, err := bufio.NewReader(a.host.Stdin).ReadString('\n')
	if strings.TrimSpace(line) == "yes" {
		return nil
	}
	why := "the answer was not yes"
	if err != nil && line == "" {
		why = "no answer"
	}
	return fmt.Errorf("nothing was %s: %s; run it again and type yes, or pass --yes", done, why)
}

// uninstallHealthWait is how long a daemon whose socket is there has to answer.
var uninstallHealthWait = 10 * time.Second

// planUninstall reads this machine (changing nothing) and returns what
// uninstall does, in order, and what the person should know of it.
func (a *app) planUninstall(ctx context.Context, cfg *config.Config, f uninstallFlags) (p uninstallPlan, err error) {
	var steps []uninstallStep
	var left []string
	p.notes = new([]string)
	note := func(format string, args ...any) {
		*p.notes = append(*p.notes, fmt.Sprintf(format, args...))
		a.say(format, args...)
	}
	home := cfg.Home
	homeExists := isDir(home)
	if homeExists {
		p.home = home
	}
	var c *client.Client
	running := false
	if homeExists {
		if c, err = a.readClient(); err != nil {
			return p, err
		}
		// No socket file, no daemon. One that is there must answer (a stale
		// one is "not running"); a busy daemon is not told to stop by deleting
		// its home.
		var herr error
		if _, serr := os.Lstat(cfg.SocketPath()); serr == nil {
			hctx, cancel := context.WithTimeout(ctx, uninstallHealthWait)
			_, herr = c.Health(hctx)
			cancel()
		} else {
			herr = serr
		}
		if herr != nil && !client.NotRunning(herr) && !errors.Is(herr, fs.ErrNotExist) {
			return p, fmt.Errorf("the daemon on %s did not answer (%w): it may be busy or starting; wait and run %s again, or stop it with %s", cfg.SocketPath(), herr, a.cmd("uninstall"), a.cmd("daemon stop"))
		}
		running = herr == nil
		p.running = running
		if !running && homeLockHeld(home) {
			return p, fmt.Errorf("a daemon still holds %s (its lock is taken) but does not answer on %s: wait for it, or stop it with %s, then run %s again", tildePath(home), cfg.SocketPath(), a.cmd("daemon stop"), a.cmd("uninstall"))
		}
	}

	// A setup run going on would share again, and write the home again.
	if homeExists && hostBusy(home) {
		p.stopsRun = true
		steps = append(steps, uninstallStep{
			plan: "stop the setup run (" + a.cmd("host") + ") going on",
			do:   func(ctx context.Context) error { return stopHostRun(ctx, home) },
		})
	}

	if running {
		plan := "stop sharing whatever this machine lends (as " + a.cmd("host stop") + " does)"
		if shared := sharedSizeNames(ctx, c); len(shared) > 0 {
			plan = "stop sharing " + strings.Join(shared, ", ") + " (as " + a.cmd("host stop") + " does)"
		}
		steps = append(steps, uninstallStep{
			plan: plan,
			do: func(ctx context.Context) error {
				removed, err := hostStop(ctx, c, "", f.force, a.host.Stderr, a.name())
				if len(removed) > 0 {
					a.say("no longer shared: %s", strings.Join(removed, ", "))
					if err != nil { // the sizes before the refusal are off
						err = fmt.Errorf("%w (already no longer shared: %s)", err, strings.Join(removed, ", "))
					}
				}
				return err
			},
		})
		p.joinedNow = func(ctx context.Context) ([]string, error) { return joinedPools(ctx, c) }
		joined, err := p.joinedNow(ctx)
		if err != nil {
			return p, err
		}
		p.joined = joined
		if len(joined) > 0 {
			steps = append(steps, uninstallStep{
				plan: "leave the pools this machine joined, asking each pool's machine to revoke this one's token: " + strings.Join(joined, ", "),
				do: func(ctx context.Context) error {
					for _, p := range joined {
						r, err := c.Leave(ctx, p)
						if err != nil {
							return fmt.Errorf("leave %s: %w", p, err)
						}
						if r.Revoked {
							a.say("left %s", p)
						} else {
							note("left %s (the pool's machine did not answer: its owner can revoke this machine's token there)", p)
						}
					}
					return nil
				},
			})
		}
	} else if homeExists {
		left = append(left, a.cmd("daemon")+" is not running, so uninstall could not tell the pools this machine joined that it is leaving, or stop sharing through the daemon: start it ("+a.cmd("daemon --detach")+") and run "+a.cmd("uninstall")+" again to do that, or ask each pool's owner to revoke this machine's token")
	}

	runs, known := poolsRun(ctx, c, running)
	if p.poolsRun = runs; homeExists && !f.keepHome {
		p.warning = homeWarning(runs, known)
	}

	if store := softnetStore(); softnetGOOS == "darwin" && lexists(store.Dir) {
		steps = append(steps, uninstallStep{
			plan: "remove the managed Softnet in " + store.Dir + " (the one root step: asks for your Mac password)",
			do:   func(ctx context.Context) error { _, err := a.removeManagedSoftnet(ctx, store, f.noSudo); return err },
		})
	}
	if d, err := softnetDownloadDir(); softnetGOOS == "darwin" && err == nil && lexists(d) {
		steps = append(steps, uninstallStep{
			plan: "remove the Softnet download " + a.cmd("softnet install --no-sudo") + " kept in " + tildePath(d),
			do: func(context.Context) error {
				if err := os.RemoveAll(d); err != nil {
					return err
				}
				_ = os.Remove(filepath.Dir(d))
				return nil
			},
		})
	}

	if running {
		steps = append(steps, uninstallStep{
			plan: "stop the daemon",
			do:   func(ctx context.Context) error { return a.stopDaemon(ctx, cfg) },
		})
	}

	if s, err := newUpdateSchedule(); err == nil && s.Supported() == nil && s.Installed() {
		steps = append(steps, uninstallStep{
			plan: "remove the daily update job (" + tildePath(s.Files()[0]) + ")",
			do: func(context.Context) error {
				removed, foreign, err := s.Remove()
				if removed {
					a.say("daily update removed")
				}
				if foreign != "" {
					note("another job on this machine also runs potluck upgrade and was left alone: %s", foreign)
				}
				return err
			},
		})
	}

	if p := hostOverrideFile(); lexists(p) {
		steps = append(steps, uninstallStep{
			plan: "remove the host override file " + p + " (what " + a.cmd("host set") + " and " + a.cmd("host pause") + " write)",
			do:   func(context.Context) error { return removeOverride(p) },
		})
	}

	if files := pathLineFiles(); len(files) > 0 {
		steps = append(steps, uninstallStep{
			plan: "remove the lines install.sh added to put its install directory on PATH, from " + strings.Join(tildeAll(files), ", "),
			do: func(context.Context) error {
				for _, p := range files {
					// A file that cannot be written (a read-only dotfiles
					// store) must not stop the home and the programs going.
					switch changed, err := removePathLine(p); {
					case err != nil:
						note("could not edit %s (%v): remove potluck's PATH lines from it yourself", tildePath(p), err)
					case changed:
						a.say("removed potluck's PATH line from %s", tildePath(p))
					}
				}
				return nil
			},
		})
	}

	// The driver's records say which Tart VMs are Potluck's sandboxes, and
	// nothing here can show that none runs on them (a pool's own sandboxes
	// are not all counted by stopping the shares), so the person removes them.
	if dir := tartDriverState(); dir != "" {
		left = append(left, "the Tart driver's records in "+tildePath(dir)+": they say which Tart VMs are Potluck's sandboxes, so they stay. When no sandbox runs: rm -r "+shellQuote(dir))
	}

	if homeExists && !f.keepHome {
		if err := checkHomeRemovable(home); err != nil {
			return p, err
		}
		steps = append(steps, uninstallStep{
			plan: "remove the home " + tildePath(home) + ": this machine's identity, the pools it runs and their data, and the daemon's log",
			do: func(context.Context) error {
				if homeLockHeld(home) {
					return fmt.Errorf("a daemon holds %s: stop it with %s", tildePath(home), a.cmd("daemon stop"))
				}
				if err := os.RemoveAll(home); err != nil {
					return err
				}
				// The daemon removes its own record when it stops; take the
				// directory it was in too, when nothing else is there.
				if d := daemonrecord.Dir(); d != "" {
					_ = os.Remove(d)
					_ = os.Remove(filepath.Dir(d))
				}
				a.say("removed %s", tildePath(home))
				return nil
			},
		})
	}

	if dir, names := installedProgramFiles(); len(names) > 0 {
		steps = append(steps, uninstallStep{
			plan: "remove the programs in " + tildePath(dir) + ": " + strings.Join(namesOf(names), ", ") + " (this one last)",
			do: func(context.Context) error {
				for _, p := range names {
					if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				}
				a.say("removed %d files from %s", len(names), tildePath(dir))
				return nil
			},
		})
	}
	p.steps, p.left = steps, append(left, a.leftOver()...)
	return p, nil
}

// joinedPools are the slugs of the pools this machine joined.
func joinedPools(ctx context.Context, c *client.Client) (joined []string, err error) {
	remotes, err := c.Remotes(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the pools this machine joined: %w", err)
	}
	for _, r := range remotes {
		if s := r.Project.Slug; !slices.Contains(joined, s) {
			joined = append(joined, s)
		}
	}
	return joined, nil
}

// sharedSizeNames are the sizes this machine shares with the pools it joined,
// by name (those hostStop removes first; none when the daemon cannot say).
func sharedSizeNames(ctx context.Context, c *client.Client) []string {
	l, err := computeclient.HostRunTargetsLive(ctx, c)
	if err != nil {
		return nil
	}
	var names []string
	for _, t := range l.Targets {
		if t.RunTarget.Host == "" && t.Kind == "box" {
			names = append(names, t.Name)
		}
	}
	return names
}

// leftOver is what uninstall does not remove, as people-words, for this
// machine.
func (a *app) leftOver() []string {
	var left []string
	if setupGOOS == "darwin" {
		if p, ok := vmhost.SystemTartSearch().FindSoftnet(); ok {
			if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSetuid != 0 {
				left = append(left, "the softnet at "+p+" is setuid root: Potluck's root step made it so, or you did. If you do not use Tart for other things, undo it with: sudo chmod u-s "+shellQuote(p))
			}
		}
		left = append(left,
			"Tart, its images and any VM it still holds (the macOS image Potluck pulled is about 27 GB, and the current one is never removed by "+a.cmd("host images --remove")+"): `tart list` shows them and `tart delete <name>` removes one. Potluck deletes none of them: some may be yours",
			"Potluck Menu, if you installed it: quit it from its menu, remove it from System Settings > General > Login Items, and delete the app (docs/menubar.md)",
		)
	}
	return append(left, "a Tailscale node named potluck-<this Mac's host name>-potluck, if you used Tailscale: remove it in the Tailscale admin console (https://login.tailscale.com/admin/machines)")
}

func (a *app) sayLeftOver(left []string) {
	if len(left) == 0 {
		return
	}
	a.say("not removed by %s:", a.cmd("uninstall"))
	for _, l := range left {
		a.say("  - %s", l)
	}
}

// poolsRun are the slugs of the pools this machine runs; known is false when
// its daemon does not say.
func poolsRun(ctx context.Context, c *client.Client, running bool) (runs []string, known bool) {
	if !running || c == nil {
		return nil, false
	}
	ps, err := c.LocalProjects(ctx)
	if err != nil {
		return nil, false
	}
	for _, p := range ps {
		runs = append(runs, p.Slug)
	}
	return runs, true
}

// homeWarning is what removing the home loses for good.
func homeWarning(runs []string, known bool) string {
	w := "This deletes this machine's identity for good."
	switch {
	case len(runs) > 0:
		w += " This machine runs these pools, and they end for all their members: " + strings.Join(runs, ", ") + "."
	case !known:
		w += " Any pool this machine runs ends for all its members; its daemon did not say which pools those are, so none can be named."
	}
	return w
}

// checkHomeRemovable refuses to remove a directory that does not look like a
// Potluck home, or that is the user's own home or a parent of it: a
// POTLUCK_HOME pointed at the wrong place must never take a person's files.
func checkHomeRemovable(home string) error {
	h, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	if uh, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(h, uh); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
			return fmt.Errorf("%s is your own home folder or holds it: not removing it (POTLUCK_HOME names the wrong place?)", h)
		}
	}
	// config.json alone proves nothing: a project folder has one. The
	// registry is the daemon's own file, and `program` names its owner.
	if lexists(filepath.Join(h, config.FileRegistry)) || config.HomeOwner(h) == config.Program {
		return nil
	}
	return fmt.Errorf("%s does not look like a Potluck home (no %s, and no %s naming %s, in it): not removing it", h, config.FileRegistry, config.FileProgram, config.Program)
}

// installedProgramFiles are the files of InstalledPrograms (and their .prev
// copies) that exist in the directory this potluck runs from, and that
// directory. None when this potluck was not run from an installed copy.
func installedProgramFiles() (dir string, files []string) {
	exe, err := executable()
	if err != nil {
		return "", nil
	}
	// The directory the person ran it from, not where a symlink leads: a
	// link to a build elsewhere must not take that build's directory along.
	// Linux reports the resolved path, so the one on PATH that leads to it
	// stands for it.
	if onPath, err := exec.LookPath("potluck"); err == nil {
		a, aerr := filepath.EvalSymlinks(onPath)
		b, berr := filepath.EvalSymlinks(exe)
		if aerr == nil && berr == nil && a == b {
			exe = onPath
		}
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", nil
	}
	dir = filepath.Dir(exe)
	if filepath.Base(exe) != "potluck" {
		return "", nil // go run, a test binary, a renamed copy: not an install
	}
	for _, n := range InstalledPrograms {
		for _, name := range []string{n, n + ".prev"} {
			if p := filepath.Join(dir, name); lexists(p) {
				files = append(files, p)
			}
		}
	}
	return dir, files
}

// pathLineFiles are the startup files below the user's home that hold the
// line install.sh added.
func pathLineFiles() []string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return nil
	}
	var out []string
	files := slices.Clone(pathStartupFiles)
	if z := os.Getenv("ZDOTDIR"); z != "" { // install.sh writes $ZDOTDIR/.zshrc when it is under HOME
		if rel, err := filepath.Rel(h, z); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			files = append(files, filepath.Join(rel, ".zshrc"))
		}
	}
	for _, f := range files {
		p := filepath.Join(h, f)
		if b, err := os.ReadFile(p); err == nil && slices.Contains(strings.Split(string(b), "\n"), pathMarker) {
			out = append(out, p)
		}
	}
	return out
}

// pathLines match, whole, the one line install.sh writes under its marker
// (its path_setup): export PATH="<dir>:$PATH", or fish_add_path "<dir>".
var pathLines = []*regexp.Regexp{regexp.MustCompile(`^export PATH="[^":]+:\$PATH"\n?$`), regexp.MustCompile(`^fish_add_path "[^"]+"\n?$`)}

// removePathLine takes the marker line, the blank line install.sh wrote before
// it, and the one line under it that puts a directory on PATH, out of the
// startup file p, keeping its mode and every other line, and writes the file
// by renaming a finished copy over it (through a symlink, to its target). A
// line under the marker that is something else is kept. A fish file left
// empty is removed.
func removePathLine(p string) (changed bool, err error) {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false, err
	}
	lines := strings.SplitAfter(string(b), "\n")
	var keep []string
	for i := 0; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\n") != pathMarker {
			keep = append(keep, lines[i])
			continue
		}
		if i+1 < len(lines) && slices.ContainsFunc(pathLines, func(x *regexp.Regexp) bool { return x.MatchString(lines[i+1]) }) {
			i++
		}
		if n := len(keep); n > 0 && strings.TrimSpace(keep[n-1]) == "" {
			keep = keep[:n-1]
		}
	}
	out := strings.Join(keep, "")
	if out == string(b) {
		return false, nil
	}
	if strings.TrimSpace(out) == "" && filepath.Base(p) == "potluck.fish" {
		return true, os.Remove(p)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".potluck-uninstall-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return false, err
	}
	return true, nil
}

// removeOverride removes the host override file, and its directory when that
// is then empty.
func removeOverride(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(p))
	return nil
}

// tartDriverState is the directory potluck-driver-tart keeps its records and
// failure reports in (its default; a --state-dir elsewhere is not found), when
// it is there as a real directory.
func tartDriverState() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ""
	}
	dir := protocol.DriverStateDir(h, "tart")
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// hostOverrideFile is where the host-wide override file is (tests replace it).
var hostOverrideFile = vmhost.HostOverrideFile

func lexists(p string) bool { _, err := os.Lstat(p); return err == nil }

func tildeAll(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = tildePath(p)
	}
	return out
}

func namesOf(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// homeLockHeld reports that a process holds home's daemon lock, which a
// daemon keeps for its whole life (kit/config/homelock.go) even when its
// socket is gone. It opens an existing lock file read-only and takes nothing.
func homeLockHeld(home string) bool {
	f, err := os.Open(filepath.Join(home, config.FileHomeLock))
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
