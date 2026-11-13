package runtarget_test

import (
	"context"
	"encoding/json"
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

// TestAcquireProgress: before an acquire has a handle, its caller sees it
// queued (behind another acquire of the binding) or starting with the
// driver's phase, and can cancel it, queued or in flight; a cancelled
// request_id stays refused. A driver without acquire-progress still reports
// starting and ready, and refuses cancel.
func TestAcquireProgress(t *testing.T) {
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
	drivers := map[string]string{}
	for name, bug := range map[string]string{"ref": "", "old": "no-acquire-progress"} {
		state := filepath.Join(fix, name, "state")
		if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
			t.Fatal(err)
		}
		drv, err := refdriver.Install(filepath.Join(fix, name), state, bug)
		if err != nil {
			t.Fatal(err)
		}
		drivers[name] = drv
		t.Cleanup(func() { refdriver.KillAll(state) })
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(name string) (string, error) { return drivers[name], nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	mint := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "acme", Role: "member"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "acme", "flag"
		return c
	}
	ana, bo := mint("ana"), mint("bo")
	// An acquire holds in phase booting while its binding's gate file exists.
	gates := map[string]string{}
	gate := func(name string, closed bool) {
		t.Helper()
		var err error
		if closed {
			err = os.WriteFile(gates[name], nil, 0o600)
		} else {
			err = os.Remove(gates[name])
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, drv := range map[string]string{"box": "ref", "oldbox": "old"} {
		gates[name] = filepath.Join(fix, name+".gate")
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: name, Driver: drv, Kind: "box",
			Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"acquire_gate": gates[name]}}}); err != nil {
			t.Fatal(err)
		}
	}
	type landed struct {
		h   computeapi.RunTargetHandle
		err error
	}
	start := func(target, rid string) <-chan landed {
		ch := make(chan landed, 1)
		go func() {
			h, err := computeclient.AcquireRunTarget(ctx, ana, target, computeapi.AcquireRunTargetRequest{RequestID: rid, TTLS: 600})
			ch <- landed{h, err}
		}()
		return ch
	}
	progress := func(target, rid string) computeapi.RunTargetAcquireProgress {
		t.Helper()
		p, err := computeclient.RunTargetAcquireProgress(ctx, ana, target, rid, "")
		if err != nil {
			t.Fatalf("progress %s: %v", rid, err)
		}
		return p
	}
	// await polls rid's progress until it says state (and phase, if given).
	await := func(target, rid, state, phase string) {
		t.Helper()
		for end := time.Now().Add(time.Minute); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			p, err := computeclient.RunTargetAcquireProgress(ctx, ana, target, rid, "")
			if err == nil && p.State == state && (phase == "" || p.Phase == phase) {
				return
			}
		}
		t.Fatalf("%s never %s %s: %+v", rid, state, phase, progress(target, rid))
	}

	if _, err := computeclient.RunTargetAcquireProgress(ctx, ana, "box", "nope", ""); code(err) != api.CodeNotFound {
		t.Errorf("progress of a request_id never sent: %v", err)
	}

	// Watched to the end: starting, booting; then ready with the handle,
	// which a late cancel leaves alone. Another caller sees none of it.
	gate("box", true)
	a1 := start("box", "a1")
	await("box", "a1", protocol.AcquireStarting, "booting")
	if _, err := computeclient.RunTargetAcquireProgress(ctx, bo, "box", "a1", ""); code(err) != api.CodeNotFound {
		t.Errorf("another caller's progress of a1: %v", err)
	}
	// Queued behind a1 on the binding's lock, then cancelled there.
	a2 := start("box", "a2")
	await("box", "a2", protocol.AcquireStarting, protocol.AcquirePhaseQueued)
	c, err := computeclient.CancelRunTargetAcquire(ctx, ana, "box", "a2", "")
	if err != nil || !c.Cancelled {
		t.Errorf("cancel of a queued acquire: %+v %v", c, err)
	}
	gate("box", false)
	l1 := <-a1
	if l1.err != nil {
		t.Fatalf("a1: %v", l1.err)
	}
	if l2 := <-a2; code(l2.err) != api.CodeConflict {
		t.Errorf("the queued acquire after its cancel: %+v %v", l2.h, l2.err)
	}
	// The cancelled queued acquire never reached the driver: the driver
	// logs every acquire it receives, a1's among them.
	reached := func(rid string) bool {
		for _, k := range refdriver.Acquired(filepath.Join(fix, "ref", "state")) {
			if strings.HasSuffix(k, "-"+rid) {
				return true
			}
		}
		return false
	}
	if !reached("a1") {
		t.Error("the driver's acquire log lacks a1, which it served")
	}
	if reached("a2") {
		t.Error("the driver received a2, an acquire cancelled while queued")
	}
	if p := progress("box", "a1"); p.State != protocol.AcquireReady || p.Handle != l1.h.Handle {
		t.Errorf("progress after a1 answered: %+v, want ready %s", p, l1.h.Handle)
	}
	if p, err := computeclient.RunTargetAcquireProgress(ctx, owner, "box", "a1", l1.h.Actor); err != nil || p.Handle != l1.h.Handle {
		t.Errorf("the owner's progress of a1 for its actor: %+v %v", p, err)
	}
	if _, err := computeclient.RunTargetAcquireProgress(ctx, bo, "box", "a1", l1.h.Actor); code(err) != api.CodeForbidden {
		t.Errorf("a member naming another actor: %v", err)
	}
	if c, err := computeclient.CancelRunTargetAcquire(ctx, ana, "box", "a1", ""); err != nil || c.Cancelled || c.Handle != l1.h.Handle {
		t.Errorf("cancel after a1 answered: %+v %v", c, err)
	}
	if st, err := computeclient.RunTargetStatus(ctx, ana, "box", idOf(l1.h.Handle)); err != nil || st.State != "ready" {
		t.Errorf("a1 after the late cancel: %+v %v", st, err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, ana, "box", computeapi.AcquireRunTargetRequest{RequestID: "a2", TTLS: 600}); code(err) != api.CodeConflict {
		t.Errorf("a2 again after its cancel: %v", err)
	}
	if p := progress("box", "a2"); p.State != protocol.AcquireCancelled {
		t.Errorf("progress of the cancelled a2: %+v", p)
	}

	// Cancelled in flight: the acquire answers conflict and nothing is live.
	gate("box", true) // held until the cancel stops it
	a3 := start("box", "a3")
	await("box", "a3", protocol.AcquireStarting, "booting")
	if c, err := computeclient.CancelRunTargetAcquire(ctx, ana, "box", "a3", ""); err != nil || !c.Cancelled {
		t.Errorf("cancel in flight: %+v %v", c, err)
	}
	if l3 := <-a3; code(l3.err) != api.CodeConflict {
		t.Errorf("the acquire cancelled in flight: %+v %v", l3.h, l3.err)
	}
	gate("box", false)
	hs, err := computeclient.RunTargetHandles(ctx, ana, "box", true)
	if err != nil || len(hs) != 1 || hs[0].Handle != l1.h.Handle {
		t.Errorf("live handles: %+v %v, want only a1's", hs, err)
	}

	// A driver without the capability: starting with no phase, then ready;
	// cancel refused.
	gate("oldbox", true)
	a4 := start("oldbox", "a4")
	await("oldbox", "a4", protocol.AcquireStarting, "")
	if p := progress("oldbox", "a4"); p.State == protocol.AcquireStarting && p.Phase != "" && p.Phase != protocol.AcquirePhaseQueued {
		t.Errorf("a driver without progress reported phase %q", p.Phase)
	}
	if _, err := computeclient.CancelRunTargetAcquire(ctx, ana, "oldbox", "a4", ""); code(err) != api.CodeNotImplemented {
		t.Errorf("cancel on a driver without the capability: %v", err)
	}
	gate("oldbox", false)
	l4 := <-a4
	if l4.err != nil {
		t.Fatalf("a4: %v", l4.err)
	}
	if p := progress("oldbox", "a4"); p.State != protocol.AcquireReady || p.Handle != l4.h.Handle {
		t.Errorf("progress after a4 answered: %+v", p)
	}
	for target, h := range map[string]string{"box": l1.h.Handle, "oldbox": l4.h.Handle} {
		if _, err := computeclient.ReleaseRunTarget(ctx, ana, target, idOf(h)); err != nil {
			t.Error(err)
		}
	}
}

// TestAcquireProgressRelayed: progress and cancel cross to the host that
// lends the binding, keyed by project there as acquire is.
func TestAcquireProgressRelayed(t *testing.T) {
	rp := newRelayPair(t)
	ctx := context.Background()
	if _, err := computeclient.RunTargetAcquireProgress(ctx, rp.owner, lent, "r0", ""); code(err) != api.CodeNotFound {
		t.Errorf("progress of a request_id never sent: %v", err)
	}
	if c, err := computeclient.CancelRunTargetAcquire(ctx, rp.owner, lent, "r9", ""); err != nil || !c.Cancelled {
		t.Fatalf("cancel before the acquire: %+v %v", c, err)
	}
	if _, err := computeclient.AcquireRunTarget(ctx, rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: "r9", TTLS: 60}); code(err) != api.CodeConflict {
		t.Errorf("an acquire cancelled before it was sent: %v", err)
	}
	if p, err := computeclient.RunTargetAcquireProgress(ctx, rp.owner, lent, "r9", ""); err != nil || p.State != protocol.AcquireCancelled {
		t.Errorf("progress of r9: %+v %v", p, err)
	}
	h := rp.acquire(t, "r1", 60)
	if p, err := computeclient.RunTargetAcquireProgress(ctx, rp.owner, lent, "r1", ""); err != nil || p.State != protocol.AcquireReady || p.Handle != h.Handle {
		t.Errorf("progress of r1: %+v %v", p, err)
	}
	if _, err := rp.svc.HostCall(ctx, rp.project, "sandbox", protocol.VerbProgress, "", []byte(`{}`)); code(err) != api.CodeBadRequest {
		t.Errorf("a relayed progress without a request_id: %v", err)
	}
	// A host whose daemon predates progress and cancel refuses to relay
	// them, as HostCall refuses any verb it does not relay: progress falls
	// back as for a driver without the capability, cancel is not_implemented.
	rp.answer = func(_ context.Context, op computeapi.RunTargetOp) (json.RawMessage, error) {
		return runtarget.AsRelayedFromOld(api.Errorf(api.CodeBadRequest, "run target verb %q is not relayed", op.Verb))
	}
	if _, err := computeclient.RunTargetAcquireProgress(ctx, rp.owner, lent, "r8", ""); code(err) != api.CodeNotFound {
		t.Errorf("progress through an old host, never sent: %v; want not_found", err)
	}
	if _, err := computeclient.CancelRunTargetAcquire(ctx, rp.owner, lent, "r8", ""); code(err) != api.CodeNotImplemented {
		t.Errorf("cancel through an old host: %v; want not_implemented", err)
	}
	// Any other bad_request is the host's driver's own answer, passed on.
	rp.answer = func(_ context.Context, op computeapi.RunTargetOp) (json.RawMessage, error) {
		return runtarget.AsRelayed(op.Verb, nil, api.Errorf(api.CodeBadRequest, "the driver refuses this request_id"))
	}
	if _, err := computeclient.RunTargetAcquireProgress(ctx, rp.owner, lent, "r8", ""); code(err) != api.CodeBadRequest {
		t.Errorf("progress a current host's driver refuses: %v; want its bad_request", err)
	}
	if _, err := computeclient.CancelRunTargetAcquire(ctx, rp.owner, lent, "r8", ""); code(err) != api.CodeBadRequest {
		t.Errorf("cancel a current host's driver refuses: %v; want its bad_request", err)
	}
	rp.answer = nil
	// The host never passes its driver's progress message, or the words of
	// a progress or cancel refusal, on: it cannot tell whether they quote
	// what the acquire was sent.
	quoter := filepath.Join(t.TempDir(), "zm-run-quoter")
	if err := os.WriteFile(quoter, []byte(fmt.Sprintf("#!/bin/sh\n%q | sed -e 's/\"state\":\"/\"message\":\"QUOTED-SECRET\",\"state\":\"/' -e 's/no acquire/QUOTED-SECRET no acquire/'\n", rp.drv)), 0o755); err != nil {
		t.Fatal(err)
	}
	rp.svc.SetLookPath(func(string) (string, error) { return quoter, nil })
	_ = os.Remove(filepath.Join(os.Getenv("HOME"), runtarget.PinFile)) // the owner trusts the new driver
	var sent json.RawMessage                                           // a progress as the authority relays it
	rp.answer = func(_ context.Context, op computeapi.RunTargetOp) (json.RawMessage, error) {
		if op.Verb == protocol.VerbProgress {
			sent = op.Args
		}
		return nil, nil
	}
	// r1 is ready, which the authority answers itself: a request_id it has
	// not seen goes to the host, and shows how it names r1 there.
	_, _ = computeclient.RunTargetAcquireProgress(ctx, rp.owner, lent, "r7", "")
	if !strings.Contains(string(sent), `r7"`) {
		t.Fatalf("progress of r7 was not relayed: %s", sent)
	}
	sent = json.RawMessage(strings.Replace(string(sent), `r7"`, `r1"`, 1))
	rp.answer = nil
	if out, err := rp.svc.HostCall(ctx, rp.project, "sandbox", protocol.VerbProgress, "", sent); err != nil || strings.Contains(string(out), "QUOTED-SECRET") || !strings.Contains(string(out), `"state"`) {
		t.Errorf("a relayed progress: %s %v, want state without the driver's message", out, err)
	}
	unknown := json.RawMessage(strings.Replace(string(sent), `r1"`, `r6"`, 1))
	if _, err := rp.svc.HostCall(ctx, rp.project, "sandbox", protocol.VerbProgress, "", unknown); code(err) != api.CodeNotFound || strings.Contains(err.Error(), "QUOTED-SECRET") {
		t.Errorf("a relayed progress the driver refuses: %v, want its not_found without its words", err)
	}
	rp.svc.SetLookPath(func(string) (string, error) { return rp.drv, nil })
	_ = os.Remove(filepath.Join(os.Getenv("HOME"), runtarget.PinFile))
	if _, err := computeclient.ReleaseRunTarget(ctx, rp.owner, lent, idOf(h.Handle)); err != nil {
		t.Error(err)
	}
}

// TestAcquireProgressHidesJIT: a driver that puts what its acquire was sent
// in the starting message never shows a jit_config to the progress caller
// (the message is dropped for an acquire that carried one), while an
// acquire without one still shows its message.
func TestAcquireProgressHidesJIT(t *testing.T) {
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
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "progress-quotes-jit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(fix, "gate")
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box",
		Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"acquire_gate": gate, "jit": "1"}}}); err != nil {
		t.Fatal(err)
	}
	const jit = "eyJzZWtyaXQiOiJ6bS1qaXQtcHJvZ3Jlc3MtdmFsdWUifQ"
	for _, c := range []struct {
		rid, jit string
		msg      bool // the driver's message reaches the caller
	}{{"plain", "", true}, {"runner", jit, false}} {
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		var h computeapi.RunTargetHandle
		go func() {
			var err error
			h, err = computeclient.AcquireRunTarget(ctx, owner, "box", computeapi.AcquireRunTargetRequest{RequestID: c.rid, TTLS: 600, JITConfig: c.jit})
			done <- err
		}()
		// Polled while it boots: a message shows once the poll sees the
		// acquire held here (a poll that began before it was held shows
		// none), and a jit_config's never shows.
		var p computeapi.RunTargetAcquireProgress
		shown, booting := false, 0
		for end := time.Now().Add(time.Minute); time.Now().Before(end) && booting < 20 && !(c.msg && shown); time.Sleep(20 * time.Millisecond) {
			if p, err = computeclient.RunTargetAcquireProgress(ctx, owner, "box", c.rid, ""); err != nil || p.Phase != "booting" {
				continue
			}
			booting++
			shown = shown || p.Message != ""
			if b, _ := json.Marshal(p); strings.Contains(string(b), jit) {
				t.Errorf("progress of %s carries the jit_config: %s", c.rid, b)
			}
		}
		if booting == 0 {
			t.Fatalf("%s never booting: %+v %v", c.rid, p, err)
		}
		if shown != c.msg {
			t.Errorf("progress of %s: message shown %v, want %v (last %+v)", c.rid, shown, c.msg, p)
		}
		if err := os.Remove(gate); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%s: %v", c.rid, err)
		}
		if _, err := computeclient.ReleaseRunTarget(ctx, owner, "box", idOf(h.Handle)); err != nil {
			t.Error(err)
		}
	}
}

// TestCancelEndsWhatTheDriverKept: a cancel that answered cancelled never
// leaves a box live, though the driver made it anyway: one that answers the
// handle after its cancel (it could not end it), and one whose acquire was
// cut off at its time limit after it made the box, its retry then refused.
func TestCancelEndsWhatTheDriverKept(t *testing.T) {
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
	drivers := map[string]string{}
	for name, bug := range map[string]string{"keeper": "cancel-keeps-box", "dier": "", "hanger": ""} {
		state := filepath.Join(fix, name, "state")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		drv, err := refdriver.Install(filepath.Join(fix, name), state, bug)
		if err != nil {
			t.Fatal(err)
		}
		drivers[name] = drv
		t.Cleanup(func() { refdriver.KillAll(state) })
	}
	// dier's and hanger's first acquire makes the box and then hangs past
	// the limit; dier's retry is refused, hanger's hangs too; their cancel
	// always stands.
	for name, retry := range map[string]string{
		"dier":   `printf '{"ok":false,"error":{"code":"conflict","message":"cancelled"}}\n'; exit 0`,
		"hanger": "sleep 30",
	} {
		dir := filepath.Join(fix, name)
		script := fmt.Sprintf(`#!/bin/sh
in=$(cat)
case $in in
*'"verb":"acquire"'*)
	if [ -e %[1]q/first ]; then %[3]s; fi
	touch %[1]q/first
	printf %%s "$in" | %[2]q >/dev/null
	touch %[1]q/made
	sleep 30 ;;
*'"verb":"cancel"'*) printf '{"ok":true,"result":{"cancelled":true,"state":"cancelled"}}\n' ;;
*) printf %%s "$in" | exec %[2]q ;;
esac
`, dir, drivers[name], retry)
		wrap := filepath.Join(dir, "zm-run-"+name)
		if err := os.WriteFile(wrap, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		drivers[name] = wrap
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(name string) (string, error) { return drivers[name], nil })
			s.SetAcquireLimit(2 * time.Second)
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(fix, "gate")
	for _, name := range []string{"keeper", "dier", "hanger"} {
		if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: name, Driver: name, Kind: "box",
			Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"acquire_gate": gate}}}); err != nil {
			t.Fatal(err)
		}
	}
	start := func(target, rid string) <-chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := computeclient.AcquireRunTarget(ctx, owner, target, computeapi.AcquireRunTargetRequest{RequestID: rid, TTLS: 600})
			ch <- err
		}()
		return ch
	}
	ended := func(target string) {
		t.Helper()
		if live, err := computeclient.RunTargetHandles(ctx, owner, target, true); err != nil || len(live) != 0 {
			t.Errorf("%s after a cancel that stood: live %+v %v", target, live, err)
		}
		all, err := computeclient.RunTargetHandles(ctx, owner, target, false)
		if err != nil || len(all) != 1 || all[0].State != "released" {
			t.Errorf("%s: handles %+v %v, want the one it made, released", target, all, err)
		}
	}

	// keeper answers the handle after the cancel stood.
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	k := start("keeper", "k1")
	for end := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		p, err := computeclient.RunTargetAcquireProgress(ctx, owner, "keeper", "k1", "")
		if err == nil && p.Phase == "booting" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("k1 never booting: %+v %v", p, err)
		}
	}
	if c, err := computeclient.CancelRunTargetAcquire(ctx, owner, "keeper", "k1", ""); err != nil || !c.Cancelled {
		t.Fatalf("cancel of k1: %+v %v", c, err)
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	if err := <-k; code(err) != api.CodeConflict {
		t.Errorf("k1 after its cancel stood: %v, want conflict", err)
	}
	ended("keeper")

	// dier and hanger made the box, then were cut off; the cancel lands in
	// between; the retry is refused, or cut off too. Each its own request id:
	// one names a single box acquire across the project's bindings.
	for _, name := range []string{"dier", "hanger"} {
		rid := "d1-" + name
		d := start(name, rid)
		for end := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(fix, name, "made")); err == nil {
				break
			}
			if time.Now().After(end) {
				t.Fatalf("%s never made d1", name)
			}
		}
		if c, err := computeclient.CancelRunTargetAcquire(ctx, owner, name, rid, ""); err != nil || !c.Cancelled {
			t.Fatalf("cancel of %s's d1: %+v %v", name, c, err)
		}
		if err := <-d; code(err) != api.CodeConflict {
			t.Errorf("%s's d1 after its cancel stood: %v, want conflict", name, err)
		}
		ended(name)
	}
}
