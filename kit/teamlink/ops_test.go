package teamlink_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// opsDaemon is a daemon wired with tailnet and teamlink alone (nothing
// above KIT), with two fake op kinds registered: "echo" (answers its
// payload, reported at once) and "quiet" (answers "ok", reported with the
// beats, ended when not taken within 300 ms). runs counts the ops it ran.
func opsDaemon(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, runs *atomic.Int64) (*client.Client, *teamlink.Service) {
	t.Helper()
	return linkDaemon(t, tn, machine, id, func(link *teamlink.Service) {
		link.RegisterOp("echo", teamlink.OpHandler{ReportNow: true, Run: func(_ context.Context, _ string, op api.MachineOp) (json.RawMessage, error) {
			runs.Add(1)
			if string(op.Payload) == `"fail"` {
				return nil, api.Errorf(api.CodeConflict, "asked to fail").WithHint("do not")
			}
			return op.Payload, nil
		}})
		link.RegisterOp("quiet", teamlink.OpHandler{Expiry: 300 * time.Millisecond, Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) {
			runs.Add(1)
			return json.RawMessage(`"ok"`), nil
		}})
	})
}

// linkDaemon is a daemon wired with tailnet and teamlink alone (nothing
// above KIT); setup registers op kinds and beat parts before it starts.
func linkDaemon(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, setup func(*teamlink.Service)) (*client.Client, *teamlink.Service) {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = machine+"-human", machine
	var link *teamlink.Service
	wire := func(d *core.Deps) ([]core.Module, error) {
		net := tailnet.New(d)
		d.Net = net.Net()
		d.AddTransport(tailnet.TeamTransport(d, d.Net))
		link = teamlink.New(d)
		setup(link)
		return []core.Module{net, link}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wire)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(id)
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", machine)
		}
		os.RemoveAll(home)
	})
	c := client.WithHandler(dm.SocketHandler())
	if err := client.NewUnix(cfg.SocketPath()).WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if st, err := c.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
		t.Fatalf("%s: tailnet up: %+v %v", machine, st, err)
	}
	return c, link
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A fake op kind, registered on two KIT-only daemons, goes from the
// authority to a member's machine over the held-open poll and over the
// heartbeat's helpers, runs there once, and its report comes back; an op
// not taken in time expires; an unregistered kind is refused at both ends.
func TestOpQueueFakeKind(t *testing.T) {
	teamlink.BeatsOff.Store(true) // this test plays the heartbeat by hand
	t.Cleanup(func() { teamlink.BeatsOff.Store(false) })
	ctx := context.Background()
	tn := tailnettest.New()
	var aRuns, mRuns atomic.Int64
	a, alink := opsDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, &aRuns)
	m, mlink := opsDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"}, &mRuns)

	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	x := mlink.ByName("orchid")
	if x == nil {
		t.Fatal("no remote after join")
	}
	proj := p.ID
	queue := func(kind, payload string) string {
		t.Helper()
		id := ids.New()
		if err := alink.QueueOp(ctx, teamlink.Op{MachineOp: api.MachineOp{ID: id, Kind: kind, Payload: json.RawMessage(payload)},
			Project: proj, Machine: "laptop", FromMachine: "mini"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	load := func(id string) teamlink.Op {
		t.Helper()
		o, err := alink.LoadOp(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}

	// An unregistered kind is refused at the authority.
	err = alink.QueueOp(ctx, teamlink.Op{MachineOp: api.MachineOp{ID: ids.New(), Kind: "nope"}, Project: p.ID, Machine: "laptop", FromMachine: "mini"})
	if err == nil || !strings.Contains(err.Error(), "unknown op kind nope") {
		t.Fatalf("unregistered kind queued: %v", err)
	}

	// The held-open poll: the member wants ops, the op is answered at once
	// and its report posted at once (no heartbeat runs: BeatsOff).
	lctx, stop := context.WithCancel(ctx)
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); mlink.OpsLoop(lctx, x) }()
	mlink.WantsOps(p.ID, "test", true)
	id := queue("echo", `{"n":1}`)
	waitFor(t, "the polled op's report", func() bool { return load(id).State == teamlink.OpDone })
	if o := load(id); o.Result != `{"n":1}` || o.Error != "" || !o.Sent {
		t.Fatalf("polled op: %+v", o)
	}
	fail := queue("echo", `"fail"`)
	waitFor(t, "the refused op's report", func() bool { return load(fail).State == teamlink.OpDone })
	if o := load(fail); o.Error != "asked to fail" {
		t.Fatalf("refused op: %+v", o)
	}
	if n := mRuns.Load(); n != 2 {
		t.Fatalf("ran %d ops, want 2", n)
	}
	mlink.WantsOps(p.ID, "test", false)
	stop()
	<-loopDone

	// The rest uses a second project: teamlink's own poll for the first may
	// still be held open (it answers an op queued there).
	if _, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "side"); err != nil {
		t.Fatal(err)
	}
	inv2, err := a.Invite(ctx, api.InviteRequest{Project: "side", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	j2, err := m.Join(ctx, api.JoinRequest{Code: inv2.Code})
	if err != nil {
		t.Fatal(err)
	}
	p = api.Project{ID: j2.Remote.Project.ID}
	proj = p.ID

	// The heartbeat's helpers: the answer carries the op, the machine takes
	// it (once), its report rides the next beat, the answer acks it.
	qid := queue("quiet", "")
	ops := alink.PendingOps(ctx, p.ID, "laptop")
	if len(ops) != 1 || ops[0].ID != qid {
		t.Fatalf("pending ops: %+v", ops)
	}
	mlink.TakeOps(ctx, p.ID, ops)
	mlink.TakeOps(ctx, p.ID, ops) // a resent answer: recorded already, not run again
	var reps []api.MachineOpReport
	var finals []string
	waitFor(t, "the beat's report", func() bool {
		reps, finals = mlink.OpReports(ctx, p.ID)
		return len(reps) == 1
	})
	if reps[0].ID != qid || string(reps[0].Payload) != `"ok"` || len(finals) != 1 {
		t.Fatalf("reports %+v finals %v", reps, finals)
	}
	alink.ApplyOpReports(ctx, p.ID, "elsewhere", reps) // not that machine's op: ignored
	if o := load(qid); o.State != teamlink.OpPending {
		t.Fatalf("another machine's report applied: %+v", o)
	}
	alink.ApplyOpReports(ctx, p.ID, "laptop", reps)
	if o := load(qid); o.State != teamlink.OpDone || o.Result != `"ok"` {
		t.Fatalf("applied: %+v", o)
	}
	mlink.AckOps(ctx, finals)
	if reps, _ := mlink.OpReports(ctx, p.ID); len(reps) != 0 {
		t.Fatalf("acked reports sent again: %+v", reps)
	}
	if n := mRuns.Load(); n != 3 {
		t.Fatalf("ran %d ops, want 3", n)
	}

	// Expiry: an op no machine took within its kind's Expiry never goes out
	// and is ended.
	old := queue("quiet", "")
	time.Sleep(400 * time.Millisecond)
	if ops := alink.PendingOps(ctx, p.ID, "laptop"); len(ops) != 0 {
		t.Fatalf("an expired op went out: %+v", ops)
	}
	alink.ExpireOps(ctx)
	if o := load(old); o.State != teamlink.OpDone || o.Error != "not taken in time" {
		t.Fatalf("expired: %+v", o)
	}

	// An unregistered kind that reaches a machine is refused there.
	mlink.TakeOps(ctx, p.ID, []api.MachineOp{{ID: ids.New(), Kind: "nope"}})
	waitFor(t, "the refusal", func() bool {
		reps, _ = mlink.OpReports(ctx, p.ID)
		return len(reps) == 1
	})
	if reps[0].Error != "unknown op kind nope" {
		t.Fatalf("unregistered kind on the machine: %+v", reps[0])
	}
	if aRuns.Load() != 0 {
		t.Fatalf("the authority ran %d ops", aRuns.Load())
	}
	if _, err := alink.LoadOp(ctx, "missing"); !teamlink.IsNoOp(err) {
		t.Fatalf("missing op: %v", err)
	}
}

// Registering a kind twice panics.
func TestRegisterOpTwicePanics(t *testing.T) {
	s := teamlink.New(&core.Deps{})
	h := teamlink.OpHandler{Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) { return nil, nil }}
	s.RegisterOp("k", h)
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	s.RegisterOp("k", h)
}

// An op that ends (or moves to another machine) between the answer reading
// it and marking it sent is neither marked sent nor answered: a waiting
// line that placed a start elsewhere meanwhile, or a cancel, wins, and two
// machines never both get one start.
func TestPendingOpsSkipsOneEndedMeanwhile(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	var link *teamlink.Service
	a, _ := linkDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, func(l *teamlink.Service) {
		link = l
		l.RegisterOp("moving", teamlink.OpHandler{
			Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) { return nil, nil },
			// Args runs between the read and the mark: the op ends here as
			// a line or a cancel would end it.
			Args: func(ctx context.Context, _ string, op *api.MachineOp) error {
				if string(op.Payload) == `"move"` {
					_, _ = link.UpdateOp(ctx, op.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Unsent: true})
				}
				return nil
			},
		})
	})
	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "rota"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "rota"); err != nil {
		t.Fatal(err)
	}
	moved, kept := ids.New(), ids.New()
	for id, payload := range map[string]string{moved: `"move"`, kept: `"stay"`} {
		if err := link.QueueOp(ctx, teamlink.Op{MachineOp: api.MachineOp{ID: id, Kind: "moving", Payload: json.RawMessage(payload)},
			Project: p.ID, Machine: "laptop", FromMachine: "mini"}); err != nil {
			t.Fatal(err)
		}
	}
	ops := link.PendingOps(ctx, p.ID, "laptop")
	if len(ops) != 1 || ops[0].ID != kept {
		t.Fatalf("answered %+v, want only %s", ops, kept)
	}
	if o, err := link.LoadOp(ctx, moved); err != nil || o.Sent || o.State != teamlink.OpDone {
		t.Fatalf("the op ended meanwhile: %+v %v", o, err)
	}
	if o, err := link.LoadOp(ctx, kept); err != nil || !o.Sent {
		t.Fatalf("the op answered: %+v %v", o, err)
	}
}

// An op an earlier answer carried may have run on its machine: when Args
// refuses it later (its machine re-joined another way, say, and can no
// longer be given what the op holds), it is only kept out of the answer,
// never ended as refused, and its caller is not answered (Refused). An op
// no answer carried yet is ended, and then its caller is answered.
func TestPendingOpsNeverRefusesAnOpAlreadySent(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	var link *teamlink.Service
	var mu sync.Mutex // the daemon's own beats may answer too
	refuse := false
	var refused []string
	a, _ := linkDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, func(l *teamlink.Service) {
		link = l
		l.RegisterOp("guarded", teamlink.OpHandler{
			Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) { return nil, nil },
			Args: func(context.Context, string, *api.MachineOp) error {
				mu.Lock()
				defer mu.Unlock()
				if refuse {
					return &api.Error{Code: api.CodeForbidden, Message: "cannot be given it now"}
				}
				return nil
			},
			Refused: func(ctx context.Context, _ string, op api.MachineOp, err error) {
				// Only after the op ended with err.
				o, lerr := link.LoadOp(ctx, op.ID)
				if lerr != nil || o.State != teamlink.OpDone || o.Error != "cannot be given it now" {
					t.Errorf("Refused before the op ended: %+v %v", o, lerr)
				}
				if e, ok := err.(*api.Error); !ok || e.Code != api.CodeForbidden {
					t.Errorf("Refused was told %v, not Args' error", err)
				}
				mu.Lock()
				refused = append(refused, op.ID)
				mu.Unlock()
			},
		})
	})
	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "rota"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "rota"); err != nil {
		t.Fatal(err)
	}
	queue := func() string {
		id := ids.New()
		if err := link.QueueOp(ctx, teamlink.Op{MachineOp: api.MachineOp{ID: id, Kind: "guarded"}, Project: p.ID, Machine: "laptop", FromMachine: "mini"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	carried := queue()
	if ops := link.PendingOps(ctx, p.ID, "laptop"); len(ops) != 1 || ops[0].ID != carried {
		t.Fatalf("first answer: %+v", ops)
	}
	mu.Lock()
	refuse = true
	mu.Unlock()
	fresh := queue()
	if ops := link.PendingOps(ctx, p.ID, "laptop"); len(ops) != 0 {
		t.Fatalf("refused ops answered: %+v", ops)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(refused) != 1 || refused[0] != fresh {
		t.Fatalf("callers answered a refusal: %v, want only %s", refused, fresh)
	}
	if o, err := link.LoadOp(ctx, carried); err != nil || o.State != teamlink.OpPending || !o.Sent || o.Error != "" {
		t.Fatalf("the op already sent: %+v %v", o, err)
	}
	if o, err := link.LoadOp(ctx, fresh); err != nil || o.State != teamlink.OpDone || o.Error != "cannot be given it now" {
		t.Fatalf("the op not sent yet: %+v %v", o, err)
	}
}

// An answer overlapping this one (a heartbeat and an ops poll) may carry an
// op between this one reading it unsent and Args refusing it: the refusal
// then does not end it, and its caller is not answered a refusal though its
// machine may run it. The overlap runs inside Args, so it always lands in
// that window.
func TestPendingOpsRefusalSparesAnOpCarriedMeanwhile(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	var link *teamlink.Service
	nested := false
	var refused []string
	a, _ := linkDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, func(l *teamlink.Service) {
		link = l
		l.RegisterOp("overlap", teamlink.OpHandler{
			Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) { return nil, nil },
			Args: func(ctx context.Context, project string, _ *api.MachineOp) error {
				if nested {
					return nil // the overlapping answer carries it
				}
				nested = true
				if ops := link.PendingOps(ctx, project, "laptop"); len(ops) != 1 {
					t.Errorf("the overlapping answer carried %+v", ops)
				}
				return errors.New("cannot be given it now")
			},
			Refused: func(_ context.Context, _ string, op api.MachineOp, _ error) { refused = append(refused, op.ID) },
		})
	})
	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "rota"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "rota"); err != nil {
		t.Fatal(err)
	}
	id := ids.New()
	if err := link.QueueOp(ctx, teamlink.Op{MachineOp: api.MachineOp{ID: id, Kind: "overlap"}, Project: p.ID, Machine: "laptop", FromMachine: "mini"}); err != nil {
		t.Fatal(err)
	}
	if ops := link.PendingOps(ctx, p.ID, "laptop"); len(ops) != 0 {
		t.Fatalf("the refusing answer carried %+v", ops)
	}
	if o, err := link.LoadOp(ctx, id); err != nil || o.State != teamlink.OpPending || !o.Sent || o.Error != "" {
		t.Fatalf("the op the overlapping answer carried: %+v %v", o, err)
	}
	if len(refused) != 0 {
		t.Fatalf("the caller of the op the overlapping answer carried was answered a refusal: %v", refused)
	}
}

// An op whose payload holds stdin and a secret (filled in by Args as each
// answer carries it, as the run-target relay does) crosses the wire once
// while it runs for many beats: each beat names the ops its machine has
// (Took) and no answer carries them again. An answer that never reached
// the machine is made good by the next beat, so no op is lost; a Took
// naming another machine's op changes nothing.
func TestTakenOpNotSentAgain(t *testing.T) {
	teamlink.BeatsOff.Store(true) // beats by hand
	teamlink.OpsPollOff.Store(true)
	t.Cleanup(func() { teamlink.BeatsOff.Store(false); teamlink.OpsPollOff.Store(false) })
	ctx := context.Background()
	tn := tailnettest.New()
	const stdin, secret = "STDIN-0123456789", "SECRET-9876543210"
	var (
		mu      sync.Mutex
		carried = map[string]int{} // answers that carried each op's payload
		got     = map[string][]string{}
		started = make(chan string, 4)
	)
	release := make(chan struct{})
	setup := func(link *teamlink.Service) {
		link.RegisterOp("exec", teamlink.OpHandler{
			ReportWithin: time.Hour,
			Args: func(_ context.Context, _ string, op *api.MachineOp) error {
				mu.Lock()
				carried[op.ID]++
				mu.Unlock()
				op.Payload, _ = json.Marshal(map[string]string{"stdin": stdin, "secret": secret})
				return nil
			},
			Run: func(_ context.Context, _ string, op api.MachineOp) (json.RawMessage, error) {
				mu.Lock()
				got[op.ID] = append(got[op.ID], string(op.Payload))
				mu.Unlock()
				started <- op.ID
				<-release // a long exec: many beats go by
				return json.RawMessage(`"ok"`), nil
			},
		})
	}
	a, alink := linkDaemon(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "judy@example.com", Host: "mini"}, setup)
	m, mlink := linkDaemon(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "ana@example.com", Host: "laptop"}, setup)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock) // before the daemons stop: they wait for running ops

	p, err := a.CreateProject(ctx, api.NewProjectRequest{Slug: "orchid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ProjectTeam(ctx, "orchid"); err != nil {
		t.Fatal(err)
	}
	inv, err := a.Invite(ctx, api.InviteRequest{Project: "orchid", As: "ana", Role: api.RoleMember})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Join(ctx, api.JoinRequest{Code: inv.Code}); err != nil {
		t.Fatal(err)
	}
	x := mlink.ByName("orchid")
	if x == nil {
		t.Fatal("no remote after join")
	}
	queue := func() string {
		t.Helper()
		id := ids.New()
		if err := alink.QueueOp(ctx, teamlink.Op{MachineOp: api.MachineOp{ID: id, Kind: "exec"}, Project: p.ID, Machine: "laptop", FromMachine: "mini"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	count := func(id string) (int, []string) {
		mu.Lock()
		defer mu.Unlock()
		return carried[id], append([]string(nil), got[id]...)
	}
	want := `{"secret":"` + secret + `","stdin":"` + stdin + `"}`
	run := func(id string, wantCarried int) {
		t.Helper()
		mlink.Heartbeat(ctx) // its answer carries the op; the machine takes it
		select {
		case s := <-started:
			if s != id {
				t.Fatalf("started %s, want %s", s, id)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the op did not start")
		}
		for range 10 { // the exec runs on: every beat names it in Took
			mlink.Heartbeat(ctx)
		}
		if n, runs := count(id); n != wantCarried || len(runs) != 1 || runs[0] != want {
			t.Fatalf("op %s: carried by %d answers (want %d), ran with %q", id, n, wantCarried, runs)
		}
		// A beat names it, as the next real beat would: the wire answer
		// holds no op and none of its payload.
		var resp json.RawMessage
		if err := mlink.CallRemote(ctx, x, http.MethodPost, "/v0/team/presence", api.PresenceRequest{Project: p.ID, Took: mlink.TookOps(ctx, p.ID)}, &resp); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(resp), id) || strings.Contains(string(resp), secret) || strings.Contains(string(resp), stdin) {
			t.Fatalf("an answer carried the taken op again: %s", resp)
		}
	}

	// The first answer reaches the machine: the payload crosses once.
	first := queue()
	run(first, 1)

	// The first answer is lost on the way (read at the authority, never
	// taken): the op is not lost, the next beat brings it, and then no
	// answer carries it again. A Took from another machine moves nothing.
	lost := queue()
	if ops := alink.PendingOps(ctx, p.ID, "laptop"); len(ops) != 1 || ops[0].ID != lost {
		t.Fatalf("pending: %+v", ops)
	}
	alink.MarkTook(ctx, p.ID, "elsewhere", []string{lost})
	alink.MarkTook(ctx, ids.New(), "laptop", []string{lost}) // another project's
	run(lost, 2)

	// The exec ends: its report rides the beats and both ops finish.
	unblock()
	waitFor(t, "both reports", func() bool {
		mlink.Heartbeat(ctx)
		a, _ := alink.LoadOp(ctx, first)
		b, _ := alink.LoadOp(ctx, lost)
		return a.State == teamlink.OpDone && b.State == teamlink.OpDone
	})
	for _, id := range []string{first, lost} {
		if o, _ := alink.LoadOp(ctx, id); o.Result != `"ok"` || o.Error != "" {
			t.Fatalf("op %s: %+v", id, o)
		}
	}
	if n, _ := count(first); n != 1 {
		t.Fatalf("the first op was carried by %d answers", n)
	}

	// Took is bounded like the reports.
	took := make([]string, 201)
	for i := range took {
		took[i] = ids.New()
	}
	if err := mlink.CallRemote(ctx, x, http.MethodPost, "/v0/team/presence", api.PresenceRequest{Project: p.ID, Took: took}, nil); !api.IsCode(err, api.CodeTooLarge) {
		t.Fatalf("201 taken ids posted: %v", err)
	}
}
