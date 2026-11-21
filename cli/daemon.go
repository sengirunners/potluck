package cli

// potluck daemon, daemon stop and status, and starting the daemon when a
// command needs it (copied from zm's cmd/zm main.go and autostart.go, without
// zm's swaps, upgrades and agent sessions).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/wiring"
)

// EnvNoAutostart = 1 turns autostart off (scripts, tests that expect "not running").
const EnvNoAutostart = "POTLUCK_NO_AUTOSTART"

func (a *app) daemonCmd() *cobra.Command {
	var detach, verbose, tailnet bool
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run this machine's " + a.name() + " daemon (in the foreground; --detach to start it in the background)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.refuseBesideOther(); err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if tailnet {
				cfg.File.Tailnet.Enabled = true // this run only
			}
			if detach {
				already, err := a.startDetached(cmd.Context(), cfg, tailnet)
				switch {
				case err != nil:
					return err
				case already:
					a.say("%s already running: %s", a.cmd("daemon"), cfg.SocketPath())
				default:
					a.say("%s started: %s", a.cmd("daemon"), cfg.SocketPath())
				}
				return nil
			}
			lvl := slog.LevelInfo
			if verbose {
				lvl = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(a.host.Stderr, &slog.HandlerOptions{Level: lvl}))
			return serve(cmd.Context(), cfg, log, a.prog.wiring())
		},
	}
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "start in the background, logging to $POTLUCK_HOME/daemon.log")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "log every request")
	cmd.Flags().BoolVar(&tailnet, "tailnet", false, "also join your tailnet for this run")
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop this machine's daemon and wait until it has stopped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.notWhileAttached(a.cmd("daemon stop")); err != nil {
				return err
			}
			cfg, err := a.loadClient()
			if err != nil {
				return err
			}
			return a.stopDaemon(cmd.Context(), cfg)
		},
	}
	cmd.AddCommand(stop, a.preflightCmd(), a.freezeCmd(), a.unfreezeCmd())
	return cmd
}

// serve runs the daemon on cfg's home wired by w (Potluck's: the Compute
// wiring, Program.wiring) until ctx ends; a parked one listens on the
// tailnet and the tunnel too (wiring.Parked), so its term is read from
// other machines.
func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, w server.Wiring) error {
	if err := cfg.EnsureHome(); err != nil {
		return err
	}
	release, err := server.LockHome(cfg.Home)
	if err != nil {
		return err
	}
	defer release()
	dm, err := server.New(ctx, cfg, log, w, server.ParkedListeners(wiring.Parked))
	if err == nil {
		err = dm.Serve(ctx)
	}
	log.Info("daemon exiting", "pid", os.Getpid(), "err", err)
	// Tests: a daemon slow to exit, still holding its home's lock.
	if d, derr := time.ParseDuration(os.Getenv(EnvExitDelay)); derr == nil && d > 0 {
		time.Sleep(d)
	}
	return err
}

// EnvExitDelay keeps a stopped daemon's process, and so its home's lock,
// for this long after it stopped answering (a Go duration; tests).
const EnvExitDelay = "POTLUCK_TEST_EXIT_DELAY"

// stopDaemon asks the daemon on cfg's socket to stop and waits until it is
// gone (stopAndWait).
func (a *app) stopDaemon(ctx context.Context, cfg *config.Config) error {
	c := client.NewUnix(cfg.SocketPath())
	pid := 0
	if h, err := c.Health(ctx); err == nil {
		pid = h.PID
	}
	stopped, err := stopAndWait(ctx, c, pid, api.DaemonStopRequest{What: "stop"})
	if err != nil {
		return err
	}
	if !stopped {
		a.say("no %s is running at %s", a.cmd("daemon"), cfg.SocketPath())
		return nil
	}
	a.say("%s stopped", a.cmd("daemon"))
	return nil
}

// stopWait bounds a daemon's exit after it was asked to stop.
var stopWait = 30 * time.Second

// stopAndWait asks the daemon on c (process pid, 0 when not known) to stop
// and waits until it is gone (waitGone); false when none was running. A
// daemon that no longer answers still holds its home's lock until its
// process exits, and a daemon started before that is refused (LockHome).
func stopAndWait(ctx context.Context, c *client.Client, pid int, req api.DaemonStopRequest) (bool, error) {
	if _, err := c.StopDaemon(ctx, req); err != nil {
		if client.NotRunning(err) {
			return false, nil
		}
		return false, fmt.Errorf("stop the daemon: %w", err)
	}
	if !waitGone(ctx, c, pid, stopWait) {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		if _, err := c.Health(ctx); err == nil {
			return true, fmt.Errorf("the daemon (pid %d) still answers %s after it was asked to stop", pid, stopWait)
		}
		return true, fmt.Errorf("the daemon (pid %d) stopped answering but its process has not exited %s after it was asked to stop, so it still holds its home's lock; "+
			"if it never exits: kill %d, then start again", pid, stopWait, pid)
	}
	return true, nil
}

// processGone reports that process pid has exited (tests replace it: a daemon
// they run in-process shares their pid).
var processGone = func(pid int) bool { return syscall.Kill(pid, 0) != nil }

// waitGone waits until the daemon on c stops answering and its process pid
// has exited (as zm's updater waits, cmd/zm daemon_swap.go); false when it
// has not within max, or ctx ended.
func waitGone(ctx context.Context, c *client.Client, pid int, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, herr := c.Health(hctx)
		cancel()
		if herr != nil && ctx.Err() == nil && (pid <= 0 || processGone(pid)) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Is the daemon running, and the pools this machine is in (exit 3 if not running, 1 on a self-check problem)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cfg, err := a.client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			h, err := c.Health(ctx)
			if err != nil {
				return err
			}
			// A parked home serves no pool: its term says why (a daemon
			// older than the term route answers not_found: it serves).
			var parked *api.AuthorityTerm
			if t, err := c.AuthorityTerm(ctx); err == nil && t.State != api.HQServing {
				parked = &t
			}
			type poolView struct {
				Pool     string   `json:"pool"`
				Role     string   `json:"role"`
				Machines []string `json:"machines"`
				Sealing  string   `json:"sealing,omitempty"`
				Error    string   `json:"error,omitempty"`
			}
			var pools []poolView
			if l, err := c.ProjectList(ctx); err == nil && parked == nil {
				for _, p := range l.Projects {
					v := poolView{Pool: p.Slug, Role: p.Role}
					names, err := poolMachines(ctx, inPool(c, p.Slug))
					v.Machines = names
					v.Sealing = poolSealing(ctx, a, p.Slug)
					if err != nil {
						v.Error = err.Error()
					}
					pools = append(pools, v)
				}
			}
			problems := h.SelfCheck != nil && !h.SelfCheck.OK
			type daemonView struct {
				Program string `json:"program"`
				Exe     string `json:"exe"`
			}
			var other *daemonView // another program's daemon, which this run uses
			if o := a.attachedTo(); o != nil {
				other = &daemonView{Program: o.Program, Exe: o.Exe}
			}
			if a.asJSON() {
				if err := a.printJSON(struct {
					Health api.Health         `json:"health"`
					Home   string             `json:"home"`
					Daemon *daemonView        `json:"daemon_of,omitempty"`
					Parked *api.AuthorityTerm `json:"parked,omitempty"`
					Pools  []poolView         `json:"pools"`
				}{h, cfg.Home, other, parked, pools}); err != nil {
					return err
				}
			} else {
				s := fmt.Sprintf("%s %s (%s, %s), home %s", a.name(), h.Version, h.Commit, h.Protocol, tildePath(cfg.Home))
				if other != nil {
					s = fmt.Sprintf("using %s's daemon (%s, %s), home %s: this machine's Potluck daemon",
						other.Program, h.Version, h.Protocol, tildePath(cfg.Home))
				}
				if h.Machine != "" {
					s += fmt.Sprintf("\nmachine %s, pid %d, up %s", h.Machine, h.PID, time.Since(h.Started).Round(time.Second))
				}
				if other == nil && (h.Version != api.Version || h.Commit != api.Commit) {
					s += fmt.Sprintf("\nnote: this command is %s (%s); restart the daemon to match (%s, then %s)", api.Version, api.Commit, a.cmd("daemon stop"), a.cmd("daemon --detach"))
				}
				if sc := h.SelfCheck; problems {
					s += fmt.Sprintf("\nPROBLEM: this daemon cannot serve %d thing(s):", len(sc.Problems))
					for _, p := range sc.Problems {
						s += "\n  - " + p
					}
				}
				switch {
				case parked != nil:
					s += fmt.Sprintf("\nPARKED: this home is %s at epoch %d: its daemon answers only its health and term, and serves no pool", parked.State, parked.Epoch)
					if parked.State == api.HQFrozen {
						s += " (" + a.cmd("daemon unfreeze") + " serves it again)"
					}
				case len(pools) == 0:
					s += "\nno pool yet: " + a.cmd("create <pool>") + ", or " + a.cmd("join '<invite code>'")
				}
				for _, p := range pools {
					s += fmt.Sprintf("\npool %s (%s): ", p.Pool, p.Role)
					switch {
					case p.Error != "":
						s += "machines not listed: " + p.Error
					case len(p.Machines) == 0:
						s += "no machine shared yet (" + a.cmd("host") + ")"
					default:
						s += "sandboxes on " + strings.Join(p.Machines, ", ")
					}
					if p.Sealing != "" {
						s += "; sealing " + p.Sealing
					}
				}
				a.say("%s", s)
			}
			if problems {
				return exitSilently(1)
			}
			return nil
		},
	}
}

// noAutostart are the commands (and everything under them) that never start
// the daemon: asking whether it runs, and running or stopping it. Keyed by
// the command's words under the root (commandWords).
var noAutostart = map[string]bool{
	"help":        true,
	"completion":  true,
	"status":      true,
	"daemon":      true,
	"upgrade":     true,
	"uninstall":   true, // it never starts a daemon to remove one's files
	"mcp":         true,
	"host status": true, // the menu bar item asks every few seconds
	"host set":    true,
}

// autostartAllowed reports whether cmd may start the daemon.
func autostartAllowed(cmd *cobra.Command, getenv func(string) string) bool {
	if cmd == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvNoAutostart))) {
	case "", "0", "false", "no":
	default:
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		if noAutostart[commandWords(c)] {
			return false
		}
	}
	return true
}

// commandWords are cmd's words under its root: "host status" for the
// potluck program's `potluck host status` ("" for the root).
func commandWords(cmd *cobra.Command) string {
	var words []string
	for c := cmd; c.HasParent(); c = c.Parent() {
		words = append([]string{c.Name()}, words...)
	}
	return strings.Join(words, " ")
}

// fileAutostartLock serialises autostarting commands ($POTLUCK_HOME).
const fileAutostartLock = "autostart.lock"

// autostart starts the daemon for cfg, detached, unless another process
// already did, and says so on stderr. A lock file serialises concurrent
// starters, so two commands run at once start one daemon.
func (a *app) autostart(ctx context.Context, cfg *config.Config) error {
	if o := a.otherStarting(ctx); o != nil {
		return fmt.Errorf("%s's daemon (home %s) has just started, and potluck's commands use it: run this again", o.Program, tildePath(o.Home))
	}
	if err := cfg.EnsureHome(); err != nil {
		return err
	}
	f, err := os.OpenFile(cfg.Path(fileAutostartLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	already, err := a.startDetached(ctx, cfg, false)
	if err != nil || already {
		return err
	}
	fmt.Fprintf(a.host.Stderr, "%s: started the daemon (%s)\n", a.name(), tildePath(cfg.Home))
	return nil
}

// spawnDetached starts the daemon's process for startDetached. A test stands
// a fake in: a command run in a test must never start the test binary.
var spawnDetached = swap.StartDetached

// startDetached runs `potluck daemon` in its own session, logging to
// daemon.log, and waits until it answers. already: one was running.
func (a *app) startDetached(ctx context.Context, cfg *config.Config, tailnet bool) (already bool, err error) {
	if err := cfg.EnsureHome(); err != nil {
		return false, err
	}
	c := client.NewUnix(cfg.SocketPath())
	if _, err := c.Health(ctx); err == nil {
		return true, nil
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	argv := []string{self, "daemon"}
	if tailnet {
		argv = append(argv, "--tailnet")
	}
	p, err := spawnDetached(ctx, swap.Detach{Argv: argv, Env: daemonEnv(os.Environ(), cfg.Home),
		Log: cfg.Path(config.FileLog), Home: cfg.Home})
	if err != nil {
		return false, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := c.Health(ctx); err == nil {
			return false, nil
		}
		select {
		case <-p.Exited: // say why (another daemon holds the lock), not "not running"
			return false, fmt.Errorf("daemon did not start: %s", lastLine(cfg.Path(config.FileLog)))
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("daemon did not answer within 10s; see %s", cfg.Path(config.FileLog))
		}
	}
}

// daemonEnv is the environment for a detached daemon on home: this one's,
// with POTLUCK_HOME set to home.
func daemonEnv(environ []string, home string) []string {
	var env []string
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); k == EnvHome || k == EnvNoAutostart {
			continue
		}
		env = append(env, kv)
	}
	return append(env, EnvHome+"="+home)
}

// lastLine is the last non-empty line of a file (the daemon's exit reason).
func lastLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "see " + path
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// tildePath shows p under the user's home as ~/...
func tildePath(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if rel, err := filepath.Rel(h, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		if rel == "." {
			return "~"
		}
		return "~/" + rel
	}
	return p
}
