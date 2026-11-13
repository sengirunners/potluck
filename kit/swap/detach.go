package swap

// Starting a daemon detached. On macOS a process stays in the resource
// coalition of the app that started it, whatever its session or parent:
// force-quitting that app (the Dock's Force Quit) ends the whole coalition.
// A daemon started from an app's terminal, and every holder it spawns, would
// end with the app (zm-ygk4: 11 agent sessions at once). So on macOS the
// daemon is started through a one-shot launchd job, which gets a coalition
// of its own: the job runs `zm __zm-launch <spec>`, which starts the daemon
// in its own session and exits. The daemon is never the job's main process,
// so no service manager adopts it (SPEC 2, "Swaps"). Where launchd cannot
// take a job (no GUI login, as over ssh) or elsewhere, the daemon is started
// directly, as before.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LaunchArg runs this binary as the launcher a launchd job starts.
const LaunchArg = "__zm-launch"

func init() {
	if len(os.Args) > 1 && os.Args[1] == LaunchArg {
		os.Exit(launchMain(os.Args[2:]))
	}
}

// Detach describes a daemon to start: Argv[0] is the executable.
type Detach struct {
	Argv []string
	Env  []string
	Dir  string // working directory ("" for this process's)
	Log  string // stdout and stderr, appended
	Home string // ZM_HOME: the launch files live here briefly
}

// Detached is a started daemon process.
type Detached struct {
	Pid    int
	Exited <-chan struct{} // closed once the process is gone
	// Via says how it was started: "launchd", or "direct" with the reason.
	Via string
}

// Signal sends sig to the process, never to a pid it no longer has.
func (d *Detached) Signal(sig syscall.Signal) error {
	select {
	case <-d.Exited:
		return os.ErrProcessDone
	default:
		return syscall.Kill(d.Pid, sig)
	}
}

// launchctl is the launchd CLI (tests replace it with "" to start directly).
var launchctl = "/bin/launchctl"

// launchWait bounds the launcher's run: from bootstrap to its pid file.
var launchWait = 10 * time.Second

// StartDetached starts d's daemon in its own session, outside the caller's
// coalition on macOS, and returns once its process runs.
func StartDetached(ctx context.Context, d Detach) (*Detached, error) {
	if runtime.GOOS == "darwin" && launchctl != "" {
		p, err := startLaunchd(ctx, d)
		if err == nil {
			return p, nil
		}
		var refused *launchdRefused
		if !errors.As(err, &refused) {
			return nil, err
		}
		// No GUI domain to take the job: start it as before, and say so
		// where a reader of a lost session will look.
		appendLog(d.Log, fmt.Sprintf("zm: launchd did not take the daemon's start (%v); started it directly, so force-quitting the app that ran this command ends it and its sessions", refused.err))
		p, err = startDirect(d)
		if p != nil {
			p.Via = "direct: " + refused.err.Error()
		}
		return p, err
	}
	return startDirect(d)
}

// launchdRefused: launchd did not run the job at all (nothing started).
type launchdRefused struct{ err error }

func (e *launchdRefused) Error() string { return e.err.Error() }

func startDirect(d Detach) (*Detached, error) {
	logf, err := os.OpenFile(d.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	p := exec.Command(d.Argv[0], d.Argv[1:]...)
	p.Env, p.Dir = d.Env, d.Dir
	p.Stdout, p.Stderr = logf, logf
	p.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := p.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = p.Wait(); close(exited) }()
	return &Detached{Pid: p.Process.Pid, Exited: exited, Via: "direct"}, nil
}

// launchSpec is what the launcher reads (then deletes): the environment
// stays out of the job's definition, which `launchctl print` shows.
type launchSpec struct {
	Argv    []string `json:"argv"`
	Env     []string `json:"env"`
	Dir     string   `json:"dir"`
	Log     string   `json:"log"`
	PidFile string   `json:"pid_file"`
}

func startLaunchd(ctx context.Context, d Detach) (*Detached, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, &launchdRefused{err}
	}
	var nb [6]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(nb[:])
	label := "dev.zm.launch." + nonce
	base := filepath.Join(d.Home, "launch-"+nonce)
	specPath, plistPath, pidPath := base+".json", base+".plist", base+".pid"
	defer func() {
		for _, f := range []string{specPath, plistPath, pidPath, pidPath + ".tmp"} {
			_ = os.Remove(f)
		}
	}()
	dir := d.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	spec, err := json.Marshal(launchSpec{Argv: d.Argv, Env: d.Env, Dir: dir, Log: d.Log, PidFile: pidPath})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(specPath, spec, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(plistPath, []byte(launchPlist(label, []string{self, LaunchArg, specPath}, d.Log)), 0o600); err != nil {
		return nil, err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	// Unloading the job never touches the daemon, which left the job's
	// process group; it ends a launcher still running.
	bootout := func() { _ = exec.Command(launchctl, "bootout", domain+"/"+label).Run() }
	// Not bound to ctx: a bootstrap killed midway may still have loaded the job.
	bctx, cancel := context.WithTimeout(context.Background(), launchWait)
	out, err := exec.CommandContext(bctx, launchctl, "bootstrap", domain, plistPath).CombinedOutput()
	cancel()
	if err != nil {
		bootout()
		if p := readPid(pidPath); p != nil { // it ran after all: never start a second
			return p, nil
		}
		return nil, &launchdRefused{fmt.Errorf("launchctl bootstrap %s: %v: %s", domain, err, strings.TrimSpace(string(out)))}
	}
	defer bootout()
	deadline := time.Now().Add(launchWait)
	for {
		if p := readPid(pidPath); p != nil {
			return p, nil
		}
		var err error
		if time.Now().After(deadline) {
			err = fmt.Errorf("launchd job %s gave no daemon pid within %s; see %s", label, launchWait, d.Log)
		} else if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			// A slow launcher may start the daemon still: end the job, then
			// stop a daemon it started, as a direct start's caller would.
			bootout()
			if p := readPid(pidPath); p != nil {
				_ = p.Signal(syscall.SIGTERM)
			}
			return nil, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// readPid reads the launcher's pid file: nil until it is complete.
func readPid(path string) *Detached {
	b, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(b), "\n") {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return nil
	}
	return &Detached{Pid: pid, Exited: watchGone(pid), Via: "launchd"}
}

// watchGone closes its channel once pid is gone (launchd, its parent once
// the launcher exits, reaps it). A pid this user may not signal is not the
// daemon it started (reused by another user's process): gone too.
func watchGone(pid int) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		for syscall.Kill(pid, 0) == nil {
			time.Sleep(10 * time.Millisecond)
		}
		close(gone)
	}()
	return gone
}

// launchPlist is a one-shot launchd agent running argv. Interactive: no
// background throttling for a daemon whose sessions people type into.
func launchPlist(label string, argv []string, log string) string {
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + esc(label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range argv {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
  <key>LaunchOnlyOnce</key><true/>
  <key>AbandonProcessGroup</key><true/>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>` + esc(log) + `</string>
  <key>StandardErrorPath</key><string>` + esc(log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// launchMain is the launcher: read the spec, delete it, start the daemon in
// its own session, write its pid and exit without waiting for it. It holds
// off TERM, so a bootout waits for it to finish: once bootout returns, the
// pid file is there if a daemon was started. Caught, not ignored: an ignored
// signal would stay ignored in the daemon across exec.
func launchMain(args []string) int {
	held := make(chan os.Signal, 1)
	signal.Notify(held, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(held)
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "zm: usage: zm "+LaunchArg+" <spec>")
		return 2
	}
	b, err := os.ReadFile(args[0])
	_ = os.Remove(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm: launcher:", err)
		return 1
	}
	var s launchSpec
	if err := json.Unmarshal(b, &s); err != nil {
		fmt.Fprintln(os.Stderr, "zm: launcher: bad spec:", err)
		return 1
	}
	if len(s.Argv) == 0 || s.PidFile == "" {
		fmt.Fprintln(os.Stderr, "zm: launcher: spec has no argv or pid file")
		return 1
	}
	p, err := startDirect(Detach{Argv: s.Argv, Env: s.Env, Dir: s.Dir, Log: s.Log})
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm: launcher:", err)
		return 1
	}
	tmp := s.PidFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(p.Pid)+"\n"), 0o600); err == nil {
		err = os.Rename(tmp, s.PidFile)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm: launcher: pid file:", err)
		_ = p.Signal(syscall.SIGTERM)
		return 1
	}
	return 0
}

func appendLog(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, line)
}
