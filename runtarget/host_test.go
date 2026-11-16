package runtarget_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/runtarget/rt"
	"github.com/sengirunners/potluck/wiring"
)

func TestSplitRemoteName(t *testing.T) {
	for in, want := range map[string]string{"sandbox@mini": "sandbox|mini", "a.b-c@Host-B.local": "a.b-c|Host-B.local"} {
		b, m, ok := runtarget.SplitRemoteName(in)
		if !ok || b+"|"+m != want {
			t.Errorf("%q: %q %q %v", in, b, m, ok)
		}
	}
	for _, in := range []string{"sandbox", "@mini", "sandbox@", "a@b@c", "a/b@m", "a@m/x", "a:b@m", "A@m", "a@-m", "a@m x", "../x@m", "a@.."} {
		if _, _, ok := runtarget.SplitRemoteName(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
	if runtarget.RemoteName("sandbox", "mini") != "sandbox@mini" {
		t.Error("RemoteName")
	}
}

// A machine-level binding is advertised and callable only for the projects it
// is shared with; the file is the owner's, 0600.
func TestHostBindingShare(t *testing.T) {
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
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	var svc *runtarget.Service
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			svc = s
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	orchid, acme := ids.New(), ids.New() // team projects (by id) this host lends to
	owner := client.WithHandler(dm.SocketHandler())
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{orchid, acme}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Host: true, Target: computeapi.RunTarget{
		Name: "noshare", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); err == nil {
		t.Error("a host binding with no share")
	}
	if info, err := os.Stat(filepath.Join(home, runtarget.HostFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	if l, err := computeclient.HostRunTargets(ctx, owner); err != nil || len(l.Targets) != 1 || l.Targets[0].Shared[0] != orchid {
		t.Fatalf("host list: %+v %v", l, err)
	}
	// A token holder lists nothing machine-level.
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Project: "acme", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	ana := client.WithHandler(dm.Handler)
	ana.Token = tok.Secret
	if _, err := computeclient.HostRunTargets(ctx, ana); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("a member lists the host file: %v", err)
	}
	if _, err := computeclient.BindRunTarget(ctx, ana, computeapi.BindRunTargetRequest{Host: true, Share: []string{acme}, Target: computeapi.RunTarget{
		Name: "x", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}}}); !api.IsCode(err, api.CodeForbidden) {
		t.Errorf("a member binds a host box: %v", err)
	}

	args, _ := json.Marshal(map[string]any{"request_id": "r1", "actor": "x", "ttl_s": 60})
	if _, err := svc.HostCall(ctx, ids.New(), "sandbox", "acquire", "", args); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a project it is not shared with: %v", err)
	}
	if _, err := svc.HostCall(ctx, orchid, "missing", "acquire", "", args); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a missing binding: %v", err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "setup", "", nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("setup is not relayed: %v", err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "release", "box:other/x", nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("another binding's handle: %v", err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "list", "", nil); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("list with no request_id: %v", err)
	}
	for _, k := range []string{"github_token_file", "admission_token_file", "x_file"} {
		named, _ := json.Marshal(map[string]any{"request_id": "rf", "actor": "x", "ttl_s": 60, k: "/etc/passwd"})
		if _, err := svc.HostCall(ctx, orchid, "sandbox", "acquire", "", named); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("a relayed acquire naming %s: %v", k, err)
		}
	}
	res, err := svc.HostCall(ctx, orchid, "sandbox", "acquire", "", args)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var ans struct {
		Handle string `json:"handle"`
	}
	if json.Unmarshal(res, &ans) != nil || ans.Handle == "" {
		t.Fatalf("answer: %s", res)
	}
	if l, err := computeclient.HostRunTargetsLive(ctx, owner); err != nil || len(l.Targets) != 1 || !slices.Equal(l.Targets[0].Live, []string{ans.Handle}) || len(l.Targets[0].Stopped) != 0 {
		t.Errorf("live host list: %+v %v", l, err)
	}
	// The same binding shared with acme: acme's authority cannot reach orchid's handle.
	ex, _ := json.Marshal(map[string]any{"argv": []string{"id"}, "timeout_s": 5})
	for verb, a := range map[string]json.RawMessage{"status": nil, "release": nil, "renew": []byte(`{"ttl_s":60}`), "exec": ex} {
		if _, err := svc.HostCall(ctx, acme, "sandbox", verb, ans.Handle, a); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("%s of another project's handle: %v", verb, err)
		}
	}
	sameReq, _ := json.Marshal(map[string]string{"request_id": "r1"})
	if res, err := svc.HostCall(ctx, acme, "sandbox", "list", "", sameReq); err != nil || string(res) != `{"handles":[]}` {
		t.Errorf("another project's list: %s %v", res, err)
	}
	if res, err := svc.HostCall(ctx, orchid, "sandbox", "list", "", sameReq); err != nil || !strings.Contains(string(res), ans.Handle) {
		t.Errorf("own list: %s %v", res, err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "status", ans.Handle, nil); err != nil {
		t.Errorf("own status: %v", err)
	}
	if info, err := os.Stat(filepath.Join(home, "host_handles.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("ledger file: %v %v", info, err)
	}
	// acme's authority asking with the same actor and request_id gets its
	// own box (the host's key carries the project), and neither sees the
	// other's (zm-eakh.12.5.10.4).
	res2, err := svc.HostCall(ctx, acme, "sandbox", "acquire", "", args)
	var ans2 struct {
		Handle string `json:"handle"`
	}
	if err != nil || json.Unmarshal(res2, &ans2) != nil || ans2.Handle == "" || ans2.Handle == ans.Handle {
		t.Fatalf("acme's acquire with orchid's key: %s %v (orchid's %s)", res2, err, ans.Handle)
	}
	for proj, h := range map[string]string{acme: ans2.Handle, orchid: ans.Handle} {
		res, err := svc.HostCall(ctx, proj, "sandbox", "list", "", sameReq)
		if err != nil || strings.Count(string(res), `"handle"`) != 1 || !strings.Contains(string(res), h) || !strings.Contains(string(res), `"request_id":"r1"`) {
			t.Errorf("%s's list for r1: %s %v", proj, res, err)
		}
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "release", ans2.Handle, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("orchid ends acme's box: %v", err)
	}
	if _, err := svc.HostCall(ctx, acme, "sandbox", "exec", ans2.Handle, ex); err != nil {
		t.Errorf("acme's exec in its own box: %v", err)
	}
	// A handle the ledger has for another project (one made before keys
	// carried it): a conflict, and the box is not ended.
	ledger := filepath.Join(home, "host_handles.json")
	orig, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var led map[string]any
	if err := json.Unmarshal(orig, &led); err != nil {
		t.Fatal(err)
	}
	led["handles"].(map[string]any)[ans.Handle].(map[string]any)["project"] = acme
	b, _ := json.Marshal(led)
	if err := os.WriteFile(ledger, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "acquire", "", args); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("an acquire answered with another project's box: %v", err)
	}
	if st, err := svc.HostCall(ctx, acme, "sandbox", "status", ans.Handle, nil); err != nil || !strings.Contains(string(st), `"ready"`) {
		t.Errorf("the box after the conflict: %s %v", st, err)
	}
	// orchid's list never shows (or adopts) a box the ledger holds for acme,
	// though its key is orchid's.
	if res, err := svc.HostCall(ctx, orchid, "sandbox", "list", "", sameReq); err != nil || strings.Contains(string(res), ans.Handle) {
		t.Errorf("orchid's list of a box the ledger holds for acme: %s %v", res, err)
	}
	// An unreadable ledger fails closed: nothing made, nothing answered, the
	// file left as it is.
	if err := os.WriteFile(ledger, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh, _ := json.Marshal(map[string]any{"request_id": "r9", "actor": "x", "ttl_s": 60})
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "acquire", "", fresh); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("acquire with an unreadable ledger: %v", err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "status", ans.Handle, nil); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("status with an unreadable ledger: %v", err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "list", "", sameReq); !api.IsCode(err, api.CodeUnavailable) {
		t.Errorf("list with an unreadable ledger: %v", err)
	}
	if b, _ := os.ReadFile(ledger); string(b) != "{not json" {
		t.Errorf("the unreadable ledger was rewritten: %s", b)
	}
	if err := os.WriteFile(ledger, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.HostCall(ctx, acme, "sandbox", "release", ans2.Handle, nil); err != nil {
		t.Errorf("acme's release: %v", err)
	}
	la, _ := json.Marshal(map[string]string{"request_id": "nobody"})
	if res, err := svc.HostCall(ctx, orchid, "sandbox", "list", "", la); err != nil || string(res) != `{"handles":[]}` {
		t.Errorf("list for another request: %s %v", res, err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "release", ans.Handle, nil); err != nil {
		t.Errorf("release: %v", err)
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Host: true, Target: computeapi.RunTarget{Name: "sandbox"}}); err == nil {
		t.Error("a rebind without replace")
	}
	if err := computeclient.UnbindHostRunTarget(ctx, owner, "sandbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.HostCall(ctx, orchid, "sandbox", "describe", "", nil); !api.IsCode(err, api.CodeBadRequest) && !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("after unbind: %v", err)
	}
}

// relayPair is one daemon that is the authority of project acme and, behind
// a fake relay, the host "lender" sharing the box binding sandbox with it.
// answer, when set, stands in for what the host's side of the relay
// answers a status, progress or cancel (nil, nil: as the host really does).
type relayPair struct {
	svc     *runtarget.Service
	owner   *client.Client
	state   string // the host driver's records
	drv     string // the host's driver
	project string // the project the host lends to
	answer  func(ctx context.Context, op computeapi.RunTargetOp) (json.RawMessage, error)
}

const lent = "sandbox@lender"

func newRelayPair(t *testing.T) *relayPair { return newRelayPairWrapped(t, nil) }

// newRelayPairWrapped is newRelayPair whose daemon runs wrap(driver) in place
// of the host's driver (from the start, so the pin is the wrapper's).
func newRelayPairWrapped(t *testing.T, wrap func(drv string) string) *relayPair {
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
	rp := &relayPair{state: filepath.Join(fix, "state")}
	drv, err := refdriver.Install(fix, rp.state, "")
	if err != nil {
		t.Fatal(err)
	}
	rp.drv = drv
	t.Cleanup(func() { refdriver.KillAll(rp.state) })
	run := drv
	if wrap != nil {
		run = wrap(drv)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			rp.svc = s
			s.SetLookPath(func(string) (string, error) { return run, nil })
		}
	}
	ctx := context.Background()
	rp.owner = client.WithHandler(dm.SocketHandler())
	p, err := rp.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, rp.owner, computeapi.BindRunTargetRequest{Host: true, Share: []string{p.ID}, Target: computeapi.RunTarget{
		Name: "sandbox", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{refPin}, Config: map[string]string{"image_name": "xcode"}}}); err != nil {
		t.Fatal(err)
	}
	rp.owner.Project, rp.owner.ProjectFrom = "acme", "flag"
	var ads []computeapi.RunTargetAdvert // the describe behind it is asked in the background
	for by := time.Now().Add(20 * time.Second); len(ads) != 1; time.Sleep(20 * time.Millisecond) {
		if ads = rp.svc.HostAdverts(ctx, p.ID); time.Now().After(by) {
			t.Fatalf("adverts: %+v", ads)
		}
	}
	if ads[0].Describe.ImageName != "xcode" {
		t.Fatalf("host did not stamp image name: %+v", ads)
	}
	rp.project = p.ID
	rp.svc.FakeHost(p.ID, "lender", ads, func(ctx context.Context, op computeapi.RunTargetOp) (json.RawMessage, error) {
		if rp.answer != nil && (op.Verb == "status" || op.Verb == protocol.VerbProgress || op.Verb == protocol.VerbCancel) {
			if res, err := rp.answer(ctx, op); res != nil || err != nil {
				return res, err
			}
		}
		res, err := rp.svc.HostCall(ctx, p.ID, op.Binding, op.Verb, op.ID, op.Args)
		return runtarget.AsRelayed(op.Verb, res, err)
	})
	return rp
}

func (rp *relayPair) acquire(t *testing.T, req string, ttl int) computeapi.RunTargetHandle {
	t.Helper()
	h, err := computeclient.AcquireRunTarget(context.Background(), rp.owner, lent, computeapi.AcquireRunTargetRequest{RequestID: req, TTLS: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (rp *relayPair) status(h computeapi.RunTargetHandle) (string, error) {
	st, err := computeclient.RunTargetStatus(context.Background(), rp.owner, lent, idOf(h.Handle))
	return st.State, err
}

// outOfProject is the host's own relay answering: this daemon joined no team
// project, so it says what a host out of the project says.
func (rp *relayPair) outOfProject(ctx context.Context, op computeapi.RunTargetOp) (json.RawMessage, error) {
	return rp.svc.RunOpHere(ctx, "no-such-project", op)
}

// Only the host saying it has no such handle makes a box lost
// (zm-eakh.12.53 item 9). A host that answers not_found for another reason
// ("this machine is not in that project": it dropped out of the pool, or is
// mid restart) has said nothing about the box: its status is an error, the
// box's record stays as it was, and it is live again when the host is back.
func TestOnlyAMissingHandleIsALostBox(t *testing.T) {
	rp := newRelayPair(t)
	h := rp.acquire(t, "r1", 300)
	if st, err := rp.status(h); err != nil || st != "ready" {
		t.Fatalf("status: %q %v", st, err)
	}
	rp.answer = rp.outOfProject
	if st, err := rp.status(h); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "not in that project") {
		t.Errorf("status while the host is out of the project: %q %v; want unavailable, saying why", st, err)
	}
	rp.answer = nil
	if st, err := rp.status(h); err != nil || st != "ready" {
		t.Fatalf("status once the host is back: %q %v; want ready", st, err)
	}
	// The host's driver has no such handle: the box is lost.
	recs, _ := filepath.Glob(filepath.Join(rp.state, "h-*.json"))
	if len(recs) != 1 {
		t.Fatalf("the driver's records: %v", recs)
	}
	refdriver.KillAll(rp.state)
	if err := os.Remove(recs[0]); err != nil {
		t.Fatal(err)
	}
	// What the host reports for it: still not_found on the wire, so an
	// authority on a build without the marker does what it always did.
	_, herr := rp.svc.HostCall(context.Background(), rp.project, "sandbox", "status", strings.Replace(h.Handle, "@lender", "", 1), nil)
	if old := runtarget.AsRelayedToOld("status", nil, herr); !api.IsCode(old, api.CodeNotFound) {
		t.Errorf("a missing handle as an authority without the marker reads it: %v; want not_found", old)
	}
	if _, marked := runtarget.AsRelayed("status", nil, herr); !api.IsCode(marked, api.CodeNotFound) || !rt.IsNoHandle(marked) {
		t.Errorf("a missing handle as this build's authority reads it: %v; want a not_found that is rt.IsNoHandle", marked)
	}
	if _, other := rp.outOfProject(context.Background(), computeapi.RunTargetOp{Binding: "sandbox", Verb: "status", ID: h.Handle}); !api.IsCode(other, api.CodeNotFound) || rt.IsNoHandle(other) {
		t.Errorf("a host out of the project: %v; want a not_found that is not rt.IsNoHandle", other)
	}
	if st, err := rp.status(h); err != nil || st != "lost" {
		t.Errorf("status of a handle its host no longer has: %q %v; want lost", st, err)
	}
	// Ended, its record stands: the host is not asked again, so a host that
	// is out of the project then does not turn its status into an error.
	rp.answer = rp.outOfProject
	if st, err := rp.status(h); err != nil || st != "lost" {
		t.Errorf("status of an ended handle while its host is out of the project: %q %v; want the row (lost)", st, err)
	}
}

// A host on a build without the marker answers a plain not_found for a handle
// it no longer has. That is not taken as the box being gone (it is what a
// host out of the project answers too), so the backstop is the sweep: 15
// minutes past the lease it records the box lost, and from then on its status
// answers that row (the host is not asked about an ended handle), which is
// what the runners broker's Watch acts on.
func TestSweepMarksLostWhatAnOldHostCannotSay(t *testing.T) {
	rp := newRelayPair(t)
	t.Cleanup(func() { rp.svc.SetNow(time.Now) })
	h := rp.acquire(t, "r1", 60)
	rp.answer = func(context.Context, computeapi.RunTargetOp) (json.RawMessage, error) {
		return runtarget.AsRelayedFromOld(api.NotFound("handle", h.Handle))
	}
	if st, err := rp.status(h); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("status of a handle an old host no longer has: %q %v; want unavailable", st, err)
	}
	rp.svc.SetNow(func() time.Time { return time.Now().Add(20 * time.Minute) })
	rp.svc.Sweep(context.Background())
	if st, err := rp.status(h); err != nil || st != "lost" {
		t.Errorf("status after the sweep, 15 minutes past the lease: %q %v; want lost", st, err)
	}
}

// What a remote host advertises as runner refresh results is data it wrote: a
// result with a state this build does not define, no time, or a reason outside
// the closed set is dropped from the binding's describe, as a describe with
// the wrong kind or no size is refused; the good ones stay, one per image.
func TestRemoteDescribeDropsBadRunnerResults(t *testing.T) {
	rp := newRelayPair(t)
	ads := rp.svc.HostAdverts(context.Background(), rp.project)
	if len(ads) != 1 {
		t.Fatalf("adverts: %+v", ads)
	}
	at := time.Now().UTC()
	good := protocol.RunnerStatus{Image: "a@sha256:1", Version: "2.338.2", Refresh: protocol.RunnerNotNeeded, At: at}
	failed := protocol.RunnerStatus{Image: "b@sha256:2", ImageVersion: "2.332.0", Refresh: protocol.RunnerFailed, Why: protocol.WhyDownloadFailed, At: at}
	ads[0].Describe.Runners = []protocol.RunnerStatus{
		good,
		{Image: "c@sha256:3", Version: "2.338.2", Refresh: "great", At: at},
		{Image: "d@sha256:4", Version: "2.338.2", Refresh: protocol.RunnerNotNeeded},
		{Image: "e@sha256:5", Refresh: protocol.RunnerFailed, Why: "open /Users/x/.potluck/cache: denied", At: at},
		good, // twice
		failed,
	}
	rp.svc.FakeHost(rp.project, "lender", ads, nil)
	v, err := computeclient.DescribeRunTarget(context.Background(), rp.owner, lent)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Describe.Runners
	if len(got) != 2 || got[0].Image != good.Image || got[1].Image != failed.Image || got[1].Why != protocol.WhyDownloadFailed {
		t.Errorf("describe.runners = %+v, want the two good results", got)
	}
}

// A remote host's host block is data it wrote: one outside its sets is
// dropped from the binding's describe, and a good one is passed through.
func TestRemoteDescribeCleansTheHostBlock(t *testing.T) {
	for _, c := range []struct {
		name string
		host protocol.HostStatus
		kept bool
	}{
		{"good", protocol.HostStatus{Mode: "closed", State: "closed", Until: "2026-11-13T12:00:00Z"}, true},
		{"unknown mode", protocol.HostStatus{Mode: strings.Repeat("x", 5000), State: "closed"}, false},
		{"bad time", protocol.HostStatus{Mode: "open", State: "accepting", Until: "soon\x1b[2J"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rp := newRelayPair(t)
			ads := rp.svc.HostAdverts(context.Background(), rp.project)
			if len(ads) != 1 {
				t.Fatalf("adverts: %+v", ads)
			}
			h := c.host
			ads[0].Describe.Host = &h
			rp.svc.FakeHost(rp.project, "lender", ads, nil)
			v, err := computeclient.DescribeRunTarget(context.Background(), rp.owner, lent)
			if err != nil {
				t.Fatal(err)
			}
			got := v.Describe.Host
			if c.kept != (got != nil) || c.kept && *got != c.host {
				t.Errorf("describe.host = %+v, want kept %v", got, c.kept)
			}
		})
	}
}

// A heartbeat's host block is cleaned where the authority takes it in, so a
// list with describe (which reads the advert, not describeOf) shows no bad one.
func TestHeartbeatAdvertsCleanTheHostBlock(t *testing.T) {
	rp := newRelayPair(t)
	ctx := context.Background()
	p, err := rp.owner.GetProject(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	ads := rp.svc.HostAdverts(ctx, p.ID)
	if len(ads) != 1 {
		t.Fatalf("adverts: %+v", ads)
	}
	ads[0].Describe.Host = &protocol.HostStatus{Mode: strings.Repeat("x", 5000), State: "closed"}
	if err := rp.svc.Beat(ctx, p, "lender2", ads); err != nil {
		t.Fatal(err)
	}
	list, err := computeclient.RunTargets(ctx, rp.owner, true)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, v := range list.Targets {
		if v.Host == "lender2" && v.Describe != nil {
			seen = true
			if v.Describe.Host != nil {
				t.Errorf("list with describe has a bad host block: %+v", v.Describe.Host)
			}
		}
	}
	if !seen {
		t.Fatal("the heartbeat's advert is not listed")
	}
}

// The adverts a host's heartbeat carries are cleaned where the authority takes
// them in, so every reader gets the same: a list with describe, as much as a
// describe. A host whose runner results have an unknown state, a path in the
// reason, a control character in the image or too many entries to list shows
// only the valid ones, and a failure past the cut stays.
func TestHeartbeatAdvertsCleanRunnerResultsForEveryReader(t *testing.T) {
	rp := newRelayPair(t)
	ctx := context.Background()
	p, err := rp.owner.GetProject(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	ads := rp.svc.HostAdverts(ctx, p.ID)
	if len(ads) != 1 {
		t.Fatalf("adverts: %+v", ads)
	}
	at := time.Now().UTC()
	ok := func(i int) protocol.RunnerStatus {
		return protocol.RunnerStatus{Image: fmt.Sprintf("ok%02d@sha256:1", i), Version: "2.338.2", Refresh: protocol.RunnerNotNeeded, At: at}
	}
	var runners []protocol.RunnerStatus
	runners = append(runners,
		protocol.RunnerStatus{Image: "unknown@sha256:2", Version: "2.338.2", Refresh: "great", At: at},
		protocol.RunnerStatus{Image: "path@sha256:3", Refresh: protocol.RunnerFailed, Why: "open /Users/x/.potluck/cache: denied", At: at},
		protocol.RunnerStatus{Image: "esc\x1b[2J@sha256:4", Version: "2.338.2", Refresh: protocol.RunnerNotNeeded, At: at},
		protocol.RunnerStatus{Image: "ver@sha256:5", Version: "2.338.2\x07", Refresh: protocol.RunnerNotNeeded, At: at})
	for i := 0; i < protocol.MaxRunners; i++ {
		runners = append(runners, ok(i))
	}
	late := protocol.RunnerStatus{Image: "late@sha256:6", Refresh: protocol.RunnerFailed, Why: protocol.WhyTimedOut, At: at}
	runners = append(runners, late)
	ads[0].Describe.Runners = runners
	if err := rp.svc.Beat(ctx, p, "lender2", ads); err != nil {
		t.Fatal(err)
	}

	list, err := computeclient.RunTargets(ctx, rp.owner, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []protocol.RunnerStatus
	for _, v := range list.Targets {
		if v.Host == "lender2" && v.Describe != nil {
			got = v.Describe.Runners
		}
	}
	if len(got) != protocol.MaxRunners {
		t.Fatalf("list with describe has %d runner results from the host, want %d: %+v", len(got), protocol.MaxRunners, got)
	}
	for _, r := range got {
		if !r.Valid() {
			t.Errorf("list with describe has an invalid result: %+v", r)
		}
	}
	if got[len(got)-1].Image != late.Image {
		t.Errorf("the failing result past the cut was lost: last is %+v", got[len(got)-1])
	}
}

// A host advertises only the untrusted-capable class (isolation ephemeral,
// credentials false). An advert claiming another class is not believed: the
// authority drops it where it takes the heartbeat in, so no reader sees a
// credentialed or unisolated box on another machine, and the well-formed
// advert beside it stays.
func TestHeartbeatAdvertsDropAnyOtherClass(t *testing.T) {
	rp := newRelayPair(t)
	ctx := context.Background()
	p, err := rp.owner.GetProject(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	ads := rp.svc.HostAdverts(ctx, p.ID)
	if len(ads) != 1 {
		t.Fatalf("adverts: %+v", ads)
	}
	cred, none := ads[0], ads[0]
	cred.Binding, cred.Describe.Credentials = "cred", true
	none.Binding, none.Describe.Isolation = "none", computeapi.RunTargetIsolationNone
	blank := ads[0]
	blank.Binding, blank.Describe.Isolation = "blank", ""
	if err := rp.svc.Beat(ctx, p, "lender2", []computeapi.RunTargetAdvert{cred, none, blank, ads[0]}); err != nil {
		t.Fatal(err)
	}
	list, err := computeclient.RunTargets(ctx, rp.owner, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range list.Targets {
		if v.Host == "lender2" {
			got = append(got, v.HostBinding)
			if v.Credentials || v.Isolation != computeapi.RunTargetIsolationEphemeral {
				t.Errorf("%s is listed as %s, credentials %v", v.Name, v.Isolation, v.Credentials)
			}
		}
	}
	if len(got) != 1 || got[0] != ads[0].Binding {
		t.Fatalf("lender2's listed bindings = %v, want only %s", got, ads[0].Binding)
	}
	for _, n := range []string{"cred", "none", "blank"} {
		if _, err := computeclient.DescribeRunTarget(ctx, rp.owner, runtarget.RemoteName(n, "lender2")); err == nil {
			t.Errorf("%s@lender2 describes, though its advert claims another class", n)
		}
	}
}

// A lending host cannot tell a cancel of a known key from a new one, so it
// bounds every cancel it relays: per actor of the project (the tag its
// request_id carries), under a cap for the project. One actor using up its
// own bound leaves the others'.
func TestHostCancelBudgetIsPerActor(t *testing.T) {
	rp := newRelayPair(t)
	ctx := context.Background()
	cancel := func(tag, rid string) error {
		args, _ := json.Marshal(map[string]string{"request_id": tag + "-" + rid})
		_, err := rp.svc.HostCall(ctx, rp.project, "sandbox", protocol.VerbCancel, "", args)
		return err
	}
	const ana, bo = "aaaaaaaaaa", "bbbbbbbbbb"
	for i := range runtarget.CancelsActorRelayedMax {
		if err := cancel(ana, fmt.Sprintf("r%d", i)); err != nil {
			t.Fatalf("ana's cancel %d: %v", i, err)
		}
	}
	err := cancel(ana, "one-more")
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeRateLimited || e.RetryAfter <= 0 {
		t.Fatalf("ana past her bound: %v, want rate_limited with a retry_after", err)
	}
	if !strings.Contains(err.Error(), "retry in 1440 minutes") { // the message is what a refusal keeps over the relay
		t.Errorf("the refusal does not say when to retry: %v", err)
	}
	if err := cancel(bo, "r0"); err != nil {
		t.Errorf("bo's cancel after ana used up her own bound: %v", err)
	}
}
