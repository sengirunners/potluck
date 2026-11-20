package vmhost

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// The lender's job list (docs/menubar.md "Jobs this Mac ran"): every job a
// state directory ran, from its records while it keeps them (keepFor past
// their end) and then from the job archive, one line per job that the
// record leaves in jobs.jsonl when it is forgotten (store.remove). The
// archive never holds what a job was sent or printed: only when, how long,
// its size and how it ended.

// jobsFile is the archive in the state directory; jobsMax bounds it: past
// it, an append first drops the oldest half.
const jobsFile = "jobs.jsonl"

var jobsMax int64 = 8 << 20

// Job is one job (a box or a run) a state directory ran.
type Job struct {
	Handle  string `json:"handle"`
	Binding string `json:"binding"`
	Kind    string `json:"kind"`
	Size    string `json:"size,omitempty"`
	// CPU and MemoryMB are what the binding gave each guest (0: the image's
	// own, not known here).
	CPU      int    `json:"cpu,omitempty"`
	MemoryMB int    `json:"memory_mb,omitempty"`
	OS       string `json:"os,omitempty"`
	// Start is when the driver began making it; End when it ended (zero:
	// it still runs, or is a persistent box stopped with its disk kept).
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"`
	// UpS is how long it was up: to End, else to now; a persistent box
	// counts only its time up, as its spend does.
	UpS float64 `json:"up_s"`
	// State is the protocol's (running, ready, stopped, released,
	// expired, reclaimed, lost, ended) or failed: it never started.
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
}

// Running is whether the job holds its cores now.
func (j Job) Running() bool { return j.End.IsZero() && j.State != stStopped && j.State != stFailed }

// job is r as the list shows it, up to now while it has no End. A record
// that is going (removed) while still starting never started.
func (r record) job(now time.Time, going bool) Job {
	cpu, _ := strconv.Atoi(r.Config["cpu"])
	mem, _ := strconv.Atoi(r.Config["memory"])
	j := Job{Handle: r.Handle, Binding: r.Target, Kind: r.Kind, Size: r.Config["size"], CPU: cpu, MemoryMB: mem,
		OS: r.OS, Start: r.Start.UTC(), End: r.End.UTC(), State: r.wireState(), Why: r.jobWhy(), Exit: r.Exit}
	if j.Kind == "" {
		j.Kind = "run"
	}
	if r.End.IsZero() {
		j.End = time.Time{}
	}
	if r.State == stFailed || going && r.live() {
		j.State = stFailed
		if j.End.IsZero() {
			j.End = now.UTC()
		}
	}
	end := j.End
	if end.IsZero() {
		end = now
	}
	if j.State != stFailed { // a job that never started was up for none of it
		j.UpS = max(float64(r.upMs(end))/1000, 0)
	}
	return j
}

// didNotStart begins the reason a kept box's failed start leaves (fail).
const didNotStart = "the box did not start: "

// jobWhy is r's reason as the job list keeps it: never a start failure's
// own words, which can quote what the job printed or was fetched from (fail
// keeps them in a redacted report, named here); else the driver's reason,
// its first line, redacted and cut short.
func (r record) jobWhy() string {
	w := r.Why
	if r.State == stFailed || strings.HasPrefix(w, didNotStart) {
		out := ""
		if strings.HasPrefix(w, didNotStart) {
			out = "the box did not start"
		}
		if r.NoRoom != nil {
			return join2(out, "no room on this Mac then")
		}
		if i := strings.LastIndex(w, "; report: "); i >= 0 {
			return join2(out, "its report: "+tilde(firstLine(w[i+len("; report: "):], 200)))
		}
		return out
	}
	return redact(firstLine(w, 200))
}

func join2(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// firstLine is s's first line, at most n bytes (cut on a rune).
func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// isJob: a record an acquire took. A warm box no acquire took was nobody's.
func (r record) isJob() bool { return r.Handle != "" && !r.Warm && !r.Start.IsZero() }

// archive appends r's job line, once: a record removed twice (its file
// could not be deleted) is listed once by Jobs. Lock held.
func (s store) archive(r record) {
	if !r.isJob() {
		return
	}
	b, err := json.Marshal(r.job(time.Now(), true))
	if err != nil {
		return
	}
	p := filepath.Join(s.dir, jobsFile)
	if fi, err := os.Stat(p); err == nil && fi.Size()+int64(len(b)) >= jobsMax {
		s.trimJobs(p)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// trimJobs keeps the newer half of the archive. Lock held.
func (s store) trimJobs(p string) {
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	cut := len(b) / 2
	if i := bytes.IndexByte(b[cut:], '\n'); i >= 0 {
		cut += i + 1
	} else {
		cut = len(b)
	}
	if os.WriteFile(p+".tmp", b[cut:], 0o600) == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

// gone: no process has pid (one of another user's still counts as there).
func gone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// lastSeen is the latest a record or its supervisor's log was written,
// never before its start and never after now (now wins if the start is
// ahead of it): as late as a job whose supervisor died is known to have
// run. A file stamped ahead of now (the clock stepped back since) cannot
// end a job in the future.
func (s store) lastSeen(r record) time.Time {
	t := r.Start
	for _, p := range []string{s.file(r.ID), filepath.Join(s.dir, "logs", r.ID+".supervisor.log"), s.beatFile(r.ID)} {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	if now := time.Now(); t.After(now) {
		t = now
	}
	return t.UTC()
}

// Jobs is every job a state directory has run, newest start first: its
// records, then the archive's lines for jobs whose records are gone. A
// state directory that does not exist has run none. A line that does not
// parse is skipped; only a directory or archive that cannot be read is an
// error.
func Jobs(dir string, now time.Time) ([]Job, error) {
	s := store{dir: dir}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Job
	for _, r := range s.all() {
		if r.isJob() && !seen[r.Handle] {
			seen[r.Handle] = true
			s.stamp(&r)
			at := now
			if r.asOf.Before(at) { // its last beat, or the last its files show of it (stamp)
				at = r.asOf
			}
			if r.live() && r.Supervisor > 0 && gone(r.Supervisor) {
				// What the next sweep records (sweep): no daemon may run to
				// call one, and a job nothing supervises is not running.
				// Its time up counts to the last its files show of it.
				if r.Persist { // the sweep stops it, its disk kept (persist.go)
					r.State, r.Why = stStopped, "its supervisor died"
				} else {
					r.State, r.Why, r.End = stLost, "its supervisor died", s.lastSeen(r)
					at = r.End
				}
			}
			out = append(out, r.job(at, false))
		}
	}
	f, err := os.Open(filepath.Join(dir, jobsFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		defer f.Close()
		var arch []Job
		rd := bufio.NewReader(f)
		for {
			line, err := rd.ReadBytes('\n') // no length limit: the archive is at most jobsMax
			var j Job
			if json.Unmarshal(line, &j) == nil && j.Handle != "" {
				arch = append(arch, j)
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
		}
		for i := len(arch) - 1; i >= 0; i-- { // the newest line for a handle wins
			if j := arch[i]; !seen[j.Handle] {
				seen[j.Handle] = true
				out = append(out, j)
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Start.After(out[b].Start) })
	return out, nil
}
