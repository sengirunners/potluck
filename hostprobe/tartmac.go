package hostprobe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
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

// runBounded runs one preflight command under macProbeTimeout.
func runBounded(ctx context.Context, run Runner, name string, args ...string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, macProbeTimeout)
	defer cancel()
	return run.Run(ctx, name, args...)
}

// envFacts is an optional Facts method: the environment, for TART_HOME. A
// Facts without it reads the real environment.
type envFacts interface{ Getenv(string) string }

// tartDir is where Tart keeps its images: $TART_HOME, else ~/.tart.
func tartDir(f Facts) string {
	get := os.Getenv
	if e, ok := f.(envFacts); ok {
		get = e.Getenv
	}
	if d := get("TART_HOME"); d != "" {
		return d
	}
	h := get("HOME")
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	return filepath.Join(h, ".tart")
}

// detectSoftnet checks Softnet, the helper Tart runs a VM's isolated network
// through. It must be owned by root and setuid, or a box on
// the isolated network cannot start without a password. The one admin step is a root step, so
// doctor's root line and `potluck host` offer it.
func detectSoftnet(ctx context.Context, run Runner, f Facts, l *Layer) {
	path, ok := f.LookPath("softnet")
	if !ok {
		l.Problems = append(l.Problems, "Softnet is not installed (Tart's isolated network needs it): download softnet from github.com/cirruslabs/softnet/releases and put it on your PATH, then run `zm host doctor` for the root step that makes it setuid")
		return
	}
	res, err := runBounded(ctx, run, "stat", "-L", "-f", "%Sp %Su", path)
	mode, owner, _ := strings.Cut(strings.TrimSpace(res.Stdout), " ")
	if err != nil || res.Exit != 0 || len(mode) != 10 {
		l.Problems = append(l.Problems, "Softnet is installed at "+path+" but its permissions could not be read")
		return
	}
	if mode[3] == 's' && owner == "root" && mode[5] != 'w' && mode[8] != 'w' {
		return
	}
	l.Problems = append(l.Problems, "Softnet is installed at "+path+" but is not setuid root (or another user can write to it), so Tart cannot start a VM's isolated network without asking for a password each time")
	l.RootSteps = append(l.RootSteps, "chown root "+shQuote(path), "chmod u+s,go-w "+shQuote(path))
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
