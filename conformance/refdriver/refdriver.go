// Package refdriver is the reference run-target driver the conformance
// suite's own tests use, with a Bug switch for each way a driver can be wrong:
// every check needs a driver that fails it. Tests only:
// a test binary calls Main from TestMain and Install writes a zm-run-ref
// wrapper that re-executes it. State is files in one temp directory. A run is a
// clean clone of config remote at commit; a database is a file; a box is a
// directory that exec runs commands in, with HOME set to it. A detached
// supervisor per resource ends it at the deadline, as a real target must.
package refdriver

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	flagDriver    = "-zm-ref-driver"
	flagSupervise = "-zm-ref-supervise"
	ttlMax        = 3600
	costUnit      = "cpu_s"
	boxSize       = "micro"
	boxImage      = "ref-image"
	boxDigest     = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	execCap       = 1 << 20
)

// Main runs the driver or a supervisor when the test binary was started as
// one, and reports whether it was.
func Main() (code int, handled bool) {
	if len(os.Args) < 4 {
		return 0, false
	}
	switch os.Args[1] {
	case flagDriver:
		return driver(os.Args[2], os.Args[3]), true
	case flagSupervise:
		if len(os.Args) < 5 {
			return 2, true
		}
		return supervise(os.Args[2], os.Args[3], os.Args[4]), true
	case flagListen:
		return listenUntil(os.Args[2], os.Args[3]), true // servestops.go
	case flagHold:
		if len(os.Args) < 6 {
			return 2, true
		}
		return hold(os.Args[2], os.Args[3], os.Args[4], os.Args[5]), true
	}
	return 0, false
}

// Install writes an executable named zm-run-ref in dir, keeping its state in
// state, with bug switched on ("" is the correct driver).
func Install(dir, state, bug string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return "", err
	}
	if err := closeTempDirs(dir); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "zm-run-ref")
	script := fmt.Sprintf("#!/bin/sh\nexec %q %s %q %q\n", self, flagDriver, state, bug)
	return p, os.WriteFile(p, []byte(script), 0o755)
}

// closeTempDirs clears group and other write on dir and on each directory
// between it and the temp directory, which the daemon's pin check refuses to
// trust the driver through. A umask of 002 (a shared box's) makes them
// group-writable; the test, not the machine, sets the modes it needs. A
// directory outside the temp directory is left as it is.
func closeTempDirs(dir string) error {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return err
	}
	d, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	for ; strings.HasPrefix(d, tmp+string(filepath.Separator)); d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil {
			return err
		}
		if m := fi.Mode().Perm(); m&0o022 != 0 {
			if err := os.Chmod(d, m&^0o022); err != nil {
				return err
			}
		}
	}
	return nil
}

// Firewall writes into dir an nc that refuses to connect to any of addrs
// (host:port, the host as the probe passes it) and otherwise runs the real
// nc, and answers dir. Put first on PATH, it stands for a guest firewall: the
// reference guest is this machine, so a test that needs an address that
// opens here but not inside the guest blocks it.
func Firewall(dir string, addrs ...string) (string, error) {
	real, err := exec.LookPath("nc")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\nprev=\nfor a in \"$@\"; do case \"$prev $a\" in")
	for _, a := range addrs {
		h, p, err := net.SplitHostPort(a)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, " %q) exit 1 ;;", h+" "+p)
	}
	fmt.Fprintf(&b, " esac; prev=$a; done\nexec %q \"$@\"\n", real)
	return dir, os.WriteFile(filepath.Join(dir, "nc"), []byte(b.String()), 0o755)
}

// findLANAddr answers this machine's first IPv4 address on an up interface that
// is not loopback or link-local, for a test's forbidden address: the suite
// skips loopback ones, which mean the guest itself inside a VM. False when
// the machine has none.
func findLANAddr() (string, bool) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", false
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		as, _ := i.Addrs()
		for _, a := range as {
			n, ok := a.(*net.IPNet)
			if ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				return n.IP.String(), true
			}
		}
	}
	return "", false
}

// lanAddr is findLANAddr; a test of LANAddrCI replaces it to stand in for a machine with no LAN.
var lanAddr = findLANAddr

// TB is the part of testing.TB that the LAN helpers use.
type TB interface {
	Helper()
	Fatal(args ...any)
	Skip(args ...any)
}

// inCI is whether CI is set to a value other than "" or "false", as GitHub
// Actions sets "true".
func inCI() bool {
	ci := os.Getenv("CI")
	return ci != "" && ci != "false"
}

// FirewallCI is Firewall for a test that cannot run without nc: with no nc on
// PATH it fails the test under CI and skips it elsewhere, since go test
// without -v hides a skip and CI must not silently stop proving a strict pass.
func FirewallCI(t TB, dir string, addrs ...string) string {
	t.Helper()
	bin, err := Firewall(dir, addrs...)
	if err != nil {
		NoNC(t, err.Error())
	}
	return bin
}

// NoNC ends a test that found no nc (here or in the guest; why says where):
// a failure under CI, a skip elsewhere.
func NoNC(t TB, why string) {
	t.Helper()
	if inCI() {
		t.Fatal("no nc; CI must have one, a skip would hide that: " + why)
		return
	}
	t.Skip("no nc here: " + why)
}

// LANAddrCI answers this machine's LAN address. With none it fails the test under CI
// (CI set to a value other than "" or "false", as GitHub Actions sets "true"):
// go test without -v hides a skip, and CI must not silently stop proving a
// strict pass. Elsewhere it answers false and the caller decides what to leave
// unchecked. what says what the test then cannot check.
func LANAddrCI(t TB, what string) (string, bool) {
	t.Helper()
	ip, ok := lanAddr()
	if !ok {
		if inCI() {
			t.Fatal("no non-loopback, non-link-local IPv4 address on an up interface; CI must have one, a skip would hide that: " + what)
		}
	}
	return ip, ok
}

// RequireLANAddr is LANAddrCI for a test that cannot run without the
// address: it skips when there is none and CI is not set.
func RequireLANAddr(t TB, what string) string {
	t.Helper()
	ip, ok := LANAddrCI(t, what)
	if !ok {
		t.Skip("no non-loopback, non-link-local IPv4 address on an up interface: " + what)
	}
	return ip
}

// KillAll ends everything a driver left in state, for a test's cleanup.
func KillAll(state string) error {
	var errs []error
	for _, r := range records(state) {
		if r.PID > 0 {
			_ = syscall.Kill(-r.PID, syscall.SIGKILL)
		}
		if r.Supervisor > 0 {
			if err := stopSupervisor(r.Supervisor); err != nil {
				errs = append(errs, err)
			}
		}
		if r.Kind == "box" {
			endProcs(state, r.Handle)
		}
	}
	return errors.Join(errs...)
}

// Repo is a git fixture: a bare remote with two commits, and a checkout of its tip.
type Repo struct {
	Remote, Worktree string
	Commit           string // the first commit: not the tip, so ignoring it shows
	Tip              string
}

// NewRepo builds a Repo under dir whose commits hold probe as probe.sh.
func NewRepo(dir, probe string) (Repo, error) {
	r := Repo{Remote: filepath.Join(dir, "remote.git"), Worktree: filepath.Join(dir, "work")}
	git := func(cwd string, a ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=ref", "-c", "user.email=ref@example.invalid", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, a...)...)
		cmd.Dir = cwd
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %v: %s", a, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git(dir, "init", "-q", "--bare", r.Remote); err != nil {
		return r, err
	}
	if _, err := git(dir, "init", "-q", r.Worktree); err != nil {
		return r, err
	}
	if err := os.WriteFile(filepath.Join(r.Worktree, "probe.sh"), []byte(probe), 0o755); err != nil {
		return r, err
	}
	var err error
	for _, step := range [][]string{{"add", "."}, {"commit", "-q", "-m", "first"}} {
		if _, err = git(r.Worktree, step...); err != nil {
			return r, err
		}
	}
	if r.Commit, err = git(r.Worktree, "rev-parse", "HEAD"); err != nil {
		return r, err
	}
	if err = os.WriteFile(filepath.Join(r.Worktree, "tip.txt"), []byte("tip\n"), 0o644); err != nil {
		return r, err
	}
	for _, step := range [][]string{{"add", "."}, {"commit", "-q", "-m", "tip"}, {"push", "-q", r.Remote, "HEAD:refs/heads/main"}} {
		if _, err = git(r.Worktree, step...); err != nil {
			return r, err
		}
	}
	r.Tip, err = git(r.Worktree, "rev-parse", "HEAD")
	return r, err
}

// record is one handle's file.
type record struct {
	Handle, Key, Kind, State string
	Commit, URL, File        string
	Name, Checkout, Script   string
	Dir                      string    // a box's directory: its HOME, removed when it ends
	Net                      string    // a box's acquire net, echoed with net_off=1 (it isolates nothing)
	Window                   string    // the reclaim window at acquire: the supervisor ends the resource when it closes
	HostTmp                  string    // config host_tmp, where volume-cow-left-in-host-tmp leaves cow views (volumes.go)
	Persist                  bool      // a box acquired with persist: stop keeps Dir (persist.go)
	Warm                     bool      // a box taken from the warm pool (warm.go)
	Seg                      time.Time // when the box last started (acquire or start)
	Ran                      float64   // a persistent box's seconds up before Seg
	Keep                     int       // seconds a stopped box's disk is kept
	Args                     []string
	Volumes                  []rvol   // a box's named volumes (volumes.go)
	Seal                     bool     // the box's release said seal: its build volumes are sealed (volumes.go)
	KeepPaths                []string // that release's keep: all its build volumes keep (keepOnly)
	PID, Supervisor, Polled  int
	Releases                 int
	Exit                     *int
	Start, Deadline, End     time.Time
	PublishFile              string          // a run's publish file, relative to Checkout (publish.go)
	Publish                  json.RawMessage `json:",omitempty"` // what it held when the run ended
}

type request struct {
	Verb   string            `json:"verb"`
	ID     string            `json:"id"`
	Args   json.RawMessage   `json:"args"`
	Config map[string]string `json:"config"`
}

type acquireArgs struct {
	Database  *protocol.Database `json:"database"`
	RequestID string             `json:"request_id"`
	TTL       int                `json:"ttl_s"`
	Name      string             `json:"name"`
	Image     string             `json:"image"`
	Commit    string             `json:"commit"`
	Script    string             `json:"script"`
	Args      []string           `json:"args"`
	Net       string             `json:"net"`
	Persist   bool               `json:"persist"`
	// Volumes are volume_owner's, else actor's (volumes.go).
	Volumes     []rvol `json:"volumes"`
	VolumeOwner string `json:"volume_owner"`
	Actor       string `json:"actor"`
	JITConfig   string `json:"jit_config"` // taken and ignored, but for bug progress-quotes-jit
}

type refusal struct {
	code, msg string
	data      any // the answer's data, when there is any
}

func (e refusal) Error() string { return e.code + ": " + e.msg }

func refuse(code, format string, a ...any) error {
	return refusal{code: code, msg: fmt.Sprintf(format, a...)}
}

type drv struct {
	state, bug string
	cfg        map[string]string
	now        func() time.Time // optional clock for deterministic observation/expiry races
}

func (d *drv) is(bugs ...string) bool { return slices.Contains(bugs, d.bug) }

// boxSize is the box's size name: config size (a broker test's pool of
// sizes), else micro.
func (d *drv) boxSize() string { return cmp.Or(d.cfg["size"], boxSize) }

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func driver(state, bug string) int {
	d := &drv{state: state, bug: bug}
	in, _ := io.ReadAll(os.Stdin)
	var req request
	err := json.Unmarshal(in, &req)
	if err != nil && d.is("malformed-crash") {
		return 2
	}
	if d.is("dump-env", "needs-env") { // appended: every call's environment
		if f, ferr := os.OpenFile(filepath.Join(state, "env"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
			fmt.Fprintln(f, strings.Join(os.Environ(), "\n"))
			f.Close()
		}
	}
	if d.is("log-kind") {
		if f, ferr := os.OpenFile(filepath.Join(state, "kinds"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
			fmt.Fprintf(f, "%s kind=%s\n", req.Verb, req.Config["kind"])
			f.Close()
		}
	}
	if d.is("needs-env") && os.Getenv("POTLUCK_REF_EXTRA") == "" && os.Getenv("ZM_REF_EXTRA") == "" {
		err = refuse("unavailable", "POTLUCK_REF_EXTRA is not set")
	}
	var res any
	if err == nil {
		d.cfg = req.Config
		res, err = d.dispatch(req)
	} else if !errors.As(err, &refusal{}) {
		err = refuse("bad_request", "invalid request: %v", err)
	}
	if d.is("stdout-noise") {
		fmt.Println("starting up")
	}
	out := map[string]any{"ok": true, "result": res}
	exit := 0
	if err != nil {
		var e refusal
		if !errors.As(err, &e) {
			e = refusal{code: "internal", msg: err.Error()}
		}
		if d.is("bad-code") {
			e.code = "oops"
		}
		if d.is("exit-1-on-refusal") {
			exit = 1
		}
		wire := map[string]any{"code": e.code, "message": e.msg}
		if e.data != nil {
			wire["data"] = e.data
		}
		out = map[string]any{"ok": false, "error": wire}
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
	return exit
}

var handleRe = regexp.MustCompile(`^(db|run|box):([^/]+)/([^/]+)$`)

// parse answers the record behind id: SPEC 20's bad_request and not_found.
func (d *drv) parse(id string) (string, error) {
	m := handleRe.FindStringSubmatch(id)
	if d.is("foreign-not-found") && m != nil && (m[2] != d.cfg["target"] || m[1] != d.cfg["kind"]) {
		return "", refuse("not_found", "no %s", id)
	}
	if d.is("malformed-not-found") && m == nil {
		return "", refuse("not_found", "no %s", id)
	}
	if m == nil || m[2] != d.cfg["target"] || m[1] != d.cfg["kind"] {
		return "", refuse("bad_request", "%s is not a handle of this binding", id)
	}
	if d.is("strict-names") && !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(m[3]) {
		return "", refuse("bad_request", "%q is not a database name", m[3]) // fair: a db name is the caller's, the target may limit it
	}
	p := d.file(id)
	if r, err := load(p); err == nil && r.State == "warm" {
		return "", refuse("not_found", "no %s", id)
	}
	if _, err := os.Stat(p); err != nil {
		if d.is("unknown-bad-request") {
			return "", refuse("bad_request", "bad %s", id)
		}
		return "", refuse("not_found", "no %s", id)
	}
	return p, nil
}

func (d *drv) file(id string) string { return recordFile(d.state, id) }

func recordFile(state, id string) string {
	return filepath.Join(state, "h-"+strings.NewReplacer("/", "_", ":", "_").Replace(id)+".json")
}

func load(p string) (record, error) {
	var r record
	b, err := os.ReadFile(p)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

func save(state string, r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// A temp file of its own per write: the driver and a box's supervisor
	// (another process) save the same record, and with one shared temp name
	// one writer's rename published the file while the other was still
	// writing into it, so a reader saw half a record (zm-k5bv).
	p := recordFile(state, r.Handle)
	f, err := os.CreateTemp(state, filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func records(state string) []record {
	ps, _ := filepath.Glob(filepath.Join(state, "h-*.json"))
	var out []record
	for _, p := range ps {
		if r, err := load(p); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// locked runs f with the state directory's lock held.
func locked(state string, f func() error) error {
	l, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := syscall.Flock(int(l.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(l.Fd()), syscall.LOCK_UN)
	return f()
}

func (d *drv) dispatch(req request) (any, error) {
	switch req.Verb {
	case "describe":
		return d.describe(), nil
	case "acquire":
		return d.acquire(req)
	case "status":
		return d.status(req)
	case "release":
		return d.release(req)
	case "list":
		return d.list()
	case "renew":
		if d.is("no-renew") {
			return nil, refuse("not_implemented", "no renew")
		}
		return d.renew(req)
	case "exec":
		if d.cfg["kind"] == "box" {
			return d.exec(req)
		}
	case "spawn", "output", "signal":
		if d.cfg["kind"] == "box" && !d.is("box-no-spawn") {
			return map[string]func(request) (any, error){"spawn": d.spawn, "output": d.output, "signal": d.signal}[req.Verb](req)
		}
	case protocol.VerbPort:
		if d.cfg["kind"] == "box" {
			return d.port(req)
		}
	case "input":
		return d.input(req) // pty.go
	case "stop", "start":
		if d.persists() {
			return map[string]func(request) (any, error){"stop": d.stop, "start": d.startBox}[req.Verb](req) // persist.go
		}
	case protocol.VerbSecrets:
		if d.boxSecretsServed() {
			return d.boxSecrets(req) // secrets.go
		}
	case protocol.VerbProgress, protocol.VerbCancel:
		if d.progressServed() {
			return map[string]func(request) (any, error){protocol.VerbProgress: d.progress, protocol.VerbCancel: d.cancel}[req.Verb](req) // progress.go
		}
	}
	if d.is("unknown-verb-ok") {
		return map[string]any{}, nil
	}
	return nil, refuse("not_implemented", "verb %q not supported", req.Verb)
}

func (d *drv) describe() map[string]any {
	verbs := []string{"describe", "acquire", "status", "release", "list", "renew"}
	if d.is("no-renew", "renew-unlisted") {
		verbs = verbs[:5]
	}
	if d.is("describe-no-list") {
		verbs = []string{"describe", "acquire", "status", "release"}
	}
	out := map[string]any{"potluck": "potluck/1", "zm": "zm/v0", "name": "ref", "kind": d.cfg["kind"], "version": "0.0.1", "verbs": verbs,
		"available": true, "isolation": "ephemeral", "credentials": false,
		"cost": map[string]any{"unit": costUnit, "amount": 0}, "reports_spend": !d.is("no-spend", "spend-undeclared"),
		"ttl_max_s": ttlMax, "capabilities": []string{}}
	switch { // not bugs: a driver with one of the two keys must pass
	case d.is("potluck-names"): // a driver that sends the new key alone
		delete(out, "zm")
	case d.is("zm-names"): // a driver from before the rename
		delete(out, "potluck")
	}
	if out["kind"] == "" {
		out["kind"] = "run"
	}
	if out["kind"] == "box" {
		if !d.is("box-no-exec") {
			out["verbs"] = append(verbs, "exec", protocol.VerbPort)
		}
		switch {
		case d.is("box-no-spawn", "spawn-unlisted", "box-no-exec"):
		case d.is("spawn-only"):
			out["verbs"] = append(out["verbs"].([]string), "spawn")
		default:
			out["verbs"] = append(out["verbs"].([]string), procVerbs...)
		}
		if !d.is("box-no-size") {
			out["size"] = map[string]any{"name": d.boxSize(), "os": cmp.Or(d.cfg["os"], "linux"), "cpu": max(1, atoi(d.cfg["cpu"])), "memory_mb": 512, "disk_gb": 1}
		}
		caps := d.databaseCaps([]string{"github-jit-guard"}) // it starts no runner: jit_config and jit_guard (its run too) are taken and ignored
		if d.cfg["jit"] == "1" {                             // a box the runner broker may place a job on (its tests)
			caps = append(caps, "github-jit")
			if d.cfg["jit_guard_private"] != "0" { // "0": a driver that cannot honour jit_guard.open_if_private
				caps = append(caps, protocol.CapJITGuardPrivate)
			}
		}
		if !d.is("box-no-base64") {
			caps = append(caps, "base64")
		}
		if d.cfg["net_off"] == "1" { // it isolates nothing: net is taken and ignored (the daemon's tests; conformance fails it)
			caps = append(caps, "net-off")
		}
		if d.secretsCapable() {
			caps = append(caps, "secrets")
		}
		if d.boxSecretsListed() { // secrets.go
			caps = append(caps, protocol.CapBoxSecrets)
			caps = append(caps, protocol.CapJITGuardRun) // jit_guard.run is taken and ignored with the rest of the guard
			if d.cfg["jit_guard_reguard"] != "0" {       // "0": a driver whose hook cannot be re-guarded
				caps = append(caps, protocol.CapJITGuardReguard)
			}
			out["verbs"] = append(out["verbs"].([]string), protocol.VerbSecrets)
		}
		out["capabilities"] = d.shelfDescribe(out, d.volumeDescribe(d.rootDescribe(d.persistDescribe(out, d.ptyDescribe(out, caps))))) // persist.go, root.go, volumes.go, shelf.go
		d.warmDescribe(out)                                                                                                            // warm.go
	}
	d.progressDescribe(out) // progress.go
	d.publishDescribe(out)  // publish.go
	switch d.bug {
	case "describe-no-class":
		delete(out, "isolation")
	case "describe-no-credentials":
		delete(out, "credentials")
	case "describe-no-cost-unit":
		out["cost"] = map[string]any{"amount": 0}
	case "describe-no-reports-spend":
		delete(out, "reports_spend")
	case "unavailable":
		out["available"], out["why"] = false, "down"
	case "credentialed", "box-credentialed":
		out["credentials"] = true
	}
	if closed(d.cfg["window"]) && !d.is("reclaim-available") {
		out["available"], out["why"] = false, "reclaimed: outside the window "+d.cfg["window"]
	}
	return out
}

func (d *drv) spend(r record) map[string]any {
	if d.is("no-spend") {
		return nil
	}
	end := r.End
	if end.IsZero() {
		end = time.Now()
		if d.now != nil {
			end = d.now()
		}
	}
	amount := end.Sub(r.Start).Seconds()
	if r.Persist { // only while up (persist.go)
		amount = r.Ran
		if r.State == "ready" {
			amount += end.Sub(r.Seg).Seconds()
		}
	}
	unit := costUnit
	switch {
	case d.is("spend-falls"):
		amount = 1000 - amount - float64(r.Polled)
	case d.is("spend-unit"):
		unit = "usd"
	case d.is("spend-gone") && r.State != "running" && r.State != "ready":
		return nil
	}
	return map[string]any{"unit": unit, "amount": amount}
}

func (d *drv) acquire(req request) (any, error) {
	var a acquireArgs
	if err := json.Unmarshal(req.Args, &a); err != nil {
		return nil, refuse("bad_request", "bad args: %v", err)
	}
	if err := d.databaseAcquire(a.Database); err != nil {
		return nil, err
	}
	if err := d.logAcquire(a.RequestID); err != nil { // acquirelog.go
		return nil, err
	}
	if d.is("acquire-refused") || d.cfg["refuse"] == "1" { // config refuse: describe says available, acquire refuses
		return nil, refuse("unavailable", "no capacity")
	}
	if closed(d.cfg["window"]) && !d.is("reclaim-acquire-ok") {
		return nil, refuse("unavailable", "reclaimed: outside the window %s", d.cfg["window"])
	}
	kind := d.cfg["kind"]
	if a.RequestID == "" && !d.is("no-request-id-ok") {
		return nil, refuse("bad_request", "no request_id")
	}
	if a.RequestID == "" {
		a.RequestID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if a.TTL == 0 {
		a.TTL = 60
	}
	if d.progressServed() { // progress.go
		if err := d.progressBegin(a.RequestID); err != nil {
			return nil, err
		}
	}
	var res map[string]any
	var err error
	if a.TTL > ttlMax && !d.is("ttl-no-cap") {
		err = refuse("bad_request", "ttl_s past %d", ttlMax) // recorded failed, as any refusal after progressBegin
	} else if err = d.slowAcquire(a.RequestID, a.JITConfig); err == nil {
		err = locked(d.state, func() error {
			var err error
			res, err = d.acquireLocked(kind, a)
			return err
		})
		if h, ok := res["handle"].(string); ok && err == nil && kind == "run" {
			err = d.waitRun(h)
		}
	}
	if d.progressServed() {
		res, err = d.progressEnd(a.RequestID, res, err)
	}
	if err != nil {
		return nil, err
	}
	switch d.bug {
	case "url-on-stderr":
		fmt.Fprintln(os.Stderr, "made", res["url"])
	case "handle-shape":
		res["handle"] = "x" + res["handle"].(string)
	case "no-expires-at":
		delete(res, "expires_at")
	case "no-url":
		delete(res, "url")
	}
	return res, nil
}

func (d *drv) acquireLocked(kind string, a acquireArgs) (map[string]any, error) {
	all := records(d.state)
	for _, r := range all {
		if r.Key == a.RequestID && !d.is("new-resource") && !(d.is("new-after-release") && r.State == "released") {
			if kind == "box" && !d.is("volume-key-ignores-volumes") && !sameVolumes(r.Volumes, a) && (len(r.Volumes) > 0 || len(a.Volumes) > 0) {
				return nil, refuse("bad_request", "request_id %q was used with other volumes", a.RequestID)
			}
			return d.answer(r), nil
		}
	}
	n := len(all) + 1
	r := record{Key: a.RequestID, Kind: kind, Window: d.cfg["window"], Net: a.Net, Start: time.Now().UTC(), Deadline: time.Now().UTC().Add(time.Duration(a.TTL) * time.Second)}
	r.Seg = r.Start
	if a.Persist && (kind != "box" || !d.persists()) {
		return nil, refuse("bad_request", "persist is a box's, on a driver listing persist")
	}
	r.Persist, r.Keep = a.Persist, d.keepS()
	if kind == "db" {
		if a.Name == "" && d.is("no-name-ok") {
			a.Name = "anon"
		}
		if a.Name == "" {
			return nil, refuse("bad_request", "no name")
		}
		if err := d.createIn(); err != nil {
			if d.is("leak") {
				left := a.Name + "_leak"
				return nil, errors.Join(err, d.start(record{Key: a.RequestID + "-leak", Kind: kind, Name: left, Start: r.Start,
					Deadline: r.Start.Add(10 * time.Minute), Handle: "db:" + d.cfg["target"] + "/" + left}))
			}
			return nil, err
		}
		for _, o := range all {
			if o.Name == a.Name && o.State == "ready" {
				if d.is("no-conflict") {
					return d.answer(o), nil
				}
				return nil, refuse("conflict", "%s exists", a.Name)
			}
		}
		r.Name, r.Commit = a.Name, a.Commit
		r.Handle = "db:" + d.cfg["target"] + "/" + a.Name
		if d.is("db-prefix-handle") {
			r.Handle = "db:" + d.cfg["target"] + "/ref_" + a.Name
		}
		if d.is("new-resource", "new-after-release") {
			r.Handle += "_" + strconv.Itoa(n)
		}
	} else if kind == "box" {
		if a.Image != "" && a.Image != boxImage && !d.is("box-any-image") {
			return nil, refuse("bad_request", "image %q is not one of the binding's images", a.Image)
		}
		if err := d.boxRoom(all); err != nil { // warm.go
			return nil, err
		}
		if w, ok := d.takeWarm(a); ok { // warm.go
			return w, nil
		}
		vols, err := d.takeVolumes(a, all) // volumes.go
		if err != nil {
			return nil, err
		}
		r.Volumes = vols
		if d.is("volume-cow-left-in-host-tmp") {
			r.HostTmp = d.cfg["host_tmp"]
		}
		r.Commit = a.Commit
		r.Handle = "box:" + d.cfg["target"] + "/b" + strconv.Itoa(n)
		if d.is("new-resource", "new-after-release") {
			r.Handle += "_" + strconv.Itoa(n)
		}
		res, err := d.create(r)
		if err != nil {
			dropNewBuilds(vols, d.bug) // volumes.go: the box never started
		}
		return res, err
	} else {
		if !regexp.MustCompile(`^[a-z0-9._-]+$`).MatchString(a.Script) && !d.is("shell-string") {
			return nil, refuse("bad_request", "script %q is a name, never a shell string", a.Script)
		}
		r.Commit, r.Script, r.Args = a.Commit, a.Script, a.Args
		var err error
		if r.PublishFile, err = d.publishAcquire(); err != nil { // publish.go
			return nil, err
		}
		r.Handle = "run:" + d.cfg["target"] + "/r" + strconv.Itoa(n)
		if d.is("shell-string") && strings.ContainsAny(a.Script, "; ") {
			return map[string]any{}, exec.Command("sh", "-c", a.Script).Run()
		}
	}
	return d.create(r)
}

// create makes the resource and answers for it.
func (d *drv) create(r record) (map[string]any, error) {
	if err := d.start(r); err != nil {
		return nil, err
	}
	r, err := load(recordFile(d.state, r.Handle))
	if err != nil {
		return nil, err
	}
	return d.answer(r), nil
}

// createIn is SPEC 20's check: a server not listed is refused before connecting.
func (d *drv) createIn() error {
	server := d.cfg["server"]
	if server == "" {
		return nil
	}
	if d.is("create-in-connects") {
		if c, err := net.DialTimeout("tcp", strings.SplitN(server, "/", 2)[0], time.Second); err == nil {
			c.Close()
		}
	}
	if slices.Contains(strings.Split(d.cfg["create_in"], ","), server) || d.is("create-in-ok") {
		return nil
	}
	return refuse("forbidden", "%s is not in create_in", server)
}

func (d *drv) answer(r record) map[string]any {
	out := map[string]any{"handle": r.Handle, "expires_at": r.Deadline.Format(time.RFC3339Nano), "ttl_s": int(r.Deadline.Sub(cmp.Or(r.Seg, r.Start)).Seconds() + 0.5)}
	if !d.is("no-commit-acquire") && r.Commit != "" {
		out["commit"] = r.Commit
	}
	if r.URL != "" {
		out["url"] = r.URL
	}
	if r.Kind == "box" {
		out["image_digest"] = boxDigest
		if !d.is("box-no-size-acquire") {
			out["size"] = d.boxSize()
		}
		if len(r.Volumes) > 0 && !d.is("volume-no-answer") {
			out["volumes"] = volAnswers(r)
		}
		if d.cfg["net_off"] == "1" && r.Net != "" {
			out["net"] = r.Net // what it was asked: the daemon's tests; conformance fails its net-off box's reach
		}
		switch v := d.cfg["net_answer"]; v { // a driver that lists net-off and does not keep to it (the daemon's tests)
		case "":
		case "omit":
			delete(out, "net")
		default:
			out["net"] = v
		}
	}
	if s := d.spend(r); s != nil {
		out["spend"] = s
	}
	return out
}

// start makes the resource and its detached supervisor.
func (d *drv) start(r record) error {
	switch r.Kind {
	case "db":
		r.File = filepath.Join(d.state, "db-"+r.Name)
		r.State = "ready"
		r.URL = "postgres://conf:sekrit-" + r.Name + "@127.0.0.1:5432/" + r.Name + "?file=" + r.File
		if err := os.WriteFile(r.File, []byte("db"), 0o600); err != nil {
			return err
		}
		if d.is("db-fake") {
			_ = os.Remove(r.File)
		}
	case "run":
		r.State = "running"
		id := r.Handle[strings.LastIndex(r.Handle, "/")+1:]
		r.Checkout = filepath.Join(d.state, "co-"+id)
		if err := d.checkout(&r); err != nil {
			return err
		}
	case "box":
		r.State = "ready"
		id := r.Handle[strings.LastIndex(r.Handle, "/")+1:]
		r.Dir = filepath.Join(d.state, "box-"+id)
		if d.is("box-reuse") { // every box gets the same directory, never wiped
			r.Dir = filepath.Join(d.state, "box-shared")
		}
		if r.Commit != "" && d.cfg["remote"] == "" {
			return refuse("bad_request", "this binding has no repo: a box cannot take a ref")
		}
		if d.is("volume-cow-left-behind") && len(r.Volumes) == 0 {
			_ = os.Rename(filepath.Join(d.state, "box-recycled"), r.Dir)
		}
		if err := os.MkdirAll(r.Dir, 0o700); err != nil {
			return err
		}
		d.startShelf(r) // shelf.go
		if i := d.checkoutVolOf(r); i >= 0 {
			// The checkout goes into the box's cow view of the volume
			// (volumes.go): mounted first, never emptied (checkoutInto).
			if err := d.mountVolumes(r); err != nil {
				_ = os.RemoveAll(r.Dir)
				return err
			}
			r.Checkout = filepath.Join(r.Dir, r.Volumes[i].Path)
			if err := d.checkoutInto(r); err != nil {
				_ = os.RemoveAll(r.Dir)
				return err
			}
		} else if r.Commit != "" {
			r.Checkout = filepath.Join(r.Dir, "checkout")
			if err := d.checkout(&r); err != nil {
				_ = os.RemoveAll(r.Dir)
				return err
			}
			if d.is("box-dirty-checkout") {
				_ = os.WriteFile(filepath.Join(r.Checkout, "stray.txt"), []byte("left\n"), 0o644)
			}
		}
	}
	if r.Kind == "box" && d.checkoutVolOf(r) < 0 { // a checkout volume is mounted before the checkout
		if err := d.mountVolumes(r); err != nil { // volumes.go
			_ = os.RemoveAll(r.Dir)
			return err
		}
	}
	if err := save(d.state, r); err != nil {
		return err
	}
	return d.superviseRecord(r)
}

// superviseRecord starts a saved record's detached supervisor. The state lock is
// held: the supervisor saves the record too, and the lock keeps the pid saved here.
func (d *drv) superviseRecord(r record) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, flagSupervise, d.state, r.Handle, d.bug)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	r.Supervisor = cmd.Process.Pid
	_ = cmd.Process.Release()
	return save(d.state, r)
}

func (d *drv) checkout(r *record) error {
	git := func(cwd string, a ...string) error {
		cmd := exec.Command("git", a...)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			return refuse("unavailable", "git %v: %v: %s", a, err, out)
		}
		return nil
	}
	switch {
	case d.is("dirty"):
		r.Checkout = d.cfg["worktree"]
		return git(r.Checkout, "checkout", "-q", r.Commit)
	case d.is("run-worktree"): // runs the worktree as it is, commit ignored
		r.Checkout = d.cfg["worktree"]
		return nil
	}
	if err := git(d.state, "clone", "-q", d.cfg["remote"], r.Checkout); err != nil {
		return err
	}
	if d.is("wrong-head") {
		return nil
	}
	return git(r.Checkout, "checkout", "-q", r.Commit)
}

// checkoutVolOf is the index of box r's volume its checkout goes into, or
// -1 (no commit, no checkout volume, or checkout-volume-ignored).
func (d *drv) checkoutVolOf(r record) int {
	if r.Commit == "" || d.is("checkout-volume-ignored") {
		return -1
	}
	return checkoutVol(r.Volumes)
}

// checkoutInto checks r.Commit out in r.Checkout, a directory that already
// holds a volume's files: git writes the commit's files over what is there
// and leaves every other file as it is; a .git there is replaced (with checkout-volume-wipes, it
// empties the directory first; with checkout-volume-trusts-git, it keeps that .git).
func (d *drv) checkoutInto(r record) error {
	if d.is("checkout-volume-wipes") {
		es, _ := os.ReadDir(r.Checkout)
		for _, e := range es {
			_ = os.RemoveAll(filepath.Join(r.Checkout, e.Name()))
		}
	}
	if !d.is("checkout-volume-trusts-git") {
		if err := os.RemoveAll(filepath.Join(r.Checkout, ".git")); err != nil {
			return err
		}
	}
	for _, a := range [][]string{{"init", "-q"}, {"-c", "maintenance.auto=false", "fetch", "-q", d.cfg["remote"], r.Commit}, {"-c", "advice.detachedHead=false", "checkout", "-q", "-f", "--detach", r.Commit}} {
		cmd := exec.Command("git", a...)
		cmd.Dir = r.Checkout
		if out, err := cmd.CombinedOutput(); err != nil {
			return refuse("unavailable", "git %v: %v: %s", a, err, out)
		}
	}
	return nil
}

// waitRun answers once the run has started, or (a bug) has ended.
func (d *drv) waitRun(handle string) error {
	p := d.file(handle)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		r, err := load(p)
		switch {
		case err != nil:
			return err
		case r.State != "running", r.PID != 0 && !d.is("acquire-blocks"):
			return nil
		case d.is("acquire-blocks"):
			deadline = time.Now().Add(10 * time.Second)
		}
	}
	return refuse("unavailable", "run did not start")
}

func (d *drv) status(req request) (any, error) {
	p, err := d.parse(req.ID)
	if err != nil {
		return nil, err
	}
	if d.is("slow-status") {
		time.Sleep(10 * time.Second)
	}
	if d.is("lazy-deadline") {
		_ = locked(d.state, func() error {
			r, err := load(p)
			if err == nil && time.Now().After(r.Deadline) {
				endAtDeadline(d.state, r, d.bug)
			}
			return nil
		})
	}
	if d.is("lazy-reclaim") {
		_ = locked(d.state, func() error {
			r, err := load(p)
			if err == nil && closed(r.Window) {
				reclaimNow(d.state, r, d.bug)
			}
			return nil
		})
	}
	var r record
	var spend map[string]any
	err = locked(d.state, func() error {
		var err error
		if r, err = load(p); err != nil {
			return err
		}
		r.Polled++
		if err := save(d.state, r); err != nil {
			return err
		}
		// Sample running spend under the same lock as the state snapshot. A
		// supervisor must not record an earlier terminal End between the two.
		spend = d.spend(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"handle": r.Handle, "state": r.State, "expires_at": r.Deadline.Format(time.RFC3339Nano)}
	if r.Commit != "" && !d.is("no-commit-status") {
		out["commit"] = r.Commit
	}
	if r.Exit != nil {
		out["exit"] = *r.Exit
	}
	if spend != nil {
		out["spend"] = spend
	}
	if d.is("url-in-status") {
		out["url"] = r.URL
	}
	if d.is("status-released-exit") && r.State == "released" {
		out["exit"] = 0
	}
	if d.is("status-wrong-state") {
		out["state"] = "ended"
	}
	d.publishStatus(r, out) // publish.go
	return out, nil
}

func (d *drv) list() (any, error) {
	switch d.bug {
	case "list-missing":
		return map[string]any{"handles": []any{}}, nil
	case "list-no-array":
		return map[string]any{}, nil
	}
	es := []map[string]any{}
	for _, r := range records(d.state) {
		if r.State == "warm" && !d.is("warm-listed") { // nobody's handle (warm.go)
			continue
		}
		e := map[string]any{"handle": r.Handle, "request_id": r.Key, "state": r.State, "expires_at": r.Deadline.Format(time.RFC3339Nano)}
		if d.is("list-no-key") {
			delete(e, "request_id")
		}
		if d.is("list-stale") {
			e["state"] = "running"
		}
		es = append(es, e)
	}
	return map[string]any{"handles": es}, nil
}

func (d *drv) release(req request) (any, error) {
	if d.is("release-unknown-ok") {
		if _, err := os.Stat(d.file(req.ID)); err != nil && handleRe.MatchString(req.ID) {
			return map[string]any{"released": false}, nil
		}
	}
	p, err := d.parse(req.ID)
	if err != nil {
		return nil, err
	}
	var ra struct {
		Seal bool     `json:"seal"`
		Keep []string `json:"keep"`
	}
	if len(req.Args) > 0 {
		if err := json.Unmarshal(req.Args, &ra); err != nil {
			return nil, refuse("bad_request", "bad args: %v", err)
		}
	}
	if ra.Keep != nil && d.keepOn() && !d.is("seal-keep-bad-args-ok") {
		if !ra.Seal {
			return nil, refuse("bad_request", "keep is for a release with seal")
		}
		if err := protocol.KeepPathsOK(ra.Keep); err != nil {
			return nil, refuse("bad_request", "%v", err)
		}
	}
	seal := ra.Seal && d.sealOn()
	out := map[string]any{}
	err = locked(d.state, func() error {
		r, err := load(p)
		if err != nil {
			return err
		}
		released := false
		r.Releases++
		switch r.State {
		case "stopped": // its kept disk goes (persist.go)
			released = !d.is("release-stopped-false")
			end(r, d.bug)
			r.State, r.End = "released", time.Now().UTC()
		case "running", "ready":
			released = !d.is("release-false-first")
			r.Seal = seal && !(r.Kind == "box" && time.Now().After(r.Deadline)) // volumes.go: past its deadline the box has expired, never sealed
			if r.Seal && d.keepOn() && protocol.KeepPathsOK(ra.Keep) == nil {
				r.KeepPaths = ra.Keep
			}
			if !d.is("release-noop") {
				end(r, d.bug)
			}
			r.State, r.End = "released", time.Now().UTC()
			if r.Persist {
				r.Ran += r.End.Sub(r.Seg).Seconds() // its last time up
			}
			if r.Kind == "box" && (d.is("warm-reused") || d.is("warm-reused-late")) { // its directory back in the pool, as it is
				_ = save(d.state, record{Handle: r.Handle + "w", Kind: "box", State: "warm", Dir: r.Dir, Start: time.Now().UTC()})
			}
			if r.Kind == "box" && len(r.Volumes) > 0 && d.is("warm-volume-reused") {
				d.poolTakes(r) // warm.go
			}
			if d.is("release-exit-running") {
				out["exit"] = 137
			}
		case "ended":
			if !d.is("release-no-exit") && !(d.is("release-ended-once") && r.Releases > 1) {
				out["exit"] = *r.Exit
			}
		default:
			if d.is("release-twice-error") {
				return refuse("conflict", "already released")
			}
			released = d.is("release-twice-true") || d.is("release-expired-true") && r.State == "expired" ||
				d.is("release-reclaimed-true") && r.State == "reclaimed"
		}
		if err := save(d.state, r); err != nil {
			return err
		}
		out["released"] = released
		if s := d.spend(r); s != nil {
			out["spend"] = s
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (d *drv) renew(req request) (any, error) {
	p, err := d.parse(req.ID)
	if err != nil {
		return nil, err
	}
	var a struct {
		TTL int `json:"ttl_s"`
	}
	_ = json.Unmarshal(req.Args, &a)
	if a.TTL <= 0 && !d.is("renew-no-ttl-ok") {
		return nil, refuse("bad_request", "no ttl_s")
	}
	var at time.Time
	err = locked(d.state, func() error {
		r, err := load(p)
		if err != nil {
			return err
		}
		if r.State == "stopped" {
			return refuse("conflict", "%s is stopped: start it", r.Handle)
		}
		at = time.Now().UTC().Add(time.Duration(a.TTL) * time.Second)
		over := at.After(cmp.Or(r.Seg, r.Start).Add(ttlMax * time.Second))
		if d.is("renew-cap-per-call") {
			over = a.TTL > ttlMax
		}
		if over && !d.is("renew-no-cap") {
			return refuse("bad_request", "past the lifetime cap")
		}
		if d.is("renew-same") {
			at = r.Deadline
		}
		if d.is("renew-ignored", "renew-same") {
			return nil
		}
		r.Deadline = at
		return save(d.state, r)
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"expires_at": at.Format(time.RFC3339Nano)}, nil
}

// end ends a resource: the run's process group, or the database's file.
func end(r record, bug string) {
	switch {
	case r.Kind == "db":
		_ = os.Remove(r.File)
	case r.Kind == "box" && bug == "seal-before-end" && r.Seal && hasBuild(r):
		endBuilds(r, bug) // sealed while its processes still run, and they are never ended
		_ = os.RemoveAll(r.Dir)
	case r.Kind == "box":
		killExecs(r)     // persist.go
		endCOW(r, bug)   // volumes.go
		endShelf(r, bug) // shelf.go
		switch {
		case bug == "box-reuse" || bug == "warm-reused" || bug == "warm-reused-late":
		case bug == "box-dir-kept": // the next box gets a directory of its own, this one stays readable
		case bug == "warm-volume-reused" && len(r.Volumes) > 0: // release puts it in the pool (warm.go)
		case bug == "volume-cow-left-behind" && hasCOW(r): // the next plain box gets its directory (volumes.go)
			_ = os.RemoveAll(filepath.Join(filepath.Dir(r.Dir), "box-recycled"))
			_ = os.Rename(r.Dir, filepath.Join(filepath.Dir(r.Dir), "box-recycled"))
		default:
			unlockTree(r.Dir) // a ro volume's copy (volumes.go)
			_ = os.RemoveAll(r.Dir)
		}
		endProcs(filepath.Dir(r.Dir), r.Handle)
		endMem(filepath.Dir(r.Dir), r, bug) // secrets.go
		endBuilds(r, bug)                   // volumes.go: last, once nothing of the box is left
	case r.PID == 0:
	case bug == "kill-leader-only":
		_ = syscall.Kill(r.PID, syscall.SIGKILL)
	default:
		_ = syscall.Kill(-r.PID, syscall.SIGKILL)
	}
}

// endAtDeadline marks a live record expired. The lock is held.
func endAtDeadline(state string, r record, bug string) {
	if r.State != "running" && r.State != "ready" && r.State != "stopped" {
		return
	}
	if bug == "seal-expired-kept" && r.Kind == "box" {
		r.Seal = hasBuild(r) // a builder its deadline ended is sealed, as if released with seal
	}
	end(r, bug)
	r.State, r.End = "expired", time.Now().UTC()
	if r.Kind == "run" {
		code := 124
		if bug == "expired-exit-0" {
			code = 0
		}
		r.Exit = &code
		if bug == "expired-as-ended" {
			r.State = "ended"
		}
	}
	_ = save(state, r)
}

// reclaimNow ends a live resource because its owner took the machine back.
// The lock is held.
func reclaimNow(state string, r record, bug string) {
	if r.State != "running" && r.State != "ready" {
		return
	}
	if r.Persist { // the owner took the machine back: the box stops, its disk kept
		stopNow(state, r, "reclaimed", bug)
		return
	}
	end(r, bug)
	r.State, r.End = "reclaimed", time.Now().UTC()
	switch bug {
	case "reclaim-as-lost":
		r.State = "lost"
	case "reclaim-exit":
		code := 137
		r.Exit = &code
	}
	_ = save(state, r)
}

// closed says whether a reclaim window "HH:MM:SS-HH:MM:SS" (local time, may
// wrap midnight) is closed now; "" never closes.
func closed(window string) bool {
	from, to, ok := strings.Cut(window, "-")
	if !ok {
		return false
	}
	f, err1 := time.Parse("15:04:05", from)
	t, err2 := time.Parse("15:04:05", to)
	if err1 != nil || err2 != nil {
		return false
	}
	now := time.Now()
	sec := func(x time.Time) int { return x.Hour()*3600 + x.Minute()*60 + x.Second() }
	n, a, b := sec(now), sec(f), sec(t)
	if a < b {
		return n < a || n >= b
	}
	return n < a && n >= b
}

// supervise lives as long as the resource and is what enforces its deadline.
func supervise(state, handle, bug string) int {
	p := recordFile(state, handle)
	r, err := load(p)
	if err != nil {
		return 1
	}
	done := make(chan int, 1)
	if r.Kind == "run" {
		cmd := exec.Command("sh", append([]string{filepath.Join(r.Checkout, r.Script)}, r.Args...)...)
		if bug == "fake-run" {
			cmd = exec.Command("sleep", "60")
		}
		cmd.Dir = r.Checkout
		cmd.Env = (&drv{bug: bug}).publishEnv(r) // publish.go
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			return 1
		}
		_ = locked(state, func() error {
			r, err := load(p)
			if err != nil {
				return err
			}
			r.PID = cmd.Process.Pid
			return save(state, r)
		})
		go func() {
			_ = cmd.Wait()
			done <- cmd.ProcessState.ExitCode()
		}()
	}
	enforces := bug != "no-deadline" && bug != "lazy-deadline"
	early := time.Duration(1<<62 - 1)
	switch bug {
	case "early-kill":
		early = 2 * time.Second
	case "die-at-once":
		early = 0
	}
	for t := time.NewTicker(25 * time.Millisecond); ; <-t.C {
		select {
		case code := <-done:
			_ = locked(state, func() error {
				r, err := load(p)
				if err != nil || r.State != "running" {
					return err
				}
				if bug == "ended-wrong-exit" {
					code = 0
				}
				r.State, r.Exit, r.End = "ended", &code, time.Now().UTC()
				r.Publish = readPublish(r, bug) // publish.go
				return save(state, r)
			})
			return 0
		default:
		}
		var over bool
		_ = locked(state, func() error {
			r, err := load(p)
			if err != nil {
				over = true
				return err
			}
			switch {
			case enforces && r.Persist && r.State == "ready" && time.Now().After(r.Deadline) && bug != "persist-deadline-expires":
				stopNow(state, r, "", bug) // a persistent box's lease ends in a stop: its disk is kept (persist.go)
			case enforces && (time.Now().After(r.Deadline) || time.Since(r.Start) > early):
				endAtDeadline(state, r, bug)
			case closed(r.Window) && bug != "no-reclaim" && bug != "lazy-reclaim" && r.State != "stopped":
				reclaimNow(state, r, bug)
			}
			over = r.Kind != "run" && r.State != "ready" && r.State != "stopped" // a stopped box's disk expires here
			return nil
		})
		if over {
			return 0
		}
	}
}
