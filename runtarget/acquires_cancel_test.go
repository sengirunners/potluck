package runtarget_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// cancelRig is a daemon with one box binding per driver: each a reference
// driver (with a bug, or none), optionally wrapped by a shell script that
// answers some verbs itself. In a wrapper, DIR is the binding's directory
// and REF the reference driver.
type cancelRig struct {
	t     *testing.T
	ctx   context.Context
	svc   *runtarget.Service
	owner *client.Client
	fix   string
	gate  string // while it exists, a reference acquire holds in phase booting
	// network is a client over the daemon's network handler, as a token's.
	network func() *client.Client
}

type rigDriver struct{ bug, wrap string }

func newCancelRig(t *testing.T, drivers map[string]rigDriver) *cancelRig {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	r := &cancelRig{t: t, ctx: context.Background(), fix: fix, gate: filepath.Join(fix, "gate"),
		network: func() *client.Client { return client.WithHandler(dm.Handler) }}
	paths := map[string]string{}
	for name, d := range drivers {
		dir := filepath.Join(fix, name)
		state := filepath.Join(dir, "state")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		drv, err := refdriver.Install(dir, state, d.bug)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { refdriver.KillAll(state) })
		paths[name] = drv
		if d.wrap != "" {
			script := "#!/bin/sh\nin=$(cat)\ncase $in in\n" +
				strings.NewReplacer("DIR", "'"+dir+"'", "REF", "'"+drv+"'").Replace(d.wrap) +
				"\n*) printf %s \"$in\" | exec '" + drv + "' ;;\nesac\n"
			wrap := filepath.Join(dir, "zm-run-"+name)
			if err := os.WriteFile(wrap, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			paths[name] = wrap
		}
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			r.svc = s
			s.SetLookPath(func(name string) (string, error) { return paths[name], nil })
			s.SetAcquireLimit(2 * time.Second)
		}
	}
	r.owner = client.WithHandler(dm.SocketHandler())
	r.owner.Project, r.owner.ProjectFrom = "acme", "flag"
	if _, err := r.owner.CreateProject(r.ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	for name := range drivers {
		if _, err := computeclient.BindRunTarget(r.ctx, r.owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: name, Driver: name, Kind: "box",
			Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"acquire_gate": r.gate}}}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// member is a token client of the project acting as as.
func (r *cancelRig) member(as string) *client.Client {
	tok, err := r.owner.MintToken(r.ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
	if err != nil {
		r.t.Fatal(err)
	}
	c := r.network()
	c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
	return c
}

func (r *cancelRig) start(ctx context.Context, c *client.Client, target, rid string) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := computeclient.AcquireRunTarget(ctx, c, target, computeapi.AcquireRunTargetRequest{RequestID: rid, TTLS: 600})
		ch <- err
	}()
	return ch
}

func (r *cancelRig) closeGate(closed bool) {
	r.t.Helper()
	var err error
	if closed {
		err = os.WriteFile(r.gate, nil, 0o600)
	} else {
		err = os.Remove(r.gate)
	}
	if err != nil {
		r.t.Fatal(err)
	}
}

// waitFor polls cond for a minute.
func (r *cancelRig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	for end := time.Now().Add(time.Minute); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			r.t.Fatalf("never %s", what)
		}
	}
}

func (r *cancelRig) exists(name, file string) bool {
	_, err := os.Stat(filepath.Join(r.fix, name, file))
	return err == nil
}

// ended checks that target made one handle and it is released, and that
// progress and a second cancel of rid say cancelled.
func (r *cancelRig) ended(target, rid string) {
	r.t.Helper()
	if live, err := computeclient.RunTargetHandles(r.ctx, r.owner, target, true); err != nil || len(live) != 0 {
		r.t.Errorf("%s after a cancel that stood: live %+v %v", target, live, err)
	}
	all, err := computeclient.RunTargetHandles(r.ctx, r.owner, target, false)
	if err != nil || len(all) != 1 || all[0].State != "released" {
		r.t.Errorf("%s: handles %+v %v, want the one it made, released", target, all, err)
	}
	if p, err := computeclient.RunTargetAcquireProgress(r.ctx, r.owner, target, rid, ""); err != nil || p.State != protocol.AcquireCancelled || p.Handle != "" {
		r.t.Errorf("progress of %s's %s after its cancel ended the box: %+v %v, want cancelled", target, rid, p, err)
	}
	if c, err := computeclient.CancelRunTargetAcquire(r.ctx, r.owner, target, rid, ""); err != nil || !c.Cancelled || c.State != protocol.AcquireCancelled || c.Handle != "" {
		r.t.Errorf("a second cancel of %s's %s: %+v %v, want cancelled", target, rid, c, err)
	}
}

// TestCancelQuota: a member's cancels of request_ids the daemon never
// received are bounded; a cancel of an acquire it holds is not, and another
// member's count is their own.
func TestCancelQuota(t *testing.T) {
	r := newCancelRig(t, map[string]rigDriver{"box": {}})
	ana, bo := r.member("ana"), r.member("bo")
	for i := range runtarget.CancelsUnknownMax {
		if c, err := computeclient.CancelRunTargetAcquire(r.ctx, ana, "box", fmt.Sprintf("never-%d", i), ""); err != nil || !c.Cancelled {
			t.Fatalf("cancel %d of a key never sent: %+v %v", i, c, err)
		}
	}
	if _, err := computeclient.CancelRunTargetAcquire(r.ctx, ana, "box", "one-more", ""); code(err) != api.CodeRateLimited {
		t.Fatalf("a cancel past the quota: %v, want rate_limited", err)
	}
	if c, err := computeclient.CancelRunTargetAcquire(r.ctx, bo, "box", "bos-own", ""); err != nil || !c.Cancelled {
		t.Errorf("another member's cancel: %+v %v", c, err)
	}
	r.closeGate(true)
	a := r.start(r.ctx, ana, "box", "real")
	r.waitFor("booting", func() bool {
		p, err := computeclient.RunTargetAcquireProgress(r.ctx, ana, "box", "real", "")
		return err == nil && p.Phase == "booting"
	})
	if c, err := computeclient.CancelRunTargetAcquire(r.ctx, ana, "box", "real", ""); err != nil || !c.Cancelled {
		t.Errorf("cancel of an acquire in flight, past the quota: %+v %v", c, err)
	}
	r.closeGate(false)
	if err := <-a; code(err) != api.CodeConflict {
		t.Errorf("the acquire after its cancel: %v, want conflict", err)
	}
}

const cancelStands = `*'"verb":"cancel"'*) printf '{"ok":true,"result":{"cancelled":true,"state":"cancelled"}}\n' ;;`

// TestCancelFollowUps: a cancel that stood ends what its acquire made in
// every way zm can learn of it, and afterwards progress and a second cancel
// say cancelled. keeper answers the handle though its cancel stood, after
// the caller left. crasher makes the box and dies once the cancel is with
// it, before the cancel's answer is back. flaky and flakier
// make it, are cut off, and their list fails: flaky's cancel comes after
// its acquire answered (as after a restart, nothing here is with the
// driver), flakier's while it is in flight, and the sweep follows it up.
func TestCancelFollowUps(t *testing.T) {
	cutOff := `*'"verb":"acquire"'*)
	if [ -e DIR/first ]; then sleep 30; fi
	touch DIR/first
	printf %s "$in" | REF >/dev/null
	touch DIR/made
	sleep 30 ;;
*'"verb":"list"'*) if [ -e DIR/nolist ]; then exit 1; fi; printf %s "$in" | exec REF ;;
` + cancelStands
	r := newCancelRig(t, map[string]rigDriver{
		"keeper": {bug: "cancel-keeps-box"},
		"crasher": {wrap: `*'"verb":"acquire"'*)
	printf %s "$in" | REF >/dev/null
	touch DIR/made
	while [ ! -e DIR/cancelled ]; do sleep 0.05; done
	exit 1 ;;
*'"verb":"cancel"'*) touch DIR/cancelled; sleep 1; printf '{"ok":true,"result":{"cancelled":true,"state":"cancelled"}}\n' ;;`},
		"flaky":   {wrap: cutOff},
		"flakier": {wrap: cutOff},
	})
	cancel := func(target, rid string) {
		t.Helper()
		if c, err := computeclient.CancelRunTargetAcquire(r.ctx, r.owner, target, rid, ""); err != nil || !c.Cancelled {
			t.Fatalf("cancel of %s's %s: %+v %v", target, rid, c, err)
		}
	}

	// keeper: the caller goes away while the acquire is with the driver;
	// the acquire still reads its row and ends the box.
	r.closeGate(true)
	gone, leave := context.WithCancel(r.ctx)
	k := r.start(gone, r.owner, "keeper", "k1")
	r.waitFor("k1 booting", func() bool {
		p, err := computeclient.RunTargetAcquireProgress(r.ctx, r.owner, "keeper", "k1", "")
		return err == nil && p.Phase == "booting"
	})
	cancel("keeper", "k1")
	leave()
	r.closeGate(false)
	<-k
	r.waitFor("k1's box released", func() bool {
		all, err := computeclient.RunTargetHandles(r.ctx, r.owner, "keeper", false)
		return err == nil && len(all) == 1 && all[0].State == "released"
	})
	r.ended("keeper", "k1")

	// crasher: dies after making the box, while its cancel is with the
	// driver: the acquire's failure comes before the cancel's answer.
	c := r.start(r.ctx, r.owner, "crasher", "c1")
	r.waitFor("crasher made c1", func() bool { return r.exists("crasher", "made") })
	cancel("crasher", "c1")
	if err := <-c; err == nil {
		t.Errorf("crasher's c1 after its cancel stood answered a handle")
	}
	r.ended("crasher", "c1")

	// flaky: cut off with no cancel, its list failing; the row stays
	// acquiring with nothing here holding it. Its cancel finds the box.
	for _, name := range []string{"flaky", "flakier"} {
		if err := os.WriteFile(filepath.Join(r.fix, name, "nolist"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-r.start(r.ctx, r.owner, "flaky", "f1"); code(err) != api.CodeUnavailable {
		t.Fatalf("flaky's f1 cut off: %v, want unavailable", err)
	}
	if err := os.Remove(filepath.Join(r.fix, "flaky", "nolist")); err != nil {
		t.Fatal(err)
	}
	cancel("flaky", "f1")
	r.ended("flaky", "f1")

	// flakier: the cancel lands in flight; the acquire's list fails, so it
	// answers conflict with the box unfound; the sweep finds and ends it.
	f := r.start(r.ctx, r.owner, "flakier", "f2")
	r.waitFor("flakier made f2", func() bool { return r.exists("flakier", "made") })
	cancel("flakier", "f2")
	if err := <-f; code(err) != api.CodeConflict {
		t.Errorf("flakier's f2 after its cancel stood: %v, want conflict", err)
	}
	unfound := func(when string) {
		t.Helper()
		if all, err := computeclient.RunTargetHandles(r.ctx, r.owner, "flakier", false); err != nil || len(all) != 1 || all[0].Handle != "" {
			t.Fatalf("flakier %s: %+v %v, want f2 still acquiring", when, all, err)
		}
	}
	unfound("before its list worked")
	if err := os.Remove(filepath.Join(r.fix, "flakier", "nolist")); err != nil {
		t.Fatal(err)
	}
	r.svc.Sweep(r.ctx) // too soon: an acquire may still be with a driver
	unfound("after a sweep while an acquire could still hold f2")
	r.svc.SetNow(func() time.Time { return time.Now().Add(2 * time.Minute) })
	r.svc.Sweep(r.ctx)
	r.svc.SetNow(time.Now)
	r.ended("flakier", "f2")
}

// TestProgressAnswers: progress keeps the daemon's own errors' words and
// withholds a driver's; polling it runs describe once, not every time.
func TestProgressAnswers(t *testing.T) {
	r := newCancelRig(t, map[string]rigDriver{
		"mute": {wrap: `*'"verb":"progress"'*'"request_id":'*'-words'*) printf '{"ok":false,"error":{"code":"internal","message":"it was sent s3cr3t"}}\n' ;;
*'"verb":"progress"'*) echo not json ;;`},
		"counted": {wrap: `*'"verb":"describe"'*) echo x >> DIR/describes; printf %s "$in" | exec REF ;;`},
	})
	_, err := computeclient.RunTargetAcquireProgress(r.ctx, r.owner, "mute", "garbled", "")
	if err == nil || !strings.Contains(err.Error(), "not one JSON answer") {
		t.Errorf("progress of a driver that answered no JSON: %v, want the daemon's words", err)
	}
	_, err = computeclient.RunTargetAcquireProgress(r.ctx, r.owner, "mute", "x-words", "")
	if code(err) != api.CodeInternal || strings.Contains(fmt.Sprint(err), "s3cr3t") {
		t.Errorf("progress the driver refused: %v, want internal with its words withheld", err)
	}
	describes := func() int {
		b, _ := os.ReadFile(filepath.Join(r.fix, "counted", "describes"))
		return strings.Count(string(b), "x")
	}
	before := describes()
	for range 5 {
		if _, err := computeclient.RunTargetAcquireProgress(r.ctx, r.owner, "counted", "nope", ""); code(err) != api.CodeNotFound {
			t.Fatalf("progress of a key never sent: %v", err)
		}
	}
	if n := describes() - before; n > 1 {
		t.Errorf("5 progress polls ran describe %d times, want at most 1", n)
	}
}

// TestCancelAnsweredHere: a cancel of an acquire still queued here counts
// against the quota, and a repeat of a cancel that stood is answered
// without the driver, so a caller cannot grow the driver's state past the
// quota with either.
func TestCancelAnsweredHere(t *testing.T) {
	r := newCancelRig(t, map[string]rigDriver{
		"box": {wrap: `*'"verb":"cancel"'*) echo x >> DIR/cancels; printf %s "$in" | exec REF ;;`},
	})
	r.svc.SetAcquireLimit(30 * time.Second) // holder stays with the driver through every step below
	ana := r.member("ana")
	sent := func() int {
		b, _ := os.ReadFile(filepath.Join(r.fix, "box", "cancels"))
		return strings.Count(string(b), "x")
	}
	progress := func(rid, phase string) func() bool {
		return func() bool {
			p, err := computeclient.RunTargetAcquireProgress(r.ctx, ana, "box", rid, "")
			return err == nil && p.Phase == phase
		}
	}
	cancel := func(rid string) (computeapi.RunTargetAcquireCancel, error) {
		return computeclient.CancelRunTargetAcquire(r.ctx, ana, "box", rid, "")
	}
	for i := range runtarget.CancelsUnknownMax - 1 {
		if c, err := cancel(fmt.Sprintf("never-%d", i)); err != nil || !c.Cancelled {
			t.Fatalf("cancel %d of a key never sent: %+v %v", i, c, err)
		}
	}
	r.closeGate(true)
	holder := r.start(r.ctx, ana, "box", "holder") // with the driver, holding the binding
	r.waitFor("holder booting", progress("holder", "booting"))
	q := r.start(r.ctx, ana, "box", "queued")
	r.waitFor("queued queued", progress("queued", protocol.AcquirePhaseQueued))
	if c, err := cancel("queued"); err != nil || !c.Cancelled { // the quota's last
		t.Fatalf("cancel of the queued acquire: %+v %v", c, err)
	}
	q2 := r.start(r.ctx, ana, "box", "queued2")
	r.waitFor("queued2 queued", progress("queued2", protocol.AcquirePhaseQueued))
	if _, err := cancel("queued2"); code(err) != api.CodeRateLimited {
		t.Errorf("a cancel of a queued acquire past the quota: %v, want rate_limited", err)
	}
	r.closeGate(false)
	if err := <-holder; err != nil {
		t.Errorf("holder: %v", err)
	}
	if err := <-q; code(err) != api.CodeConflict {
		t.Errorf("queued after its cancel: %v, want conflict", err)
	}
	if err := <-q2; err != nil {
		t.Errorf("queued2, its cancel refused: %v, want its handle", err)
	}

	r.closeGate(true)
	first := r.start(r.ctx, ana, "box", "first")
	r.waitFor("first booting", progress("first", "booting"))
	before := sent()
	for range 3 {
		if c, err := cancel("first"); err != nil || !c.Cancelled {
			t.Fatalf("cancel of first: %+v %v", c, err)
		}
	}
	if n := sent() - before; n != 1 {
		t.Errorf("3 cancels of one acquire with the driver sent it %d cancels, want 1", n)
	}
	r.closeGate(false)
	if err := <-first; code(err) != api.CodeConflict {
		t.Errorf("first after its cancel: %v, want conflict", err)
	}
}
