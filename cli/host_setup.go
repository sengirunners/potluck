package cli

// A host run's stages and the setup checklist. potluck host records each
// stage (hostsetup) and ends with the one thing to do next; potluck host
// status, and the menu bar item through it, read the same record with what
// else is set up here (the menu bar item, the daily update), and the rules
// that turn those facts into the checklist are menubar's (setup.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/menubar"
)

// installScript is Potluck's installer, as a person runs it.
const installScript = "curl -fsSL https://potluck.sh/install.sh | sh -s --"

// menuSeenName is the file in the home where host status notes the menu
// bar item's last ask: the proof that it runs.
const menuSeenName = "menubar-seen.json"

// Test hooks: this machine's OS for the setup checklist, the program that
// started this one, and where the menu bar item may be installed.
var (
	setupGOOS  = runtime.GOOS
	menuParent = func() string {
		exe, err := processExe(os.Getppid())
		if err != nil {
			return ""
		}
		return exe
	}
	menuAppDirs = func() []string {
		var dirs []string
		if d := os.Getenv("POTLUCK_MENUBAR_DIR"); d != "" {
			dirs = append(dirs, d)
		}
		if h, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(h, "Applications"))
		}
		return append(dirs, "/Applications")
	}
	// probeMac reads this Mac's preflight part by part (host status
	// --probe).
	probeMac = func(ctx context.Context) hostprobe.MacParts {
		return hostprobe.CheckMacSetup(ctx, hostprobe.ExecRunner{WaitDelay: time.Second}, hostprobe.SystemFacts{})
	}
)

// EnvBundledRelease names a signed release's directory (manifest.json, its
// signature and the tarballs) that the Mac app carries: when this program
// does not run from the install directory, the setup checklist's first
// step installs it from there.
const EnvBundledRelease = "POTLUCK_BUNDLED_RELEASE"

// installDir is where the installer puts Potluck: $POTLUCK_INSTALL_DIR,
// else ~/.local/bin (install.sh), symlinks resolved ("" when no home).
func installDir() string {
	d := os.Getenv("POTLUCK_INSTALL_DIR")
	if d == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		d = filepath.Join(h, ".local", "bin")
	}
	d, err := filepath.Abs(d)
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	return d
}

// startSetup begins home's setup record for this host run of arg (an invite
// code, which is never written, or a pool), and says preflight began.
// background is a run another one started (host --detach), whose --no-sudo
// is that run's, not the owner's.
func (a *app) startSetup(cmd *cobra.Command, o *hostprobe.ShareOptions, home, arg string, background bool) *hostsetup.Tracker {
	again := hostsetup.Command{Prefix: a.cmd("host"), Path: hostPath(cmd), Pool: arg}
	redact := ""
	if project.LooksLikeJoinCode(arg) {
		again.Pool, redact = "", arg
	}
	// The pool (or invite) is the command's argument, whichever flag gave it.
	skip := []string{"detach", "json", "pool"}
	if background {
		skip = append(skip, "no-sudo")
	}
	again.Flags = givenFlags(cmd, skip...)
	st := hostsetup.Start(home, again, redact)
	o.Stages = st
	if o.Progress != nil {
		o.Progress(st.Begin(hostsetup.Preflight, "checking this machine before any download"))
	}
	return st
}

// hostPath is the host command's path below the program's root (["host"]),
// for cmd or the old spelling below it (host share), which takes no argument:
// the command that runs a setup again is always host.
func hostPath(cmd *cobra.Command) []string {
	c := cmd
	for c.Name() != "host" && c.HasParent() {
		c = c.Parent()
	}
	if c.Name() != "host" {
		c = cmd
	}
	return strings.Fields(c.CommandPath())[1:]
}

// endSetup ends the record with the run's error and says, beside the
// answer, where a blocked run stopped (its error, which follows, says why
// and what to do), or after a run that went through the one thing to do
// next: the checklist's, as host status says it.
func (a *app) endSetup(ctx context.Context, st *hostsetup.Tracker, err error) {
	b := st.End(err)
	if a.asJSON() { // the answer is the JSON; host status has the rest
		return
	}
	w := a.host.Stderr
	if werr := st.Err(); werr != nil {
		fmt.Fprintf(w, "note: the setup record could not be written (%v): %s will not show this run\n", werr, a.cmd("host status"))
	}
	if b != nil {
		fmt.Fprintln(w, hostsetup.Line(b.Name, hostsetup.Blocked, "what happened and what to do next are below"))
		if b.Flag != "" { // the error below says why the flag was refused; this is the command that can work
			r := st.Run()
			fmt.Fprintf(w, "next: %s\n", menubar.RunAgain(&r, a.cmd("host")))
		}
		return
	}
	if next := menubar.Build(a.hostFacts(ctx)).Setup.Next; next != "" {
		fmt.Fprintf(w, "next: %s   (%s shows the whole checklist)\n", next, a.cmd("host status"))
	} else {
		fmt.Fprintf(w, "setup is complete: nothing Potluck can check is left (%s shows the checklist)\n", a.cmd("host status"))
	}
}

// validationWords is the validation stage's result: the sizes that passed
// and those that did not; ok when one passed.
func validationWords(res hostprobe.ShareResult) (string, bool) {
	var passed, failed []string
	for _, s := range res.Sizes {
		if s.Passed {
			passed = append(passed, s.Binding)
		} else {
			failed = append(failed, s.Binding)
		}
	}
	if len(passed) == 0 {
		return "no size passed", false
	}
	words := "passed: " + strings.Join(passed, ", ")
	if len(failed) > 0 {
		words += "; did not pass (below): " + strings.Join(failed, ", ")
	}
	return words, true
}

// capacityWords is the usable-capacity stage's result: what pool can use
// now, and what was not shared.
func capacityWords(res hostprobe.ShareResult, pool string) string {
	var shared, not []string
	for _, s := range res.Sizes {
		switch {
		case s.Bound:
			shared = append(shared, fmt.Sprintf("%s (%d vCPU, %d MB)", s.Binding, s.Size.CPU, s.Size.MemoryMB))
		case !s.Passed:
			not = append(not, s.Binding+" (it did not pass validation)")
		default:
			not = append(not, s.Binding+" (it passed, but sharing it failed)")
		}
	}
	words := "pool " + pool + " can use nothing from this machine"
	if len(shared) > 0 {
		words = "pool " + pool + " can use " + strings.Join(shared, ", ")
	}
	if len(not) > 0 {
		words += "; not shared (below): " + strings.Join(not, ", ")
	}
	return words
}

// setupFacts are the checklist's facts beside the lending ones.
func (a *app) setupFacts() menubar.SetupFacts {
	s := menubar.SetupFacts{Version: api.Version, InstallMenu: installScript + " --menubar",
		Upgrade: a.cmd("upgrade enable"), Doctor: a.cmd("doctor")}
	home := ""
	if cfg, err := a.loadClient(); err == nil {
		home = cfg.Home
	}
	if home != "" {
		r, err := hostsetup.Read(home)
		if err != nil {
			s.RunErr = err.Error()
		}
		s.Run, s.RunLive = r, hostBusy(home)
	}
	s.SoftnetInstall, s.SoftnetUninstall = a.cmd("softnet install"), a.cmd("softnet uninstall")
	if !a.own { // a program that mounts Potluck installs and updates itself its own way
		return s
	}
	if rel, err := filepath.Abs(os.Getenv(EnvBundledRelease)); os.Getenv(EnvBundledRelease) != "" && err == nil {
		if exe, err := executable(); err == nil {
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			s.Exe, s.InstallDir, s.BundledRelease = exe, installDir(), rel
			s.InstallBundled = a.cmd("upgrade") + " --release " + shellQuote(rel) + " --dir " + shellQuote(s.InstallDir)
		}
	}
	if setupGOOS == "darwin" {
		s.Menu = menuApp(home)
	}
	s.Updates = updateFacts()
	return s
}

// menuSeen is the menu bar item's last ask.
type menuSeen struct {
	App string `json:"app"`
	At  string `json:"at"`
}

// noteMenuSeen notes in home that the menu bar item asked for this status
// now, when the program that started this one is the app. It never makes the
// home: the app still asks after an uninstall removed it, and a home with
// only this note in it is none that anything uses.
func noteMenuSeen(home string) {
	app := menuAppOf(menuParent())
	if app == "" || home == "" || !isDir(home) {
		return
	}
	b, _ := json.Marshal(menuSeen{App: app, At: time.Now().UTC().Format(time.RFC3339)})
	tmp := filepath.Join(home, "."+menuSeenName+".new")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(home, menuSeenName))
	}
}

// menuAppOf is the app an executable inside Potluck Menu.app belongs to
// ("" for any other program).
func menuAppOf(exe string) string {
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(exe) != "PotluckMenu" || filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return ""
	}
	return app
}

// menuApp is the menu bar item: where it is installed (the one that asks
// now or last asked, else the first of menuAppDirs), and when it last asked.
func menuApp(home string) *menubar.MenuApp {
	m := &menubar.MenuApp{}
	if b, err := os.ReadFile(filepath.Join(home, menuSeenName)); err == nil {
		var s menuSeen
		if json.Unmarshal(b, &s) == nil && isDir(s.App) {
			m.Path = s.App
			m.Seen, _ = time.Parse(time.RFC3339, s.At)
		}
	}
	if app := menuAppOf(menuParent()); app != "" { // no home may be there to have noted it in
		m.Path, m.Seen = app, time.Now().UTC()
	}
	for _, d := range menuAppDirs() {
		if p := filepath.Join(d, "Potluck Menu.app"); m.Path == "" && isDir(p) {
			m.Path = p
		}
	}
	if m.Path != "" {
		m.Open = "open " + shellQuote(m.Path)
	}
	return m
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// updateFacts is the daily update's state, as potluck upgrade status reads
// it.
func updateFacts() *menubar.Updates {
	s, err := newUpdateSchedule()
	if err == nil {
		err = s.Supported()
	}
	if err != nil {
		return &menubar.Updates{Err: err.Error()}
	}
	u := &menubar.Updates{Enabled: s.Installed(), Foreign: s.Foreign()}
	if u.Foreign != "" {
		u.Foreign = tildePath(u.Foreign)
	}
	if u.Enabled {
		u.Loaded = s.Loaded()
	}
	return u
}

// printSetup is host status's checklist and its one next step.
func (a *app) printSetup(s menubar.Setup) {
	lines := []string{"setup:"}
	for _, c := range s.Checks {
		lines = append(lines, fmt.Sprintf("  %-8s %s", c.State, c.Detail))
	}
	switch {
	case s.Next != "":
		lines = append(lines, "next: "+s.Next)
	case s.NextID != "": // blocked with nothing Potluck runs to fix it
		lines = append(lines, "next: what the "+s.NextID+" line says (Potluck has no command for it)")
	default:
		lines = append(lines, "next: nothing Potluck can check is left to set up")
	}
	a.say("%s", strings.Join(lines, "\n"))
}
