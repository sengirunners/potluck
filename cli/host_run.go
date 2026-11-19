package cli

// One host run: one at a time per home (host.lock), the host program's
// AfterLend once a size is shared, a line now and then while a long step is
// quiet, and --detach, which does the checks and the admin step here and
// the rest in the background, its words in the home's host.log for host
// status.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/menubar"
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
	o, err := a.shareOptions(cmd, s)
	if err != nil {
		return err
	}
	var c *client.Client
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
	}
	err = a.poolHost(ctx, c, shareRunner, arg, o, a.host.Stdout, a.rootStep(ctx, s), a.afterLend(s, o.DryRun))
	if sc := (*spentCodeError)(nil); errors.As(err, &sc) {
		sc.Flags = shareFlagArgs(cmd)
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
	return words{a.host.Stdout}
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
// returns.
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
		return err
	}
	logPath := filepath.Join(home, hostLogName)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	pid, err := hostSpawn(detachArgs(cmd, a.asJSON()), log, lock)
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

// hostSpawn starts the run in the background, in a session of its own, its
// output to log, the held lock its file descriptor 3; tests stand in for
// it.
var hostSpawn = func(args []string, log, lock *os.File) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	c := exec.Command(exe, args...)
	c.Stdout, c.Stderr = log, log
	c.ExtraFiles = []*os.File{lock}
	c.Env = append(os.Environ(), envHostLockFD+"=3")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, c.Process.Release()
}

// detachArgs are the background run's arguments: the command as the
// program spells it (its path below the program's root), its arguments, and
// every flag set on it, without --detach (and --json, whose answer the
// detaching run gave), with --no-sudo (the admin step ran here, or was
// refused).
func detachArgs(cmd *cobra.Command, asJSON bool) []string {
	out := strings.Fields(cmd.CommandPath())[1:]
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "detach" || f.Name == "no-sudo" || asJSON && f.Name == "json" {
			return
		}
		vals := []string{f.Value.String()}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			vals = sv.GetSlice()
		}
		for _, v := range vals {
			out = append(out, "--"+f.Name+"="+v)
		}
	})
	out = append(out, "--no-sudo")
	args := cmd.Flags().Args()
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		return append(append(append(out, args[:dash]...), "--"), args[dash:]...)
	}
	return append(out, args...)
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
