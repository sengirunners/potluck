package cli

// potluck upgrade: install the newest signed release (internal/upgrade) over
// this potluck's programs with kit/swap's Place (each old one kept as
// <name>.prev), then restart the daemon on the new build if one runs, and put
// everything back if the new daemon does not come up. Once the new build is
// final it re-pins the drivers it placed (runtarget.RepinPlaced, DRIVERS.md).
// install.sh runs a release's own potluck upgrade to install it.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/internal/upgrade"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/runtarget"
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
	return cmd
}

func (a *app) upgrade(ctx context.Context, from, dir string, dryRun bool) error {
	cfg, err := config.LoadClient()
	if err != nil {
		return err
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
	var src upgrade.Source = upgrade.NewGitHub(os.Getenv)
	if from != "" {
		src = upgrade.Dir(from)
	}
	stage, err := os.MkdirTemp("", "potluck-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	keys, err := upgrade.Keys(cfg.File.UpdateKeys)
	if err != nil {
		return fmt.Errorf("refusing to upgrade: %w", err)
	}
	st, err := upgrade.Stage(src, keys, runtime.GOOS, runtime.GOARCH, stage)
	if err != nil {
		return err
	}
	m := st.Manifest
	// A GitHub source is named, and a --release directory outside the temporary directory; install.sh's release
	// directory is temporary, so it is not.
	where := ""
	if from == "" || !inTempDir(from) {
		where = " from " + src.String()
	}
	a.say("verified potluck %s (%s)%s: its manifest is signed by a key this potluck trusts, and its %s/%s tarball matches", m.Version, short(m.Commit), where, runtime.GOOS, runtime.GOARCH)

	// This build, installed where it runs: nothing to do.
	if filepath.Dir(self) == dir && m.RunBy(api.Version, api.Commit) {
		a.say("potluck %s is already installed in %s", api.Version, tildePath(dir))
		return nil
	}
	state, installed, err := release.LoadState(cfg.Home)
	if err != nil {
		return err
	}
	if why := upgrade.Older(m, state, installed); why != "" {
		return errors.New(why)
	}
	c := client.NewUnix(cfg.SocketPath())
	h, running := health(ctx, c)
	restart := running && !m.RunBy(h.Version, h.Commit)
	if dryRun {
		a.say("would install %s into %s (each old one kept as <name>%s)", strings.Join(st.Names, ", "), tildePath(dir), swap.PrevSuffix)
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
		return err
	}
	defer lock.Release()
	if running { // it may have stopped or changed while the release was fetched
		h, running = health(ctx, c)
		restart = running && !m.RunBy(h.Version, h.Commit)
	}

	// The new build's own preflight against this home and its daemon: data
	// it cannot read, or routes the running daemon serves that it lacks,
	// refuse the upgrade before anything moves.
	pf := preflightBinary(ctx, cfg, filepath.Join(stage, "potluck"))
	if !pf.OK {
		return fmt.Errorf("the new build's preflight refused this upgrade: %s", strings.Join(pf.Problems, "; "))
	}
	oneWay := oneWaySwap(pf)

	// What the running daemon is, to bring it back as it was: its own
	// program (not necessarily dir's) and whether its tailnet node runs.
	var old daemonRun
	if restart {
		old = a.runningDaemon(ctx, cfg, c, h, filepath.Join(dir, "potluck"))
	}
	if err := ownDir(dir); err != nil {
		return err
	}
	placed, err := swap.Place(stage, dir, st.Names)
	if err != nil {
		return fmt.Errorf("install into %s: %w", dir, err)
	}
	if restart {
		newRun := daemonRun{bin: filepath.Join(dir, "potluck"), tailnet: old.tailnet}
		if err := a.restartOn(ctx, cfg, c, h.PID, lock, newRun, m.Commit); err != nil {
			if oneWay {
				a.repin(cfg, dir, placed)
				if serr := release.WriteState(cfg.Home, state.Installing(m, time.Now())); serr != nil {
					err = errors.Join(err, serr)
				}
				return fmt.Errorf("the new daemon did not come up (%v). Its migrations are one-way (the previous build cannot read the data), "+
					"so potluck %s stays installed in %s, the old programs are kept beside it as <name>%s, and no daemon is running now; see %s",
					err, m.Version, tildePath(dir), swap.PrevSuffix, cfg.Path(config.FileLog))
			}
			rerr := placed.Restore()
			if rerr == nil {
				rerr = a.startOldAgain(ctx, cfg, c, h.PID, old)
			}
			if rerr != nil {
				return fmt.Errorf("the new daemon did not come up (%v), and putting the old one back failed: %w", err, rerr)
			}
			return fmt.Errorf("the new daemon did not come up, so the old programs are back and the old daemon (%s) runs again: %w", old.bin, err)
		}
	}
	repinned := a.repin(cfg, dir, placed)
	if err := release.WriteState(cfg.Home, state.Installing(m, time.Now())); err != nil {
		return err
	}
	a.say("installed potluck %s (%s) into %s: %s", m.Version, short(m.Commit), tildePath(dir), strings.Join(st.Names, ", "))
	if len(repinned) > 0 {
		a.say("re-pinned drivers: %s", strings.Join(repinned, ", "))
	}
	if restart {
		a.say("restarted the daemon on %s", m.Version)
	}
	return nil
}

// repin re-pins the drivers already pinned in dir to the files placed (a
// home with no pins file pins each driver on its first use).
func (a *app) repin(cfg *config.Config, dir string, placed *swap.Placed) []string {
	if _, err := os.Stat(filepath.Join(cfg.Home, runtarget.PinFile)); err != nil {
		return nil
	}
	repinned, err := runtarget.RepinPlaced(cfg.Home, dir, placed.Sums)
	if err != nil {
		fmt.Fprintf(a.stderr, "potluck: warning: re-pinning the drivers: %v (re-pin by hand: potluck driver pin <driver>)\n", err)
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
		msg := fmt.Sprintf("%s has no working `potluck daemon preflight`", bin)
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
			cfg, err := config.LoadClient()
			if err != nil {
				return err
			}
			pf, err := preflightLocal(cmd.Context(), cfg, a.prog.wiring())
			if err != nil {
				return err
			}
			if a.json {
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
