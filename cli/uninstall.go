package cli

// potluck uninstall: take Potluck off this machine. It undoes, in an order
// that never strands a later step, what install.sh and the first commands put
// here: what the machine shares, the pools it joined, the managed Softnet (the
// one root step), the daemon, the daily update job, the line install.sh added
// to a shell startup file, the host override file, the home and the programs.
// It says first what it will do and asks; it never starts a daemon; every
// step is safe to run again. What it cannot remove for the person (the person's
// own Tart and images, a menu bar app's login item) it names at the end
// (docs/lending.md). The menu bar app's words for all of it are
// uninstall_view.go's; Tart's part is uninstall_tart.go.

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/attach"
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
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/menubar"
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
// how it does it. id names it, the same over two runs, and will and did are the
// menu bar app's words for it (uninstall_view.go).
type uninstallStep struct {
	id, will, did string
	plan          string
	do            func(ctx context.Context) error
}

type uninstallFlags struct {
	yes, dryRun, keepHome, force, noSudo bool
	withTart                             tartScope
}

func (a *app) uninstallCmd() *cobra.Command {
	var f uninstallFlags
	cmd := &cobra.Command{
		Use:   "uninstall [--yes] [--keep-home] [--force] [--with-tart[=images]] [--dry-run] [--no-sudo]",
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
  6. on a Mac, with --with-tart, removes the Tart images Potluck pulled and
     the tart.app Potluck installed (below);
  7. on a Mac, deletes the image downloads a setup left unfinished (in
     Potluck's own folder in Tart's), and removes the home (default
     ~/.potluck): your identity, the pools this machine runs and everything
     they hold, so the pools end for their members. --keep-home keeps both;
  8. removes the programs install.sh put in the directory this potluck runs
     from (potluck, its drivers, potluck-conformance, potluck-e2b and their
     .prev copies), this one last.

A step that fails stops the run and says so; run it again to go on, since
every step is safe to repeat. It never starts a daemon: with none running,
nothing is unshared and no pool is left by step 1 and 2, and the pools this machine joined are not told
(their owners can revoke its token). It does not touch Tailscale, a menu bar
app, or, without --with-tart, Tart and its images; it names what is left.

--with-tart never touches a Tart, an image or a VM of yours. It removes an
image only when Potluck's record says Potluck pulled it, and nothing else
here may need it (a tag of its repository, another Potluck home); an image
pulled before that record was kept stays. It removes tart.app only when
Potluck installed it (` + a.cmd("tart install") + `), unchanged since, and Tart then
holds nothing: any VM or image left keeps it. What it keeps it names.
--with-tart=images removes the images only.

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
ask and the arguments that run the plan. "view" is the same in the words the
menu bar app shows.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.withTart != tartNone && f.withTart != tartImages && f.withTart != tartAll {
				return fmt.Errorf("--with-tart takes images or all, not %q", f.withTart)
			}
			return a.uninstall(cmd.Context(), f)
		},
	}
	cmd.Flags().BoolVar(&f.yes, "yes", false, "do not ask")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print what it would do and change nothing")
	cmd.Flags().BoolVar(&f.keepHome, "keep-home", false, "keep the home: your identity and the pools this machine runs")
	cmd.Flags().StringVar((*string)(&f.withTart), "with-tart", "", "on a Mac, also remove the Tart images Potluck pulled and the tart.app Potluck installed, never yours")
	cmd.Flags().Lookup("with-tart").NoOptDefVal = string(tartAll)
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
	// View is all of it as the menu bar app shows it.
	View *uninstallView `json:"view,omitempty"`
}

// uninstallAction is what an app may run after the person said yes to
// Confirm (Detail says more): potluck's arguments.
type uninstallAction struct {
	Confirm string   `json:"confirm"`
	Detail  string   `json:"detail,omitempty"`
	Args    []string `json:"args"`
}

// action is the run an app may start with these flags, its question first.
func (p uninstallPlan) action(f uninstallFlags, confirm, detail string) *uninstallAction {
	args := []string{"--json", "uninstall", "--yes", "--no-sudo"}
	if f.keepHome {
		args = append(args, "--keep-home")
	}
	if f.force {
		args = append(args, "--force")
	}
	// Never for Tart no plan the person saw names.
	if s := p.tart.scope(f.withTart); s != tartNone {
		args = append(args, "--with-tart="+string(s))
	}
	return &uninstallAction{Confirm: confirm, Detail: strings.TrimSpace(detail), Args: args}
}

// uninstallPlan is what uninstall does on this machine, in order, and what
// the person should know of it.
type uninstallPlan struct {
	steps    []uninstallStep
	left     []string // not removed, as people-words
	stays    []string // the same in the app's words
	tart     *tartPart
	poolsRun []string
	// poolsKnown is that the daemon said which those are.
	poolsKnown bool
	warning    string
	home       string   // the home, when there is one
	stopsRun   bool     // the first step stops a host run going on
	running    bool     // its daemon answered
	joined     []string // the pools it joined, when running
	// joinedNow reads those again (nil: not running).
	joinedNow func(ctx context.Context) ([]string, error)
	// notes are what the step running could not do though it goes on, as
	// people-words: they go with it in the answer's done. said is the app's
	// words for what it did instead of its own (uninstallStep.did).
	notes *[]string
	said  *string
	// kept is what the step running kept after all, in the app's words: it
	// goes with what stays, and the step is not done unless said says what was.
	kept *string
}

func (a *app) uninstall(ctx context.Context, f uninstallFlags) error {
	if err := a.ownLeftover(); err != nil {
		return err
	}
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
	done := []viewStep{}
	show := func(left []uninstallStep, o viewOutcome) { ans.View = p.view(f, done, left, o, ans) }
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
	if s := p.tart.refusal(f); s != "" {
		a.say("%s", s)
	}
	if f.dryRun {
		a.sayLeftOver(left)
		ans.Left = plans(steps)
		if len(steps) > 0 {
			ans.Remove = p.action(f, "Remove Potluck from this Mac?", p.confirmDetail(f))
		}
		show(steps, viewPlan)
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
			p.keepElsewhere(a, cfg.Home, err)
		}
		if err == nil {
			err = s.do(ctx)
		}
		if err == nil && i == 0 && p.stopsRun {
			release, err = holdHost(ctx, p)
			p.keepElsewhere(a, cfg.Home, err)
		}
		var root *adminStepLeft
		if errors.As(err, &root) {
			ans.AdminStep, ans.Left = &root.step, plans(steps[i:])
			show(steps[i:], viewAdmin)
			a.say("--no-sudo: stopped at step %d, the root step, with the steps before it done. %s\nRun this as root, then run %s again to finish:\n  %s",
				i+1, cmp.Or(root.step.detail, root.step.Why), a.uninstallAgain(f), root.step.command())
			return ans, nil
		}
		if err != nil {
			var busy *needsForce
			if errors.As(err, &busy) && !f.force {
				forced := f
				forced.force = true
				ans.Force = p.action(forced, forceQuestion(busy.live), forceDetail+p.alsoTart(f))
			}
			err = fmt.Errorf("step %d (%s) did not finish: %w\nnothing after it was done; fix that and run %s again (each step is safe to repeat)", i+1, s.plan, err, a.uninstallAgain(f))
			ans.Left, ans.Error = plans(steps[i:]), err.Error()
			show(steps[i:], outcomeOf(s, err, f.force))
			return ans, err
		}
		if i == 0 { // from here Potluck is being removed: nothing starts its daemon by itself
			_ = markStopped(cfg.Home)
		}
		words := s.plan
		notes := strings.Join(*p.notes, "; ")
		if notes != "" {
			words += " (but: " + notes + ")"
		}
		*p.notes = nil
		if *p.kept != "" {
			p.stays, *p.kept = append(p.stays, *p.kept), ""
			stayed := words
			if *p.said != "" { // part of it was done
				stayed = notes
			}
			ans.NotRemoved, left = append(ans.NotRemoved, stayed), append(left, stayed)
			if *p.said == "" {
				continue
			}
		}
		ans.Done = append(ans.Done, words)
		v := s.view()
		v.Did, *p.said = cmp.Or(*p.said, v.Did), ""
		done = append(done, v)
	}
	a.sayLeftOver(left)
	show(nil, viewDone)
	return ans, nil
}

// uninstallAgain is the command that goes on with a run with f: as asked,
// so that it neither takes nor leaves more. --force and --yes are said anew.
func (a *app) uninstallAgain(f uninstallFlags) string {
	c := "uninstall"
	if f.keepHome {
		c += " --keep-home"
	}
	switch f.withTart {
	case tartAll:
		c += " --with-tart"
	case tartImages:
		c += " --with-tart=images"
	}
	if f.noSudo {
		c += " --no-sudo"
	}
	return a.cmd(c)
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
	p.notes, p.said, p.kept = new([]string), new(string), new(string)
	var stays []string
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
			return p, fmt.Errorf("a daemon still holds %s (its lock is taken) but does not answer on %s: wait for it, or stop it with %s, then run %s again", attach.Tilde(home), cfg.SocketPath(), a.cmd("daemon stop"), a.cmd("uninstall"))
		}
	}

	// A setup run going on would share again, and write the home again.
	if homeExists && hostBusy(home) {
		p.stopsRun = true
		steps = append(steps, uninstallStep{
			id: "setup", will: "Stop the setup that is going on", did: "Stopped the setup that was going on",
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
			id: "lending", will: "Stop lending this Mac", did: "Stopped lending this Mac",
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
				id: "groups", will: "Leave the groups this Mac joined: " + strings.Join(joined, ", "), did: "Left the groups this Mac joined: " + strings.Join(joined, ", "),
				plan: "leave the pools this machine joined, asking each pool's machine to revoke this one's token: " + strings.Join(joined, ", "),
				do: func(ctx context.Context) error {
					var unheard []string
					for _, p := range joined {
						r, err := c.Leave(ctx, p)
						if err != nil {
							return fmt.Errorf("leave %s: %w", p, err)
						}
						if r.Revoked {
							a.say("left %s", p)
						} else {
							note("left %s (the pool's machine did not answer: its owner can revoke this machine's token there)", p)
							unheard = append(unheard, p)
						}
					}
					if len(unheard) > 0 {
						*p.said = "Left the groups this Mac joined, but could not tell " + strings.Join(unheard, ", ") + ": ask whoever runs it to remove this Mac"
					}
					return nil
				},
			})
		}
	} else if homeExists {
		stays = append(stays, "Any group this Mac is still in can't be told that it left, because Potluck isn't running: ask whoever runs that group to remove this Mac")
		left = append(left, a.cmd("daemon")+" is not running, so uninstall could not tell the pools this machine joined that it is leaving, or stop sharing through the daemon: start it ("+a.cmd("daemon --detach")+") and run "+a.cmd("uninstall")+" again to do that, or ask each pool's owner to revoke this machine's token")
	}

	runs, known := poolsRun(ctx, c, running)
	p.poolsRun, p.poolsKnown = runs, known
	dropping := homeLeftover(home) // a removal cut off part way through the home
	if f.keepHome {
		dropping = ""
	}
	switch {
	case homeExists && !f.keepHome:
		p.warning = homeWarning(runs, known)
	case dropping != "":
		p.warning = "This deletes for good what an earlier uninstall did not: it may hold this machine's identity and the pools it ran."
	}

	if store := softnetStore(); softnetGOOS == "darwin" && lexists(store.Dir) {
		steps = append(steps, uninstallStep{
			id: "helper", will: "Remove the network helper (macOS asks for this Mac's password)", did: "Removed the network helper",
			plan: "remove the managed Softnet in " + store.Dir + " (the one root step: asks for your Mac password)",
			do:   func(ctx context.Context) error { _, err := a.removeManagedSoftnet(ctx, store, f.noSudo); return err },
		})
	}
	if d, err := softnetDownloadDir(); softnetGOOS == "darwin" && err == nil && lexists(d) {
		steps = append(steps, uninstallStep{
			id: "helper-download", will: "Remove the network helper's download", did: "Removed the network helper's download",
			plan: "remove the Softnet download " + a.cmd("softnet install --no-sudo") + " kept in " + attach.Tilde(d),
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
			id: "stop", will: "Stop Potluck", did: "Stopped Potluck",
			plan: "stop the daemon",
			do:   func(ctx context.Context) error { return a.stopDaemon(ctx, cfg) },
		})
	}

	if s, err := newUpdateSchedule(); err == nil && s.Supported() == nil && s.Installed() {
		steps = append(steps, uninstallStep{
			id: "updates", will: "Turn off daily updates", did: "Turned off daily updates",
			plan: "remove the daily update job (" + attach.Tilde(s.Files()[0]) + ")",
			do: func(context.Context) error {
				removed, foreign, err := s.Remove()
				if removed {
					a.say("daily update removed")
				}
				if foreign != "" {
					note("another job on this machine also runs potluck upgrade and was left alone: %s", foreign)
					*p.said = "Left alone daily updates that something other than Potluck set up"
					if removed {
						*p.said = "Turned off daily updates, but not ones something other than Potluck set up"
					}
				}
				return err
			},
		})
	}

	if p := hostOverrideFile(); lexists(p) {
		steps = append(steps, uninstallStep{
			id: "limits", will: "Remove your lending settings", did: "Removed your lending settings",
			plan: "remove the host override file " + p + " (what " + a.cmd("host set") + " and " + a.cmd("host pause") + " write)",
			do:   func(context.Context) error { return removeOverride(p) },
		})
	}

	if files := pathLineFiles(); len(files) > 0 {
		steps = append(steps, uninstallStep{
			id: "shell", will: "Remove what the installer added to your Terminal settings", did: "Removed what the installer added to your Terminal settings",
			plan: "remove the lines install.sh added to put its install directory on PATH, from " + strings.Join(tildeAll(files), ", "),
			do: func(context.Context) error {
				for _, file := range files {
					// A file that cannot be written (a read-only dotfiles
					// store) must not stop the home and the programs going.
					switch changed, err := removePathLine(file); {
					case err != nil:
						note("could not edit %s (%v): remove potluck's PATH lines from it yourself", attach.Tilde(file), err)
						*p.said = "Could not change your Terminal settings: the lines the installer added stay there, and do no harm"
					case changed:
						a.say("removed potluck's PATH line from %s", attach.Tilde(file))
					}
				}
				return nil
			},
		})
	}

	tartWait := uninstallTartWait
	if f.withTart != tartNone && !f.dryRun {
		tartWait = uninstallTartRunWait
	}
	if p.tart, err = a.readTartPart(ctx, home, f.keepHome, tartWait); err != nil {
		return p, err
	}
	if p.tart.unread(f.withTart) != nil && f.withTart != tartNone { // it stops the run: before anything is given up
		steps = append(p.tartSteps(a, f, note), steps...)
	} else {
		steps = append(steps, p.tartSteps(a, f, note)...)
	}
	tartLeft, tartStays := p.tart.left(a, f)
	if tartLeft != "" {
		left, stays = append(left, tartLeft), append(stays, tartStays)
	}

	steps = append(steps, p.tartLockSteps(note)...)
	if files := p.tart.records(f); len(files) > 0 {
		left = append(left, "Potluck's records for Tart ("+strings.Join(tildeAll(files), ", ")+"): what it pulled or installed, and lock files that keep two runs apart")
		stays = append(stays, "Potluck's small records of what it downloaded for Tart")
	}

	// The driver's records say which Tart VMs are Potluck's sandboxes, and
	// nothing here can show that none runs on them (a pool's own sandboxes
	// are not all counted by stopping the shares), so the person removes them.
	if dir := tartDriverState(); dir != "" {
		left = append(left, "the Tart driver's records in "+attach.Tilde(dir)+": they say which Tart VMs are Potluck's sandboxes, so they stay. When no sandbox runs: rm -r "+shellQuote(dir))
		stays = append(stays, "Potluck's small records of the virtual machines that jobs ran in")
	}

	if setupGOOS == "darwin" && !f.keepHome {
		if leave := hostprobe.TartLeaveAt("", home, false, nil); leave.HasPullHome() || (leave.Dir != "" && p.stopsRun) {
			steps = append(steps, uninstallStep{
				id: "downloads", will: "Delete any download a setup left unfinished", did: "Deleted any download a setup left unfinished",
				plan: "delete the downloads a setup left unfinished in " + attach.Tilde(leave.PullHome()) + ", Potluck's own folder in Tart's (not one another Potluck home here goes on with)",
				do: func(ctx context.Context) error {
					if err := leave.DropPartials(ctx); err != nil { // the rest can go without it
						note("could not delete the unfinished downloads in %s: %v", attach.Tilde(leave.PullHome()), err)
						*p.kept = "A download a setup left unfinished, in " + attach.Tilde(leave.PullHome())
					}
					return nil
				},
			})
		}
	}

	if homeExists && !f.keepHome {
		if err := checkHomeRemovable(home); err != nil {
			return p, err
		}
		also := ""
		if dropping != "" {
			also = "; and " + attach.Tilde(dropping) + ", what an earlier " + a.cmd("uninstall") + " left of it"
		}
		steps = append(steps, uninstallStep{
			id: "data", will: "Delete Potluck's data on this Mac, with this Mac's identity and any groups it runs", did: "Deleted Potluck's data on this Mac",
			plan: "remove the home " + attach.Tilde(home) + ": this machine's identity, the pools it runs and their data, and the daemon's log" + also,
			do: func(context.Context) error {
				if homeLockHeld(home) {
					return fmt.Errorf("a daemon holds %s: stop it with %s", attach.Tilde(home), a.cmd("daemon stop"))
				}
				if err := removeHome(home); err != nil {
					return err
				}
				// The daemon removes its own record when it stops; take the
				// directory it was in too, when nothing else is there.
				if d := daemonrecord.Dir(); d != "" {
					_ = os.Remove(d)
					_ = os.Remove(filepath.Dir(d))
				}
				a.say("removed %s", attach.Tilde(home))
				return nil
			},
		})
	} else if dropping != "" {
		steps = append(steps, uninstallStep{
			id: "data", will: "Delete the rest of Potluck's data on this Mac", did: "Deleted the rest of Potluck's data on this Mac",
			plan: "remove " + attach.Tilde(dropping) + ", the home an earlier " + a.cmd("uninstall") + " did not finish removing",
			do:   func(context.Context) error { return dropHome(dropping) },
		})
	}

	if dir, names := installedProgramFiles(); len(names) > 0 {
		steps = append(steps, uninstallStep{
			id: "programs", will: "Remove the Potluck program", did: "Removed the Potluck program",
			plan: "remove the programs in " + attach.Tilde(dir) + ": " + strings.Join(namesOf(names), ", ") + " (this one last)",
			do: func(context.Context) error {
				for _, p := range names {
					if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				}
				a.say("removed %d files from %s", len(names), attach.Tilde(dir))
				return nil
			},
		})
	}
	machine, machineStays := a.leftOver()
	p.steps, p.left, p.stays = steps, append(left, machine...), append(stays, machineStays...)
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

// leftOver is what uninstall does not remove for this machine, as
// people-words and in the menu bar app's, which leaves out the app itself:
// it offers to move that to the Trash.
func (a *app) leftOver() (left, stays []string) {
	if d := softnetStore().EmptyParent(); softnetGOOS == "darwin" && d != "" {
		left = append(left, "the empty folder the managed Softnet was in: sudo rmdir "+shellQuote(d))
		stays = append(stays, "An empty folder the network helper was in: it does no harm")
	}
	if setupGOOS == "darwin" {
		if p, ok := vmhost.SystemTartSearch().FindSoftnet(); ok {
			if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSetuid != 0 {
				left = append(left, "the softnet at "+p+" is setuid root: Potluck's root step made it so, or you did. If you do not use Tart for other things, undo it with: sudo chmod u-s "+shellQuote(p))
				stays = append(stays, "A network helper from outside Potluck that still has an administrator's permission")
			}
		}
		left = append(left, "Potluck's menu bar app, if you installed it ("+strings.Join(menubar.AppNames, ", or before its rename ")+", in your Applications folder unless you put it elsewhere; not another program's app of that name): quit it from its menu, remove it from System Settings > General > Login Items if it is there, and delete the app (docs/menubar.md)")
		if files := menuAppFiles(); len(files) > 0 {
			left = append(left, "the menu bar app's own files ("+strings.Join(tildeAll(files), ", ")+"): the app may be using them; its Move to Trash removes them, else once it is gone, delete them")
			stays = append(stays, "The Potluck app's own settings and temporary files, until the app moves itself to the Trash")
		}
		left = append(left, "what you downloaded to install Potluck (Potluck.zip, a Potluck.app in Downloads): those are your files, and uninstall does not look in your Downloads folder")
		stays = append(stays, "What you downloaded to install Potluck, such as Potluck.zip or a Potluck app in your Downloads folder: those are yours to delete")
	}
	left = append(left, "a Tailscale node named potluck-<this Mac's host name>-potluck, if you used Tailscale: remove it in the Tailscale admin console (https://login.tailscale.com/admin/machines)")
	return left, append(stays, "If you used Tailscale with Potluck: this Mac's Potluck entry in your Tailscale account")
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

// installedProgramFiles are the files of InstalledPrograms (and their .prev
// copies) that exist in the directory this potluck runs from, and that
// directory: potluck itself first, so that with part of it gone the Mac app
// and install.sh set up anew, and what an install cut off left there. None when this potluck was not run from an installed copy.
// The copy the Mac app carries stands for the install directory's: the app
// runs it when there is none there, and its own files are the app's.
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
	if d := installDir(); os.Getenv(EnvBundledRelease) != "" && d != "" {
		dir = d
	}
	for _, n := range InstalledPrograms {
		for _, name := range []string{n, n + swap.PrevSuffix} {
			if p := filepath.Join(dir, name); lexists(p) {
				files = append(files, p)
			}
		}
	}
	return dir, append(files, swap.Leftovers(dir, InstalledPrograms)...)
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

// menuAppFiles are the Mac app's caches and settings, those that are there.
func menuAppFiles() []string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return nil
	}
	lib := filepath.Join(h, "Library")
	files := []string{filepath.Join(lib, "Caches", menuAppID), filepath.Join(lib, "Preferences", menuAppID+".plist")}
	return slices.DeleteFunc(files, func(p string) bool { return !lexists(p) })
}

// menuAppID is the Mac app's bundle id (Place.swift's appBundleID).
const menuAppID = "sh.potluck.menu"

// hostOverrideFile is where the host-wide override file is (tests replace it).
var hostOverrideFile = vmhost.HostOverrideFile

func lexists(p string) bool { _, err := os.Lstat(p); return err == nil }

func tildeAll(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = attach.Tilde(p)
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
