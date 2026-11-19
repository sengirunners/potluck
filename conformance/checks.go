package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

const actor = "potluck-conformance"

var liveStates = []string{"ready", "running"}

func (r *run) key() string {
	r.seq++
	return "zmconf-" + r.tag + "-" + strconv.Itoa(r.seq)
}

func (r *run) ttlMax() int {
	if r.desc.TTLMaxS == nil {
		return 0
	}
	return *r.desc.TTLMaxS
}

func (r *run) shortTTL() int { return min(r.opt.ShortTTL, r.ttlMax()) }

// longTTL outlasts every check that does not expire on purpose.
func (r *run) longTTL() int {
	return min(max(600, int(r.opt.RunWait/time.Second)+60), r.ttlMax())
}

func (r *run) has(verb string) bool { return slices.Contains(r.desc.Verbs, verb) }

func (r *run) describe() bool {
	var d description
	err := must(r.call(verbDescribe, "", nil, &d))
	if err == nil {
		err = describeShape(d, r.opt.Kind)
	}
	r.desc, r.kind = d, d.Kind
	if r.opt.Kind != "" {
		r.kind = r.opt.Kind
	}
	if !r.add("describe (runs on only "+strings.Join(envAllowlist, ", ")+")", err) {
		return false
	}
	r.cfg["kind"] = r.kind
	if d.Isolation == "none" || (d.Credentials != nil && *d.Credentials) {
		r.warn("the class is credentialed or unisolated", fmt.Sprintf("isolation %q, credentials %v: zm sends it only trusted commits and listed scripts, and this suite's probe runs on it", d.Isolation, *d.Credentials))
	}
	return true
}

func describeShape(d description, wantKind string) error {
	var bad []string
	fail := func(format string, a ...any) { bad = append(bad, fmt.Sprintf(format, a...)) }
	switch {
	case d.Spec == "" && d.SpecNext == "":
		fail("potluck %q, want %s", d.SpecNext, protocol.Protocol)
	case d.Spec != "" && !protocol.SameProtocol(d.Spec, protocol.Protocol):
		fail("zm %q, want %s", d.Spec, protocol.ProtocolOld)
	case d.SpecNext != "" && !protocol.SameProtocol(d.SpecNext, protocol.Protocol):
		fail("potluck %q, want %s", d.SpecNext, protocol.Protocol)
	}
	if d.Name == "" {
		fail("empty name")
	}
	if d.Version == "" {
		fail("empty version")
	}
	switch {
	case d.Kind != "db" && d.Kind != "run" && d.Kind != "box":
		fail("kind %q, want db, run or box", d.Kind)
	case wantKind != "" && d.Kind != wantKind:
		fail("kind %q, config says %q", d.Kind, wantKind)
	}
	if d.Kind == "box" {
		if !slices.Contains(d.Verbs, verbExec) {
			fail("a box's verbs %v lack exec", d.Verbs)
		}
		if d.Isolation != "ephemeral" || d.Credentials == nil || *d.Credentials {
			fail("a box runs whatever its holder sends: isolation %q, credentials %v, want ephemeral and false", d.Isolation, deref2(d.Credentials))
		}
		if z := d.Size; z == nil || z.Name == "" || z.OS == "" || z.CPU <= 0 || z.MemoryMB <= 0 || (z.DiskGB != nil && *z.DiskGB < 0) {
			fail("a box needs size {name, os, cpu, memory_mb, disk_gb?}")
		}
	}
	for _, v := range requiredVerbs {
		if !slices.Contains(d.Verbs, v) {
			fail("verbs %v lack %s", d.Verbs, v)
		}
	}
	if d.Available == nil {
		fail("no available")
	} else if !*d.Available && d.Why == "" {
		fail("available false without why")
	}
	if d.Isolation != "none" && d.Isolation != "ephemeral" {
		fail("isolation %q, want none or ephemeral", d.Isolation)
	}
	if d.Credentials == nil {
		fail("no credentials")
	}
	if d.Cost == nil || d.Cost.Unit == "" || d.Cost.Amount == nil {
		fail("cost needs unit and amount")
	}
	if d.ReportsSpend == nil {
		fail("no reports_spend")
	}
	if d.TTLMaxS == nil || *d.TTLMaxS <= 0 {
		fail("ttl_max_s must be positive")
	}
	if d.Capabilities == nil {
		fail("no capabilities array")
	}
	if slices.Contains(d.Verbs, verbSetup) && (d.Setup == nil || d.Setup.Touches == nil) {
		fail("setup is listed without describe.setup.touches")
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

func (r *run) envelope() {
	r.add("an unknown verb answers not_implemented",
		r.expect("no-such-verb", "", nil, protocol.CodeNotImplemented))
	stdout, exit, err := r.exchange(verbStatus, []byte("this is not json"), r.env)
	if err == nil {
		var e *protocol.Error
		if e, err = decode(verbStatus, stdout, exit, nil); err == nil && e == nil {
			err = fmt.Errorf("answered ok")
		}
	}
	r.add("a malformed request answers ok:false, exit 0", err)
	if r.desc.Serve {
		r.skip("serve mode", "not checked: this suite runs one-shot, which every driver has")
	}
	if r.has(verbSetup) {
		r.skip("setup", "not sent: only an owner's bind runs it")
	}
}

// expect sends a call that must be refused with one of codes.
func (r *run) expect(verb, id string, a json.RawMessage, codes ...string) error {
	e, err := r.call(verb, id, a, nil)
	return wantCode(e, err, codes...)
}

func (r *run) checkList() {
	_, err := r.list()
	r.add("list answers a handles array", err)
}

// ours: a handle this run made, by its request_id (every key carries the tag).
func (r *run) ours(e listEntry) bool {
	return r.live[e.Handle] || strings.Contains(e.RequestID, r.tag)
}

func (r *run) list() ([]listEntry, error) {
	var l listing
	if err := must(r.call(verbList, "", nil, &l)); err != nil {
		return nil, err
	}
	if l.Handles == nil {
		return nil, fmt.Errorf(`no "handles" array`)
	}
	return *l.Handles, nil
}

func (r *run) handles() {
	var bad, foreign, missing error
	verbs := []string{verbStatus, verbRelease}
	if r.has(verbRenew) {
		verbs = append(verbs, verbRenew)
	}
	if r.kind == "box" {
		verbs = append(verbs, verbExec)
	}
	other := "db"
	if r.kind == "db" {
		other = "run"
	}
	try := func(verb, id string, code string) error {
		var a json.RawMessage
		switch verb {
		case verbRenew:
			a = args("ttl_s", 60)
		case verbExec:
			a = args("argv", []string{"true"})
		}
		if err := r.expect(verb, id, a, code); err != nil {
			return fmt.Errorf("%s %s: %v", verb, id, err)
		}
		return nil
	}
	for _, v := range verbs {
		for _, id := range []string{"garbage", r.kind + ":", r.kind + ":" + r.opt.Target} {
			if err := try(v, id, protocol.CodeBadRequest); bad == nil {
				bad = err
			}
		}
		name := "zmconf_x_" + r.tag
		for _, id := range []string{r.kind + ":zmconf-other-" + r.tag + "/" + name, other + ":" + r.opt.Target + "/" + name} {
			if err := try(v, id, protocol.CodeBadRequest); foreign == nil {
				foreign = err
			}
		}
		if err := try(v, r.kind+":"+r.opt.Target+"/zmconf_missing_"+r.tag, protocol.CodeNotFound); missing == nil {
			missing = err
		}
	}
	r.add("a malformed handle answers bad_request", bad)
	r.add("another binding's handle answers bad_request", foreign)
	r.add("a handle never issued answers not_found", missing)
}

// acquire sends one acquire and tracks what it made.
func (r *run) acquire(a json.RawMessage, cfg map[string]string) (acquired, *protocol.Error, error) {
	var res acquired
	e, err := r.callCfg(verbAcquire, "", a, cfg, &res)
	return r.track(a, res, e, err)
}

// track records what an acquire sent with a made: its handle to end, its
// spend, its url as a secret.
func (r *run) track(a json.RawMessage, res acquired, e *protocol.Error, err error) (acquired, *protocol.Error, error) {
	if err != nil { // timed out, cut off or broken: it may still have made something
		r.reconcile(a)
	}
	if err != nil || e != nil {
		return res, e, err
	}
	if res.Handle != "" {
		r.live[res.Handle] = true
	}
	r.noteSpend(res.Handle, res.Spend)
	if res.URL != "" {
		r.addSecret(res.URL)
	}
	return res, nil, nil
}

// reconcile finds what a failed acquire made, through list, as zm does after
// a timeout (SPEC 20), so the suite ends it. It runs even when ctx is cancelled.
func (r *run) reconcile(a json.RawMessage) {
	ctx := r.ctx
	r.ctx = context.WithoutCancel(ctx)
	defer func() { r.ctx = ctx }()
	var sent struct {
		Key string `json:"request_id"`
	}
	if json.Unmarshal(a, &sent) != nil || sent.Key == "" {
		return
	}
	es, err := r.list()
	if err != nil {
		return
	}
	for _, e := range es {
		if e.RequestID == sent.Key {
			r.live[e.Handle] = true
		}
	}
}

// addSecret records a url and its password, which no other place may carry.
func (r *run) addSecret(raw string) {
	if !slices.Contains(r.secrets, raw) {
		r.secrets = append(r.secrets, raw)
	}
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && len(pw) >= 4 && !slices.Contains(r.secrets, pw) {
			r.secrets = append(r.secrets, pw)
		}
	}
}

func (r *run) runArgs(key string, ttl int, script ...string) json.RawMessage {
	return args("request_id", key, "actor", actor, "ttl_s", ttl, "ref", r.opt.Ref, "commit", r.opt.Commit,
		"script", r.opt.Script, "args", script)
}

func (r *run) dbArgs(key, name string, ttl int) json.RawMessage {
	m := []any{"request_id", key, "actor", actor, "ttl_s", ttl, "name", name}
	if r.opt.Ref != "" {
		m = append(m, "ref", r.opt.Ref)
	}
	if r.opt.Commit != "" {
		m = append(m, "commit", r.opt.Commit)
	}
	return args(m...)
}

// boxArgs asks for a box; withCode adds the ref and commit to check out.
func (r *run) boxArgs(key string, ttl int, withCode bool) json.RawMessage {
	m := []any{"request_id", key, "actor", actor, "ttl_s", ttl}
	if withCode {
		m = append(m, "ref", r.opt.Ref, "commit", r.opt.Commit)
	}
	return args(m...)
}

func (r *run) dbName() string {
	r.seq++
	return "zmconf_" + r.tag + "_" + strconv.Itoa(r.seq)
}

// held is a resource under test: its handle, its heartbeat or url, its key.
type held struct {
	acquired
	key, beat string
	sent      json.RawMessage
}

// acquireAny acquires one resource of the driver's kind and checks the answer.
func (r *run) acquireAny(ttl int) (held, error) { return r.acquireIn(r.cfg, ttl) }

// acquireIn is acquireAny with another config.
func (r *run) acquireIn(cfg map[string]string, ttl int) (held, error) {
	h := r.newHeld(ttl)
	before := time.Now()
	a, e, err := r.acquire(h.sent, cfg)
	h.acquired = a
	if err = must(e, err); err != nil {
		return h, err
	}
	return h, r.acquireShape(a, ttl, before)
}

// newHeld is a fresh key and the acquire args for one resource of the
// driver's kind, not yet sent.
func (r *run) newHeld(ttl int) held {
	h := held{key: r.key()}
	switch r.kind {
	case "db":
		h.sent = r.dbArgs(h.key, r.dbName(), ttl)
	case "box":
		h.sent = r.boxArgs(h.key, ttl, false)
	default:
		h.beat = r.beatFile()
		h.sent = r.runArgs(h.key, ttl, longScript(h.beat, r.longTTL())...)
	}
	return h
}

func (r *run) acquireShape(a acquired, ttl int, before time.Time) error {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(r.kind+":"+r.opt.Target+"/") + ".+$")
	if !re.MatchString(a.Handle) {
		return fmt.Errorf("handle %q is not %s:%s/<id>", a.Handle, r.kind, r.opt.Target)
	}
	exp, err := wireTime(a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("expires_at: %v", err)
	}
	if a.TTLS == nil || *a.TTLS <= 0 || *a.TTLS > ttl {
		return fmt.Errorf("ttl_s %v, sent %d", deref(a.TTLS), ttl)
	}
	got := time.Duration(*a.TTLS) * time.Second
	if exp.Before(before.Add(got-2*time.Second)) || exp.After(time.Now().Add(got+2*time.Second)) {
		return fmt.Errorf("expires_at %s is not ttl_s (%d) from now", a.ExpiresAt, *a.TTLS)
	}
	return nil
}

func deref(p *int) any {
	if p == nil {
		return "absent"
	}
	return *p
}

// wireTime parses SPEC 2's wire time: RFC 3339 UTC.
func wireTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return t, err
	}
	if _, off := t.Zone(); off != 0 {
		return t, fmt.Errorf("%s is not UTC", s)
	}
	return t, nil
}

func (r *run) status(h string) (statusReport, error) {
	var st statusReport
	if err := must(r.call(verbStatus, h, nil, &st)); err != nil {
		return st, err
	}
	r.noteSpend(h, st.Spend)
	switch {
	case st.Handle != h:
		return st, fmt.Errorf("status answers handle %q, want %q", st.Handle, h)
	case !slices.Contains([]string{"ready", "running", "stopped", "ended", "expired", "released", "reclaimed", "lost"}, st.State):
		return st, fmt.Errorf("state %q is not one of SPEC 20's", st.State)
	}
	if _, err := wireTime(st.ExpiresAt); err != nil {
		return st, fmt.Errorf("status expires_at: %v", err)
	}
	return st, nil
}

// wantState checks a status against one state and its exit: nil wants none.
func wantState(st statusReport, state string, exit *int) error {
	if st.State != state {
		return fmt.Errorf("state %q, want %q", st.State, state)
	}
	switch {
	case exit == nil && st.Exit != nil:
		return fmt.Errorf("state %s carries exit %d", state, *st.Exit)
	case exit != nil && st.Exit == nil:
		return fmt.Errorf("state %s has no exit, want %d", state, *exit)
	case exit != nil && *st.Exit != *exit:
		return fmt.Errorf("exit %d, want %d", *st.Exit, *exit)
	}
	return nil
}

func (r *run) release(h string) (releaseReport, error) {
	var rel releaseReport
	if err := must(r.call(verbRelease, h, nil, &rel)); err != nil {
		return rel, err
	}
	r.noteSpend(h, rel.Spend)
	if rel.Released == nil {
		return rel, fmt.Errorf(`release has no "released"`)
	}
	return rel, nil
}

func wantRelease(rel releaseReport, released bool, exit *int) error {
	switch {
	case *rel.Released != released:
		return fmt.Errorf("released %v, want %v", *rel.Released, released)
	case exit == nil && rel.Exit != nil:
		return fmt.Errorf("release carries exit %d", *rel.Exit)
	case exit != nil && rel.Exit == nil:
		return fmt.Errorf("release has no exit, want %d", *exit)
	case exit != nil && *rel.Exit != *exit:
		return fmt.Errorf("release exit %d, want %d", *rel.Exit, *exit)
	}
	return nil
}

func ptr(i int) *int { return &i }

// listed checks that list shows the handle with its key and state.
func (r *run) listed(h, key, state string) error {
	es, err := r.list()
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.Handle != h {
			continue
		}
		switch {
		case e.RequestID != key:
			return fmt.Errorf("request_id %q, want %q", e.RequestID, key)
		case e.State != state:
			return fmt.Errorf("state %q, want %q", e.State, state)
		}
		if _, err := wireTime(e.ExpiresAt); err != nil {
			return fmt.Errorf("list expires_at: %v", err)
		}
		return nil
	}
	return fmt.Errorf("list does not show %s", h)
}

// oneFor checks that list shows exactly one handle for the key.
func (r *run) oneFor(key string) error {
	es, err := r.list()
	if err != nil {
		return err
	}
	n := 0
	for _, e := range es {
		if e.RequestID == key {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("list shows %d handles for one request_id", n)
	}
	return nil
}

// noteSpend holds a spend report against describe and the last one seen.
func (r *run) noteSpend(h string, s *spendReport) {
	if s == nil {
		return
	}
	r.sawSpend = true
	bad := func(format string, a ...any) { r.spendBad = append(r.spendBad, fmt.Sprintf(format, a...)) }
	if r.desc.ReportsSpend != nil && !*r.desc.ReportsSpend {
		bad("%s reports spend with reports_spend false", h)
	}
	if s.Amount == nil {
		bad("%s: spend has no amount", h)
		return
	}
	if r.desc.Cost != nil && s.Unit != r.desc.Cost.Unit {
		bad("%s: spend in %q, cost.unit is %q", h, s.Unit, r.desc.Cost.Unit)
	}
	if last, ok := r.spend[h]; ok && *s.Amount < last {
		bad("%s: spend fell from %v to %v", h, last, *s.Amount)
		return
	}
	r.spend[h] = *s.Amount
}

// endSpend is the status zm calls once after the end: a driver that reports
// spend must still answer it.
func (r *run) endSpend(st statusReport) {
	if r.desc.ReportsSpend != nil && *r.desc.ReportsSpend && st.Spend == nil {
		r.spendBad = append(r.spendBad, st.Handle+": status after the end has no spend")
	}
}

// pollState calls status until it answers one of states.
func (r *run) pollState(h string, deadline time.Time, states ...string) (statusReport, error) {
	for {
		st, err := r.status(h)
		if err != nil {
			return st, err
		}
		if slices.Contains(states, st.State) {
			return st, nil
		}
		if !time.Now().Before(deadline) || r.ctx.Err() != nil {
			return st, fmt.Errorf("state %q, want %s", st.State, strings.Join(states, " or "))
		}
		r.sleep(tick)
	}
}

func (r *run) gone(a acquired, want bool) error {
	exists, err := r.opt.DBExists(r.ctx, a.URL)
	switch {
	case err != nil:
		return fmt.Errorf("database probe: %v", err)
	case exists && !want:
		return fmt.Errorf("the database is still there")
	case !exists && want:
		return fmt.Errorf("the database is not there")
	}
	return nil
}

func (r *run) lifecycle() {
	switch r.kind {
	case "db":
		r.lifecycleDB()
	case "box":
		r.lifecycleBox()
	default:
		r.lifecycleRun()
	}
}

func (r *run) lifecycleRun() {
	h, err := r.acquireAny(r.longTTL())
	if !r.add("acquire answers handle, expires_at and ttl_s once the run has started, not when it ends", err) {
		r.skip("lifecycle", "acquire failed")
		return
	}
	var want error
	if h.Commit != r.opt.Commit {
		want = fmt.Errorf("commit %q, sent %q", h.Commit, r.opt.Commit)
	}
	r.add("acquire echoes commit", want)
	if h.beat != "" {
		r.add("the run is going", r.waitBeat(h.beat))
	} else {
		r.skip("the run is going", "--remote: a run's heartbeat cannot be seen")
	}
	st, err := r.status(h.Handle)
	if err == nil {
		err = wantState(st, "running", nil)
	}
	if err == nil && st.Commit != r.opt.Commit {
		err = fmt.Errorf("commit %q, sent %q", st.Commit, r.opt.Commit)
	}
	r.add("status answers running, commit echoed, no exit", err)
	r.add("list shows the handle and its request_id", r.listed(h.Handle, h.key, "running"))

	again, e, err := r.acquire(h.sent, r.cfg)
	err = must(e, err)
	if err == nil && again.Handle != h.Handle {
		err = fmt.Errorf("handle %q, first answer %q", again.Handle, h.Handle)
	}
	if err == nil {
		err = r.oneFor(h.key)
	}
	if err == nil && h.beat != "" {
		r.sleep(2 * tick)
		if n := runners(h.beat); n != 1 {
			err = fmt.Errorf("%d runs going", n)
		}
	}
	r.add("the same request_id gives the same handle and one resource", err)

	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, true, nil)
	}
	if r.add("release of a running run: released true, no exit", err) {
		delete(r.live, h.Handle)
	}
	if h.beat != "" {
		r.add("release kills the run's process group", r.waitStopped(time.Now().Add(r.opt.Grace), h.beat))
	} else {
		r.skip("release kills the run's process group", "--remote")
	}
	r.afterEnd(h, "released", nil)
}

// afterEnd is the same for both kinds, once a resource is released.
func (r *run) afterEnd(h held, state string, exit *int) {
	st, err := r.status(h.Handle)
	if err == nil {
		err = wantState(st, state, exit)
		r.endSpend(st)
	}
	r.add("status after release answers "+state, err)
	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, false, nil)
	}
	r.add("release twice: released false, no exit, not an error", err)
	again, e, err := r.acquire(h.sent, r.cfg)
	err = must(e, err)
	if err == nil && again.Handle != h.Handle {
		err = fmt.Errorf("handle %q, first answer %q", again.Handle, h.Handle)
	}
	if err == nil {
		delete(r.live, h.Handle)
		st, err = r.status(h.Handle)
		if err == nil {
			err = wantState(st, state, exit)
		}
	}
	r.add("the same request_id after the end makes nothing new", err)
}

func (r *run) lifecycleDB() {
	h, err := r.acquireAny(r.longTTL())
	if err == nil && h.URL == "" {
		err = fmt.Errorf("no url")
	}
	if !r.add("acquire answers handle, expires_at, ttl_s and url once the database exists", err) {
		r.skip("lifecycle", "acquire failed")
		return
	}
	var name struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(h.sent, &name)
	var want error
	if h.Handle != "db:"+r.opt.Target+"/"+name.Name {
		want = fmt.Errorf("handle %q is not db:%s/%s", h.Handle, r.opt.Target, name.Name)
	}
	if want == nil && r.opt.Commit != "" && h.Commit != "" && h.Commit != r.opt.Commit {
		want = fmt.Errorf("commit %q, sent %q", h.Commit, r.opt.Commit)
	}
	r.add("the handle names the caller's database, any commit echoed", want)
	if r.opt.DBExists != nil {
		r.add("the database is there", r.gone(h.acquired, true))
	} else {
		r.skip("the database is there", "no database probe (--db-probe)")
	}
	st, err := r.status(h.Handle)
	if err == nil {
		err = wantState(st, "ready", nil)
	}
	r.add("status answers ready, no exit", err)
	r.add("list shows the handle and its request_id", r.listed(h.Handle, h.key, "ready"))

	again, e, err := r.acquire(h.sent, r.cfg)
	err = must(e, err)
	if err == nil && (again.Handle != h.Handle || again.URL != h.URL) {
		err = fmt.Errorf("a repeat answered another handle or url")
	}
	if err == nil {
		err = r.oneFor(h.key)
	}
	r.add("the same request_id gives the same handle, its url and one resource", err)

	a, e, err := r.acquire(r.dbArgs(r.key(), name.Name, r.longTTL()), r.cfg)
	if err == nil && e == nil && a.Handle != h.Handle {
		r.live[a.Handle] = true
	}
	r.add("an existing name under a new key answers conflict", wantCode(e, err, protocol.CodeConflict))

	rel, err := r.release(h.Handle)
	if err == nil {
		err = wantRelease(rel, true, nil)
	}
	if r.add("release of a ready database: released true, no exit", err) {
		delete(r.live, h.Handle)
	}
	if r.opt.DBExists != nil {
		r.add("release drops the database", r.gone(h.acquired, false))
	} else {
		r.skip("release drops the database", "no database probe (--db-probe)")
	}
	r.afterEnd(h, "released", nil)
}

// ended checks a run that exits on its own: only ended reports a script's exit.
func (r *run) ended() {
	if r.kind != "run" {
		r.skip("a run that ends on its own", "run targets only")
		return
	}
	key := r.key()
	a, e, err := r.acquire(r.runArgs(key, r.longTTL(), "exit", "7"), r.cfg)
	if !r.add("acquire a run that exits 7", must(e, err)) {
		return
	}
	st, err := r.pollState(a.Handle, time.Now().Add(r.opt.RunWait), "ended", "expired", "lost")
	if err == nil {
		err = wantState(st, "ended", ptr(7))
	}
	if err == nil && st.Commit != r.opt.Commit {
		err = fmt.Errorf("commit %q, sent %q", st.Commit, r.opt.Commit)
	}
	if !r.add("status after the script exits: ended, its exit, commit echoed", err) {
		return
	}
	r.endSpend(st)
	r.add("list shows it ended", r.listed(a.Handle, key, "ended"))
	rel, err := r.release(a.Handle)
	if err == nil {
		err = wantRelease(rel, false, ptr(7))
	}
	r.add("release of a run already ended: released false, its exit", err)
	rel, err = r.release(a.Handle)
	if err == nil {
		err = wantRelease(rel, false, ptr(7))
	}
	if r.add("release twice of a run already ended: the same answer", err) {
		delete(r.live, a.Handle)
	}
}

// runExit runs the probe to its end and answers its exit.
func (r *run) runExit(script ...string) (int, error) {
	a, e, err := r.acquire(r.runArgs(r.key(), r.longTTL(), script...), r.cfg)
	if err = must(e, err); err != nil {
		return 0, err
	}
	st, err := r.pollState(a.Handle, time.Now().Add(r.opt.RunWait), "ended", "expired", "lost")
	if err != nil {
		return 0, err
	}
	if st.State != "ended" || st.Exit == nil {
		return 0, wantState(st, "ended", ptr(0))
	}
	if _, err := r.release(a.Handle); err == nil {
		delete(r.live, a.Handle)
	}
	return *st.Exit, nil
}

func headErr(code int, err error) error {
	switch {
	case err != nil:
		return err
	case code == exitWrongHead:
		return fmt.Errorf("the run's HEAD is not the commit sent")
	case code == exitDirty:
		return fmt.Errorf("the run's checkout has uncommitted changes")
	case code != 0:
		return fmt.Errorf("the probe exited %d", code)
	}
	return nil
}

func (r *run) code() {
	if r.kind == "box" {
		r.codeBox()
		return
	}
	if r.kind == "db" {
		r.skip("a run's checkout", "run targets only")
		return
	}
	const head = "a run's HEAD is the commit sent, in a clean checkout"
	err := headErr(r.runExit("head", r.opt.Commit))
	why := r.unproven()
	switch {
	case err != nil:
		r.add(head, err)
		return
	case why != "":
		r.skip(head, "passed, not proven: "+why)
	default:
		r.add(head, nil)
	}
	code, err := r.runExit("head", strings.Repeat("0", 40))
	if err == nil && code != exitWrongHead {
		err = fmt.Errorf("the probe exited %d for a commit that is not HEAD, want %d: the driver is not running the script, or not passing its exit", code, exitWrongHead)
	}
	r.add("the probe can fail: a wrong commit exits 3", err)
	if r.opt.Worktree == "" {
		r.skip("a dirty working tree does not leak in", "no --worktree")
		return
	}
	undo, err := dirty(r.opt.Worktree)
	if err == nil {
		err = headErr(r.runExit("head", r.opt.Commit))
		if uerr := undo(); uerr != nil {
			r.warn("the worktree is put back", uerr.Error())
		}
	}
	if err == nil && worktreeHead(r.opt.Worktree) == r.opt.Commit {
		r.note("a dirty working tree does not leak in", "--commit is the worktree's HEAD: a driver ignoring commit would pass, use an older one")
		return
	}
	r.add("a dirty working tree does not leak in", err)
}

// unproven says why a passing HEAD check proves nothing: a driver that runs
// the ref's tip instead of commit passes when they are the same commit.
func (r *run) unproven() string {
	if r.opt.Repo == "" {
		return "no --repo, so the suite cannot show --commit differs from the tip of --ref (a driver ignoring commit would pass)"
	}
	out, err := exec.Command("git", "ls-remote", r.opt.Repo, r.opt.Ref).Output()
	if err != nil {
		return fmt.Sprintf("git ls-remote %s %s: %v", r.opt.Repo, r.opt.Ref, err)
	}
	f := strings.Fields(string(out))
	switch {
	case len(f) == 0:
		return fmt.Sprintf("--repo has no ref %s", r.opt.Ref)
	case f[0] == r.opt.Commit:
		return "--commit is the tip of --ref, so a driver ignoring commit would pass: use an older pushed commit (the ref moving after zm resolved it)"
	}
	return ""
}

func (r *run) renew() {
	if !r.has(verbRenew) {
		h, err := r.acquireAny(r.longTTL())
		if err == nil {
			err = r.expect(verbRenew, h.Handle, args("ttl_s", 60), protocol.CodeNotImplemented)
		}
		r.add("renew answers not_implemented when not listed", err)
		r.endHeld(h)
		r.skip("the lifetime cap across renewals", "no renew: a lease cannot be extended")
		return
	}
	short, limit := r.shortTTL(), r.ttlMax()
	start := time.Now()
	h, err := r.acquireAny(short)
	if !r.add("acquire with a short ttl", err) {
		return
	}
	defer r.endHeld(h)
	first, _ := wireTime(h.ExpiresAt)
	var ren renewal
	err = must(r.call(verbRenew, h.Handle, args("ttl_s", min(short*3, limit)), &ren))
	var moved time.Time
	if err == nil {
		if moved, err = wireTime(ren.ExpiresAt); err == nil && limit > short && !moved.After(first) {
			err = fmt.Errorf("expires_at %s is not after %s", ren.ExpiresAt, h.ExpiresAt)
		}
	}
	r.add("renew answers a later expires_at", err)
	r.add("renew without ttl_s answers bad_request", r.expect(verbRenew, h.Handle, nil, protocol.CodeBadRequest))
	r.add("renew past the cap answers bad_request", r.expect(verbRenew, h.Handle, args("ttl_s", limit+1), protocol.CodeBadRequest))
	r.sleep(time.Until(start.Add(1200 * time.Millisecond)))
	err = r.expect(verbRenew, h.Handle, args("ttl_s", limit), protocol.CodeBadRequest)
	r.add("the cap counts the whole lifetime since acquire: renewing to ttl_max_s now is past it", err)
	st, err := r.status(h.Handle)
	if err == nil && st.ExpiresAt != ren.ExpiresAt {
		if at, perr := wireTime(st.ExpiresAt); perr != nil || !at.Equal(moved) {
			err = fmt.Errorf("status expires_at %s, renew answered %s", st.ExpiresAt, ren.ExpiresAt)
		}
	}
	r.add("status answers the renewed expires_at, a refused renew changes nothing", err)
	if limit <= short {
		r.skip("a renewed resource outlives its first deadline", "ttl_max_s leaves no room")
		return
	}
	r.sleep(time.Until(first.Add(time.Second)))
	err = r.alive(h)
	r.add("a renewed resource outlives its first deadline", err)
}

// alive checks a resource is still going, by the driver's word and the probe's.
func (r *run) alive(h held) error {
	want := "running"
	if r.kind != "run" {
		want = "ready"
	}
	st, err := r.status(h.Handle)
	if err == nil {
		err = wantState(st, want, nil)
	}
	switch {
	case err != nil:
	case h.beat != "":
		err = r.beating(h.beat, 5*time.Second)
	case r.kind == "db" && r.opt.DBExists != nil:
		err = r.gone(h.acquired, true)
	case r.kind == "box":
		err = r.execExit(h.Handle, 0, "true")
	}
	return err
}

// endHeld releases a resource a check is done with.
func (r *run) endHeld(h held) {
	if h.Handle == "" || !r.live[h.Handle] {
		return
	}
	r.end(h.Handle)
}

// end releases a handle, tracked or not.
func (r *run) end(h string) {
	if _, err := r.release(h); err == nil {
		delete(r.live, h)
	}
}

// boxGone is the box's form of expiry's "ends with no call" check. It rests
// on the driver's word (list): a target that ends a box lazily on any call,
// list included, still passes it.
const boxGone = "a short ttl ends a box with no call to the driver: list, the first call after the deadline and grace, shows it expired"

// expiry leaves a resource to its deadline. From after the first status until
// the resource is gone the suite does not call the driver at all: a target
// that only ends a resource when asked fails here.
func (r *run) expiry() {
	ttl := r.shortTTL()
	h, err := r.acquireAny(ttl)
	if !r.add("acquire with a short ttl", err) {
		return
	}
	exp, _ := wireTime(h.ExpiresAt)
	if h.beat != "" {
		err = r.waitBeat(h.beat)
	}
	if err == nil {
		err = r.alive(h)
	}
	r.add("alive before its deadline", err)
	const gone = "a short ttl ends the resource with no release and no call to the driver"
	deadline := exp.Add(r.opt.Grace)
	switch {
	case h.beat != "":
		r.sleep(time.Until(exp))
		err = r.waitStopped(deadline, h.beat)
		if last := lastBeat(h.beat); err == nil && last.Before(exp.Add(-2*time.Second)) {
			err = fmt.Errorf("ended at %s, before expires_at %s", last.UTC().Format(time.RFC3339), h.ExpiresAt)
		}
		r.add(gone, err)
	case r.kind == "db" && r.opt.DBExists != nil:
		for err = r.gone(h.acquired, false); err != nil && time.Now().Before(deadline) && r.ctx.Err() == nil; err = r.gone(h.acquired, false) {
			r.sleep(tick)
		}
		if time.Now().Before(exp.Add(-time.Second)) {
			err = fmt.Errorf("gone before expires_at")
		}
		r.add(gone, err)
	case r.kind == "box" && r.opt.Strict:
		// A box is seen only through its driver. No call at all until the
		// deadline and the grace have passed; then the first call is list,
		// not status, so a target that ends a box only when its status is
		// asked (no supervisor) shows it still live here. Strict only: it
		// waits out the whole grace, which a default run does not.
		r.sleep(time.Until(deadline))
		r.add(boxGone, r.listed(h.Handle, h.key, "expired"))
	default:
		r.sleep(time.Until(exp))
		r.skip(gone, "the resource cannot be seen here: only status is checked")
	}
	var exit *int
	if r.kind == "run" {
		exit = ptr(exitExpired)
	}
	st, err := r.pollState(h.Handle, deadline, "expired", "ended", "lost", "released")
	if err == nil {
		err = wantState(st, "expired", exit)
		r.endSpend(st)
	}
	if r.add("status after the deadline: expired, a run with exit 124", err) {
		delete(r.live, h.Handle)
	}
	r.add("list shows it expired", r.listed(h.Handle, h.key, "expired"))
	if r.kind == "box" {
		r.add("exec after the deadline answers conflict", r.expect(verbExec, h.Handle, args("argv", []string{"true"}), protocol.CodeConflict))
	}
	rel, err := r.release(h.Handle)
	if err == nil && *rel.Released {
		err = fmt.Errorf("released true: the deadline had already ended it")
	}
	r.add("release after expiry: released false, not an error", err)
}

func (r *run) refuse() {
	ttl := r.shortTTL()
	over := r.ttlMax() + 1
	var noKey, overTTL, negativeTTL json.RawMessage
	switch r.kind {
	case "db":
		noKey = args("actor", actor, "ttl_s", ttl, "name", r.dbName())
		overTTL = r.dbArgs(r.key(), r.dbName(), over)
		negativeTTL = r.dbArgs(r.key(), r.dbName(), -1)
	case "box":
		noKey = args("actor", actor, "ttl_s", ttl)
		overTTL = r.boxArgs(r.key(), over, false)
		negativeTTL = r.boxArgs(r.key(), -1, false)
	default:
		noKey = args("actor", actor, "ttl_s", ttl, "ref", r.opt.Ref, "commit", r.opt.Commit, "script", r.opt.Script, "args", []string{"exit", "0"})
		overTTL = r.runArgs(r.key(), over, "exit", "0")
		negativeTTL = r.runArgs(r.key(), -1, "exit", "0")
	}
	refused := func(a json.RawMessage, cfg map[string]string, codes ...string) error {
		res, e, err := r.acquire(a, cfg)
		if err == nil && e == nil && res.Handle != "" {
			r.endHeld(held{acquired: res})
		}
		return wantCode(e, err, codes...)
	}
	r.add("acquire without request_id answers bad_request", refused(noKey, r.cfg, protocol.CodeBadRequest))
	r.add("a ttl_s past ttl_max_s answers bad_request", refused(overTTL, r.cfg, protocol.CodeBadRequest))
	r.add("a negative ttl_s answers bad_request", refused(negativeTTL, r.cfg, protocol.CodeBadRequest))
	if r.kind == "db" {
		r.add("acquire without a name answers bad_request",
			refused(args("request_id", r.key(), "actor", actor, "ttl_s", ttl), r.cfg, protocol.CodeBadRequest))
		r.createIn()
		return
	}
	if r.kind == "box" {
		r.refuseBox(refused)
		return
	}
	marker := r.dir + "/shell-ran"
	sent := args("request_id", r.key(), "actor", actor, "ttl_s", ttl, "ref", r.opt.Ref, "commit", r.opt.Commit,
		"script", r.opt.Script+"; touch "+marker)
	err := refused(sent, r.cfg, protocol.CodeBadRequest, protocol.CodeForbidden, protocol.CodeNotFound)
	if err == nil && !r.opt.Remote {
		r.sleep(2 * tick)
		if _, serr := os.Stat(marker); serr == nil {
			err = fmt.Errorf("the shell ran it")
		}
	}
	r.add("a shell-string script is refused", err)
}

// createIn points the target at a host not in create_in. The listener proves
// "before connecting": a target that tries the host first shows up on it.
func (r *run) createIn() {
	const name = "a db host not in create_in answers forbidden, before connecting"
	if len(r.opt.OutsideConfig) == 0 {
		r.skip(name, "no --outside-config: how a host reaches a driver is the driver's config")
		return
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.add(name, err)
		return
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	cfg := map[string]string{}
	for k, v := range r.cfg {
		cfg[k] = v
	}
	for k, v := range r.opt.OutsideConfig {
		cfg[k] = strings.ReplaceAll(v, "{port}", port)
	}
	err = checkCreateInConnection(r.ctx, l, func() error {
		res, e, err := r.acquire(r.dbArgs(r.key(), r.dbName(), r.shortTTL()), cfg)
		if err == nil && e == nil && res.Handle != "" {
			r.endHeld(held{acquired: res})
		}
		return wantCode(e, err, protocol.CodeForbidden)
	})
	listened := false
	for _, v := range r.opt.OutsideConfig {
		listened = listened || strings.Contains(v, "{port}")
	}
	if err == nil && !listened {
		r.skip(name, "answered forbidden; before connecting not proven: no {port} in --outside-config")
		return
	}
	r.add(name, err)
}

// finish ends what checks left going, then the checks over the whole run.
func (r *run) finish() {
	if r.ctx.Err() != nil { // cancelled: the resources must still end
		r.ctx = context.WithoutCancel(r.ctx)
	}
	for h := range r.live { // what a failed check left: the suite's own
		r.end(h)
	}
	// Anything else live under this run's keys was made without the suite
	// knowing: that is the driver leaving it behind. End it too.
	es, err := r.list()
	if err == nil {
		var left []string
		for _, e := range es {
			if slices.Contains(liveStates, e.State) && r.ours(e) {
				left = append(left, e.Handle)
				r.end(e.Handle)
			}
		}
		if len(left) > 0 {
			err = fmt.Errorf("still live, released now: %s", strings.Join(left, ", "))
		}
	}
	if es, lerr := r.list(); err == nil && lerr == nil {
		for _, e := range es {
			if slices.Contains(liveStates, e.State) && r.ours(e) {
				err = fmt.Errorf("%s is still %s after release", e.Handle, e.State)
			}
		}
	}
	if err == nil && len(r.beats) > 0 {
		err = r.waitStopped(time.Now().Add(r.opt.Grace), r.beats...)
	}
	r.add("nothing left behind", err)

	if r.kind == "db" {
		var leak error
		if r.seen(r.stderr.Bytes()) {
			leak = fmt.Errorf("stderr carries the url or its password")
		}
		r.add("no url on stderr", leak)
		leak = nil
		if r.seen(r.answers.Bytes()) {
			leak = fmt.Errorf("an answer other than acquire carries the url or its password")
		}
		r.add("url only in acquire answers", leak)
	}
	if r.sawSecret {
		var leak error
		if r.seen(r.stderr.Bytes()) || r.seen(r.answers.Bytes()) {
			leak = fmt.Errorf("a renter secret on the driver's stderr or in an answer")
		}
		r.add("no secret on stderr or in any answer", leak)
	}
	var late []string
	for _, c := range r.calls {
		if c.late {
			late = append(late, fmt.Sprintf("%s over %s", c.verb, c.limit))
		}
	}
	var slow error
	if len(late) > 0 {
		slow = fmt.Errorf("%s", strings.Join(slices.Compact(late), ", "))
	}
	r.add(timingCheck, slow)
	switch {
	case len(r.spendBad) > 0:
		r.add("spend in cost.unit, never falling, readable after the end", fmt.Errorf("%s", strings.Join(r.spendBad, "; ")))
	case !r.sawSpend && (r.desc.ReportsSpend == nil || !*r.desc.ReportsSpend):
		r.skip("spend in cost.unit, never falling, readable after the end", "reports_spend false")
	default:
		r.add("spend in cost.unit, never falling, readable after the end", nil)
	}
}

func (r *run) seen(b []byte) bool {
	for _, s := range r.secrets {
		if strings.Contains(string(b), s) {
			return true
		}
	}
	return false
}
