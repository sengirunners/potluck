package runners_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// A job a grant names that ran on the runner of a job no grant names, whose
// box is unguarded, with none of the broker's word of it (no queued serve,
// no in_progress: the broker restarted, a delivery was lost) ran there
// without its secrets. Its completion, the sweep, or a serve that finds it
// already finished records it bounced and re-runs it on a box of its own,
// and the ungranted job is served again. U is job 9421 of run 942 (z.yml,
// no grant); B is job 9401 of run 940 (x.yml on main, GH_TOKEN).

// doneOnGitHub has GitHub say job of run completed with conclusion on
// runner, its run still running (the door's answer: a grant names run 940
// as GitHub answers it).
func (df *doorFixture) doneOnGitHub(job, run int64, conclusion, runner string) {
	df.gh.mu.Lock()
	df.gh.jobDone[job], df.gh.conclusion[job], df.gh.jobRun[job], df.gh.runnerOf[job] = true, conclusion, run, runner
	delete(df.gh.status, job)
	df.gh.mu.Unlock()
}

// logText is everything df's broker logged so far.
func (df *doorFixture) logText() string {
	df.logs.mu.Lock()
	defer df.logs.mu.Unlock()
	return df.logs.buf.String()
}

// unguardedU serves U on an unguarded box as the n-th acquire.
func (df *doorFixture) unguardedU(t *testing.T, n int) {
	t.Helper()
	if g := df.served(t, 9421, 942, n); g.Run != nil {
		t.Fatalf("U's box is guarded %+v, want unguarded", g.Run)
	}
}

// The incident of 2026-11-25 (onCall run 45600346721): B was never served
// and its in_progress never applied; GitHub fails it on U's runner. B is
// recorded bounced off an unguarded box, an ALERT is logged once, U is
// served again unguarded, nothing is issued, and B is re-run once its run
// completed, once, however often its completion is applied.
func TestJobGrantTakerNeverServedOfAnUnguardedBoxIsRerun(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.unguardedU(t, 1)
	df.doneOnGitHub(9401, 940, "failure", "potluck-9421")
	end := endOn(9401, 940, "failure", 1, "potluck-9421")
	// Not applied (Undone): B's re-run waits in the inbox until its run ends.
	df.deliver(t, "workflow_job", end)
	eventually(t, "B bounced", func() bool { _, b := df.svc.LostWhy(ctx, 9401); return b })
	if why, bounced := df.svc.LostWhy(ctx, 9401); !bounced || !strings.Contains(why, "not guarded to any run") {
		t.Fatalf("B's record %q (bounced %v), want a bounce off an unguarded box", why, bounced)
	}
	if !df.logged("ALERT: job 9401 of acme/app, which a job grant names, ran on the runner potluck-9421 of job 9421") {
		t.Fatal("no ALERT logged for B")
	}
	eventuallyBox(t, "U served again", func() bool { return len(df.guards()) >= 2 })
	if g := df.guards()[1]; g == nil || g.Run != nil {
		t.Fatalf("U served again with guard %+v, want an unguarded box", g)
	}
	df.deliver(t, "workflow_job", end) // GitHub's redelivery
	time.Sleep(500 * time.Millisecond)
	if n := strings.Count(df.logText(), "ALERT: job 9401"); n != 1 {
		t.Fatalf("%d ALERT lines for B, want 1", n)
	}
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	df.failedOnGitHub(9401, 940)
	eventuallyBox(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9401") })
	time.Sleep(time.Second)
	if r := df.reruns(); len(r) != 1 {
		t.Fatalf("re-runs %v, want B's once", r)
	}
	if n := len(df.guards()); n != 2 {
		t.Fatalf("%d serves, want 2: U served again once", n)
	}
}

// A job no grant names that fails on U's runner, unknown to the broker, is
// not recorded, re-run or alerted on, and U is not served again for it.
func TestJobGrantUngrantedJobFailingOnAnUnguardedBoxIsNotRerun(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.set(func(d *doorGitHub) {
		d.runs[943] = doorRun{event: "push", branch: "main", sha: mainSHA, path: ".github/workflows/z.yml"}
	})
	df.unguardedU(t, 1)
	df.doneOnGitHub(9431, 943, "failure", "potluck-9421")
	df.applied(t, endOn(9431, 943, "failure", 1, "potluck-9421"))
	time.Sleep(time.Second)
	if why, bounced := df.svc.LostWhy(ctx, 9431); why != "" || bounced {
		t.Fatalf("the ungranted job recorded %q (bounced %v), want no record", why, bounced)
	}
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
	if df.logged("ALERT") {
		t.Fatal("an ALERT logged for a job no grant names")
	}
	if n := len(df.guards()); n != 1 {
		t.Fatalf("%d serves, want U's one", n)
	}
}

// B that succeeded on U's runner is left as it ended: no record, no re-run,
// and GitHub is not asked about its run.
func TestJobGrantTakerOfAnUnguardedBoxThatPassedIsLeftAlone(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.unguardedU(t, 1)
	df.set(func(d *doorGitHub) { d.fail["run"] = true }) // asking would fail the delivery
	df.doneOnGitHub(9401, 940, "success", "potluck-9421")
	df.applied(t, endOn(9401, 940, "success", 1, "potluck-9421"))
	time.Sleep(time.Second)
	if why, bounced := df.svc.LostWhy(ctx, 9401); why != "" || bounced {
		t.Fatalf("B recorded %q (bounced %v), want no record", why, bounced)
	}
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
	if df.svc.Undone(ctx) != 0 {
		t.Fatal("B's completion not applied: GitHub was asked about its run")
	}
}

// GitHub not answering for B's run: its completion is applied again until
// it does, then B is recorded bounced and re-run once.
func TestJobGrantCompletedGrantCheckWaitsForGitHub(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.unguardedU(t, 1)
	df.set(func(d *doorGitHub) { d.fail["run"] = true })
	df.doneOnGitHub(9401, 940, "failure", "potluck-9421")
	df.deliver(t, "workflow_job", endOn(9401, 940, "failure", 1, "potluck-9421"))
	time.Sleep(time.Second)
	if why, bounced := df.svc.LostWhy(ctx, 9401); why != "" || bounced {
		t.Fatalf("B recorded %q (bounced %v) while GitHub did not answer, want nothing yet", why, bounced)
	}
	df.set(func(d *doorGitHub) { d.fail["run"] = false })
	eventuallyBox(t, "B bounced once GitHub answers", func() bool { _, b := df.svc.LostWhy(ctx, 9401); return b })
	df.failedOnGitHub(9401, 940)
	eventuallyBox(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9401") })
	time.Sleep(time.Second)
	if r := df.reruns(); len(r) != 1 {
		t.Fatalf("re-runs %v, want B's once", r)
	}
}

// B was served a box of its own, but GitHub gave it U's runner and the
// broker heard neither its in_progress nor its completed: the sweep finds
// B completed on U's runner, records it bounced, and re-runs it.
func TestSweepRerunsAGrantedTakerWhoseDeliveriesNeverCame(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.unguardedU(t, 1)
	df.served(t, 9401, 940, 2)
	df.doneOnGitHub(9401, 940, "failure", "potluck-9421")
	df.svc.Sweep(ctx)
	if why, bounced := df.svc.LostWhy(ctx, 9401); !bounced || !strings.Contains(why, "not guarded to any run") {
		t.Fatalf("B's record %q (bounced %v), want a bounce off an unguarded box", why, bounced)
	}
	df.failedOnGitHub(9401, 940)
	df.svc.Sweep(ctx)
	eventuallyBox(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9401") })
}

// B's queued delivery comes late, after GitHub already failed it on U's
// runner (its in_progress and completed were lost): the serve that finds it
// finished records it bounced and re-runs it, and serves it nothing.
func TestServeRerunsAWaitingGrantedJobThatRanOnAnUnguardedBox(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.unguardedU(t, 1)
	df.doneOnGitHub(9401, 940, "failure", "potluck-9421")
	df.deliver(t, "workflow_job", jobEvent("queued", 9401, 940, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "B bounced", func() bool { _, b := df.svc.LostWhy(ctx, 9401); return b })
	if why, bounced := df.svc.LostWhy(ctx, 9401); !bounced || !strings.Contains(why, "not guarded to any run") {
		t.Fatalf("B's record %q (bounced %v), want a bounce off an unguarded box", why, bounced)
	}
	df.failedOnGitHub(9401, 940)
	eventuallyBox(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9401") })
	if j := df.jobs(t)[9401]; j.State == "serving" {
		t.Fatalf("B's record %+v, want it not served", j)
	}
}
