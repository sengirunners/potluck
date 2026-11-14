// Package localdriver is the local driver, potluck-driver-local (built
// under its old name zm-run-local too, from cmd/zm-run-local): a SPEC 20 run target (kind "run") that runs each
// job on the machine whose daemon calls it, as the daemon's user: the
// automations home's credentialed jobs (bead reminders, sweeps, digests) that
// need that user's files and tokens. No zm task and no session: zm-run-farm
// makes a run a task of a live session on another machine, so it cannot serve
// the home itself, nor a caller (a headless Z3) that is not a session.
//
// Per run: a bare mirror of config remote (one per remote, under state_dir)
// fetches ref, refuses a commit that ref's history on that remote does not
// hold, makes a clean detached checkout of exactly the commit, and a detached
// supervisor runs the script there with SPEC 20's environment allowlist and
// stdin closed, ends it at the deadline (exit 124), and removes the checkout.
// The class is isolation "none", credentials true: zm sends it only the
// remote head of the binding's branch, acquires it only from the owner's
// socket, and runs only the binding's scripts.
//
// State is one record per handle plus the run's output, under
// state_dir/bindings/<target>; a record is kept 7 days after its run ends.
package localdriver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

const (
	name          = "local"
	version       = "0.1.0"
	defaultTTL    = 1800
	defaultTTLMax = 4 * 3600
	keepEnded     = 7 * 24 * time.Hour
	startWait     = 20 * time.Second // the supervisor has started the script
	killGrace     = 5 * time.Second  // TERM, then KILL
	overdue       = 30 * time.Second // past expires_at plus the grace, a live record's supervisor has failed
	prepareMax    = 5 * time.Minute  // a record still preparing after this, with nobody preparing it, is lost
	gitBudget     = 90 * time.Second // clone, fetch and checkout together: inside acquire's 120 s
	flagSupervise = "-potluck-driver-local-supervise"
	// flagSuperviseOld is the flag's name before the rename, taken as well.
	flagSuperviseOld = "-zm-run-local-supervise"
)

var (
	verbs         = []string{"describe", "acquire", "status", "release", "list"}
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	targetPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	refPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	handlePattern = regexp.MustCompile(`^run:([^/]+)/(.+)$`)
	handleID      = regexp.MustCompile(`^r[0-9a-f]{20}$`) // never a path
	// envAllowlist is SPEC 20's: all of this process's environment a run gets.
	envAllowlist = []string{"PATH", "HOME", "LANG", "TERM", "TZ"}
)

type refusal struct{ code, msg string }

func (r refusal) Error() string { return r.code + ": " + r.msg }

func refuse(code, format string, a ...any) error { return refusal{code, fmt.Sprintf(format, a...)} }

// Main is the driver's program (cmd/potluck-driver-local, cmd/zm-run-local):
// one envelope on stdin, one answer on stdout; or, behind its flag, a run's
// supervisor. It returns the exit code.
func Main() int {
	if len(os.Args) == 3 && (os.Args[1] == flagSupervise || os.Args[1] == flagSuperviseOld) {
		return supervise(os.Args[2])
	}
	in, _ := io.ReadAll(os.Stdin)
	_ = json.NewEncoder(os.Stdout).Encode(serve(in))
	return 0
}

// serve answers one SPEC 11 envelope.
func serve(in []byte) map[string]any {
	var req struct {
		Verb   string            `json:"verb"`
		ID     string            `json:"id"`
		Args   json.RawMessage   `json:"args"`
		Config map[string]string `json:"config"`
	}
	var res any
	err := json.Unmarshal(in, &req)
	if err != nil {
		err = refuse("bad_request", "invalid request: %v", err)
	} else {
		res, err = dispatch(req.Verb, req.ID, req.Args, req.Config)
	}
	if err != nil {
		var e refusal
		if !errors.As(err, &e) {
			e = refusal{"internal", err.Error()}
		}
		return map[string]any{"ok": false, "error": map[string]string{"code": e.code, "message": e.msg}}
	}
	return map[string]any{"ok": true, "result": res}
}

// config is the binding's driver keys plus what zm adds (target, kind).
type config struct {
	Target, Remote string
	Root           string // state_dir
	Dir            string // this binding's records: Root/bindings/<target>
	TTLMax         int
	DefaultTTL     int
}

func parseConfig(m map[string]string) (config, error) {
	c := config{Target: m["target"], Remote: m["remote"], TTLMax: defaultTTLMax, DefaultTTL: defaultTTL}
	if k := m["kind"]; k != "" && k != "run" {
		return c, refuse("bad_request", "potluck-driver-local serves kind run only, not %q", k)
	}
	if !targetPattern.MatchString(c.Target) {
		return c, refuse("bad_request", "config target (the binding name) is required")
	}
	if c.Remote == "" || strings.HasPrefix(c.Remote, "-") || strings.ContainsAny(c.Remote, " \t\r\n") ||
		strings.HasPrefix(strings.ToLower(c.Remote), "ext::") {
		return c, refuse("bad_request", "config remote (the git URL of the binding's repo) is required")
	}
	for key, dst := range map[string]*int{"ttl_max_s": &c.TTLMax, "default_ttl_s": &c.DefaultTTL} {
		if v := m[key]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 || n > 7*24*3600 {
				return c, refuse("bad_request", "config %s must be seconds between 1 and 604800", key)
			}
			*dst = n
		}
	}
	c.DefaultTTL = min(c.DefaultTTL, c.TTLMax)
	c.Root = m["state_dir"]
	if c.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, refuse("internal", "no HOME for state_dir: %v", err)
		}
		c.Root = protocol.DriverStateDir(home, "local") // ~/.potluck-driver-local, or an existing ~/.zm-run-local
	}
	if !filepath.IsAbs(c.Root) {
		return c, refuse("bad_request", "config state_dir must be an absolute path")
	}
	// One directory per binding: one binding's handles never answer another's.
	c.Dir = filepath.Join(c.Root, "bindings", c.Target)
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return c, refuse("unavailable", "state_dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(c.Root, "mirrors"), 0o700); err != nil {
		return c, refuse("unavailable", "state_dir: %v", err)
	}
	return c, nil
}

// record is what the driver keeps per handle.
type record struct {
	Handle     string    `json:"handle"`
	RequestID  string    `json:"request_id"`
	Ref        string    `json:"ref"`
	Commit     string    `json:"commit"`
	Script     string    `json:"script"`
	Args       []string  `json:"args,omitempty"`
	Checkout   string    `json:"checkout"`
	Log        string    `json:"log"`
	Mirror     string    `json:"mirror"`
	TTL        int       `json:"ttl_s"`
	Acquired   time.Time `json:"acquired"`
	Expires    time.Time `json:"expires_at"`
	Supervisor int       `json:"supervisor,omitempty"` // set once it holds sup.lock
	PID        int       `json:"pid,omitempty"`        // the script's process group
	State      string    `json:"state"`                // starting, running, ended, expired, released, lost
	Exit       *int      `json:"exit,omitempty"`
	Ended      time.Time `json:"ended,omitzero"`
	Why        string    `json:"why,omitempty"`
}

func (r record) live() bool { return r.State == "starting" || r.State == "running" }

type drv struct{ cfg config }

func dispatch(verb, id string, args json.RawMessage, m map[string]string) (any, error) {
	if !slices.Contains(verbs, verb) {
		return nil, refuse("not_implemented", "verb %q not supported", verb)
	}
	cfg, err := parseConfig(m)
	if err != nil {
		return nil, err
	}
	d := &drv{cfg: cfg}
	switch verb {
	case "describe":
		return d.describe(), nil
	case "acquire":
		return d.acquire(args)
	case "status":
		return d.status(id)
	case "release":
		return d.release(id)
	}
	return d.list()
}

func (d *drv) describe() map[string]any {
	out := map[string]any{"potluck": "potluck/1", "zm": "zm/v0", "name": name, "kind": "run", "version": version, "verbs": verbs,
		"available": true, "isolation": "none", "credentials": true,
		"cost": map[string]any{"unit": "run_min", "amount": 0}, "reports_spend": false,
		"ttl_max_s": d.cfg.TTLMax, "capabilities": []string{}}
	if _, err := exec.LookPath("git"); err != nil {
		out["available"], out["why"] = false, "git is not on PATH"
	}
	return out
}

// ---- records ----

func (d *drv) file(handle string) string {
	return filepath.Join(d.cfg.Dir, "h-"+handle[strings.LastIndex(handle, "/")+1:]+".json")
}

func load(p string) (record, error) {
	var r record
	b, err := os.ReadFile(p)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

func save(p string, r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (d *drv) records() []record {
	ps, _ := filepath.Glob(filepath.Join(d.cfg.Dir, "h-*.json"))
	var out []record
	for _, p := range ps {
		if r, err := load(p); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// locked runs f holding the flock on file lock.
func locked(lock string, f func() error) error {
	l, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(l.Fd()), syscall.LOCK_UN) }()
	return f()
}

// lockedBy runs f holding lock, waiting for it no later than by.
func lockedBy(lock string, by time.Time, f func() error) error {
	for {
		if l := hold(lock); l != nil {
			defer l.Close()
			return f()
		}
		if time.Now().After(by) {
			return refuse("unavailable", "the mirror is busy with another run's checkout; try again")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stateLock is the binding's lock: records change only under it.
func stateLock(dir string) string { return filepath.Join(dir, "lock") }

// parse answers the record file behind a handle: SPEC 20's bad_request (not
// this binding's, or no handle at all) and not_found (never issued, or gone).
func (d *drv) parse(handle string) (string, error) {
	m := handlePattern.FindStringSubmatch(handle)
	if m == nil || m[1] != d.cfg.Target {
		return "", refuse("bad_request", "%q is not a handle of run target %s", handle, d.cfg.Target)
	}
	if !handleID.MatchString(m[2]) {
		return "", refuse("not_found", "no handle %s", handle) // this binding never issues such an id
	}
	p := d.file(handle)
	if _, err := os.Stat(p); err != nil {
		return "", refuse("not_found", "no handle %s", handle)
	}
	return p, nil
}

// Liveness is a lock, never a bare PID: a lock frees when its holder dies
// (a crash, a reboot), and a PID may be anyone's by then. In each run's
// directory: prep.lock, held by the acquire making the checkout; sup.lock, by
// the supervisor alone; run.lock, by the supervisor and inherited by the
// script (fd 3), so it is held while any of the script's processes that kept
// it run. A PID is signalled only while its lock shows it is still that run's.
func runDir(r record) string { return filepath.Dir(r.Log) }

// hold takes lock without waiting, and keeps it until the file is closed or
// the process ends; nil if someone else holds it.
func hold(lock string) *os.File {
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil
	}
	return f
}

// holdWithin is hold, retried for up to d: a probe (held) may hold a free
// lock for an instant.
func holdWithin(lock string, d time.Duration) *os.File {
	for end := time.Now().Add(d); ; time.Sleep(5 * time.Millisecond) {
		if f := hold(lock); f != nil || time.Now().After(end) {
			return f
		}
	}
}

// held says whether someone holds lock (a missing file: nobody).
func held(lock string) bool {
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func supAlive(r record) bool  { return held(filepath.Join(runDir(r), "sup.lock")) }
func runAlive(r record) bool  { return held(filepath.Join(runDir(r), "run.lock")) }
func prepAlive(r record) bool { return held(filepath.Join(runDir(r), "prep.lock")) }

// settle ends a live record whose supervisor failed (gone, or alive but past
// the deadline without a verdict) and one stranded preparing. The state lock
// is held. It answers the record as it now stands.
func settle(p string, r record) record {
	if !r.live() {
		return r
	}
	now := time.Now()
	stranded := r.State == "starting" && r.Supervisor == 0 && now.Sub(r.Acquired) > prepareMax && !prepAlive(r) && !supAlive(r)
	gone := r.Supervisor > 0 && !supAlive(r)
	late := r.Supervisor > 0 && !gone && now.After(r.Expires.Add(killGrace+overdue))
	if !stranded && !gone && !late {
		return r
	}
	again, err := load(p) // it may have just written a verdict
	if err != nil || !again.live() {
		if err == nil {
			return again
		}
		return r
	}
	r = again
	if r.PID > 0 && runAlive(r) { // the script (or what is left of it) still runs
		_ = syscall.Kill(-r.PID, syscall.SIGKILL)
	}
	if late && supAlive(r) {
		_ = syscall.Kill(r.Supervisor, syscall.SIGKILL) // hung: it holds sup.lock, so the PID is still it
	}
	r.Ended = now.UTC()
	if late {
		code := 124
		r.State, r.Exit = "expired", &code
	} else {
		r.State = "lost"
		r.Why = or(r.Why, "the supervisor ended without a verdict")
	}
	_ = save(p, r)
	settled = append(settled, r)
	return r
}

// settled are the runs settle ended in this call; their checkouts are
// removed by cleanup once the state lock is released (git can take a while).
var settled []record

func cleanup() {
	by := time.Now().Add(2 * time.Second) // one budget for the call: a verb answers in 30 s
	for _, r := range settled {
		removeCheckout(r, by)
	}
	settled = nil
}

// ---- acquire ----

type acquireArgs struct {
	RequestID string   `json:"request_id"`
	Actor     string   `json:"actor"`
	TTL       int      `json:"ttl_s"`
	Ref       string   `json:"ref"`
	Commit    string   `json:"commit"`
	Script    string   `json:"script"`
	Args      []string `json:"args"`
}

func (d *drv) acquire(raw json.RawMessage) (any, error) {
	var a acquireArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, refuse("bad_request", "invalid args: %v", err)
	}
	switch {
	case a.RequestID == "" || len(a.RequestID) > 200:
		return nil, refuse("bad_request", "request_id is required (at most 200 bytes)")
	case !refPattern.MatchString(a.Ref) || strings.Contains(a.Ref, "..") || strings.HasPrefix(a.Ref, "refs/"):
		return nil, refuse("bad_request", "ref must be a pushed branch or tag name, such as main")
	case !commitPattern.MatchString(a.Commit):
		return nil, refuse("bad_request", "commit must be the full 40-character commit zm resolved")
	}
	if err := checkScript(a.Script); err != nil {
		return nil, err
	}
	if len(a.Args) > 64 {
		return nil, refuse("bad_request", "at most 64 args")
	}
	for _, x := range a.Args {
		if len(x) > 4096 || strings.ContainsRune(x, 0) {
			return nil, refuse("bad_request", "an arg is over 4096 bytes or holds a NUL")
		}
	}
	ttl := a.TTL
	if ttl == 0 {
		ttl = d.cfg.DefaultTTL
	}
	if ttl < 0 || ttl > d.cfg.TTLMax {
		return nil, refuse("bad_request", "ttl_s must be between 1 and the lifetime cap %d", d.cfg.TTLMax)
	}
	// The record first, under the state lock (one request id, one handle);
	// the slow part (clone, fetch, checkout) after, so status calls of other
	// handles never wait on git. A repeat of the request whose first acquire
	// died while it prepared (killed at acquire's time limit) takes it over.
	var r record
	var p string
	var prep *os.File // held while this acquire prepares the run
	defer func() {    // also keeps it referenced: a finalizer must not close it (and drop the lock) early
		if prep != nil {
			prep.Close()
		}
	}()
	prepare := false
	by := time.Now().Add(gitBudget)
	err := locked(stateLock(d.cfg.Dir), func() error {
		d.prune()
		for _, o := range d.records() {
			if o.RequestID != a.RequestID {
				continue
			}
			if o.Commit != a.Commit || o.Script != a.Script || !slices.Equal(o.Args, a.Args) {
				return refuse("conflict", "request_id %s was used for a different run", a.RequestID)
			}
			r, p = o, d.file(o.Handle)
			if r.State == "starting" && r.Supervisor == 0 && !supAlive(r) {
				if prep = hold(filepath.Join(runDir(r), "prep.lock")); prep != nil {
					prepare = true // its preparer died: take it over
				}
			}
			return nil
		}
		id := make([]byte, 10)
		_, _ = rand.Read(id)
		now := time.Now().UTC()
		dir := filepath.Join(d.cfg.Dir, "r"+hex.EncodeToString(id))
		r = record{Handle: "run:" + d.cfg.Target + "/r" + hex.EncodeToString(id), RequestID: a.RequestID,
			Ref: a.Ref, Commit: a.Commit, Script: a.Script, Args: a.Args,
			Checkout: filepath.Join(dir, "checkout"), Log: filepath.Join(dir, "output.log"),
			Mirror: d.mirrorDir(), TTL: ttl, Acquired: now, Expires: now.Add(time.Duration(ttl) * time.Second),
			State: "starting"}
		p, prepare = d.file(r.Handle), true
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if prep = hold(filepath.Join(dir, "prep.lock")); prep == nil {
			return refuse("unavailable", "could not lock the new run")
		}
		return save(p, r)
	})
	if err != nil {
		return nil, err
	}
	if prepare {
		if err := d.start(p, r, by); err != nil {
			return nil, err
		}
	}
	// Answer once the script has started (or already ended).
	for deadline := time.Now().Add(startWait); ; time.Sleep(20 * time.Millisecond) {
		var cur record
		err := locked(stateLock(d.cfg.Dir), func() error {
			var err error
			if cur, err = load(p); err == nil {
				cur = settle(p, cur)
			}
			return err
		})
		cleanup()
		switch {
		case errors.Is(err, fs.ErrNotExist): // its start was refused meanwhile, and the record dropped
			return nil, refuse("unavailable", "the run with this request_id did not start; acquire again")
		case err != nil:
			return nil, err
		case cur.State == "lost" && cur.PID == 0:
			return nil, refuse("unavailable", "the run with this request_id did not start (%s); acquire with a new request_id",
				or(cur.Why, "its supervisor ended first"))
		case cur.State != "starting":
			return answer(cur), nil
		case time.Now().After(deadline) && (prepare || cur.Supervisor > 0):
			return nil, refuse("unavailable", "the run did not start within %s", startWait)
		case time.Now().After(deadline.Add(gitBudget)): // another acquire is still preparing it
			return nil, refuse("unavailable", "the run with this request_id is still being prepared; try again")
		}
	}
}

// start makes a record's checkout and its supervisor. A refusal (the commit,
// the script) or a failed checkout drops the record, so the same request id
// asked again is judged again and answers the same way.
func (d *drv) start(p string, r record, by time.Time) error {
	drop := func(e error) error { // the caller holds prep.lock: nobody else prepares this record
		_ = locked(stateLock(d.cfg.Dir), func() error {
			if cur, err := load(p); err == nil && cur.State == "starting" && cur.Supervisor == 0 {
				_ = os.RemoveAll(filepath.Dir(cur.Log))
				_ = os.Remove(p)
			}
			return nil
		})
		return e
	}
	err := lockedBy(r.Mirror+".lock", by, func() error { return d.checkout(r, by) }) // one git at a time per mirror
	if err != nil {
		return drop(err)
	}
	var pid int
	owned := false
	err = locked(stateLock(d.cfg.Dir), func() error {
		cur, err := load(p)
		if err != nil {
			return err
		}
		if cur.State != "starting" { // released while it checked out
			return refuse("unavailable", "the run was %s before it started", cur.State)
		}
		// The supervisor says it holds its locks on a pipe (nothing probes a
		// lock it is about to take); it records its own PID in its first
		// write, so a record names it even if this acquire dies right now.
		if pid, err = startSupervisor(p, 5*time.Second); err != nil {
			return refuse("unavailable", "the run could not start: %v", err)
		}
		cur.Supervisor = pid
		if err := save(p, cur); err != nil {
			owned = true // the supervisor has the run (it records itself) and cleans up
			return err
		}
		return nil
	})
	if owned {
		return err
	}
	if err != nil {
		removeCheckout(r, time.Now().Add(2*time.Second))
		var e refusal
		if errors.As(err, &e) && strings.Contains(e.msg, "before it started") {
			return err // released: the record stays, as released
		}
		return drop(err)
	}
	return nil
}

func answer(r record) map[string]any {
	return map[string]any{"handle": r.Handle, "expires_at": r.Expires.Format(time.RFC3339), "ttl_s": r.TTL,
		"commit": r.Commit, "log": r.Log}
}

// checkScript: a clean path inside the repository, never a shell string.
func checkScript(s string) error {
	switch {
	case s == "" || len(s) > 200:
		return refuse("bad_request", "script must be 1 to 200 characters")
	case strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n;&|$`<>()'\"\\*?!{}"):
		return refuse("bad_request", "script %q is a shell string, not a path in the repository", s)
	case filepath.IsAbs(s) || filepath.Clean(s) != s || s == "." || s == ".." || strings.HasPrefix(s, "../"):
		return refuse("bad_request", "script %q is not a clean path inside the repository", s)
	}
	return nil
}

func (d *drv) mirrorDir() string {
	h := sha256.Sum256([]byte(d.cfg.Remote))
	return filepath.Join(d.cfg.Root, "mirrors", hex.EncodeToString(h[:8])+".git")
}

// localRemote: a path or file:// remote (a fixture, or a mirror on disk),
// the only kind git's file protocol is allowed for.
func localRemote(remote string) bool {
	return filepath.IsAbs(remote) || strings.HasPrefix(strings.ToLower(remote), "file://")
}

// git runs git with no hooks, no prompts, the allowlist and a time limit
// taken from the deadline.
func git(deadline time.Time, remote, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	pre := []string{"-c", "core.hooksPath=/dev/null", "-c", "maintenance.auto=false"}
	if localRemote(remote) {
		pre = append(pre, "-c", "protocol.file.allow=always")
	}
	cmd := exec.CommandContext(ctx, "git", append(pre, args...)...)
	cmd.Dir = dir
	cmd.Env = append(allowedEnv(), "GIT_TERMINAL_PROMPT=0")
	// TERM first: git removes its own lock files on it; KILL after the delay.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: out of time", args[0])
		}
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, oneLine(strings.TrimSpace(string(out))))
	}
	return strings.TrimSpace(string(out)), nil
}

// checkout makes r's clean checkout of exactly r.Commit from the mirror, after
// fetching ref and refusing a commit that ref's history on the remote lacks.
// The mirror's lock is held.
func (d *drv) checkout(r record, by time.Time) error {
	m := r.Mirror
	if _, err := os.Stat(filepath.Join(m, "HEAD")); err != nil {
		_ = os.RemoveAll(m)
		tmp := m + ".clone"
		_ = os.RemoveAll(tmp)
		if _, err := git(by, d.cfg.Remote, d.cfg.Root, "clone", "--bare", "--quiet", "--", d.cfg.Remote, tmp); err != nil {
			_ = os.RemoveAll(tmp)
			return refuse("unavailable", "%v", err)
		}
		if err := os.Rename(tmp, m); err != nil {
			return err
		}
	}
	_, err := git(by, d.cfg.Remote, m, "fetch", "--no-tags", "--quiet", "origin", "--", r.Ref)
	if err != nil && strings.Contains(err.Error(), ".lock") {
		// A git killed outright (the daemon's time limit kills the whole
		// process group) leaves its lock files; every git here holds the
		// mirror's lock, so any left now are stale.
		clearStaleLocks(m)
		_, err = git(by, d.cfg.Remote, m, "fetch", "--no-tags", "--quiet", "origin", "--", r.Ref)
	}
	if err != nil {
		return refuse("unavailable", "fetch %s: %v", r.Ref, err)
	}
	if _, err := git(by, d.cfg.Remote, m, "merge-base", "--is-ancestor", r.Commit, "FETCH_HEAD"); err != nil {
		return refuse("forbidden", "commit %s is not in %s's history on the remote", r.Commit[:12], r.Ref)
	}
	_, _ = git(by, d.cfg.Remote, m, "worktree", "prune")
	_ = os.RemoveAll(r.Checkout) // this record's own: left by an acquire killed mid checkout
	unlockWorktree(r)
	_, _ = git(by, d.cfg.Remote, m, "worktree", "prune")
	if _, err := git(by, d.cfg.Remote, m, "worktree", "add", "--detach", "--quiet", r.Checkout, r.Commit); err != nil {
		return refuse("unavailable", "check out %s: %v", r.Commit[:12], err)
	}
	if head, err := git(by, d.cfg.Remote, r.Checkout, "rev-parse", "HEAD"); err != nil || head != r.Commit {
		removeCheckoutLocked(r, by)
		return refuse("unavailable", "checkout is not at %s", r.Commit[:12])
	}
	script := filepath.Join(r.Checkout, r.Script)
	real, err := filepath.EvalSymlinks(script)
	root, _ := filepath.EvalSymlinks(r.Checkout)
	var fi os.FileInfo
	if err == nil {
		fi, err = os.Stat(real)
	}
	if err != nil || !strings.HasPrefix(real, root+string(filepath.Separator)) || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
		removeCheckoutLocked(r, by)
		return refuse("bad_request", "%s must be an executable file inside the repository at %s", r.Script, r.Commit[:12])
	}
	return nil
}

// removeCheckout removes r's checkout: the files at once, without a lock,
// then git's worktree record under the mirror's lock, waiting for it at most
// wait (past that, the next checkout's prune drops the record).
func removeCheckout(r record, by time.Time) {
	if r.Checkout == "" {
		return
	}
	_ = os.RemoveAll(r.Checkout)
	_ = lockedBy(r.Mirror+".lock", by, func() error { removeCheckoutLocked(r, by); return nil })
}

// removeCheckoutLocked is removeCheckout with the mirror's lock held.
func removeCheckoutLocked(r record, by time.Time) {
	if r.Checkout == "" {
		return
	}
	_ = os.RemoveAll(r.Checkout)
	unlockWorktree(r)
	if lim := time.Now().Add(5 * time.Second); by.Before(lim) {
		by = lim // prune on a mirror is instant; a little past the caller's budget at most
	}
	_, _ = git(by, "", r.Mirror, "worktree", "prune")
}

// unlockWorktree drops git's "initializing" lock on r's worktree record: a
// worktree add killed outright leaves it, and prune never removes a locked
// record, so an add at the same path would refuse. The mirror's lock is held
// and the path is this record's own.
func unlockWorktree(r record) {
	ds, _ := filepath.Glob(filepath.Join(r.Mirror, "worktrees", "*"))
	for _, d := range ds {
		b, err := os.ReadFile(filepath.Join(d, "gitdir"))
		if err == nil && filepath.Clean(strings.TrimSpace(string(b))) == filepath.Join(r.Checkout, ".git") {
			_ = os.Remove(filepath.Join(d, "locked"))
		}
	}
}

// clearStaleLocks removes git's lock files from a mirror whose lock is held,
// only old ones: a running script's own git may hold a fresh one (a linked
// worktree locks refs in the common directory too), and git holds a lock
// for seconds at most.
func clearStaleLocks(m string) {
	_ = filepath.WalkDir(m, func(path string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() && path == filepath.Join(m, "worktrees") {
			return filepath.SkipDir
		}
		if err != nil || e.IsDir() || !strings.HasSuffix(e.Name(), ".lock") {
			return nil
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 30*time.Second {
			_ = os.Remove(path)
		}
		return nil
	})
}

func allowedEnv() []string {
	var env []string
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// startSupervisor starts the supervisor and waits until it holds its locks
// (a byte on the readiness pipe, fd 3 there).
func startSupervisor(recordPath string, wait time.Duration) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer rd.Close()
	cmd := exec.Command(self, flagSupervise, recordPath)
	cmd.Env = allowedEnv()
	cmd.Dir = filepath.Dir(recordPath)
	cmd.ExtraFiles = []*os.File{wr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives this call and the daemon's process group
	err = cmd.Start()
	wr.Close()
	if err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = rd.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, 1)
	if n, err := rd.Read(b); n != 1 || err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL) // not reaped yet, so the PID is still the child we started
		_, _ = cmd.Process.Wait()
		return 0, fmt.Errorf("its supervisor did not start")
	}
	_ = cmd.Process.Release()
	return pid, nil
}

// prune drops records (and their output) of runs that ended over a week ago.
// The lock is held.
func (d *drv) prune() {
	for _, r := range d.records() {
		if !r.live() && !r.Ended.IsZero() && time.Since(r.Ended) > keepEnded {
			_ = os.RemoveAll(filepath.Dir(r.Log))
			_ = os.Remove(d.file(r.Handle))
		}
	}
}

// ---- status, release, list ----

func shownState(s string) string {
	if s == "starting" {
		return "running"
	}
	return s
}

func (d *drv) status(handle string) (any, error) {
	p, err := d.parse(handle)
	if err != nil {
		return nil, err
	}
	var r record
	err = locked(stateLock(d.cfg.Dir), func() error {
		var err error
		if r, err = load(p); err == nil {
			r = settle(p, r)
		}
		return err
	})
	cleanup()
	if err != nil {
		return nil, err
	}
	out := map[string]any{"handle": r.Handle, "state": shownState(r.State), "commit": r.Commit, "expires_at": r.Expires.Format(time.RFC3339)}
	if r.Exit != nil {
		out["exit"] = *r.Exit
	}
	return out, nil
}

func (d *drv) release(handle string) (any, error) {
	p, err := d.parse(handle)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"released": false}
	var pg int
	err = locked(stateLock(d.cfg.Dir), func() error {
		r, err := load(p)
		if err != nil {
			return err
		}
		r = settle(p, r)
		switch {
		case r.live():
			r.State, r.Ended = "released", time.Now().UTC()
			if r.PID > 0 && runAlive(r) {
				pg = r.PID
			}
			out["released"] = true
			if err := save(p, r); err != nil {
				return err
			}
		case r.State == "ended" && r.Exit != nil:
			out["exit"] = *r.Exit
		}
		return nil
	})
	cleanup()
	if err != nil {
		return nil, err
	}
	if pg > 0 { // the supervisor sees "released", reaps and cleans up
		terminate(pg)
	}
	return out, nil
}

func (d *drv) list() (any, error) {
	es := []map[string]any{}
	err := locked(stateLock(d.cfg.Dir), func() error {
		d.prune()
		for _, r := range d.records() {
			r = settle(d.file(r.Handle), r)
			es = append(es, map[string]any{"handle": r.Handle, "request_id": r.RequestID, "state": shownState(r.State), "expires_at": r.Expires.Format(time.RFC3339)})
		}
		return nil
	})
	cleanup()
	if err != nil {
		return nil, err
	}
	return map[string]any{"handles": es}, nil
}

// terminate sends the process group TERM, then KILL after the grace.
func terminate(pg int) {
	if syscall.Kill(-pg, syscall.SIGTERM) != nil {
		return
	}
	for end := time.Now().Add(killGrace); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(-pg, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(-pg, syscall.SIGKILL)
}

// ---- the supervisor ----

// supervise lives as long as the run: it starts the script, enforces the
// deadline, writes the verdict, and removes the checkout.
func supervise(p string) int {
	lock := stateLock(filepath.Dir(p))
	r, err := load(p)
	if err != nil {
		return 1
	}
	var st syscall.Stat_t
	if syscall.Fstat(3, &st) != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return 2 // started by something other than startSupervisor
	}
	ready := os.NewFile(3, "ready")                                        // the acquire waits on it, holding the state lock
	sup := holdWithin(filepath.Join(runDir(r), "sup.lock"), 2*time.Second) // for this process's life: it is alive
	if sup == nil {
		return 1 // another supervisor has this run
	}
	defer sup.Close()
	run := holdWithin(filepath.Join(runDir(r), "run.lock"), 2*time.Second) // handed to the script; closed here once it runs
	if run == nil {
		return 1
	}
	_, _ = ready.Write([]byte{1})
	ready.Close() // never inherited by the script
	// Name itself in the record (the acquire does too, unless it dies first).
	if err := locked(lock, func() error {
		cur, err := load(p)
		if err != nil {
			return err
		}
		cur.Supervisor = os.Getpid()
		return save(p, cur)
	}); err != nil {
		return 1
	}
	logf, err := os.OpenFile(r.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 1
	}
	defer logf.Close()
	cmd := exec.Command(filepath.Join(r.Checkout, r.Script), r.Args...)
	cmd.Dir = r.Checkout
	cmd.Env = allowedEnv()
	cmd.Stdout, cmd.Stderr = logf, logf // stdin stays /dev/null
	cmd.ExtraFiles = []*os.File{run}    // fd 3: run.lock is held while the script's processes run
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startErr := cmd.Start()
	run.Close() // the script's copy keeps run.lock: it now says only whether the script's processes run
	_ = locked(lock, func() error {
		cur, err := load(p)
		if err != nil {
			return err
		}
		switch {
		case startErr != nil:
			cur.State, cur.Why, cur.Ended = "lost", "the script did not start: "+oneLine(startErr.Error()), time.Now().UTC()
		case cur.State == "starting":
			cur.State, cur.PID = "running", cmd.Process.Pid
		default: // released before it started
			cur.PID = cmd.Process.Pid
		}
		return save(p, cur)
	})
	if startErr != nil {
		removeCheckout(r, time.Now().Add(time.Minute))
		return 1
	}
	done := make(chan int, 1)
	go func() {
		_ = cmd.Wait()
		code := cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		done <- code
	}()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case code := <-done:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // its leftover children
			_ = os.RemoveAll(r.Checkout)                        // before the verdict: an ended run has no checkout
			_ = locked(lock, func() error {
				cur, err := load(p)
				if err != nil || !cur.live() {
					return err
				}
				cur.State, cur.Exit, cur.Ended = "ended", &code, time.Now().UTC()
				return save(p, cur)
			})
			removeCheckout(r, time.Now().Add(time.Minute)) // git's record, after the verdict: a busy mirror never delays it
			return 0
		case <-t.C:
		}
		var kill bool
		_ = locked(lock, func() error {
			cur, err := load(p)
			if err != nil {
				return err
			}
			switch {
			case cur.State == "released":
				kill = true
			case cur.live() && time.Now().After(cur.Expires):
				code := 124
				cur.State, cur.Exit, cur.Ended = "expired", &code, time.Now().UTC()
				kill = true
				return save(p, cur)
			}
			return nil
		})
		if kill {
			terminate(cmd.Process.Pid)
			<-done
			removeCheckout(r, time.Now().Add(time.Minute))
			return 0
		}
	}
}

func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
