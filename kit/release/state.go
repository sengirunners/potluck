package release

// What a home knows about the channel (SPEC 2 "Release channel"), in files
// beside its databases: update-release.json, written by the updater (what
// it installed, which releases ran stably here, which failed), and
// update.hold, the hold switch. The daemon reads both for its machine
// numbers; other machines' updaters read those numbers to decide whether a
// release has been proven.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Files in $ZM_HOME.
const (
	StateFile = "update-release.json"
	HoldFile  = "update.hold"
)

// Soak is how long a machine runs a release with no daemon restart before
// it vouches for it: a rolled-back release never runs, and a crash restarts
// the clock.
const Soak = 20 * time.Minute

// keep bounds the stable and failed lists.
const keep = 20

// State is what this home last installed from the channel (the rollback
// guard) and what it learned about releases.
type State struct {
	Version string    `json:"version"`
	Commit  string    `json:"commit"`
	Created time.Time `json:"created"`
	// Installed is when this release last started here through the
	// channel: its install, or a rollback to it (zero in a state written
	// before 2026-11-06: the updater then starts counting from its next
	// tick).
	Installed time.Time `json:"installed,omitzero"`
	// Stable are the releases this home ran for Soak with no daemon
	// restart, newest first.
	Stable []string `json:"stable,omitempty"`
	// Failed are the releases whose install failed here, newest first:
	// never retried here, and no other machine takes them.
	Failed []Failure `json:"failed,omitempty"`
}

// Failure is one failed install: the swap's result and why.
type Failure struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
	Result  string    `json:"result"` // rolled_back | rolled_forward | failed
	Reason  string    `json:"reason,omitempty"`
}

// StatePath is home's state file.
func StatePath(home string) string { return filepath.Join(home, StateFile) }

// ReadState reads home's state. installed is false when this home never
// installed from the channel; st may still carry failed installs. An
// unreadable state reads as empty: only for display (LoadState decides).
func ReadState(home string) (st State, installed bool) {
	st, installed, _ = LoadState(home)
	return st, installed
}

// LoadState is ReadState for the updater: a state file that exists but
// cannot be read or parsed (an empty file after a power loss included) is
// an error, never a fresh home, which would forget the failed installs.
func LoadState(home string) (st State, installed bool, err error) {
	b, err := os.ReadFile(StatePath(home))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil {
		return State{}, false, fmt.Errorf("%s is unreadable (%v): fix or remove it to update again", StatePath(home), err)
	}
	return st, !st.Created.IsZero() && st.Version != "", nil
}

// WriteState replaces home's state atomically (0600).
func WriteState(home string, st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeAtomic(StatePath(home), b)
}

// Installing is the state after m is installed now: the lists carry over.
func (st State) Installing(m Manifest, now time.Time) State {
	return State{Version: m.Version, Commit: m.Commit, Created: m.Created, Installed: now.UTC(), Stable: st.Stable, Failed: st.Failed}
}

// SameCommit reports whether a and b name one commit: equal, or one an
// abbreviation (at least 7 hex digits) of the other. A dev build's commit
// ("unknown") names none.
func SameCommit(a, b string) bool {
	if !hexRE.MatchString(a) || !hexRE.MatchString(b) {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// NamesCommit reports whether commit names one (7 to 40 hex digits): a
// development build's ("unknown") does not, and is no release.
func NamesCommit(commit string) bool { return hexRE.MatchString(commit) }

// Running is how a running build stands against the release a home
// installed last (State.Runs).
type Running int

const (
	// RunsDev: the build names no commit (a development build): no release.
	RunsDev Running = iota
	// RunsSource: built from source (go build or go install in a checkout,
	// commit from vcs.revision), not by scripts/release, which stamps a
	// manifest's version and full commit: no release, whatever its commit,
	// and a restart runs it again.
	RunsSource
	// RunsUninstalled: a release build, and the home installed none through
	// the channel.
	RunsUninstalled
	// RunsInstalled: the release the home installed last.
	RunsInstalled
	// RunsOther: a release build, not the one the home installed last (an
	// older one still running): a restart runs the installed one.
	RunsOther
)

// Runs is how the build version, commit (api.Version, api.Commit) stands
// against st, what the home installed (installed: LoadState's). The one
// comparison of a running build with the installed release: the daemon's
// machine numbers and the CI doctor both read it.
func (st State) Runs(installed bool, version, commit string) Running {
	switch {
	case !NamesCommit(commit):
		return RunsDev
	case !commitRE.MatchString(commit) || !versionRE.MatchString(version):
		return RunsSource
	case !installed:
		return RunsUninstalled
	case st.Version == version && SameCommit(st.Commit, commit):
		return RunsInstalled
	default:
		return RunsOther
	}
}

// RunBy reports whether the build version, commit is m's release (State.Runs).
func (m Manifest) RunBy(version, commit string) bool {
	return State{Version: m.Version, Commit: m.Commit}.Runs(true, version, commit) == RunsInstalled
}

// StableNow reports whether the daemon (build version, commit, started at
// started) runs st's release (State.Runs) and has run it for Soak with no restart since
// it was installed.
func (st State) StableNow(version, commit string, started, now time.Time) bool {
	return st.Runs(true, version, commit) == RunsInstalled && !st.Installed.IsZero() && !started.IsZero() &&
		!started.After(st.Installed) && now.Sub(st.Installed) >= Soak
}

// MarkStable puts version first in the stable list; false when it is
// there already.
func (st *State) MarkStable(version string) bool {
	if slices.Contains(st.Stable, version) {
		return false
	}
	st.Stable = append([]string{version}, st.Stable...)
	if len(st.Stable) > keep {
		st.Stable = st.Stable[:keep]
	}
	return true
}

// MarkFailed puts f first in the failed list (replacing an older entry for
// its version). A failure with no version names no release: it is
// dropped.
func (st *State) MarkFailed(f Failure) {
	if f.Version == "" {
		return
	}
	st.Failed = slices.DeleteFunc(st.Failed, func(x Failure) bool { return x.Version == f.Version })
	st.Failed = append([]Failure{f}, st.Failed...)
	if len(st.Failed) > keep {
		st.Failed = st.Failed[:keep]
	}
}

// Forgive drops version from the failed list (a later verified upgrade to
// it, zm-4b8g.1); false when it was not there.
func (st *State) Forgive(version string) bool {
	n := len(st.Failed)
	st.Failed = slices.DeleteFunc(st.Failed, func(x Failure) bool { return x.Version == version })
	return len(st.Failed) != n
}

// FailedHere is version's failure on this home, if it failed.
func (st State) FailedHere(version string) (Failure, bool) {
	for _, f := range st.Failed {
		if f.Version == version {
			return f, true
		}
	}
	return Failure{}, false
}

// FailedVersions are the failed list's versions, newest first.
func (st State) FailedVersions() []string {
	var out []string
	for _, f := range st.Failed {
		out = append(out, f.Version)
	}
	return out
}

// Hold is the hold switch's file: while it exists the updater installs
// nothing. A file made with touch has no reason; Since is then its mtime.
type Hold struct {
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since"`
}

// HoldPath is home's hold switch.
func HoldPath(home string) string { return filepath.Join(home, HoldFile) }

// ReadHold reads the hold file at path; held is false when there is none.
func ReadHold(path string) (h Hold, held bool, err error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Hold{}, false, nil
	}
	if err != nil {
		return Hold{}, false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Hold{}, true, err
	}
	if json.Unmarshal(b, &h) != nil {
		h = Hold{Reason: strings.TrimSpace(string(b))}
	}
	if len(h.Reason) > 500 {
		h.Reason = strings.ToValidUTF8(h.Reason[:500], "")
	}
	if h.Since.IsZero() {
		h.Since = fi.ModTime().UTC()
	}
	return h, true, nil
}

// WriteHold writes the hold file at path (0600).
func WriteHold(path string, h Hold) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

// writeAtomic replaces path durably: the data and the rename both reach
// the disk before it returns, so a power loss leaves the old file or the
// new one, never an empty one.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, e := os.Open(filepath.Dir(path)); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
