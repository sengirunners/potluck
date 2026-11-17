package vmhost

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hostUsage is `potluck-driver-tart host`'s usage.
const hostUsage = `usage: potluck-driver-tart host set <open|drain|closed|auto> [--for <duration> | --until <HH:MM or RFC 3339>] [--deadline <duration>]

Sets this host's override for every binding on it: open (run work whatever the
schedule says), drain (no new work; running work finishes, and is cut at the deadline), closed
(no new work; running work ends now) or auto (the sizes' own rules). --for and --until return
the host to auto then. --deadline is a drain's: how long running work has (else each binding's
drain_deadline_s), counted from when the drain began: drain set again keeps that start. The
host's state is read with potluck host status, not here.

       potluck-driver-tart host shelf fill --npm-lock FILE... < request
`

// hostCmd is `potluck-driver-tart host ...`.
func hostCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "shelf" { // shelf_fill.go
		return shelfHostCmd(args[1:], stdin, stdout, stderr)
	}
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help", "-help":
			fmt.Fprint(stdout, hostUsage)
			return 0
		}
	}
	if len(args) == 0 || args[0] != "set" {
		fmt.Fprint(stderr, hostUsage)
		return 2
	}
	o, err := parseHostSet(args[1:], time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "potluck-driver-tart host set: %v\n\n%s", err, hostUsage)
		return 2
	}
	if o, err = writeOverride(hostOverridePath, o); err != nil {
		fmt.Fprintln(stderr, "potluck-driver-tart host set:", err)
		return 1
	}
	fmt.Fprintln(stdout, o.Summary())
	return 0
}

// Summary says in a line what the host switch now does: its mode in plain
// words, until when if it returns to auto, and a drain's deadline.
func (o HostOverride) Summary() string {
	what := map[string]string{
		modeOpen:   "taking work whatever the schedule says",
		modeDrain:  "no new work; running work finishes",
		modeClosed: "no new work; running work ends now",
		modeAuto:   "following the sizes' own rules (schedule, idle time)",
	}[o.Mode]
	line := "host " + o.Mode
	if what != "" {
		line += ": " + what
	}
	if o.Mode == modeDrain {
		// The cut is counted from when the drain began (a drain set again keeps that start); a
		// drain that returns to auto first is never cut.
		deadline := drainDeadlineDefault
		if o.DeadlineS != nil {
			deadline = *o.DeadlineS
		}
		cut := o.Since.Add(time.Duration(deadline) * time.Second)
		switch {
		case o.DeadlineS != nil && o.Until != nil && !cut.Before(*o.Until):
		case o.DeadlineS != nil:
			line += ", and is cut at " + cut.Local().Format("Mon Jan 2 15:04:05")
		default: // each size's own drain_deadline_s applies, which may be sooner than the default
			verb := "is"
			if o.Until != nil {
				verb = "may be" // it returns to auto first unless a size's deadline is shorter
			}
			line += fmt.Sprintf(", and %s cut at each size's drain deadline (%d minutes after the drain began unless a size sets another)", verb, drainDeadlineDefault/60)
		}
	}
	if o.Until != nil {
		line += "; back to auto at " + o.Until.Local().Format(time.RFC3339)
	}
	return line
}

// HostOverride is the host override as written: the mode, when it was set,
// when it returns to auto (nil: it does not) and a drain's deadline in
// seconds (nil: each binding's drain_deadline_s).
type HostOverride struct {
	Mode      string     `json:"mode"`
	Since     time.Time  `json:"since"`
	Until     *time.Time `json:"until,omitempty"`
	DeadlineS *int       `json:"deadline_s,omitempty"`
}

// SetHost is `host set` for a program that links vmhost: it sets this host's
// override to mode (open, drain, closed or auto) and answers the override as
// written. forD, until and deadline are the flags' values ("" for none):
// --for a duration, --until HH:MM[:SS] or an RFC 3339 time, --deadline a
// drain's duration. A mode set while it is in force keeps its start.
func SetHost(mode, forD, until, deadline string) (HostOverride, error) {
	o, err := hostSetOverride(mode, forD, until, deadline, time.Now())
	if err != nil {
		return HostOverride{}, err
	}
	if o, err = writeOverride(hostOverridePath, o); err != nil {
		return HostOverride{}, err
	}
	return o, nil
}

// parseHostSet reads `host set`'s arguments (the mode, then flags, or flags
// then the mode) into the override to write at now.
func parseHostSet(args []string, now time.Time) (override, error) {
	fs := flag.NewFlagSet("host set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	forD := fs.String("for", "", "")
	until := fs.String("until", "", "")
	deadline := fs.String("deadline", "", "")
	var mode string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		mode, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return override{}, err
	}
	switch {
	case mode == "" && fs.NArg() == 1:
		mode = fs.Arg(0)
	case fs.NArg() > 0:
		return override{}, fmt.Errorf("unexpected %q", fs.Arg(0))
	}
	var empty error
	fs.Visit(func(f *flag.Flag) {
		if f.Value.String() == "" && empty == nil {
			empty = fmt.Errorf("--%s needs a value", f.Name)
		}
	})
	if empty != nil {
		return override{}, empty
	}
	return hostSetOverride(mode, *forD, *until, *deadline, now)
}

// hostSetOverride is the override `host set` writes at now: mode with the
// flags' values ("" for a flag not given). It is the one home of host set's
// rules, for the command and SetHost alike.
func hostSetOverride(mode, forD, until, deadline string, now time.Time) (override, error) {
	o := override{Mode: mode, Since: now.UTC().Truncate(time.Second)}
	switch {
	case mode == "":
		return override{}, errors.New("no mode")
	case forD != "" && until != "":
		return override{}, errors.New("--for or --until, not both")
	case mode == modeAuto && (forD != "" || until != ""):
		return override{}, errors.New("auto is not for a while: it is where --for and --until return to")
	case deadline != "" && mode != modeDrain:
		return override{}, errors.New("--deadline is a drain's")
	}
	if forD != "" {
		d, err := time.ParseDuration(forD)
		if err != nil {
			return override{}, fmt.Errorf("--for %q is not a duration (30m, 2h)", forD)
		}
		if d < time.Second {
			return override{}, errors.New("--for must be at least 1s")
		}
		t := o.Since.Add(d).Truncate(time.Second)
		o.Until = &t
	}
	if until != "" {
		t, err := parseUntil(until, now)
		if err != nil {
			return override{}, err
		}
		o.Until = &t
	}
	if deadline != "" {
		d, err := time.ParseDuration(deadline)
		if err != nil || d < 0 {
			return override{}, fmt.Errorf("--deadline %q is not a duration (30m, 0s)", deadline)
		}
		n := int(d / time.Second)
		o.DeadlineS = &n
	}
	return o, o.check()
}

// parseUntil reads --until: an RFC 3339 time, or a local HH:MM[:SS], the next
// one after now.
func parseUntil(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("--until %s has passed", s)
		}
		return t.UTC(), nil
	}
	m := clockRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("--until %q is not HH:MM[:SS] or an RFC 3339 time", s)
	}
	from, _, _ := parseWindow(s + "-" + s)
	t := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, from, 0, now.Location()) // wall clock (DST days)
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, from, 0, now.Location())
	}
	return t.UTC(), nil
}

// writeOverride replaces the override file atomically (a temp file in its
// directory, then a rename), as its owner: the directory is made 0755 and the
// file 0644, so a run user reads both and writes neither. A mode set while
// the same mode is in force keeps its start, so a drain's deadline does not
// move back; auto set after an override ran out starts when it ran out. What
// is written is read back as the drivers will read it, and returned.
func writeOverride(path string, o override) (override, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return o, fmt.Errorf("%v (make %s yourself, owned by you, mode 0755)", err, dir)
	}
	me := os.Getuid()
	if di, err := os.Lstat(dir); err != nil {
		return o, err
	} else if err := ownersOnly(dir, di, true, me); err != nil {
		return o, err
	}
	if err := os.Chmod(dir, 0o755); err != nil { // MkdirAll's mode passed the umask
		return o, err
	}
	if old, err := readOverride(path, me); err == nil && old.at(o.Since) == o.Mode {
		// Only a start already past: no file has none, and one ahead of a
		// corrected clock would never let the same mode be set again.
		switch start := old.endedAt(o.Since); {
		case old.Mode == o.Mode && !old.Since.IsZero() && !old.Since.After(o.Since):
			o.Since = old.Since
		case old.Mode != o.Mode && !start.IsZero() && !start.After(o.Since):
			o.Since = start
		}
		if err := o.check(); err != nil {
			return o, err
		}
	}
	b, err := json.Marshal(o)
	if err != nil {
		return o, err
	}
	f, err := os.CreateTemp(dir, ".host-override-*")
	if err != nil {
		return o, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return o, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return o, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return o, err
	}
	if err := f.Close(); err != nil {
		return o, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return o, err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	if _, err := readOverride(path, me); err != nil {
		return o, fmt.Errorf("wrote %s, but it does not read back: %v", path, err)
	}
	return o, nil
}
