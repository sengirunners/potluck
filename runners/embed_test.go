package runners_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/runtarget/rt"
)

// gatekeeper is an embedding program's box rule: it records every box
// request run targets ask it about, and refuses those of a held job.
type gatekeeper struct {
	mu   sync.Mutex
	asks []boxAsk
	hold map[int64]string // job id: why its box is refused
	// flip: job ids whose every second ask is refused: the broker's check
	// before the mint passes, the acquire's own ask is refused.
	flip map[int64]bool
}

type boxAsk struct {
	area string
	req  rt.BoxRequest
	live []computeapi.RunTargetHandle
}

func (g *gatekeeper) check(_ context.Context, _, area string, req rt.BoxRequest, live func() ([]computeapi.RunTargetHandle, error)) error {
	hs, err := live()
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asks = append(g.asks, boxAsk{area: area, req: req, live: hs})
	if req.Job != nil && g.hold[req.Job.ID] != "" {
		return api.Errorf(api.CodeForbidden, "%s", g.hold[req.Job.ID])
	}
	if req.Job != nil && g.flip[req.Job.ID] {
		n := 0
		for _, a := range g.asks {
			if a.req.Job != nil && a.req.Job.ID == req.Job.ID {
				n++
			}
		}
		if n%2 == 0 {
			return api.Errorf(api.CodeForbidden, "refused when the box was acquired")
		}
	}
	return nil
}

func (g *gatekeeper) setHold(job int64, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if why == "" {
		delete(g.hold, job)
	} else {
		g.hold[job] = why
	}
}

// asksFor are the requests asked about job's box.
func (g *gatekeeper) asksFor(job int64) []boxAsk {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []boxAsk
	for _, a := range g.asks {
		if a.req.Job != nil && a.req.Job.ID == job {
			out = append(out, a)
		}
	}
	return out
}

// embedder is a program built on Potluck: wiring.Compute plus a module of
// its own that holds the broker and subscribes to run targets' box requests,
// as its code compiled into the daemon would.
func embedder(t *testing.T, before func(*runners.Service)) (*fixture, *program, *gatekeeper) {
	t.Helper()
	prog, gate := &program{}, &gatekeeper{hold: map[int64]string{}, flip: map[int64]bool{}}
	wire := func(d *core.Deps) ([]core.Module, error) {
		mods, err := programWiring(prog)(d)
		if svc := rt.From(d); svc != nil {
			svc.OnBoxRequest(gate.check)
		}
		return mods, err
	}
	f := newPoolFixtureWired(t, func(r *config.Runners) {
		r.Label, r.Pool.Sizes, r.Pool.DefaultSize = "sengi", map[string]string{"small": "small"}, "small"
	}, before, wire, box("small", "small", "linux", "1"))
	if prog.broker == nil {
		t.Fatal("the program's wiring did not find the broker")
	}
	if _, err := prog.broker.LinkVerified(context.Background(), inst, "", "program:supper"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.allow("acme/app", false); err != nil {
		t.Fatal(err)
	}
	return f, prog, gate
}

// liveBoxes are the binding's ready boxes.
func liveBoxes(t *testing.T, f *fixture) []computeapi.RunTargetHandle {
	t.Helper()
	hs, err := computeclient.RunTargetHandles(context.Background(), f.owner, "small", false)
	if err != nil {
		t.Fatal(err)
	}
	var out []computeapi.RunTargetHandle
	for _, h := range hs {
		if h.State == "ready" {
			out = append(out, h)
		}
	}
	return out
}

func completedEvent(job, run int64, repo, runner, conclusion, completedAt string) []byte {
	b, _ := json.Marshal(map[string]any{"action": "completed", "installation": map[string]any{"id": inst},
		"repository": map[string]any{"id": repoID(repo), "full_name": repo, "private": true},
		"workflow_job": map[string]any{"id": job, "run_id": run, "labels": []string{"self-hosted", "sengi"}, "runner_name": runner,
			"conclusion": conclusion, "completed_at": completedAt}})
	return b
}

// (a) An embedding program reads a job in-process: its id, repository,
// installation, labels, the handle of the box it runs on, and GitHub's start
// and end.
func TestProgramReadsAJob(t *testing.T) {
	f, prog, _ := embedder(t, nil)
	ctx := context.Background()
	if _, err := prog.broker.Job(ctx, 6001); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a job never delivered: %v, want not_found", err)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 6001, 90, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the job's box", func() bool { return len(liveBoxes(t, f)) == 1 })
	h := liveBoxes(t, f)[0]
	var j runners.Job
	eventually(t, "the job's handle", func() bool {
		var err error
		j, err = prog.broker.Job(ctx, 6001)
		return err == nil && j.Handle != ""
	})
	if j.ID != 6001 || j.Run != 90 || j.Repository != "acme/app" || j.Installation != inst || j.Area != "" ||
		!slices.Equal(j.Labels, []string{"self-hosted", "sengi"}) || j.State != "serving" || j.Target != "small" || j.Handle != h.Handle {
		t.Fatalf("job %+v, want its box %s", j, h.Handle)
	}
	if !j.Started.IsZero() || !j.Ended.IsZero() {
		t.Errorf("a job not started yet has start %v and end %v", j.Started, j.Ended)
	}

	f.gh.mu.Lock()
	f.gh.jobStarts[6001] = "2026-11-23T10:00:00Z"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", startEvent(6001, 90, "acme/app", "potluck-6001"))
	eventually(t, "GitHub's start", func() bool {
		j, _ = prog.broker.Job(ctx, 6001)
		return !j.Started.IsZero()
	})
	if want := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC); !j.Started.Equal(want) || j.Handle != h.Handle {
		t.Errorf("started job %+v, want start %v on %s", j, want, h.Handle)
	}

	f.deliver(t, "workflow_job", completedEvent(6001, 90, "acme/app", "potluck-6001", "success", "2026-11-23T10:07:30Z"))
	eventually(t, "GitHub's end", func() bool {
		j, _ = prog.broker.Job(ctx, 6001)
		return j.State == "done" && !j.Ended.IsZero()
	})
	if want := time.Date(2026, 11, 23, 10, 7, 30, 0, time.UTC); !j.Ended.Equal(want) || j.Handle != "" {
		t.Errorf("ended job %+v, want end %v and no box", j, want)
	}
	// A job whose completed never came: the sweep finds it ended on GitHub,
	// and the end is when the broker saw it.
	f.deliver(t, "workflow_job", jobEvent("queued", 6002, 91, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the second job's box", func() bool { return len(liveBoxes(t, f)) == 1 })
	f.started(t, 6002, 91, "acme/app", "potluck-6002")
	eventually(t, "the second job running", func() bool { j, _ := prog.broker.Job(ctx, 6002); return !j.Started.IsZero() })
	f.gh.mu.Lock()
	f.gh.status[6002], f.gh.conclusion[6002] = "completed", "success"
	f.gh.mu.Unlock()
	f.svc.Sweep(ctx)
	if j2, err := prog.broker.Job(ctx, 6002); err != nil || j2.State != "done" || j2.Ended.IsZero() {
		t.Errorf("a job the sweep found ended: %+v %v, want done with an end", j2, err)
	}
	all, err := prog.broker.Jobs(ctx, inst, 10)
	byID := map[int64]runners.Job{}
	for _, a := range all {
		byID[a.ID] = a
	}
	if err != nil || len(all) != 2 || !byID[6001].Ended.Equal(j.Ended) || byID[6002].Ended.IsZero() {
		t.Errorf("the installation's jobs: %+v %v", all, err)
	}
	if none, err := prog.broker.Jobs(ctx, 99, 10); err != nil || len(none) != 0 {
		t.Errorf("another installation's jobs: %+v %v", none, err)
	}
}

// (b) A program's acquire hook gets the request: the binding, its size and,
// for the broker's acquire, the job; the live boxes it reads carry their
// size. OnBoxAcquire subscribers (the usage module's) are still asked.
func TestProgramHookSeesTheRequest(t *testing.T) {
	f, _, gate := embedder(t, nil)
	f.deliver(t, "workflow_job", jobEvent("queued", 6101, 90, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the first box", func() bool { return len(liveBoxes(t, f)) == 1 })
	asks := gate.asksFor(6101)
	if len(asks) == 0 {
		t.Fatal("the program was not asked about job 6101's box")
	}
	for _, a := range asks {
		job := a.req.Job
		if a.area != "" || a.req.Target != "small" || a.req.Start || a.req.Size == nil || a.req.Size.Name != "small" || a.req.Size.CPU != 1 ||
			job.Repository != "acme/app" || job.Installation != inst || !slices.Equal(job.Labels, []string{"self-hosted", "sengi"}) {
			t.Errorf("request %+v (job %+v, size %+v)", a.req, job, a.req.Size)
		}
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 6102, 91, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the second box", func() bool { return len(liveBoxes(t, f)) == 2 })
	asks = gate.asksFor(6102)
	if len(asks) == 0 {
		t.Fatal("the program was not asked about job 6102's box")
	}
	last := asks[len(asks)-1]
	if len(last.live) != 1 || last.live[0].Size != "small" {
		t.Errorf("the live boxes the second request saw: %+v, want the first box with its size", last.live)
	}

	// An acquire that is not the broker's names no job, however it is asked.
	ctx := context.Background()
	if _, err := computeclient.AcquireRunTarget(ctx, f.owner, "small", computeapi.AcquireRunTargetRequest{RequestID: "mine-1", TTLS: 600}); err != nil {
		t.Fatal(err)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	mine := gate.asks[len(gate.asks)-1]
	if mine.req.Job != nil || mine.req.Target != "small" || mine.req.Size == nil {
		t.Errorf("an owner's acquire: %+v", mine.req)
	}
}

// (c) A job whose box the program refuses stays queued with the reason on its
// row and is asked about again, on the waiting serve's backoff: no runner is
// minted for it, it is not failed, and the binding is not held against other
// jobs. Once the program lets it go, it is served.
func TestProgramHoldsAJob(t *testing.T) {
	f, prog, gate := embedder(t, func(s *runners.Service) { s.RefuseCool = time.Hour })
	ctx := context.Background()
	gate.setHold(6201, "the account is at its 2 vCPU limit")
	f.deliver(t, "workflow_job", jobEvent("queued", 6201, 90, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the job asked about again", func() bool { return len(gate.asksFor(6201)) >= 3 })
	j, err := prog.broker.Job(ctx, 6201)
	if err != nil || j.State != "waiting" || !strings.Contains(j.Why, "the account is at its 2 vCPU limit") {
		t.Fatalf("held job %+v %v, want waiting with the program's reason", j, err)
	}
	if minted, _ := f.gh.snapshot(); len(minted) != 0 {
		t.Errorf("runners minted for a held job: %v", minted)
	}
	// Not a hot loop: in a second, at most a try per WaitMax (100 ms here).
	n := len(gate.asksFor(6201))
	time.Sleep(time.Second)
	if more := len(gate.asksFor(6201)) - n; more > 15 {
		t.Errorf("asked %d times in a second", more)
	}
	// A refusal of the acquire itself (the check before the mint passed, the
	// area changed by the acquire) also leaves the job waiting with it.
	gate.mu.Lock()
	gate.flip[6203] = true
	gate.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 6203, 92, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the acquire refused twice", func() bool { return len(gate.asksFor(6203)) >= 4 })
	if j, err := prog.broker.Job(ctx, 6203); err != nil || j.State != "waiting" || !strings.Contains(j.Why, "refused when the box was acquired") {
		t.Fatalf("job refused at its acquire: %+v %v", j, err)
	}
	// Another job is served on the same binding meanwhile: neither no is the
	// binding's (RefuseCool is an hour here).
	f.deliver(t, "workflow_job", jobEvent("queued", 6202, 91, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the other job's box", func() bool { return len(liveBoxes(t, f)) == 1 })
	if j, _ := prog.broker.Job(ctx, 6201); j.State != "waiting" {
		t.Errorf("the held job is %s", j.State)
	}

	gate.mu.Lock()
	gate.flip[6203] = false
	gate.mu.Unlock()
	gate.setHold(6201, "")
	eventuallyBox(t, "the held jobs served once let go", func() bool { return len(liveBoxes(t, f)) == 3 })
	for _, id := range []int64{6201, 6203} {
		eventually(t, "its row", func() bool { j, _ := prog.broker.Job(ctx, id); return j.State == "serving" && j.Handle != "" })
	}
}

// (d) A program ends one running job early: its box is released, its row
// says why, and the broker neither serves it again nor re-runs it once
// GitHub reports it failed.
func TestProgramEndsARunningJob(t *testing.T) {
	f, prog, _ := embedder(t, nil)
	ctx := context.Background()
	if err := prog.broker.EndJob(ctx, 6301, "over budget"); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("ending a job the broker never had: %v, want not_found", err)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 6301, 90, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the job's box", func() bool { return len(liveBoxes(t, f)) == 1 })
	eventually(t, "its handle", func() bool { j, _ := prog.broker.Job(ctx, 6301); return j.Handle != "" })
	if err := prog.broker.EndJob(ctx, 6301, "over budget"); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("ending a job not started yet: %v, want conflict", err)
	}
	if err := prog.broker.EndJob(ctx, 6301, " "); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("ending a job without a reason: %v, want bad_request", err)
	}
	f.started(t, 6301, 90, "acme/app", "potluck-6301")
	eventually(t, "the job running", func() bool { j, _ := prog.broker.Job(ctx, 6301); return !j.Started.IsZero() })

	// The host loses the box just as it is released: a watch then must not
	// take the end for a loss to re-run. The first release is not answered.
	var mu sync.Mutex
	lostNow, refuse, watched := false, true, false
	f.svc.AnswerStatus(func(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
		mu.Lock()
		defer mu.Unlock()
		if lostNow {
			h.State = "lost"
		}
		return h, err
	})
	f.svc.RefuseRelease(func() bool {
		mu.Lock()
		defer mu.Unlock()
		r := refuse
		refuse = false
		return r
	})
	f.svc.AfterRelease(func() {
		mu.Lock()
		first := !watched
		watched, lostNow = true, true
		mu.Unlock()
		if first {
			f.svc.Watch(ctx)
		}
	})
	if err := prog.broker.EndJob(ctx, 6301, "the account's card was declined"); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("an end whose release the host did not answer: %v, want its error", err)
	}
	if j, _ := prog.broker.Job(ctx, 6301); j.State != "serving" || j.Handle == "" || len(liveBoxes(t, f)) != 1 {
		t.Fatalf("after a failed end the job is %+v", j)
	}
	if c := f.svc.StopCause(ctx, 6301); c != "" {
		t.Fatalf("after a failed end the job keeps stop cause %q: its time limit and a lost box would not apply", c)
	}
	if err := prog.broker.EndJob(ctx, 6301, "the account's card was declined"); err != nil {
		t.Fatalf("end: %v", err)
	}
	f.svc.AnswerStatus(nil)
	eventuallyBox(t, "its box released", func() bool { return len(liveBoxes(t, f)) == 0 })
	j, err := prog.broker.Job(ctx, 6301)
	if err != nil || j.State != "done" || !strings.Contains(j.Why, "the account's card was declined") || j.Handle != "" {
		t.Fatalf("ended job %+v %v", j, err)
	}
	if err := prog.broker.EndJob(ctx, 6301, "again"); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("ending it again: %v, want conflict", err)
	}

	// GitHub fails the job once its runner is gone: nothing is served or re-run.
	f.gh.mu.Lock()
	f.gh.status[6301], f.gh.conclusion[6301] = "completed", "failure"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", completedEvent(6301, 90, "acme/app", "potluck-6301", "failure", "2026-11-23T11:00:00Z"))
	eventually(t, "the end applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.svc.Watch(ctx)
	f.svc.Sweep(ctx)
	time.Sleep(300 * time.Millisecond)
	if n := len(liveBoxes(t, f)); n != 0 {
		t.Errorf("%d boxes after the end", n)
	}
	minted, _ := f.gh.snapshot()
	f.gh.mu.Lock()
	reruns := slices.Clone(f.gh.reruns)
	f.gh.mu.Unlock()
	if len(minted) != 1 || len(reruns) != 0 {
		t.Errorf("after the end: minted %d runners, re-runs %v; want the one runner and no re-run", len(minted), reruns)
	}
	if j, _ := prog.broker.Job(ctx, 6301); j.State != "done" || !strings.Contains(j.Why, "the account's card was declined") {
		t.Errorf("after GitHub's end: %+v", j)
	}
}
