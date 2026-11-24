package cli

// One host run: one at a time per home (host.lock), the host program's
// AfterLend once a size is shared, a line now and then while a long step is
// quiet, and --detach, which does the checks and the admin step here and
// the rest in the background, its words in the home's host.log for host
// status.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/softnet"
)

// The files of a host run in the program's home: the lock, the pid of the
// run that holds it, the pid of a background run while it holds it, and the
// background run's words.
const (
	hostLockName  = "host.lock"
	hostPIDName   = "host.pid"
	hostBgPIDName = "host.bgpid"
	hostLogName   = "host.log"
)

// envHostLockFD tells a background host run that the run that started it
// handed it the lock, held, as this file descriptor (so no other run takes
// it in between).
const envHostLockFD = "POTLUCK_HOST_LOCK_FD"

// runHost is host's run, and its old spelling host share's: arg is the
// invite code or pool ("" for the only pool this machine is in). With
// askFirst, a run that would share again what this machine already shares
// with the pool only says so (askAgain, host_again.go).
func (a *app) runHost(cmd *cobra.Command, s *shareFlags, arg string, askFirst bool) error {
	ctx := cmd.Context()
	handed := handedLock() // before this run starts any process
	if s.inviteStdin {
		var err error
		if arg, err = a.stdinInvite(cmd); err != nil {
			return err
		}
	}
	// An invite cut short or damaged is refused before the daemon, the lock
	// or a pull.
	if err := a.checkInvite(arg, a.cmd("invite <you> --host")); err != nil {
		return err
	}
	o, err := a.shareOptions(cmd, s)
	if err != nil {
		return err
	}
	var c *client.Client
	var setup *hostsetup.Tracker
	if !o.DryRun { // a dry run never starts the daemon, nor needs the pool or the lock
		var cfg *config.Config
		if c, cfg, err = a.client(); err != nil {
			return err
		}
		if cfg.Home == "" {
			return errors.New("no home to keep the host run's lock in")
		}
		if err := os.MkdirAll(cfg.Home, 0o700); err != nil { // a new home: the daemon starts on first use
			return err
		}
		lock, unlock, err := a.hostLock(cfg.Home, handed)
		if err != nil {
			return err
		}
		defer unlock()
		if arg == "" {
			if arg, err = a.onlyPool(ctx, c); err != nil {
				return a.hostManyPools(err)
			}
		}
		if askFirst {
			if err := a.askAgain(ctx, c, arg, o); err != nil {
				return err
			}
		}
		if s.detach {
			return a.hostDetach(cmd, c, arg, o, s, cfg.Home, lock)
		}
		setup = a.startSetup(cmd, &o, cfg.Home, arg, handed >= 0)
	}
	err = a.poolHost(ctx, c, shareRunner, arg, o, a.host.Stdout, a.rootStep(ctx, s), a.afterLend(s, o.DryRun))
	if setup != nil {
		defer a.endSetup(ctx, setup, err)
	}
	if sc := (*spentCodeError)(nil); errors.As(err, &sc) {
		sc.Flags = shareFlagArgs(cmd)
	}
	if ns := (*noSizePassedError)(nil); errors.As(err, &ns) {
		ns.flags = a.shapingFlags(cmd)
	}
	if err == nil && !o.DryRun {
		if note := a.pausedNote(); note != "" {
			fmt.Fprintln(a.notes(), note)
		}
	}
	return err
}

// notes is where a line beside a command's answer goes: stdout, or stderr
// under --json so stdout stays the answer.
func (a *app) notes() io.Writer {
	if a.asJSON() {
		return a.host.Stderr
	}
	return a.host.Stdout
}

// afterLend is the host program's AfterLend for this run: nil without one,
// in a dry run, or when its skip flag was given.
func (a *app) afterLend(s *shareFlags, dryRun bool) func() {
	al := a.host.AfterLend
	if al == nil || dryRun || s.skipAfter {
		return nil
	}
	return func() { al.Run(a.notes()) }
}

// hostDetach checks what host checks before it pulls (the pool, the
// shared sizes it replaces, the admin step, a dry run of the plan), then
// starts the same command again without --detach and never asking for a
// password, its words to the home's host.log, hands it the lock, and
// returns. Under --json --no-sudo a pending Softnet root step is its answer
// (admin_step), and nothing is started.
func (a *app) hostDetach(cmd *cobra.Command, c *client.Client, arg string, o hostprobe.ShareOptions, s *shareFlags, home string, lock *os.File) error {
	ctx := cmd.Context()
	if _, err := a.hostScope(ctx, c, arg, &o); err != nil {
		return err
	}
	if err := a.rootStep(ctx, s)(&o); err != nil {
		return err
	}
	dry := o
	dry.DryRun, dry.Progress = true, nil
	if _, err := hostprobe.Check(ctx, shareRunner, dry); err != nil {
		left, err := a.softnetStepLeft(ctx, o, s, err)
		if left != nil {
			return a.printJSON(map[string]adminStep{"admin_step": left.step})
		}
		return err
	}
	logPath := filepath.Join(home, hostLogName)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	pid, err := hostSpawn(detachArgs(cmd, arg, a.asJSON()), arg, log, lock)
	if err != nil {
		return err
	}
	// Marked now, while this run still holds the lock, so host status says
	// it goes on before it reaches its own lock; its unlock removes it.
	_ = os.WriteFile(filepath.Join(home, hostBgPIDName), []byte(strconv.Itoa(pid)+"\n"), 0o600)
	if a.asJSON() {
		return a.printJSON(map[string]any{"pid": pid, "log": logPath})
	}
	a.say("sharing goes on in the background (process %d): %s says how it goes, and %s holds its words", pid, a.cmd("host status"), logPath)
	return nil
}

// softnetStepLeft is the root step a --json --no-sudo run of the Mac app
// stopped at (err, from Check's dry run), for the app to run as root before
// the same command again: the managed Softnet's install, whose root half
// checks again, as root, what it installs (cli/adminstep.go). With none, the
// error the run is refused with: what no root step fixes (Precheck), so a
// password is never asked for a run that would not go on, else err, when
// Softnet is not what is wrong, something else is as well (Tart, say), or
// this Mac has no validated managed Softnet.
func (a *app) softnetStepLeft(ctx context.Context, o hostprobe.ShareOptions, s *shareFlags, err error) (*adminStepLeft, error) {
	var nr *hostprobe.NotReadyError
	if !a.asJSON() || !s.noSudo || !errors.As(err, &nr) || nr.Layer != "tart" {
		return nil, err
	}
	l, _ := o.Report.Layer(nr.Layer)
	adv := softnet.For(hostprobe.MacOSMajor(o.Report.MacOS), o.Report.Arch)
	if !l.SoftnetAlone || adv.Kind != softnet.Managed || adv.Candidate {
		return nil, err
	}
	if perr := hostprobe.Precheck(ctx, shareRunner, withSoftnetInstall(o)); perr != nil {
		return nil, perr
	}
	ierr := a.wordsAside(func() error { return a.softnetInstall(ctx, "", false, false, true) })
	var left *adminStepLeft
	if errors.As(ierr, &left) {
		return left, nil
	}
	if ierr != nil {
		fmt.Fprintf(a.host.Stderr, "Softnet could not be made ready: %v\n", ierr)
	}
	return nil, err
}

// withSoftnetInstall is o with the managed Softnet's install as its tart
// layer's root step, when the layer offers none (Softnet is missing, or is
// one no permission fixes).
func withSoftnetInstall(o hostprobe.ShareOptions) hostprobe.ShareOptions {
	o.Report.Layers = slices.Clone(o.Report.Layers)
	for i := range o.Report.Layers {
		if l := &o.Report.Layers[i]; l.Name == "tart" && len(l.RootSteps) == 0 {
			l.RootSteps = []string{"potluck softnet install"}
		}
	}
	return o
}

// hostSpawn starts the run in the background, in a session of its own, its
// output to log, the held lock its file descriptor 3, and arg (the invite
// code or pool) one line on its stdin, never in its argv or environment
// (detachArgs gives it --invite-stdin); tests stand in for it.
var hostSpawn = func(args []string, arg string, log, lock *os.File) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer r.Close()
	// Written whole and closed before the run starts: a line far below a
	// pipe's buffer never blocks, and the run reads it, then the end, even
	// once this one is gone.
	_, err = io.WriteString(w, arg+"\n")
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	c := exec.Command(exe, args...)
	c.Stdin, c.Stdout, c.Stderr = r, log, log
	c.ExtraFiles = []*os.File{lock}
	c.Env = append(os.Environ(), envHostLockFD+"=3")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, c.Process.Release()
}

// detachArgs are the background run's arguments: the command as the
// program spells it (its path below the program's root) and every flag set
// on it, without --detach (and --json, whose answer the detaching run
// gave), with --no-sudo (the admin step ran here, or was refused) and
// --invite-stdin: its argument arg, the invite code or pool, goes on its
// stdin (hostSpawn), never in its argv, where any user on this machine
// reads it for the whole run. So an invite given as a pool flag's value
// (the program's -P, or a host program's own) is left out too.
func detachArgs(cmd *cobra.Command, arg string, asJSON bool) []string {
	out := strings.Fields(cmd.CommandPath())[1:]
	skip := []string{"detach", "no-sudo"}
	if asJSON {
		skip = append(skip, "json")
	}
	invite := project.LooksLikeJoinCode(arg)
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if invite && strings.TrimSpace(f.Value.String()) == strings.TrimSpace(arg) {
			skip = append(skip, f.Name)
		}
	})
	for _, f := range givenFlags(cmd, skip...) {
		out = append(out, f.Args...)
	}
	return append(out, "--no-sudo", "--"+flagInviteStdin)
}

// flagInviteStdin is host's --invite-stdin, by its name: givenFlags and
// sameShare leave it out (it says where the argument came from, not how to
// share).
const flagInviteStdin = "invite-stdin"

// maxStdinInvite is the most --invite-stdin reads: an invite is far
// shorter.
const maxStdinInvite = 4096

// stdinInvite is host's argument read from stdin (--invite-stdin): its
// first line, trimmed. None, or an argument given as well, is refused.
// Neither the flags given (givenFlags) nor a bare re-run's test
// (sameShare) count the flag: it says where the argument came from.
func (a *app) stdinInvite(cmd *cobra.Command) (string, error) {
	if len(cmd.Flags().Args()) > 0 {
		return "", exitWith{2, errors.New("--invite-stdin reads the invite code (or pool) from stdin: give it no argument as well")}
	}
	// A host program that gives no Stdin still reads the process's own:
	// the background run's line comes there (hostSpawn), whichever program
	// mounts host.
	in := a.host.Stdin
	if a.noStdin {
		in = os.Stdin
	}
	line, err := bufio.NewReader(io.LimitReader(in, maxStdinInvite)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("--invite-stdin: reading stdin: %w", err)
	}
	arg := strings.TrimSpace(line)
	if arg == "" {
		return "", errors.New("--invite-stdin: no invite code (or pool) came on stdin, so there is nothing to join or share with; run host again with the invite")
	}
	return arg, nil
}

// hostLock takes the home's host lock for this run (an flock, so it ends
// with the process; a background run is handed it held, envHostLockFD) and
// writes the run's pid beside it; another run holds it: refused, naming
// that run. lock is the held file, for a background run to inherit.
func (a *app) hostLock(home string, handed int) (lock *os.File, unlock func(), err error) {
	path := filepath.Join(home, hostLockName)
	background := false
	if handed >= 0 {
		var fst, pst syscall.Stat_t
		if syscall.Fstat(handed, &fst) == nil && syscall.Stat(path, &pst) == nil && fst.Dev == pst.Dev && fst.Ino == pst.Ino &&
			syscall.Flock(handed, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			lock, background = os.NewFile(uintptr(handed), path), true
		}
		// Another file, or none: not this run's to close.
	}
	if lock == nil {
		if lock, err = takeLock(path); err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, nil, fmt.Errorf("another host run is going on%s: %s says how it goes; run this again once it ends", pidWords(home), a.cmd("host status"))
			}
			return nil, nil, err
		}
	}
	me := []byte(strconv.Itoa(os.Getpid()) + "\n")
	pidPath, bgPath := filepath.Join(home, hostPIDName), filepath.Join(home, hostBgPIDName)
	_ = os.WriteFile(pidPath, me, 0o600)
	if background {
		_ = os.WriteFile(bgPath, me, 0o600)
	} else {
		_ = os.Remove(bgPath) // this run holds the lock: a marker a killed background run left is stale
	}
	var once sync.Once
	return lock, func() {
		once.Do(func() {
			for _, p := range []string{pidPath, bgPath} {
				if b, _ := os.ReadFile(p); string(b) == string(me) {
					_ = os.Remove(p)
				}
			}
			lock.Close()
		})
	}, nil
}

// handedLock is the lock fd a run that went on in the background was
// handed (envHostLockFD), or -1. It clears the variable and marks the fd
// close-on-exec, so no process this run starts inherits either.
func handedLock() int {
	v := os.Getenv(envHostLockFD)
	if v == "" {
		return -1
	}
	os.Unsetenv(envHostLockFD)
	n, err := strconv.Atoi(v)
	var st syscall.Stat_t
	if err != nil || n < 3 || syscall.Fstat(n, &st) != nil { // never stdin, stdout or stderr
		return -1
	}
	syscall.CloseOnExec(n)
	return n
}

// takeLock opens path and takes its flock, trying again for a moment: host
// status (which the menu bar asks every few seconds) probes it with a short
// shared lock.
func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || i == 20 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	// Uninstall may have removed the home while this waited: a lock on a
	// file that is gone keeps no one out.
	held, herr := f.Stat()
	there, terr := os.Stat(path)
	if herr != nil || terr != nil || !os.SameFile(held, there) {
		f.Close()
		return nil, fmt.Errorf("%s was removed meanwhile (an uninstall?): run this again if that was not meant", path)
	}
	return f, nil
}

// hostBusy reports whether a host run holds the home's lock now. It only
// probes (a shared try-lock, no pid written).
func hostBusy(home string) bool {
	f, err := os.Open(filepath.Join(home, hostLockName))
	if err != nil {
		return false // never locked: no host run has been here
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
}

func pidWords(home string) string {
	b, err := os.ReadFile(filepath.Join(home, hostPIDName))
	if err != nil {
		return ""
	}
	return " (process " + strings.TrimSpace(string(b)) + ")"
}

// hostStopWait is how long a host run asked to stop has to end.
var hostStopWait = 20 * time.Second

// stopHostRun ends the host run that holds home's lock, and with a
// background one (which leads its own process group) the programs it
// started, and waits until the lock is free. None: nothing to do.
func stopHostRun(ctx context.Context, home string) error {
	asked := 0 // the run told to end; a run handing its lock to a background one changes the pid
	for end := time.Now().Add(hostStopWait); hostBusy(home); {
		pid := pidIn(home, hostPIDName)
		if pid > 1 && pid != asked && pid != os.Getpid() {
			target := pid
			if g, err := syscall.Getpgid(pid); err == nil && g == pid && pidIn(home, hostBgPIDName) == pid {
				target = -pid
			}
			if err := syscall.Kill(target, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("stop the host run (process %d): %w", pid, err)
			}
			asked = pid
		}
		if time.Now().After(end) {
			return fmt.Errorf("the host run that holds %s did not stop within %s%s: end it, then run this again", filepath.Join(home, hostLockName), hostStopWait, pidWords(home))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// pidIn is the pid in one of home's files (0: none).
func pidIn(home, name string) int {
	b, err := os.ReadFile(filepath.Join(home, name))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// hostBackground is a background host run going on now, or the last one's
// end, with its log's last line (nil when the home has neither). A run in
// the foreground is not one.
func hostBackground(home string) *menubar.Background {
	if home == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, hostLogName))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	bg := &menubar.Background{State: "ended", Last: lines[len(lines)-1]}
	if _, err := os.Stat(filepath.Join(home, hostBgPIDName)); err == nil && hostBusy(home) {
		bg.State = "running"
	}
	return bg
}

// hostBeat is the longest a host run's progress stays quiet.
var hostBeat = 20 * time.Second

// startBeat makes o.Progress say, whenever it has been quiet for hostBeat,
// that the run is still going and what it did last, so a person or a
// program watching never takes a long pull or check for a dead run. stop
// ends it; with no Progress (--json) it does nothing.
func startBeat(o *hostprobe.ShareOptions) (stop func()) {
	say := o.Progress
	if say == nil {
		return func() {}
	}
	var mu sync.Mutex
	start := time.Now()
	last, at := "checking this machine", start
	o.Progress = func(line string) {
		mu.Lock()
		defer mu.Unlock()
		last, at = line, time.Now()
		say(line)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(hostBeat / 4)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				mu.Lock()
				if now.Sub(at) >= hostBeat {
					at = now
					say(fmt.Sprintf("still going (%s so far); last: %s", now.Sub(start).Round(time.Second), last))
				}
				mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done); wg.Wait() }) }
}
