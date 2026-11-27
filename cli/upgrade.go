package cli

// potluck upgrade: install the newest signed release (internal/upgrade) over
// this potluck's programs with kit/swap's Place (each old one kept as
// <name>.prev), then restart the daemon on the new build if one runs, and put
// everything back if the new daemon does not come up. Once the new build is
// final it re-pins the drivers it placed (runtarget.RepinPlaced, DRIVERS.md).
// install.sh runs a release's own potluck upgrade to install it.
// upgrade enable|disable|status (upgrade_auto.go) schedule it daily.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sengirunners/potluck/attach"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/internal/upgrade"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/daemonrecord"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/softnet"
)

// upgradeStartWait bounds the new daemon's start.
var upgradeStartWait = 30 * time.Second

func (a *app) upgradeCmd() *cobra.Command {
	var from, dir string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Install the newest signed potluck release and restart the daemon on it",
		Long: `Install the newest signed potluck release: from GitHub (GH_TOKEN or
GITHUB_TOKEN, if set, goes with each request; a private repository needs
one), or from a directory holding one release's files (--release). The manifest must be signed by a key this
build trusts (the channel's keys, built in, and config update_keys) and the
tarball must match it; a release older than the one installed here is
refused. Each program replaced is kept beside the new one as <name>.prev.
A running daemon is restarted on the new build; if it does not come up,
the old programs go back and the old daemon starts again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.upgrade(cmd.Context(), from, dir, dryRun)
		},
	}
	cmd.Flags().StringVar(&from, "release", "", "install from this directory (manifest.json, manifest.json.sig and the tarballs) instead of GitHub")
	cmd.Flags().StringVar(&dir, "dir", "", "install into this directory (default: the one this potluck runs from)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "verify the release and say what would change; install nothing")
	cmd.AddCommand(a.upgradeEnableCmd(), a.upgradeDisableCmd(), a.upgradeStatusCmd())
	return cmd
}

func (a *app) upgrade(ctx context.Context, from, dir string, dryRun bool) error {
	dh, herr := a.home()
	if herr != nil && ownHomeExists() {
		return herr // potluck's own home and daemon, which it cannot read: refused, not half done
	}
	filesOnly := filesOnlyWhy(dh, herr)
	var updateKeys []string // only potluck's own home's config adds keys to trust
	cfg := dh.Cfg
	if filesOnly == "" {
		updateKeys = cfg.File.UpdateKeys
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return err
	}
	if dir == "" {
		dir = filepath.Dir(self)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	if err := notOverInstalled(ctx, self, dir, cfg); err != nil {
		return api.Plainly(err, "There is already a Potluck on this Mac, so the copy in this app wasn't installed. Quit the app and open it again; if that doesn't help, restart this Mac first."+plainGetHelp)
	}
	var src upgrade.Source = upgrade.NewGitHub(os.Getenv)
	if from != "" {
		src = upgrade.Dir(from)
	}
	stage, err := os.MkdirTemp("", "potluck-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	keys, err := upgrade.Keys(updateKeys)
	if err != nil {
		return fmt.Errorf("refusing to upgrade: %w", err)
	}
	st, err := upgrade.Stage(src, keys, runtime.GOOS, runtime.GOARCH, stage)
	if err != nil {
		words := "Potluck couldn't download and check its newest version, so nothing was changed. If this Mac is offline, connect it, then try again." + plainGetHelp
		if from != "" {
			words = "The copy of Potluck to install couldn't be unpacked and checked, so it wasn't installed. If this Mac is low on disk space, see to that, then try again." + plainGetHelp
		}
		return api.Plainly(err, words)
	}
	m := st.Manifest
	// A GitHub source is named, and a --release directory outside the temporary directory; install.sh's release
	// directory is temporary, so it is not.
	where := ""
	if from == "" || !inTempDir(from) {
		where = " from " + src.String()
	}
	a.say("verified potluck %s (%s)%s: its manifest is signed by a key this potluck trusts, and its %s/%s tarball matches", m.Version, short(m.Commit), where, runtime.GOOS, runtime.GOARCH)

	// This build, installed where it runs: nothing to do, unless the upgrade to
	// it was cut off.
	if filepath.Dir(self) == dir && m.RunBy(api.Version, api.Commit) && !a.upgradeCutOff(ctx, cfg, dir, st, filesOnly != "") {
		a.say("potluck %s is already installed in %s", api.Version, attach.Tilde(dir))
		if filesOnly == "" && !dryRun {
			return endedRestart(ctx, cfg)
		}
		return nil
	}
	if filesOnly != "" {
		return a.upgradeFilesOnly(ctx, dir, stage, st, filesOnly, dryRun)
	}
	state, installed, err := release.LoadState(cfg.Home)
	if err != nil {
		return err
	}
	if why := upgrade.Older(m, state, installed); why != "" {
		return api.Plainly(errors.New(why), "A newer Potluck is already on this Mac, so this one wasn't installed. Nothing was changed."+plainGetHelp)
	}
	c := client.NewUnix(cfg.SocketPath())
	h, running := health(ctx, c)
	restart := running && !m.RunBy(h.Version, h.Commit)
	if dryRun {
		a.say("would install %s into %s (each old one kept as <name>%s)", strings.Join(st.Names, ", "), attach.Tilde(dir), swap.PrevSuffix)
		switch {
		case restart:
			a.say("would restart the daemon (pid %d, %s) on %s", h.PID, h.Version, m.Version)
		case running:
			a.say("the daemon already runs %s", m.Version)
		}
		return nil
	}

	// One swap at a time on this home, daemon or not.
	if err := cfg.EnsureHome(); err != nil {
		return err
	}
	lock, err := swap.Acquire(cfg.Home, api.SwapHolder{By: cfg.HumanAddress(), What: "upgrade"})
	if err != nil {
		words := "Potluck couldn't write in its own folder on this Mac, so nothing was changed." + plainGetHelp
		if busy := (*swap.BusyError)(nil); errors.As(err, &busy) {
			words = "Potluck is being updated or restarted right now, so nothing was changed. Try again in a few minutes." + plainGetHelp
		}
		return api.Plainly(err, words)
	}
	defer lock.Release()
	if running { // it may have stopped or changed while the release was fetched
		h, running = health(ctx, c)
		restart = running && !m.RunBy(h.Version, h.Commit)
	}
	if !running && restartCutOff(cfg.Home) {
		// What the one cut off was doing: the old daemon may still be ending, or
		// the new one starting.
		if h, running = settledDaemon(ctx, c, cfg.Home); !running && ctx.Err() == nil {
			was, _, _ := swap.ReadPending(cfg.Home)
			if pid, gone := endUnanswering(cfg.Home, filepath.Join(dir, "potluck"), was.Bin); !gone {
				who, end := "", ""
				if pid > 0 {
					who, end = fmt.Sprintf(" (pid %d)", pid), fmt.Sprintf(" (kill %d if it never does)", pid)
				}
				return api.Plainly(fmt.Errorf("a daemon%s holds %s's lock and did not answer in %s, so nothing was changed: run this again once it answers or is gone%s", who, attach.Tilde(cfg.Home), stopWait, end),
					"Potluck is still starting or stopping on this Mac, so nothing was changed. Try again in a few minutes; if it says this again, restart this Mac first."+plainGetHelp)
			}
		}
		restart = running && !m.RunBy(h.Version, h.Commit)
	}
	resume := !running && restartCutOff(cfg.Home)

	// The new build's own preflight against this home and its daemon: data
	// it cannot read, or routes the running daemon serves that it lacks,
	// refuse the upgrade before anything moves.
	pf := preflightBinary(ctx, cfg, filepath.Join(stage, "potluck"))
	if !pf.OK {
		return api.Plainly(fmt.Errorf("the new build's preflight refused this upgrade: %s", strings.Join(pf.Problems, "; ")),
			"The new version can't go on from what this Mac's Potluck has, so nothing was changed. Press Copy details and send what it copies to the person who invited you.")
	}
	was, _, wasErr := swap.ReadPending(cfg.Home)
	wasOneWay := was.OneWay || wasErr != nil
	oneWay := oneWaySwap(pf)

	// What the running daemon is, to bring it back as it was: its own
	// program (not necessarily dir's) and whether its tailnet node runs.
	var old daemonRun
	if restart {
		old = a.runningDaemon(ctx, cfg, c, h, filepath.Join(dir, "potluck"))
	} else if resume {
		// The daemon the cut-off one stopped, and what that one knew of its data.
		if !lexists(was.Bin) {
			was.Bin = ""
		}
		old, restart = daemonRun{bin: oldDaemonBin(was.Bin, wasErr, filepath.Join(dir, "potluck")), tailnet: was.Tailnet}, true
		oneWay = oneWay || wasOneWay
	}
	if running && swap.Placing(dir) {
		// What an earlier install was cut off placing runs: it, not what it
		// replaced, is what this one goes back to.
		if now := preflightBinary(ctx, cfg, filepath.Join(dir, "potluck")); now.Running != "" && now.Build == now.Running {
			if err := swap.Settle(dir, "potluck"); err != nil {
				return err
			}
		}
		oneWay = oneWay || wasOneWay && swap.Placing(dir) // what it replaced may not read the data
	}
	if err := ownDir(dir); err != nil {
		return err
	}
	if restart {
		if err := swap.WritePending(cfg.Home, swap.Unfinished{What: "upgrade", From: pf.Running, To: pf.Build, PID: os.Getpid(), At: time.Now(),
			Bin: old.bin, Tailnet: old.tailnet, OneWay: oneWay}); err != nil {
			return err
		}
	}
	// The word stays until a daemon runs again or none is to.
	placed, err := swap.Begin(ctx, stage, dir, programLast(st.Names), a.waitingToPlace(dir))
	if err != nil {
		if !resume {
			err = errors.Join(err, swap.ClearPending(cfg.Home))
		}
		return fmt.Errorf("install into %s: %w", dir, err)
	}
	if restart {
		newRun := daemonRun{bin: filepath.Join(dir, "potluck"), tailnet: old.tailnet}
		if err := a.restartOn(ctx, cfg, c, h.PID, lock, newRun, m.Commit); err != nil {
			if oneWay {
				a.repin(cfg, dir, placed)
				if serr := errors.Join(release.WriteState(cfg.Home, state.Installing(m, time.Now())), placed.Finish()); serr != nil {
					err = errors.Join(err, serr)
				}
				return fmt.Errorf("the new daemon did not come up (%v). Its migrations are one-way (the previous build cannot read the data), "+
					"so potluck %s stays installed in %s, the old programs are kept beside it as <name>%s, and no daemon is running now; see %s",
					err, m.Version, attach.Tilde(dir), swap.PrevSuffix, cfg.Path(config.FileLog))
			}
			back := "the old programs are back"
			if len(placed.Names) == 0 {
				back = "its programs were there before this"
			}
			rerr := placed.Restore()
			if rerr == nil { // also when this one is being ended
				rerr = a.startOldAgain(context.WithoutCancel(ctx), cfg, c, h.PID, old)
			}
			if rerr != nil {
				return fmt.Errorf("the new daemon did not come up (%v), and putting the old one back failed: %w", err, rerr)
			}
			return fmt.Errorf("the new daemon did not come up, so %s and the old daemon (%s) runs again: %w", back, old.bin, errors.Join(err, swap.ClearPending(cfg.Home)))
		}
	}
	repinned := a.repin(cfg, dir, placed)
	if err := errors.Join(release.WriteState(cfg.Home, state.Installing(m, time.Now())), placed.Finish(), swap.ClearPending(cfg.Home)); err != nil {
		return err
	}
	a.say("installed potluck %s (%s) into %s: %s", m.Version, short(m.Commit), attach.Tilde(dir), strings.Join(st.Names, ", "))
	if len(repinned) > 0 {
		a.say("re-pinned drivers: %s", strings.Join(repinned, ", "))
	}
	if restart {
		a.say("restarted the daemon on %s", m.Version)
	}
	return nil
}

// waitingToPlace says that another install has dir, the first time it is asked.
func (a *app) waitingToPlace(dir string) func() {
	return func() { a.say("waiting for another install into %s to end", attach.Tilde(dir)) }
}

// settledDaemon waits for a daemon that holds home's lock to answer, or to be
// gone.
func settledDaemon(ctx context.Context, c *client.Client, home string) (api.Health, bool) {
	for deadline := time.Now().Add(stopWait); homeLockHeld(home) && time.Now().Before(deadline) && ctx.Err() == nil; time.Sleep(50 * time.Millisecond) {
		if h, up := health(ctx, c); up {
			return h, true
		}
	}
	return health(ctx, c)
}

// endUnanswering ends the daemon that holds home's lock, which has not answered
// in all the time a new one is given: a later release could never be installed
// past it. Only one that runs one of programs, the new or the old potluck of
// the upgrade that was cut off, and never one that took the lock meanwhile.
// gone is that nothing holds the lock now.
func endUnanswering(home string, programs ...string) (pid int, gone bool) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if !homeLockHeld(home) {
			return pid, true
		}
		b, err := os.ReadFile(filepath.Join(home, config.FileHomeLock))
		asked := pid
		if pid, _ = strconv.Atoi(strings.TrimSpace(string(b))); err != nil || pid <= 0 {
			return 0, false
		}
		if asked != 0 && pid != asked {
			return pid, false
		}
		exe, err := processExe(pid)
		if err != nil || !slices.ContainsFunc(programs, func(p string) bool { return p != "" && samePath(strings.TrimSuffix(exe, " (deleted)"), p) }) {
			return pid, false
		}
		_ = syscall.Kill(pid, sig)
		for deadline := time.Now().Add(upgradeStopWait); homeLockHeld(home) && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		}
	}
	return pid, !homeLockHeld(home)
}

// endedRestart ends the word of a restart cut off when no upgrade goes on and
// a daemon runs: nothing is left to start.
func endedRestart(ctx context.Context, cfg *config.Config) error {
	if _, cut, err := swap.ReadPending(cfg.Home); !cut && err == nil {
		return nil
	}
	lock, err := swap.Acquire(cfg.Home, api.SwapHolder{By: cfg.HumanAddress(), What: "upgrade"})
	if err != nil {
		return nil // another's
	}
	defer lock.Release()
	if _, running := health(ctx, client.NewUnix(cfg.SocketPath())); !running {
		return nil
	}
	return swap.ClearPending(cfg.Home)
}

// notOverInstalled refuses to put the release the Mac app carries (it runs
// this program from its cache, with EnvBundledRelease) over a potluck
// installed in dir meanwhile, or beside one that runs from elsewhere: either
// may be newer, and the home may hold no record to tell.
func notOverInstalled(ctx context.Context, self, dir string, cfg *config.Config) error {
	if os.Getenv(EnvBundledRelease) == "" || filepath.Dir(self) == dir {
		return nil
	}
	if cfg != nil {
		if _, err := client.NewUnix(cfg.SocketPath()).Health(ctx); err == nil || !client.NotRunning(err) {
			return errors.New("Potluck is already running on this Mac, so the copy in this app was not installed. Restart this Mac, then open the app again")
		}
	}
	// What the app would run counts as installed; anything else there is replaced.
	if fi, err := os.Stat(filepath.Join(dir, "potluck")); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return nil
	}
	return errors.New("Potluck is already installed in " + attach.Tilde(dir) + ", so the copy in this app was not put over it. Quit the app and open it again")
}

// filesOnlyWhy says why potluck upgrade installs the programs alone ("" when
// it upgrades potluck's own home and daemon too): it never makes a home,
// and writes none but potluck's own, so not with no home yet, another
// program's, or no answer from the rule that picks one (home).
func filesOnlyWhy(dh daemonHome, herr error) string {
	switch {
	case herr != nil:
		return programName + " could not tell which daemon to use (" + herr.Error() + "), so " + programName + " upgrade restarted none"
	case dh.Owner != "":
		return othersHome(dh) + "; " + programName + " upgrade leaves that daemon and its home as they are: " + dh.Owner + " keeps them, so update " + dh.Owner + " its own way"
	case !dh.Existed:
		return "potluck has no home yet (" + attach.Tilde(dh.Cfg.Home) + "): its first command makes one"
	}
	return ""
}

// ownHomeExists reports whether the home potluck's names give (POTLUCK_HOME,
// else ~/.potluck) is a directory.
func ownHomeExists() bool {
	h, err := config.HomeDir()
	if err != nil {
		return false
	}
	fi, err := os.Stat(h)
	return err == nil && fi.IsDir()
}

// upgradeFilesOnly installs the staged release st into dir and nothing else
// (note says why): no home is locked or written (no release state, so a
// downgrade is not checked against one), no driver re-pinned and no daemon
// restarted.
func (a *app) upgradeFilesOnly(ctx context.Context, dir, stage string, st upgrade.Staged, note string, dryRun bool) error {
	m := st.Manifest
	names, held := withoutAttachedDrivers(st.Names, dir, a.attachedTo())
	if dryRun {
		a.say("would install %s into %s (each old one kept as <name>%s)", strings.Join(names, ", "), attach.Tilde(dir), swap.PrevSuffix)
		a.say("%s", note)
		a.sayHeldDrivers(held)
		return nil
	}
	if err := ownDir(dir); err != nil {
		return err
	}
	placed, err := swap.Begin(ctx, stage, dir, programLast(names), a.waitingToPlace(dir))
	if err == nil {
		err = placed.Finish()
	}
	if err != nil {
		return fmt.Errorf("install into %s: %w", dir, err)
	}
	a.say("installed potluck %s (%s) into %s: %s", m.Version, short(m.Commit), attach.Tilde(dir), strings.Join(names, ", "))
	a.say("%s", note)
	a.sayHeldDrivers(held)
	return nil
}

// programLast is names with potluck itself last: one that is there finds its
// drivers beside it, so an install cut off before its end is one to do again
// (the Mac app installs the release it carries when there is no potluck).
func programLast(names []string) []string {
	out := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == "potluck" })
	if len(out) < len(names) {
		out = append(out, "potluck")
	}
	return out
}

// upgradeCutOff reports that what put st's potluck in dir was cut off, or that
// the removal of it was: dir says so or lacks one of its programs, the daemon
// still runs the potluck it replaced there or was not started again, or a
// driver it placed was not re-pinned (with filesOnly neither is potluck's own to tell).
func (a *app) upgradeCutOff(ctx context.Context, cfg *config.Config, dir string, st upgrade.Staged, filesOnly bool) bool {
	names, _ := withoutAttachedDrivers(st.Names, dir, a.attachedTo())
	if swap.Placing(dir) || slices.ContainsFunc(names, func(n string) bool { return !lexists(filepath.Join(dir, n)) }) {
		return true
	}
	if filesOnly {
		return false
	}
	h, running := health(ctx, client.NewUnix(cfg.SocketPath()))
	if !running && restartCutOff(cfg.Home) {
		return true
	}
	if running && !st.Manifest.RunBy(h.Version, h.Commit) {
		if exe, err := processExe(h.PID); err == nil && samePath(strings.TrimSuffix(exe, " (deleted)"), filepath.Join(dir, "potluck")) {
			return true
		}
	}
	return unpinnedDrivers(cfg.Home, dir, st)
}

// restartCutOff reports that an upgrade on home was cut off while it restarted
// the daemon, and no person has stopped that since.
func restartCutOff(home string) bool {
	_, cut, err := swap.ReadPending(home)
	return (cut || err != nil) && !lexists(filepath.Join(home, stoppedName))
}

// unpinnedDrivers reports that a driver installed in dir is, byte for byte,
// the verified release's in st, but home pins another file there, so the
// daemon refuses it.
func unpinnedDrivers(home, dir string, st upgrade.Staged) bool {
	for _, n := range st.Names {
		driver := protocol.DriverProgram(n)
		if driver == "" {
			continue
		}
		at := filepath.Join(dir, n)
		pin, ok, err := runtarget.PinOf(home, driver)
		if err != nil || !ok || !samePath(pin.Path, at) {
			continue
		}
		now, _, err := softnet.FileSHA256(at)
		if err != nil || now == pin.SHA256 {
			continue
		}
		if want, _, err := softnet.FileSHA256(filepath.Join(st.Dir, n)); err == nil && now == want {
			return true
		}
	}
	return false
}

// withoutAttachedDrivers drops the driver binaries from names when dir is a
// directory another program's daemon runs its drivers from: that daemon pins
// each driver by checksum, so replacing one under it would make it refuse the
// driver until it is re-pinned with that program's own command.
func withoutAttachedDrivers(names []string, dir string, other *daemonrecord.Record) (place, held []string) {
	if other == nil {
		return names, nil
	}
	dirs := []string{filepath.Join(other.Home, protocol.DriversDir)}
	if other.Exe != "" {
		dirs = append(dirs, filepath.Dir(other.Exe))
	}
	for _, d := range dirs {
		if !daemonrecord.SameHome(d, dir) {
			continue
		}
		for _, n := range names {
			if strings.HasPrefix(n, "potluck-driver-") {
				held = append(held, n)
			} else {
				place = append(place, n)
			}
		}
		return place, held
	}
	return names, nil
}

func (a *app) sayHeldDrivers(held []string) {
	if len(held) > 0 {
		a.say("left %s as they are: they sit where the daemon in use runs its drivers from, and it pins them; replace them with that program's own upgrade", strings.Join(held, ", "))
	}
}

// repin re-pins the drivers already pinned in dir to the files placed (a
// home with no pins file pins each driver on its first use).
func (a *app) repin(cfg *config.Config, dir string, placed *swap.Placed) []string {
	if _, err := os.Stat(filepath.Join(cfg.Home, runtarget.PinFile)); err != nil {
		return nil
	}
	repinned, err := runtarget.RepinPlaced(cfg.Home, dir, placed.Sums)
	if err != nil {
		fmt.Fprintf(a.host.Stderr, "%s: warning: re-pinning the drivers: %v (re-pin by hand: %s)\n", a.name(), err, a.cmd("driver pin <driver>"))
	}
	return repinned
}

// ownDir makes dir if it is missing, writable by its owner alone: the daemon
// runs drivers only from a directory no one else can write (DRIVERS.md),
// whatever the umask.
func ownDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Chmod(dir, 0o755)
}

// health is the daemon's health, and false when none answers.
func health(ctx context.Context, c *client.Client) (api.Health, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	h, err := c.Health(ctx)
	return h, err == nil
}

// daemonRun is how a daemon runs: its program and whether it joins the
// tailnet with --tailnet (config does not enable it).
type daemonRun struct {
	bin     string
	tailnet bool
}

func (r daemonRun) argv() []string {
	if r.tailnet {
		return []string{r.bin, "daemon", "--tailnet"}
	}
	return []string{r.bin, "daemon"}
}

// runningDaemon is the running daemon (h): its program, else fallback when
// it cannot be read, and --tailnet when its node runs but config does not
// enable the tailnet (it was started with --tailnet).
func (a *app) runningDaemon(ctx context.Context, cfg *config.Config, c *client.Client, h api.Health, fallback string) daemonRun {
	exe, err := processExe(h.PID)
	r := daemonRun{bin: oldDaemonBin(exe, err, fallback)}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ts, err := c.Tailnet(tctx)
	r.tailnet = keepTailnet(ts, err, cfg.File.Tailnet.Enabled)
	return r
}

// oldDaemonBin is the program to start the old daemon from again: the one
// it ran (exe), unless that could not be read, is not an absolute path, or
// was deleted; then fallback (the install directory's potluck, which the
// restore puts back).
func oldDaemonBin(exe string, err error, fallback string) string {
	if err != nil || !filepath.IsAbs(exe) || strings.HasSuffix(exe, " (deleted)") {
		return fallback
	}
	return exe
}

// keepTailnet: the restarted daemon needs --tailnet when the running one's
// node runs and config does not enable the tailnet.
func keepTailnet(ts api.TailnetStatus, err error, configured bool) bool {
	return err == nil && ts.Running && !configured
}

// oneWaySwap reports a preflight whose build applies migrations the
// previous build cannot read (swap.Check's note): such an upgrade is never
// rolled back across.
func oneWaySwap(pf api.Preflight) bool {
	for _, n := range pf.Notes {
		if strings.Contains(n, "the swap is one-way") {
			return true
		}
	}
	return false
}

// restartOn stops the running daemon (process pid, holding lock), waits
// until it is gone and starts run's, which must answer with commit. A new
// daemon that runs but fails that is stopped (TERM, then KILL) before
// restartOn returns.
func (a *app) restartOn(ctx context.Context, cfg *config.Config, c *client.Client, pid int, lock *swap.Lock, run daemonRun, commit string) error {
	if _, err := stopAndWait(ctx, c, pid, api.DaemonStopRequest{What: "upgrade", Swap: lock.Holder.Nonce}); err != nil {
		return err
	}
	p, err := a.startDetachedFrom(ctx, cfg, run)
	if err == nil {
		if h, up := health(ctx, c); !up || !release.SameCommit(h.Commit, commit) {
			err = fmt.Errorf("the daemon answering after the restart runs %s (%s), not the release's %s", h.Version, h.Commit, short(commit))
		}
	}
	if err != nil && p != nil {
		stopProcess(p)
	}
	return err
}

// startOldAgain brings the old daemon (process pid, as run) back after a
// failed restart: it runs on when it never stopped, else it is started
// once its process is gone (it holds the home's lock until then).
func (a *app) startOldAgain(ctx context.Context, cfg *config.Config, c *client.Client, pid int, run daemonRun) error {
	if h, up := health(ctx, c); up && h.PID == pid {
		return nil
	}
	if !waitGone(ctx, c, pid, stopWait) {
		return fmt.Errorf("the home is not free after %s: a daemon still answers, or the old one (pid %d) has not exited and holds the home's lock; "+
			"once it is gone (kill %d if it never exits), start it again: %s daemon --detach", stopWait, pid, pid, run.bin)
	}
	if _, err := a.startDetachedFrom(ctx, cfg, run); err != nil {
		return fmt.Errorf("the old daemon (%s) did not start again: %w", run.bin, err)
	}
	return nil
}

// upgradeStopWait bounds each of a failed new daemon's TERM and KILL.
var upgradeStopWait = 5 * time.Second

// stopProcess ends p: TERM, then KILL if it is still there.
func stopProcess(p *swap.Detached) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if p.Signal(sig) != nil {
			return
		}
		select {
		case <-p.Exited:
			return
		case <-time.After(upgradeStopWait):
		}
	}
}

// startDetachedFrom starts run's daemon detached and waits until it
// answers. The process is returned whenever it was started, also with an
// error (it runs but did not answer in time), so the caller can stop it.
func (a *app) startDetachedFrom(ctx context.Context, cfg *config.Config, run daemonRun) (*swap.Detached, error) {
	p, err := swap.StartDetached(ctx, swap.Detach{Argv: run.argv(), Env: upgradeEnv(os.Environ(), cfg.Home),
		Log: cfg.Path(config.FileLog), Home: cfg.Home})
	if err != nil {
		return nil, err
	}
	c := client.NewUnix(cfg.SocketPath())
	wait := upgradeStartWait
	if d, err := time.ParseDuration(os.Getenv(EnvUpgradeStartWait)); err == nil && d > 0 {
		wait = d
	}
	deadline := time.Now().Add(wait)
	for {
		if _, up := health(ctx, c); up {
			return p, nil
		}
		select {
		case <-p.Exited:
			return nil, fmt.Errorf("daemon did not start: %s", lastLine(cfg.Path(config.FileLog)))
		case <-ctx.Done():
			return p, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return p, fmt.Errorf("daemon did not answer within %s; see %s", wait, cfg.Path(config.FileLog))
		}
	}
}

// EnvUpgradeStartWait overrides how long an upgrade waits for the new
// daemon to answer (a Go duration; tests).
const EnvUpgradeStartWait = "POTLUCK_UPGRADE_START_WAIT"

// upgradeEnv is the restarted daemon's environment: daemonEnv's, without
// the GitHub tokens the upgrade may have been given.
func upgradeEnv(environ []string, home string) []string {
	var env []string
	for _, kv := range daemonEnv(environ, home) {
		if k, _, _ := strings.Cut(kv, "="); k == "GH_TOKEN" || k == "GITHUB_TOKEN" || k == EnvUpgradeStartWait {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// preflightTimeout bounds the new build's preflight.
var preflightTimeout = 60 * time.Second

// preflightBinary runs bin's `daemon preflight --json` against cfg's home.
// A build without one fails it.
func preflightBinary(ctx context.Context, cfg *config.Config, bin string) api.Preflight {
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "daemon", "preflight", "--json")
	cmd.Env = append(upgradeEnv(os.Environ(), cfg.Home), EnvNoAutostart+"=1")
	out, err := cmd.Output()
	var pf api.Preflight
	if jerr := json.Unmarshal(out, &pf); jerr != nil || pf.Build == "" {
		msg := fmt.Sprintf("%s has no working `%s daemon preflight`", bin, programName)
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg += ": " + lastLine2(string(ee.Stderr))
		}
		return api.Preflight{Build: bin, Problems: []string{msg}}
	}
	return pf
}

func lastLine2(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// preflightCmd is `potluck daemon preflight`: can this build take over
// from the daemon running on this home (or, with none, its databases)?
func (a *app) preflightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "Can this potluck take over from the running daemon? (exit 1 if not)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.notWhileAttached(a.cmd("daemon preflight")); err != nil {
				return err
			}
			cfg, err := a.loadClient()
			if err != nil {
				return err
			}
			pf, err := preflightLocal(cmd.Context(), cfg, a.prog.wiring())
			if err != nil {
				return err
			}
			if a.asJSON() {
				if err := a.printJSON(pf); err != nil {
					return err
				}
			} else {
				for _, n := range pf.Notes {
					a.say("note: %s", n)
				}
				for _, p := range pf.Problems {
					a.say("problem: %s", p)
				}
				if pf.OK {
					a.say("preflight ok: %s can take over from %s", pf.Build, cmp.Or(pf.Running, "no running daemon"))
				}
			}
			if !pf.OK {
				return exitSilently(1)
			}
			return nil
		},
	}
}

// preflightLocal is this build against the daemon running on cfg's home
// (swap.Check, as zm's updater runs it).
func preflightLocal(ctx context.Context, cfg *config.Config, w server.Wiring) (api.Preflight, error) {
	build, err := server.ThisBuild(ctx, w)
	if err != nil {
		return api.Preflight{}, fmt.Errorf("describe this build: %w", err)
	}
	applied, aerr := swap.Applied(ctx, cfg.Home)
	var running *swap.Running
	c := client.NewUnix(cfg.SocketPath())
	if h, ok := health(ctx, c); ok {
		running = &swap.Running{Health: h}
		if m, err := c.Manifest(ctx); err == nil {
			running.Manifest = &m
		} else if rs, err := c.Remotes(ctx); err == nil {
			for _, r := range rs {
				if r.State == "" || r.State == "ok" {
					running.Joined = append(running.Joined, r.Project.Slug)
				}
			}
		}
	}
	pf := swap.Check(build, running, applied, swap.Databases(cfg.Home))
	if aerr != nil {
		pf.Problems = append(pf.Problems, "read the databases: "+aerr.Error())
		pf.OK = false
	}
	return pf, nil
}

// short is a commit's first 12 digits.
func short(commit string) string { return commit[:min(12, len(commit))] }

// inTempDir reports whether dir is inside the system's temporary directory (symlinks resolved).
func inTempDir(dir string) bool {
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	t, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(t, d)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
