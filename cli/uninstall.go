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

type uninstallFlags struct{ yes, dryRun, keepHome, force bool }

func (a *app) uninstallCmd() *cobra.Command {
	var f uninstallFlags
	cmd := &cobra.Command{
		Use:   "uninstall [--yes] [--keep-home] [--force] [--dry-run]",
		Short: "Remove Potluck from this machine: stop sharing, leave its pools, stop the daemon, remove its files",
		Long: `Uninstall takes Potluck off this machine. It prints what it will do and asks
before it does anything (--dry-run only prints; --yes does not ask). In this
order:

  1. stops sharing every size this machine lends (refused while a sandbox
     runs on one, unless --force);
  2. leaves every pool this machine joined, and asks that pool's machine to
     revoke this one's token;
  3. on a Mac, removes the managed Softnet (the one root step; it asks for
     your Mac password);
  4. stops the daemon;
  5. removes the daily update job, the host override file and the lines
     install.sh added to a shell startup file;
  6. removes the home (default ~/.potluck): your identity, the pools this
     machine runs and everything they hold, so the pools end for their
     members. --keep-home keeps it;
  7. removes the programs install.sh put in the directory this potluck runs
     from (potluck, its drivers, potluck-conformance, potluck-e2b and their
     .prev copies), this one last.

A step that fails stops the run and says so; run it again to go on, since
every step is safe to repeat. It never starts a daemon: with none running,
step 1 and 2 are skipped, and the pools this machine joined are not told
(their owners can revoke its token). It does not touch Tart or Tart's images
(they may be yours), Tailscale, or a menu bar app; it names what is left.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.uninstall(cmd.Context(), f) },
	}
	cmd.Flags().BoolVar(&f.yes, "yes", false, "do not ask")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print what it would do and change nothing")
	cmd.Flags().BoolVar(&f.keepHome, "keep-home", false, "keep the home: your identity and the pools this machine runs")
	cmd.Flags().BoolVar(&f.force, "force", false, "stop sharing a size even while sandboxes run on one joined pool's machine (they go on until their lease ends)")
	return cmd
}

func (a *app) uninstall(ctx context.Context, f uninstallFlags) error {
	if err := a.notWhileAttached(a.cmd("uninstall")); err != nil {
		return err
	}
	cfg, err := a.loadClient()
	if err != nil {
		return err
	}
	steps, left, err := a.uninstallSteps(ctx, cfg, f)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		a.say("nothing of potluck's is on this machine that %s removes", a.cmd("uninstall"))
	} else {
		a.say("%s will:", a.cmd("uninstall"))
		for i, s := range steps {
			a.say("  %d. %s", i+1, s.plan)
		}
	}
	if f.dryRun {
		a.sayLeftOver(left)
		return nil
	}
	if len(steps) > 0 && !f.yes {
		if err := a.confirmUninstall(); err != nil {
			return err
		}
	}
	for i, s := range steps {
		if err := s.do(ctx); err != nil {
			return fmt.Errorf("step %d (%s) did not finish: %w\nnothing after it was done; fix that and run %s again (each step is safe to repeat)", i+1, s.plan, err, a.cmd("uninstall"))
		}
	}
	a.sayLeftOver(left)
	return nil
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

// uninstallSteps reads this machine (changing nothing) and returns what
// uninstall does, in order, and the notes about what it leaves.
func (a *app) uninstallSteps(ctx context.Context, cfg *config.Config, f uninstallFlags) (steps []uninstallStep, left []string, err error) {
	home := cfg.Home
	homeExists := isDir(home)
	var c *client.Client
	running := false
	if homeExists {
		if c, err = a.readClient(); err != nil {
			return nil, nil, err
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
			return nil, nil, fmt.Errorf("the daemon on %s did not answer (%w): it may be busy or starting; wait and run %s again, or stop it with %s", cfg.SocketPath(), herr, a.cmd("uninstall"), a.cmd("daemon stop"))
		}
		running = herr == nil
		if !running && homeLockHeld(home) {
			return nil, nil, fmt.Errorf("a daemon still holds %s (its lock is taken) but does not answer on %s: wait for it, or stop it with %s, then run %s again", tildePath(home), cfg.SocketPath(), a.cmd("daemon stop"), a.cmd("uninstall"))
		}
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
				}
				return err
			},
		})
		remotes, err := c.Remotes(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("read the pools this machine joined: %w", err)
		}
		var joined []string
		for _, r := range remotes {
			if s := r.Project.Slug; !slices.Contains(joined, s) {
				joined = append(joined, s)
			}
		}
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
							a.say("left %s (the pool's machine did not answer: its owner can revoke this machine's token there)", p)
						}
					}
					return nil
				},
			})
		}
	} else if homeExists {
		left = append(left, a.cmd("daemon")+" is not running, so uninstall could not tell the pools this machine joined that it is leaving, or stop sharing through the daemon: start it ("+a.cmd("daemon --detach")+") and run "+a.cmd("uninstall")+" again to do that, or ask each pool's owner to revoke this machine's token")
	}

	if runs := a.poolsRun(ctx, c, running); len(runs) > 0 && !f.keepHome {
		left = append(left, "the pools this machine runs end with its home: "+strings.Join(runs, ", ")+" (members lose them; --keep-home keeps them)")
	}

	if store := softnetStore(); softnetGOOS == "darwin" && lexists(store.Dir) {
		steps = append(steps, uninstallStep{
			plan: "remove the managed Softnet in " + store.Dir + " (the one root step: asks for your Mac password)",
			do:   func(ctx context.Context) error { _, err := a.removeManagedSoftnet(ctx, store); return err },
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
					a.say("another job on this machine also runs potluck upgrade and was left alone: %s", foreign)
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
						a.say("could not edit %s (%v): remove potluck's PATH lines from it yourself", tildePath(p), err)
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
			return nil, nil, err
		}
		steps = append(steps, uninstallStep{
			plan: "remove the home " + tildePath(home) + ": this machine's identity, the pools it runs and their data, and the daemon's log",
			do: func(context.Context) error {
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
	return steps, append(left, a.leftOver()...), nil
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
		if p, err := exec.LookPath("softnet"); err == nil {
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

// poolsRun are the slugs of the pools this machine runs (when its daemon
// answers).
func (a *app) poolsRun(ctx context.Context, c *client.Client, running bool) []string {
	if !running || c == nil {
		return nil
	}
	ps, err := c.LocalProjects(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range ps {
		out = append(out, p.Slug)
	}
	return out
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
