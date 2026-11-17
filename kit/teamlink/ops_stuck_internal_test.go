package teamlink

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// opsService is a Service over a temp registry, with project joined (a
// remote in state ok) and the op kind "work" registered (ReportWithin 1 s).
func opsService(t *testing.T, project string, run func(context.Context, string, api.MachineOp) (json.RawMessage, error)) *Service {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, Migrations...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Service{d: &core.Deps{Store: st, Config: &config.Config{Home: home}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		remotes: map[string]*Remote{}}
	s.remotes[project] = &Remote{R: api.Remote{Project: api.ProjectRef{ID: project, Slug: "orchid"}, State: api.RemoteOK}}
	if run == nil {
		run = func(context.Context, string, api.MachineOp) (json.RawMessage, error) { return nil, nil }
	}
	s.RegisterOp("work", OpHandler{Run: run, ReportWithin: time.Second})
	return s
}

func insertRun(t *testing.T, s *Service, id, project, kind, state string, age time.Duration) {
	t.Helper()
	if _, err := s.d.Store.Registry().ExecContext(context.Background(), `INSERT INTO team_ops_run (id, project, kind, state, created) VALUES (?, ?, ?, ?, ?)`,
		id, project, kind, state, store.Millis(time.Now().Add(-age))); err != nil {
		t.Fatal(err)
	}
}

func runRow(t *testing.T, s *Service, id string) (state, report string, final, acked bool) {
	t.Helper()
	err := s.d.Store.Registry().QueryRowContext(context.Background(), `SELECT state, report, final, acked FROM team_ops_run WHERE id = ?`, id).Scan(&state, &report, &final, &acked)
	if err != nil {
		t.Fatalf("run record %s: %v", id, err)
	}
	return
}

// A run record left running by a process that stopped mid-op (this one
// does not run it) is ended once it is older than its kind's ReportWithin
// and leaves every beat's Took; younger ones, ones of a kind with a longer
// limit, other projects' and finished ones stay (zm-eakh.12.5.10.9, .13).
func TestTookOpsEndsStuckRuns(t *testing.T) {
	ctx := context.Background()
	p, other := ids.New(), ids.New()
	s := opsService(t, p, nil)
	s.RegisterOp("slow", OpHandler{Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) { return nil, nil }, ReportWithin: time.Hour})

	insertRun(t, s, "stuck", p, "work", "running", 10*time.Second)
	insertRun(t, s, "stuck-unknown-kind", p, "gone", "running", OpTakeWithin+time.Minute)
	insertRun(t, s, "young", p, "work", "running", 10*time.Millisecond)
	insertRun(t, s, "slow", p, "slow", "running", 10*time.Second)
	insertRun(t, s, "done", p, "work", "done", 10*time.Second)
	insertRun(t, s, "other", other, "work", "running", 10*time.Second)

	got := map[string]bool{}
	for _, id := range s.TookOps(ctx, p) {
		got[id] = true
	}
	for id, want := range map[string]bool{"stuck": false, "stuck-unknown-kind": false, "young": true, "slow": true, "done": true} {
		if got[id] != want {
			t.Errorf("TookOps names %s: %v, want %v (%v)", id, got[id], want, got)
		}
	}
	state, report, final, acked := runRow(t, s, "stuck")
	if state != "done" || report != "" || !final || !acked {
		t.Fatalf("the stuck record: %s %q final %v acked %v", state, report, final, acked)
	}
	if state, _, _, acked := runRow(t, s, "other"); state != "running" || acked {
		t.Fatalf("another project's record was ended: %s acked %v", state, acked)
	}
	// It is kept (acked), so an answer that carries the op again does not
	// run it a second time.
	ran := 0
	s.ops.by["work"] = OpHandler{ReportWithin: time.Second, Run: func(context.Context, string, api.MachineOp) (json.RawMessage, error) { ran++; return nil, nil }}
	s.TakeOps(ctx, p, []api.MachineOp{{ID: "stuck", Kind: "work"}})
	s.WaitOps()
	if ran != 0 {
		t.Fatal("an op whose stuck record was ended ran again")
	}
	// Nothing is sent for it.
	if reps, finals := s.OpReports(ctx, p); len(reps) != 0 || len(finals) != 0 {
		t.Fatalf("reports for an ended stuck record: %+v %v", reps, finals)
	}
	// The record leaves with the rest once it is older than OpsKeep.
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_ops_run SET created = 0 WHERE id = 'stuck'`); err != nil {
		t.Fatal(err)
	}
	s.AckOps(ctx, nil)
	var n int
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT COUNT(*) FROM team_ops_run WHERE id = 'stuck'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an old acked record stays: %d %v", n, err)
	}
}

// An op this process is still running is never ended, however old.
func TestTookOpsKeepsALiveRun(t *testing.T) {
	ctx := context.Background()
	p := ids.New()
	started, release := make(chan struct{}), make(chan struct{})
	s := opsService(t, p, func(context.Context, string, api.MachineOp) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`"ok"`), nil
	})
	s.TakeOps(ctx, p, []api.MachineOp{{ID: "live", Kind: "work"}})
	<-started
	// The answer carried it again (a poll and a beat overlapping): the
	// record exists, the op is still live.
	s.TakeOps(ctx, p, []api.MachineOp{{ID: "live", Kind: "work"}})
	if !s.opLive("live") {
		t.Fatal("a resent op already running cleared its live mark")
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE team_ops_run SET created = ? WHERE id = 'live'`, store.Millis(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if got := s.TookOps(ctx, p); len(got) != 1 || got[0] != "live" {
		t.Fatalf("TookOps with a live op: %v", got)
	}
	close(release)
	s.WaitOps()
	if state, _, final, acked := runRow(t, s, "live"); state != "done" || !final || acked {
		t.Fatalf("the live op's record: %s final %v acked %v", state, final, acked)
	}
	if reps, finals := s.OpReports(ctx, p); len(reps) != 1 || len(finals) != 1 || string(reps[0].Payload) != `"ok"` {
		t.Fatalf("the live op's report: %+v %v", reps, finals)
	}
}

// TookOps names the 200 newest unacked records, not more.
func TestTookOpsKeepsTheNewest(t *testing.T) {
	ctx := context.Background()
	p := ids.New()
	s := opsService(t, p, nil)
	for i := range maxBeatOps + 5 {
		insertRun(t, s, ids.New(), p, "work", "done", time.Duration(maxBeatOps+5-i)*time.Millisecond)
	}
	newest := ids.New()
	insertRun(t, s, newest, p, "work", "done", 0)
	got := s.TookOps(ctx, p)
	if len(got) != maxBeatOps || got[0] != newest {
		t.Fatalf("TookOps: %d ids, first %q (want %d, newest %q)", len(got), got[0], maxBeatOps, newest)
	}
}

// A machine that sends no Took gets an op carried again by each beat's
// answer, until a beat names it (zm-eakh.12.5.10.13).
func TestPendingOpsResentUntilTook(t *testing.T) {
	ctx := context.Background()
	p := ids.New()
	s := opsService(t, p, nil)
	id := ids.New()
	if err := s.QueueOp(ctx, Op{MachineOp: api.MachineOp{ID: id, Kind: "work"}, Project: p, Machine: "laptop", FromMachine: "mini"}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if ops := s.PendingOps(ctx, p, "laptop"); len(ops) != 1 || ops[0].ID != id {
			t.Fatalf("answer %d without a Took: %+v", i, ops)
		}
	}
	s.MarkTook(ctx, p, "laptop", []string{id})
	if ops := s.PendingOps(ctx, p, "laptop"); len(ops) != 0 {
		t.Fatalf("an answer carried a taken op: %+v", ops)
	}
}

// An answer that lands after a leave (its remote is gone or retired)
// records no run row: a forget that came between would otherwise have its
// id's row put back, and its report sent with the next join's beats
// (zm-eakh.12.5.10.28).
func TestTakeOpsSkipsAGoneRemote(t *testing.T) {
	ctx := context.Background()
	p := ids.New()
	ran := make(chan string, 4)
	s := opsService(t, p, func(_ context.Context, _ string, op api.MachineOp) (json.RawMessage, error) {
		ran <- op.ID
		return nil, nil
	})
	count := func() int {
		var n int
		if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT COUNT(*) FROM team_ops_run WHERE project = ?`, p).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Retired (left, the remote still in the map): nothing is recorded or run.
	x := s.remotes[p]
	x.RetireRole()
	s.TakeOps(ctx, p, []api.MachineOp{{ID: "late-1", Kind: "work"}})
	// Gone from the map.
	delete(s.remotes, p)
	s.TakeOps(ctx, p, []api.MachineOp{{ID: "late-2", Kind: "work"}})
	s.WaitOps()
	if n := count(); n != 0 || len(ran) != 0 || s.runningOps(p) != 0 {
		t.Fatalf("after a leave: %d rows, %d ran, %d counted", n, len(ran), s.runningOps(p))
	}
	// Joined again (a new remote): the op is taken.
	s.remotes[p] = &Remote{R: api.Remote{Project: api.ProjectRef{ID: p, Slug: "orchid"}, State: api.RemoteOK}}
	s.TakeOps(ctx, p, []api.MachineOp{{ID: "fresh", Kind: "work"}})
	s.WaitOps()
	if n := count(); n != 1 || len(ran) != 1 {
		t.Fatalf("after a join: %d rows, %d ran", n, len(ran))
	}
}
