package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/softnet"
	"github.com/sengirunners/potluck/vmhost"
)

// the default pin's disk budget is the free disk Tart needs where it stores images: Potluck's
// macOS image is tens of GB when unpacked, and a box clones it.
func tartDefaultFreeGB() int {
	p, _ := DefaultImage("tart")
	return p.MinFreeGB
}

// macProbeTimeout bounds each of the Mac preflight's commands (stat on
// Softnet, pmset): a wedged powerd or a stale mount on PATH must not hang doctor.
var macProbeTimeout = 5 * time.Second

// errNoAnswer is a preflight command that did not answer in time: nothing
// is known of what it reads.
var errNoAnswer = errors.New("no answer in time")

// runBounded runs one preflight command under macProbeTimeout. One that did
// not end in time is errNoAnswer.
func runBounded(ctx context.Context, run Runner, name string, args ...string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, macProbeTimeout)
	defer cancel()
	res, err := run.Run(ctx, name, args...)
	if ctx.Err() != nil && (err != nil || res.Exit != 0) {
		return res, fmt.Errorf("%w from %s", errNoAnswer, name)
	}
	return res, err
}

// envFacts is an optional Facts method: the environment, for TART_HOME. A
// Facts without it reads the real environment.
type envFacts interface{ Getenv(string) string }

// executableFacts is an optional Facts method: whether a path is a file this
// user can run. A Facts without it (a test's) takes anything that exists.
type executableFacts interface{ Executable(path string) bool }

func executableOn(f Facts) func(string) bool {
	if e, ok := f.(executableFacts); ok {
		return e.Executable
	}
	return f.Exists
}

// hostHome is the user's home as Facts sees it.
func hostHome(f Facts) string {
	get := os.Getenv
	if e, ok := f.(envFacts); ok {
		get = e.Getenv
	}
	h := get("HOME")
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	return h
}

// tartDir is where Tart keeps its images: $TART_HOME, else ~/.tart.
func tartDir(f Facts) string {
	get := os.Getenv
	if e, ok := f.(envFacts); ok {
		get = e.Getenv
	}
	if d := get("TART_HOME"); d != "" {
		return d
	}
	return filepath.Join(hostHome(f), ".tart")
}

// managedSoftnetFacts is an optional Facts method: the managed Softnet's
// path (softnet.Store.Current) and the store's state, when one is
// installed. A Facts without it has none, so a test never reads this
// Mac's install.
type managedSoftnetFacts interface {
	ManagedSoftnet() (string, softnet.Status, bool)
}

// ManagedSoftnet is the Mac's managed Softnet, when installed.
func (SystemFacts) ManagedSoftnet() (string, softnet.Status, bool) {
	s := softnet.System()
	if _, err := os.Stat(s.Current()); err != nil {
		return "", softnet.Status{}, false
	}
	return s.Current(), s.Status(), true
}

// SoftnetAdvice is what this Mac (macOS version, hardware architecture)
// should do to get a Softnet that enforces Potluck's rules (softnet.For).
func SoftnetAdvice(macOS, arch string) string {
	major := macOSMajor(macOS)
	return softnet.For(major, arch).Text(major, softnet.Command)
}

// CheckSoftnetRules runs the Softnet at path on the exact rules a box
// boots with. Upstream parses all rules before this flag's early return,
// before sudo, bootpd or vmnet: no VM or network is created.
// One that did not answer in time is errNoAnswer.
func CheckSoftnetRules(ctx context.Context, run Runner, path string) error {
	block, err := vmhost.SoftnetBlockRules()
	if err != nil {
		return err
	}
	res, err := runBounded(ctx, run, path, "--vm-fd", "0", "--vm-mac-address", "02:00:00:00:00:01",
		"--allow="+vmhost.SoftnetAllowRules(), "--block="+block, "--sudo-escalation-probing")
	switch {
	case err != nil:
		return err
	case res.Exit != 0:
		return errors.New(commandFailure(res, nil))
	}
	return nil
}

// CheckSoftnet is the Mac preflight's Softnet part alone, for r's macOS
// version and architecture: the Softnet Potluck pins for Tart, its version
// and build, and its problems and warnings.
func CheckSoftnet(ctx context.Context, run Runner, f Facts, r Report) Layer {
	l := Layer{Name: "tart"}
	detectSoftnet(ctx, run, f, r, &l)
	return l
}

// detectSoftnet checks Softnet, the helper Tart runs a VM's isolated network
// through: the managed install when there is one (it is what Potluck pins
// for Tart, whatever else is installed), else the one FindSoftnet finds (PATH, then the
// usual bin directories, which an app's or launchd's PATH lacks). It
// must be owned by root and setuid, or a box on the isolated network cannot
// start without a password. The one admin step is a root step, so
// doctor's root line and `potluck host` offer it.
func detectSoftnet(ctx context.Context, run Runner, f Facts, r Report, l *Layer) {
	advice := SoftnetAdvice(r.MacOS, r.Arch)
	onPath, found := vmhost.TartSearch{LookPath: f.LookPath, Executable: executableOn(f)}.FindSoftnet()
	path := onPath
	managed := false
	var st softnet.Status
	major := macOSMajor(r.MacOS)
	if m, ok := f.(managedSoftnetFacts); ok {
		var p string
		if p, st, ok = m.ManagedSoftnet(); ok && !managedSoftnetFor(st, major, r.Arch) {
			// The matrix gives this Mac no managed build (its build was
			// withdrawn), or the copy left behind is not for a macOS upstream
			// covers (a Mac upgraded from macOS 15, which may run an upstream
			// release set up by hand): the managed copy is not used. A setuid
			// file that cannot be used is a problem until it is removed.
			uninstall := fmt.Sprintf("Potluck's boxes do not use it: remove it with `%s uninstall`", softnet.Command)
			if st.Current != nil && st.Current.Problem != "" {
				l.Problems = append(l.Problems, fmt.Sprintf("the managed Softnet %s at %s cannot be used (%s); %s", st.Current.ID, p, st.Current.Problem, uninstall))
			} else {
				l.Warnings = append(l.Warnings, fmt.Sprintf("the managed Softnet at %s is not for this Mac's macOS (%s); %s", p, r.MacOS, uninstall))
			}
		} else if ok {
			if found {
				if abs, err := filepath.Abs(onPath); err == nil && abs != p {
					l.Warnings = append(l.Warnings, "Potluck's boxes use the managed Softnet at "+p+"; the other softnet ("+abs+") is not the one they run")
				}
			}
			path, found, managed = p, true, true
		}
	}
	if !found {
		l.Problems = append(l.Problems, "Softnet is not installed: isolated Tart boxes need a helper that enforces Potluck's protocol and port rules; "+advice)
		return
	}
	path = absoluteExecutable(path, l)
	if path == "" {
		return
	}
	if strings.ContainsRune(path, os.PathListSeparator) {
		l.Problems = append(l.Problems, "Softnet path "+path+" contains the PATH separator and cannot be pinned for Tart; move the helper to a path without it")
		return
	}
	l.SoftnetPath = path
	l.SoftnetVersion = executableVersion(ctx, run, path, "Softnet", l)
	if l.SoftnetVersion == "" {
		if l.Unread != "" {
			return
		}
		l.Problems[len(l.Problems)-1] += "; " + advice
		return
	}
	if managed {
		detectManagedSoftnet(st, r, l)
	}
	if err := CheckSoftnetRules(ctx, run, path); errors.Is(err, errNoAnswer) {
		l.unread(fmt.Sprintf("Softnet at %s could not be asked about Potluck's isolation rules: %v", path, err))
		return
	} else if err != nil {
		l.Problems = append(l.Problems, fmt.Sprintf("Softnet %s at %s cannot parse Potluck's required host DNS-only and SMTP-blocking isolation rules: %v; %s", l.SoftnetVersion, path, err, advice))
		return
	}
	res, err := runBounded(ctx, run, "stat", "-L", "-f", "%Sp %Su", path)
	if errors.Is(err, errNoAnswer) {
		l.unread("Softnet is installed at " + path + " but its permissions could not be read: " + err.Error())
		return
	}
	mode, owner, _ := strings.Cut(strings.TrimSpace(res.Stdout), " ")
	if err != nil || res.Exit != 0 || len(mode) != 10 {
		l.Problems = append(l.Problems, "Softnet is installed at "+path+" but its permissions could not be read")
		return
	}
	if mode[3] == 's' && owner == "root" && mode[5] != 'w' && mode[8] != 'w' {
		return
	}
	l.Problems = append(l.Problems, "Softnet is installed at "+path+" but is not setuid root (or another user can write to it), so Tart cannot start a VM's isolated network without asking for a password each time")
	if managed {
		l.Problems = append(l.Problems, "install it again with `"+softnet.For(macOSMajor(r.MacOS), r.Arch).Install(softnet.Command)+"`")
		return
	}
	if len(l.Problems) == 1 { // permissions alone: root cannot repair other blockers
		l.RootSteps = append(l.RootSteps, "chown root "+shQuote(path), "chmod u+s,go-w "+shQuote(path))
	}
}

// managedSoftnetFor is true when Potluck's boxes on this Mac use its
// managed Softnet: the matrix gives it a managed build, and the copy in use
// is built for this macOS unless the matrix's build is an upstream release
// (then a copy for another macOS is set aside and the softnet on PATH, a
// release set up by hand, is checked instead; on a macOS with no upstream
// release the wrong copy is named as the problem).
func managedSoftnetFor(st softnet.Status, major int, arch string) bool {
	a := softnet.For(major, arch)
	if a.Kind != softnet.Managed {
		return false
	}
	if !a.Build.UpstreamRelease || st.Current == nil {
		return true
	}
	b, ok := softnet.ByID(st.Current.ID)
	return !ok || b.Covers(major, arch)
}

// detectManagedSoftnet reads the managed install: the build in use, and a
// warning when it is unsound or not this potluck's build for this Mac.
func detectManagedSoftnet(st softnet.Status, r Report, l *Layer) {
	if st.Current == nil {
		return
	}
	major := macOSMajor(r.MacOS)
	a := softnet.For(major, r.Arch)
	install := a.Install(softnet.Command)
	if st.Current.Problem != "" {
		l.Problems = append(l.Problems, "the managed Softnet cannot be used: "+st.Current.Problem+"; install it again with `"+install+"`")
		return
	}
	l.SoftnetBuild = st.Current.ID
	if b, ok := softnet.ByID(st.Current.ID); ok && !b.Covers(major, r.Arch) {
		l.Problems = append(l.Problems, fmt.Sprintf("the managed Softnet %s is not built for macOS %d; install this Mac's build with `%s`", st.Current.ID, major, install))
		return
	}
	if a.Kind == softnet.Managed && a.Build.ID != st.Current.ID {
		l.Warnings = append(l.Warnings, fmt.Sprintf("the managed Softnet is %s; this potluck's build for macOS %d is %s: update it with `%s`", st.Current.ID, major, a.Build.ID, install))
	}
}

// detectTartDisk reports the free disk where Tart stores images (not the
// state dir's volume), and warns when it is under the default image's budget.
// Only a warning: doctor does not know which image will be lent, and an image
// already pulled needs no more room. share refuses against the selected
// images' own budgets before it pulls (tartDiskCheck).
func detectTartDisk(f Facts, r *Report, l *Layer) {
	r.TartDir = tartDir(f)
	g, err := f.FreeDiskGB(r.TartDir)
	if err != nil {
		r.TartDiskUnread = err.Error()
		l.Warnings = append(l.Warnings, fmt.Sprintf("could not read the free disk in %s, where Tart stores images: %v", r.TartDir, err))
		return
	}
	r.TartFreeGB = g
	if need := tartDefaultFreeGB(); g < float64(need) {
		l.Warnings = append(l.Warnings, fmt.Sprintf("only %.0f GB free in %s, where Tart stores images; pulling Potluck's default image and running boxes needs about %d GB", g, r.TartDir, need))
	}
}

var (
	pmsetSleep    = regexp.MustCompile(`(?m)^\s*sleep\s+(\d+)\b(.*)$`)
	pmsetDisabled = regexp.MustCompile(`(?m)^\s*SleepDisabled\s+1\b`)
)

// detectSleep warns when this Mac goes to system sleep: a sleeping Mac runs
// no box, and a job started on it stalls. A warning, since an owner may want
// it (potluck reclaims boxes when they return to the keyboard).
func detectSleep(ctx context.Context, run Runner, l *Layer) {
	res, err := runBounded(ctx, run, "pmset", "-g")
	if err != nil || res.Exit != 0 {
		return
	}
	m := pmsetSleep.FindStringSubmatch(res.Stdout)
	// Only the settings keep a Mac awake: SleepDisabled 1 or sleep 0. "(sleep
	// prevented by ...)" is an app holding it awake for now, and when that app
	// stops the Mac sleeps after the setting's minutes.
	if m == nil || pmsetDisabled.MatchString(res.Stdout) {
		return
	}
	if min, _ := strconv.Atoi(m[1]); min > 0 {
		now := ""
		if strings.Contains(m[2], "sleep prevented by") {
			now = " (something is holding it awake right now, but that ends when it stops)"
		}
		l.Warnings = append(l.Warnings, fmt.Sprintf("this Mac goes to system sleep after %d min idle%s, and a sleeping Mac runs no jobs: to keep it awake while it lends, run `sudo pmset -a sleep 0` (or `caffeinate -s` in a terminal), then undo it when you stop lending", min, now))
	}
}
