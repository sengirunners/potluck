// Package conformance checks a run-target driver (zm-run-<name>) against SPEC
// section 20. The zm-run-conformance command runs it against an executable;
// the suite's own tests run it against refdriver and its broken variants. It
// checks the driver's side only: routing, the stricter class, roles, bindings,
// caps and budgets are the daemon's, and it has no dependency on the daemon.
//
// Everything is black box: the driver is a process, and what a run or a
// database really does is seen through a probe script (ProbeScript) or a
// database probe (Options.DBExists), never through the driver's own word.
//
// The interface is the process protocol (SPEC 20, docs/protocol.md).
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// Options select what to check. A zero Kind uses describe's.
type Options struct {
	// Kind is "db", "run" or "box", sent in config with every call. A driver
	// serving more than one needs it to answer describe. A box (a VM kept up
	// for its lease, commands sent with exec) needs no Ref, Commit or Script;
	// with Ref and Commit the suite also checks a box's checkout.
	Kind string
	// Target is the binding name sent in config and expected in handles.
	Target string
	// Config is the driver config sent with every request.
	Config map[string]string

	// Ref and Commit are the code to acquire: required for a run, sent for a
	// db when set. The commit must hold the probe script. Pick an older commit
	// than the ref's tip (the ref moving after zm resolved it), or a driver
	// ignoring commit would pass.
	Ref, Commit string
	// Repo is the binding's remote. With it the suite asks the ref's tip
	// (git ls-remote) and proves the HEAD check; without it that check is a SKIP.
	Repo string
	// Script names the probe script, as the driver resolves names.
	Script string
	// Worktree is the checkout a driver's run starts from, if it has one. The
	// suite dirties it for one run and restores it. Empty skips that check.
	Worktree string
	// Remote says runs do not share a filesystem with the suite, so a run's
	// heartbeat cannot be seen: the checks that need it are skipped.
	Remote bool

	// DBExists says whether the database at url is there, and is how the
	// suite sees one without trusting the driver. Nil skips those checks.
	DBExists func(ctx context.Context, url string) (bool, error)
	// OutsideConfig is laid over Config for one acquire that points the
	// target at a host not in create_in. "{port}" in a value becomes a port
	// the suite listens on, to see that nothing connects. Empty skips the check.
	OutsideConfig map[string]string
	// ReclaimConfig is laid over Config for the reclaim checks: a reclaim
	// policy that says no once "{window}" closes. The suite fills "{window}"
	// with "HH:MM:SS-HH:MM:SS", local time, ending ReclaimIn after it is
	// made. Empty skips the checks.
	ReclaimConfig map[string]string
	// NetAllow and NetForbid are host:port addresses ("[v6]:port" for IPv6)
	// a run or a box (the probe through exec) must, and must not, open a TCP
	// connection to: the internet it should reach, and the LAN, the host, a
	// tailnet or metadata it should not. NetMail are host:port addresses of
	// an outside mail server (MailAddrs: a host on 25, 465 and 587): a run
	// or box must not open them, and this machine must, else the check is a
	// SKIP (zm-eakh.12.5.13). NetPeers runs two at once and checks that
	// neither reaches the other (IPv4 /24 and IPv6 link-local neighbours), on
	// NetPeerPort (default 47123). NetResolve are DNS names (or name:port,
	// the port the probe opens where the guest has no getent; default 443) a
	// run or box must resolve; unset, a guest NetAllow gives a network must
	// resolve NetAllow's names, or github.com (zm-eakh.12.24.7). All empty
	// skips the net checks.
	NetAllow, NetForbid, NetMail, NetResolve []string
	NetPeers                                 bool
	NetPeerPort                              int
	// ReclaimIn is how long the reclaim window stays open (default 1 min):
	// it must outlast an acquire.
	ReclaimIn time.Duration

	// ShortTTL is the ttl_s of the expiry and renew checks (default 5).
	ShortTTL int
	// Grace is how far past expires_at a target may take to end a resource
	// (default 30 s).
	Grace time.Duration
	// RunWait bounds how long a run may take to show up or finish (default 5 min).
	RunWait time.Duration
	// Only limits the run to these groups (see Groups). describe and the
	// final checks always run.
	Only []string
	// Strict adds one last check that fails unless every group in
	// StrictGroups ran with no check skipped (and, for a run or a box, at
	// least one NetForbid was given, and NetPeers). Without it a skip is a
	// pass. It also runs a box's no-call expiry check (boxGone), which
	// waits out the grace too.
	Strict bool
	// StateDirs are the host directories the driver keeps its records and
	// logs in, which the box-secrets group searches for a value it set
	// (nil: config state_dir and ~/.potluck-driver-<name>, where they exist).
	StateDirs []string
	// LeftoverRoots are the places, besides a box's own HOME, TMPDIR, working
	// directory and XDG_RUNTIME_DIR, where the checks that a later box holds
	// nothing of an earlier one leave a marker in a box and search for it
	// in later boxes (empty: DefaultLeftoverRoots; leftover.go). A guest's
	// are small; a test fake whose boxes see the host's filesystem names a
	// directory standing in for them instead of the host's /tmp.
	LeftoverRoots []string
	// VerbLimit and AcquireLimit replace SPEC 20's 30 s and 120 s, for tests.
	VerbLimit, AcquireLimit time.Duration
	// BoxAuthority, for a box, is an authority a box can reach and a client
	// to reach it with (group box-authority). Nil skips that group.
	BoxAuthority *BoxAuthority
}

// Groups are the check groups, in the order they run. "boot" runs only when
// Only names it: one box acquired, its size, exec and release, the short check
// `zm host share` gives each size after the first (lifecycle covers it).
var Groups = []string{"boot", "handles", "lifecycle", "acquire-progress", "procs", "agent-runner", "pty", "ports", "box-secrets", "box-secrets-ops", "box-authority", "persist", "volumes", "shelf", "warm", "ended", "code", "renew", "expiry", "reclaim", "refuse", "net", "root"}

// Result is one check. Warn is a pass worth a person's look. Group is the
// group that made it ("" for describe, the envelope and the final checks).
type Result struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Warn    bool   `json:"warn,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Group   string `json:"group,omitempty"`
}

// Validate reports what kind needs and the options lack.
func (o Options) Validate(kind string) error {
	var missing []string
	if kind == "run" {
		for _, f := range []struct{ flag, v string }{{"--ref", o.Ref}, {"--commit", o.Commit}, {"--script", o.Script}} {
			if f.v == "" {
				missing = append(missing, f.flag)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("a run target needs %s", strings.Join(missing, ", "))
	}
	for _, g := range o.Only {
		if !slices.Contains(Groups, g) {
			return fmt.Errorf("no group %q (groups: %s)", g, strings.Join(Groups, ", "))
		}
	}
	if a := o.BoxAuthority; a != nil {
		if err := a.validate(kind); err != nil {
			return err
		}
	}
	return nil
}

// Kind asks describe which kind the driver answers for ("" when it does not say).
func Kind(ctx context.Context, path string, opt Options) string {
	r := newRun(ctx, path, opt)
	defer r.close()
	var d struct {
		Kind string `json:"kind"`
	}
	if e, err := r.call(verbDescribe, "", nil, &d); err != nil || e != nil {
		return ""
	}
	return d.Kind
}

type run struct {
	ctx  context.Context
	path string
	opt  Options
	cfg  map[string]string
	env  []string
	dir  string // heartbeats live here; a heartbeat stops when it goes
	seq  int
	tag  string // unique per run, in every key and name

	out     []Result
	kind    string
	desc    description
	mu      sync.Mutex // calls, stderr and answers: one exec may run on its own goroutine
	calls   []callRecord
	stderr  capped
	answers capped // stdout of every call but acquire
	secrets []string

	live  map[string]bool // handles this run must still end
	beats []string
	spend map[string]float64
	// sawSecret: a box exec carried a renter secret (its value is in secrets).
	sawSecret bool
	spendBad  []string
	sawSpend  bool
}

func newRun(ctx context.Context, path string, opt Options) *run {
	if opt.Target == "" {
		opt.Target = "potluck-conformance"
	}
	if opt.ShortTTL <= 0 {
		opt.ShortTTL = 5
	}
	if opt.Grace <= 0 {
		opt.Grace = 30 * time.Second
	}
	if opt.ReclaimIn <= 0 {
		opt.ReclaimIn = time.Minute
	}
	if opt.RunWait <= 0 {
		opt.RunWait = 5 * time.Minute
	}
	cfg := map[string]string{}
	for k, v := range opt.Config {
		cfg[k] = v
	}
	cfg["target"] = opt.Target
	if opt.Kind != "" {
		cfg["kind"] = opt.Kind
	}
	dir, err := os.MkdirTemp("", "zmrc")
	if err != nil {
		dir = os.TempDir()
	}
	return &run{ctx: ctx, path: path, opt: opt, cfg: cfg, env: allowedEnv(), dir: dir, tag: randomTag(),
		live: map[string]bool{}, spend: map[string]float64{}}
}

// close ends the heartbeats: their loop stops when the directory goes.
func (r *run) close() {
	for range 3 {
		if os.RemoveAll(r.dir) == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (r *run) add(name string, err error) bool {
	res := Result{Name: name, OK: err == nil}
	if err != nil {
		res.Detail = err.Error()
	}
	r.out = append(r.out, res)
	return err == nil
}

// note adds a passing check that carries a remark.
func (r *run) note(name, detail string) {
	r.out = append(r.out, Result{Name: name, OK: true, Detail: detail})
}

func (r *run) warn(name, detail string) {
	r.out = append(r.out, Result{Name: name, OK: true, Warn: true, Detail: detail})
}

func (r *run) skip(name, why string) {
	r.out = append(r.out, Result{Name: name, OK: true, Skipped: true, Detail: why})
}

func (r *run) group(name string) bool {
	return len(r.opt.Only) == 0 && name != "boot" || slices.Contains(r.opt.Only, name)
}

// Run checks the driver at path and returns every result in order. It ends
// every resource it made, also after a failed check or a cancelled ctx.
func Run(ctx context.Context, path string, opt Options) []Result {
	r := newRun(ctx, path, opt)
	defer r.close()
	if !r.describe() {
		return r.scrubbed()
	}
	if !r.add("the target is available (nothing else can be checked if not)", available(r.desc)) {
		return r.scrubbed()
	}
	r.envelope()
	r.checkList()
	groups := map[string]func(){"boot": r.boot, "handles": r.handles, "lifecycle": r.lifecycle, "acquire-progress": r.acquireProgress, "procs": r.procs, "agent-runner": r.agentRunner, "pty": r.pty, "ports": r.ports, "box-secrets": r.boxSecretsGroup, "box-secrets-ops": r.boxSecretsOpsGroup, "box-authority": r.boxAuthorityGroup, "persist": r.persist, "volumes": r.volumes, "shelf": r.shelf, "warm": r.warm, "ended": r.ended, "code": r.code,
		"renew": r.renew, "expiry": r.expiry, "reclaim": r.reclaim, "refuse": r.refuse, "net": r.net, "root": r.root}
	for _, g := range Groups {
		if r.group(g) && ctx.Err() == nil {
			n := len(r.out)
			groups[g]()
			for i := n; i < len(r.out); i++ {
				r.out[i].Group = g
			}
		}
	}
	r.finish()
	if r.opt.Strict {
		r.strict()
	}
	return r.scrubbed()
}

// StrictGroups are the groups Options.Strict needs run with nothing skipped:
// the network checks and the expiry checks; for a db the expiry checks alone
// (a database's outbound reach is not covered: the net group is runs' and
// boxes').
func StrictGroups(kind string) []string {
	if kind == "db" {
		return []string{"expiry"}
	}
	return []string{"net", "expiry"}
}

// StrictCheck names the check Options.Strict adds.
const StrictCheck = "strict: the network and expiry checks ran, none skipped"

// strict fails the run when a group in StrictGroups did not run or skipped a
// check: without it a driver with an open network, or one that never ends
// a resource at its deadline, can pass on skips.
func (r *run) strict() {
	var why []string
	for _, g := range StrictGroups(r.kind) {
		ran := false
		for _, res := range r.out {
			if res.Group != g {
				continue
			}
			ran = true
			if res.Skipped {
				why = append(why, fmt.Sprintf("%q skipped (%s)", res.Name, res.Detail))
			}
		}
		if !ran {
			why = append(why, "group "+g+" did not run")
		}
	}
	for _, res := range r.out { // box secrets: a value in the driver's state is unproven without --state-dir
		if res.Name == bsNoState && res.Skipped {
			why = append(why, fmt.Sprintf("%q skipped: give --state-dir", bsNoState))
		}
	}
	if r.kind != "db" && len(r.opt.NetForbid) == 0 {
		why = append(why, "no --net-forbid, so nothing showed the "+r.kind+" cannot reach what it must not")
	}
	if r.kind != "db" && !r.opt.NetPeers {
		why = append(why, "no --net-peers, so nothing showed two at once cannot reach each other")
	}
	var err error
	if len(why) > 0 {
		err = fmt.Errorf("not proven: %s", strings.Join(why, "; "))
	}
	r.add(StrictCheck, err)
}

func available(d description) error {
	if d.Available != nil && !*d.Available {
		return fmt.Errorf("describe says available false: %s", d.Why)
	}
	return nil
}

// scrubbed takes every secret out of the results: a driver's refusal may
// quote the url, and results get printed.
func (r *run) scrubbed() []Result {
	for i := range r.out {
		for _, s := range r.secrets {
			r.out[i].Detail = strings.ReplaceAll(r.out[i].Detail, s, "<secret>")
		}
	}
	return r.out
}

// Failed counts failed results.
func Failed(rs []Result) int {
	n := 0
	for _, r := range rs {
		if !r.OK {
			n++
		}
	}
	return n
}

// SkippedResults are the skipped results, in order.
func SkippedResults(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.Skipped {
			out = append(out, r)
		}
	}
	return out
}

// Skipped counts skipped results.
func Skipped(rs []Result) int {
	n := 0
	for _, r := range rs {
		if r.Skipped {
			n++
		}
	}
	return n
}

// Format renders results as PASS/FAIL/SKIP lines.
func Format(rs []Result) string {
	var b strings.Builder
	for _, r := range rs {
		tag := "PASS"
		switch {
		case r.Skipped:
			tag = "SKIP"
		case r.Warn:
			tag = "WARN"
		case !r.OK:
			tag = "FAIL"
		}
		fmt.Fprintf(&b, "%s  %s", tag, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(&b, ": %s", r.Detail)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// FirstFailure is the first FAIL line of Format's output, without its tag
// ("<check>: <detail>"), or "" when none failed.
func FirstFailure(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "FAIL  "); ok {
			return rest
		}
	}
	return ""
}

// Late reports whether out (Format's output) failed because a verb ran past
// its time limit: the timing check failed, or a check gave up waiting for a
// verb's answer. A machine busy with other work (another share, an image
// pull) fails this way though its driver is sound.
func Late(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, "FAIL  ")
		if ok && (strings.HasPrefix(rest, timingCheck+": ") || strings.Contains(rest, ": "+noAnswer+" ")) {
			return true
		}
	}
	return false
}

// must turns a refusal into an error for checks that expect success.
func must(e *protocol.Error, err error) error {
	if err != nil {
		return err
	}
	if e != nil {
		return fmt.Errorf("refused: %s", e.Error())
	}
	return nil
}

// Wire shapes. Pointers tell a missing field from a zero one.
type (
	cost struct {
		Unit   string   `json:"unit"`
		Amount *float64 `json:"amount"`
	}
	description struct {
		Spec         string    `json:"zm"`
		SpecNext     string    `json:"potluck"` // the protocol's later key; either is read
		Name         string    `json:"name"`
		Kind         string    `json:"kind"`
		Version      string    `json:"version"`
		Verbs        []string  `json:"verbs"`
		Available    *bool     `json:"available"`
		Why          string    `json:"why"`
		Isolation    string    `json:"isolation"`
		Credentials  *bool     `json:"credentials"`
		Cost         *cost     `json:"cost"`
		ReportsSpend *bool     `json:"reports_spend"`
		TTLMaxS      *int      `json:"ttl_max_s"`
		Capabilities *[]string `json:"capabilities"`
		Size         *boxSize  `json:"size"`
		Disks        *struct {
			Root bool `json:"root"` // kept disks keep the root too, not only HOME (persist.go)
		} `json:"disks"`
		Warm  *warmReport `json:"warm"` // warm.go
		Shelf *struct {
			Path string `json:"path"`
		} `json:"shelf"` // shelf.go
		Serve bool `json:"serve"`
		Setup *struct {
			Touches *[]string `json:"touches"`
		} `json:"setup"`
	}
	boxSize struct {
		Name     string `json:"name"`
		OS       string `json:"os"`
		CPU      int    `json:"cpu"`
		MemoryMB int    `json:"memory_mb"`
		DiskGB   *int   `json:"disk_gb"`
	}
	execResult struct {
		Exit      *int    `json:"exit"`
		Stdout    *string `json:"stdout"`
		Stderr    *string `json:"stderr"`
		Truncated *bool   `json:"truncated"`
	}
	spendReport struct {
		Unit   string   `json:"unit"`
		Amount *float64 `json:"amount"`
	}
	acquired struct {
		Handle    string       `json:"handle"`
		ExpiresAt string       `json:"expires_at"`
		TTLS      *int         `json:"ttl_s"`
		Commit    string       `json:"commit"`
		URL       string       `json:"url"`
		Size      string       `json:"size"`
		Digest    string       `json:"image_digest"`
		Spend     *spendReport `json:"spend"`
		Net       string       `json:"net"`
		Volumes   []volAnswer  `json:"volumes"` // volumes.go
	}
	statusReport struct {
		Handle    string       `json:"handle"`
		State     string       `json:"state"`
		Commit    string       `json:"commit"`
		Exit      *int         `json:"exit"`
		ExpiresAt string       `json:"expires_at"`
		Spend     *spendReport `json:"spend"`
	}
	releaseReport struct {
		Released *bool        `json:"released"`
		Exit     *int         `json:"exit"`
		Spend    *spendReport `json:"spend"`
	}
	listEntry struct {
		Handle    string `json:"handle"`
		RequestID string `json:"request_id"`
		State     string `json:"state"`
		ExpiresAt string `json:"expires_at"`
	}
	listing struct {
		Handles *[]listEntry `json:"handles"`
	}
	renewal struct {
		ExpiresAt string `json:"expires_at"`
	}
)

func args(kv ...any) json.RawMessage {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}
