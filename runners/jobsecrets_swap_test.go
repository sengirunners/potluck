package runners_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
)

// Same-label credentialed workflows of one repository take each other's
// runners (zm/local:xg54m6). Run 940 is x.yml, whose job grant is GH_TOKEN
// (contents); run 941 is y.yml, whose job grant is ISSUES_TOKEN (issues).
// A taker of another run gets its own grant on the box it took, after the
// broker's own checks of it, under its own run's re-guard marker
// (takeOver), never the grant of the job the box was acquired for; when a
// check refuses, it is bounced: stopped by the hook, re-run on a box of its
// own, and the job the box was acquired for is served again at once.

const (
	markerA = "940 acme/app/.github/workflows/x.yml@refs/heads/main refs/heads/main"
	markerB = "941 acme/app/.github/workflows/y.yml@refs/heads/main refs/heads/main"
	permsA  = "map[contents:write metadata:read]"
	permsB  = "map[issues:write metadata:read]"
)

// twoWorkflows adds y.yml's ISSUES_TOKEN grant and runs 940 (x.yml) and 941
// (y.yml) on main to df, which has GH_TOKEN for x.yml; other is a third
// run's workflow file, which no grant names (run 942).
func twoWorkflows(t *testing.T, df *doorFixture) {
	t.Helper()
	if _, err := computeclient.AddSecretGrant(context.Background(), df.owner, computeapi.AddSecretGrantRequest{
		To:   computeapi.GrantTo{Job: &computeapi.GrantJob{Repo: "acme/app", Workflow: ".github/workflows/y.yml", Ref: "refs/heads/main", Events: []string{"push"}}},
		Kind: computeapi.SecretKindGitHubToken, Name: "ISSUES_TOKEN", Scopes: []string{computeapi.SecretScopeBox},
		Ceiling: json.RawMessage(`{"repos":["acme/app"],"permissions":{"issues":"write"}}`)}); err != nil {
		t.Fatal(err)
	}
	df.set(func(d *doorGitHub) {
		d.files[".github/workflows/y.yml"] = pinnedWorkflow
		d.files[".github/workflows/z.yml"] = pinnedWorkflow
		d.runs[940] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.runs[941] = doorRun{event: "push", branch: "main", sha: mainSHA, path: ".github/workflows/y.yml"}
		d.runs[942] = doorRun{event: "push", branch: "main", sha: mainSHA, path: ".github/workflows/z.yml"}
	})
	df.gh.mu.Lock()
	for _, j := range []int64{9401, 9411, 9421} { // queued until started (startOn)
		df.gh.status[j] = "queued"
	}
	df.gh.mu.Unlock()
}

// endOn is GitHub's completed for job on runner (real deliveries name it).
func endOn(job, run int64, conclusion string, attempt int, runner string) []byte {
	var m map[string]any
	_ = json.Unmarshal(endEvent(job, run, "acme/app", conclusion, attempt, "self-hosted", "sengi"), &m)
	m["workflow_job"].(map[string]any)["runner_name"] = runner
	b, _ := json.Marshal(m)
	return b
}

// failedOnGitHub has GitHub say job of run failed and its run completed
// with failure (the fake's plain run answer once the door's is gone).
func (df *doorFixture) failedOnGitHub(job, run int64) {
	df.set(func(d *doorGitHub) { delete(d.runs, run) })
	df.gh.mu.Lock()
	df.gh.jobDone[job], df.gh.conclusion[job], df.gh.jobRun[job] = true, "failure", run
	df.gh.mu.Unlock()
	df.runIs(run, "completed")
}

// noGrantCrosses fails unless every box written holds, beside its marker,
// only the token of the run its marker names: GH_TOKEN under run 940's
// marker (plain or re-guard), ISSUES_TOKEN under run 941's.
func (df *doorFixture) noGrantCrosses(t *testing.T) {
	t.Helper()
	all, _ := df.issued()
	perms := map[string]string{}
	for _, i := range all {
		perms[i.token] = i.perms
	}
	for h, s := range df.boxWrites() {
		m := s[protocol.SecretNameReady]
		switch {
		case m == "940" || m == markerA:
			if s["ISSUES_TOKEN"] != "" || perms[s["GH_TOKEN"]] != permsA || len(s) != 2 {
				t.Errorf("box %s (run 940's) holds %v (perms %v), want its GH_TOKEN alone", h, s, perms)
			}
		case m == "941" || m == markerB:
			if s["GH_TOKEN"] != "" || perms[s["ISSUES_TOKEN"]] != permsB || len(s) != 2 {
				t.Errorf("box %s (run 941's) holds %v (perms %v), want its ISSUES_TOKEN alone", h, s, perms)
			}
		default:
			t.Errorf("box %s holds %v: a marker of no run served there", h, s)
		}
	}
}

// B (run 941), with no box of its own, takes A's runner (A's box guarded to
// run 940): the box is re-guarded to run 941 and gets ISSUES_TOKEN alone,
// under B's marker; GH_TOKEN is issued nowhere. The box goes to B, and A is
// served at once with a new box guarded to its own run, where it later gets
// GH_TOKEN alone. B's completion revokes B's token only, and B (served) is
// not re-run.
func TestJobGrantTakeOverGivesTheTakerItsOwnGrant(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	if g := df.served(t, 9401, 940, 1); g.Run == nil || g.Run.ID != 940 {
		t.Fatalf("A's guard %+v", g.Run)
	}
	df.applied(t, df.startOn(9411, 941, "acme/app", "potluck-9401"))
	eventually(t, "B's token under B's marker in A's box", func() bool {
		return df.boxWrites()["box:small/b1"][protocol.SecretNameReady] == markerB
	})
	all, _ := df.issued()
	if len(all) != 1 || all[0].perms != permsB {
		t.Fatalf("B on A's runner: issued %+v, want B's issues token alone", all)
	}
	if w := df.boxWrites(); len(w) != 1 || w["box:small/b1"]["ISSUES_TOKEN"] != all[0].token || len(w["box:small/b1"]) != 2 {
		t.Fatalf("B on A's runner: box writes %v, want B's token and marker in A's box alone", w)
	}
	// A is served again now, with a new box guarded to run 940.
	eventuallyBox(t, "A's new box", func() bool { return len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[1]; g == nil || g.Run == nil || g.Run.ID != 940 || !strings.Contains(g.Run.WorkflowRef, "/x.yml@") {
		t.Fatalf("A's new box's guard %+v, want run 940's x.yml", g)
	}
	if st := df.svc.JobSecretsState(ctx, 9401); st != "planned" {
		t.Fatalf("A's new row is %q, want planned", st)
	}

	// B completes: its token alone is revoked; it was served, not bounced.
	df.gh.mu.Lock()
	df.gh.jobDone[9411], df.gh.conclusion[9411] = true, "success"
	df.gh.mu.Unlock()
	df.applied(t, endOn(9411, 941, "success", 1, "potluck-9401"))
	eventually(t, "B's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
	if _, rev := df.issued(); !slices.Equal(rev, []string{all[0].token}) {
		t.Fatalf("B's completion revoked %v, want B's token alone", rev)
	}

	// A on its new box: GH_TOKEN alone, under run 940's own marker.
	df.applied(t, df.startOn(9401, 940, "acme/app", "potluck-9401-2"))
	eventually(t, "A's token in its new box", func() bool {
		all, _ := df.issued()
		return len(all) == 2 && df.svc.JobSecretsState(ctx, 9401) == "issued"
	})
	if all, _ := df.issued(); all[1].perms != permsA {
		t.Fatalf("issued %+v, want B's issues token, then A's contents token", all)
	}
	if w := df.boxWrites(); len(w) != 2 || w["box:small/b2"][protocol.SecretNameReady] != "940" {
		t.Fatalf("box writes %v, want A's new box under run 940's marker", w)
	}
	df.noGrantCrosses(t)
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none: nothing was bounced", r)
	}
}

// A takes B's runner and B takes A's, each box guarded to the other's run:
// each box is re-guarded to the run of the job on it and holds that job's
// own token alone (A's GH_TOKEN in B's box, B's ISSUES_TOKEN in A's box).
// Each completion revokes the token of the box that job held, and nothing is
// re-run or bounced.
func TestJobGrantSymmetricSwapAcrossRuns(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	df.served(t, 9401, 940, 1)
	df.served(t, 9411, 941, 2)
	df.applied(t, df.startOn(9411, 941, "acme/app", "potluck-9401"))
	df.applied(t, df.startOn(9401, 940, "acme/app", "potluck-9411"))
	eventually(t, "each job's own token in the box it took", func() bool {
		w := df.boxWrites()
		return w["box:small/b1"][protocol.SecretNameReady] == markerB && w["box:small/b2"][protocol.SecretNameReady] == markerA
	})
	all, _ := df.issued()
	if len(all) != 2 {
		t.Fatalf("issued %+v, want one token per job", all)
	}
	df.noGrantCrosses(t)
	w := df.boxWrites()
	inA, inB := w["box:small/b1"]["ISSUES_TOKEN"], w["box:small/b2"]["GH_TOKEN"]
	if inA == "" || inB == "" {
		t.Fatalf("box writes %v", w)
	}

	df.gh.mu.Lock()
	df.gh.jobDone[9401], df.gh.conclusion[9401] = true, "success"
	df.gh.mu.Unlock()
	df.applied(t, endOn(9401, 940, "success", 1, "potluck-9411"))
	eventually(t, "A's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
	if _, rev := df.issued(); !slices.Equal(rev, []string{inB}) {
		t.Fatalf("A's completion revoked %v, want the token in B's box (A's own) alone", rev)
	}
	df.gh.mu.Lock()
	df.gh.jobDone[9411], df.gh.conclusion[9411] = true, "success"
	df.gh.mu.Unlock()
	df.applied(t, endOn(9411, 941, "success", 1, "potluck-9401"))
	eventually(t, "B's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 2 })
	time.Sleep(300 * time.Millisecond)
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
	if all, _ := df.issued(); len(all) != 2 {
		t.Fatalf("issued %+v after both completions, want nothing more", all)
	}
}

// noReguardFixture is a door fixture whose box driver cannot re-guard
// (no github-jit-guard-reguard), as a driver older than the broker.
func noReguardFixture(t *testing.T) *doorFixture {
	f := newPoolFixtureStarting(t, func(r *config.Runners) {
		r.Label, r.Pool.Sizes, r.Pool.DefaultSize = "sengi", map[string]string{"small": "small"}, "small"
	}, nil, box("small", "small", "linux", "1", "jit_guard_reguard", "0"))
	df := newDoorFixtureOn(t, f, true, grantGH)
	twoWorkflows(t, df)
	return df
}

// B takes A's runner on a box that cannot be re-guarded, and GitHub's
// completed for B (failed: the hook killed it) is applied before its
// in_progress: B is still recorded bounced, re-run once its run completed,
// and A is served again at once. Nothing is issued anywhere, then or when
// the late in_progress comes.
func TestJobGrantBounceRecordedWhenCompletedComesFirst(t *testing.T) {
	df := noReguardFixture(t)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9411") })
	eventuallyBox(t, "A's new box", func() bool { return len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[1]; g == nil || g.Run == nil || g.Run.ID != 940 {
		t.Fatalf("A's new box's guard %+v, want run 940's", g)
	}
	df.applied(t, df.startOn(9411, 941, "acme/app", "potluck-9401")) // the late in_progress
	time.Sleep(500 * time.Millisecond)
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if r := df.reruns(); len(r) != 1 {
		t.Fatalf("re-runs %v, want B's once", r)
	}
}

// The same on a box that can be re-guarded but whose taker no grant names
// (run 942, z.yml), in_progress first, on the run's third attempt: B is
// refused, nothing is issued, A is served again at once (before B ends),
// and B is re-run once GitHub failed it, though attempt 3 is maxAttempts:
// bounces do not use up host-loss attempts.
func TestJobGrantBounceWhenTheTakerIsRefused(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.applied(t, df.startOn(9421, 942, "acme/app", "potluck-9401"))
	// A served again (a second box) deletes the refused row, so either shows it.
	eventuallyBox(t, "A's box refused to B", func() bool { return df.svc.JobSecretsEver(ctx, 9401, "") || len(df.guards()) >= 2 })
	eventuallyBox(t, "A's new box before B ends", func() bool { return len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[1]; g == nil || g.Run == nil || g.Run.ID != 940 {
		t.Fatalf("A's new box's guard %+v, want run 940's", g)
	}
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	df.failedOnGitHub(9421, 942)
	df.gh.mu.Lock()
	df.gh.attempt[942] = 3
	df.gh.mu.Unlock()
	df.applied(t, endOn(9421, 942, "failure", 3, "potluck-9401"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9421") })
}

// A run whose jobs were bounced and re-run maxBounceReruns times already is
// not re-run again, and says so on the job's row.
func TestJobGrantBounceCap(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.svc.SeedBounceReruns(ctx, "acme/app", 942, 5)
	df.applied(t, df.startOn(9421, 942, "acme/app", "potluck-9401"))
	// A served again (a second box) deletes the refused row, so either shows it.
	eventuallyBox(t, "A's box refused to B", func() bool { return df.svc.JobSecretsEver(ctx, 9401, "") || len(df.guards()) >= 2 })
	df.failedOnGitHub(9421, 942)
	df.gh.mu.Lock()
	df.gh.attempt[942] = 6
	df.gh.mu.Unlock()
	df.applied(t, endOn(9421, 942, "failure", 6, "potluck-9401"))
	eventually(t, "B closed at the cap", func() bool {
		return strings.Contains(df.jobs(t)[9421].Why, "re-run 5 times for bounces already")
	})
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none past the cap", r)
	}
}

// A box with an open guard (fork serving) is never taken over, nor its
// taker bounced: B gets nothing there, A is not served again before B ends,
// and B is not re-run.
func TestJobGrantOpenGuardIsNeverTakenOver(t *testing.T) {
	f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = true })
	df := newDoorFixtureOn(t, f, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	g := df.served(t, 9401, 940, 1)
	if !g.OpenIfPrivate || g.Run == nil {
		t.Fatalf("A's guard %+v, want an open guard held to run 940", g)
	}
	df.applied(t, df.startOn(9411, 941, "acme/app", "potluck-9401"))
	eventually(t, "A's box refused to B", func() bool { return df.svc.JobSecretsEver(ctx, 9401, "failed") })
	time.Sleep(500 * time.Millisecond)
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if n := len(df.guards()); n != 1 {
		t.Fatalf("%d acquires, want A's box alone (A is not served again before B ends)", n)
	}
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	time.Sleep(time.Second)
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none on an open guard", r)
	}
}

// A granted run (941, y.yml) bounced off another run's box is re-run by the
// broker after main moved on: the re-run's job, attempt 2 of a commit no
// longer main's tip, is still guarded to its run and gets its own grant
// (zm-s833), as attempt 1 would. A person's re-run of an old commit gets
// none, nor does an attempt the broker re-ran from a person's re-run, and a
// bounce re-run still being asked of GitHub holds the serve until GitHub
// answers.
func TestJobGrantBounceRerunKeepsItsGrantAfterMainMoves(t *testing.T) {
	df := noReguardFixture(t)
	ctx := context.Background()
	old := strings.Repeat("c", 40)
	df.served(t, 9401, 940, 1)
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9411") })
	eventuallyBox(t, "A's new box", func() bool { return len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0 })
	if r := df.svc.BounceReruns(ctx, "acme/app", 941); len(r) != 1 || !r[1] {
		t.Fatalf("run 941's bounce re-runs %v, want attempt 1's, asked", r)
	}

	// Main moved on: run 941's commit is behind its tip, on attempt 2.
	df.set(func(d *doorGitHub) {
		d.compare[old] = "behind"
		d.runs[941] = doorRun{event: "push", branch: "main", sha: old, path: ".github/workflows/y.yml", attempt: 2}
	})
	if g := df.served(t, 9412, 941, 3); g.Run == nil || g.Run.ID != 941 || !strings.Contains(g.Run.WorkflowRef, "/y.yml@") {
		t.Fatalf("the bounce re-run's guard %+v, want run 941's y.yml", g.Run)
	}
	df.deliver(t, "workflow_job", df.startOn(9412, 941, "acme/app", "potluck-9412"))
	eventually(t, "the re-run's token under its run's marker", func() bool {
		return df.boxWrites()["box:small/b3"][protocol.SecretNameReady] == "941"
	})
	if all, _ := df.issued(); all[0].perms != permsB {
		t.Fatalf("issued %+v, want run 941's issues token", all)
	}
	df.noGrantCrosses(t)

	// A person's re-run of an old commit (run 943, attempt 2, no bounce),
	// and a person's re-run of a bounce re-run (run 944, attempt 3: the
	// broker re-ran attempt 1, a person attempt 2): no run in the guard.
	df.svc.SeedBounceRerun(ctx, "acme/app", 944, 1, true)
	df.set(func(d *doorGitHub) {
		d.runs[943] = doorRun{event: "push", branch: "main", sha: old, path: ".github/workflows/y.yml", attempt: 2}
		d.runs[944] = doorRun{event: "push", branch: "main", sha: old, path: ".github/workflows/y.yml", attempt: 3}
	})
	if g := df.served(t, 9431, 943, 4); g.Run != nil {
		t.Fatalf("a person's re-run of an old commit is guarded to %+v, want no run", g.Run)
	}
	if g := df.served(t, 9441, 944, 5); g.Run != nil {
		t.Fatalf("a re-run of a person's re-run is guarded to %+v, want no run", g.Run)
	}

	// The broker's bounce re-run of run 945 is still asking: its next
	// attempt is not served until GitHub took it, then served guarded.
	df.svc.SeedBounceRerun(ctx, "acme/app", 945, 1, false)
	df.set(func(d *doorGitHub) {
		d.runs[945] = doorRun{event: "push", branch: "main", sha: old, path: ".github/workflows/y.yml", attempt: 2}
	})
	df.deliver(t, "workflow_job", jobEvent("queued", 9451, 945, "acme/app", true, "self-hosted", "sengi"))
	time.Sleep(time.Second)
	if n := len(df.guards()); n != 5 {
		t.Fatalf("%d acquires while the bounce re-run was asking, want 5", n)
	}
	df.svc.SeedBounceRerun(ctx, "acme/app", 945, 1, true)
	eventuallyBox(t, "run 945's job served once asked", func() bool { return len(df.guards()) == 6 })
	if g := df.guards()[5]; g == nil || g.Run == nil || g.Run.ID != 945 {
		t.Fatalf("run 945's guard %+v, want run 945", g)
	}

	// Run 946's bounce re-run was left asking by a process that stopped
	// before GitHub answered: past askingLimit it is not the broker's.
	df.svc.SeedBounceRerun(ctx, "acme/app", 946, 1, false)
	df.clock.advance(3 * time.Minute)
	df.set(func(d *doorGitHub) {
		d.runs[946] = doorRun{event: "push", branch: "main", sha: old, path: ".github/workflows/y.yml", attempt: 2}
	})
	if g := df.served(t, 9461, 946, 7); g.Run != nil {
		t.Fatalf("an attempt after an unanswered bounce re-run is guarded to %+v, want no run", g.Run)
	}
	if all, _ := df.issued(); len(all) != 1 {
		t.Fatalf("issued %+v, want run 941's token alone", all)
	}

	// Run 947's job says attempt 2 while GitHub's API still answers the run
	// as attempt 1 (it lags): a re-run of a commit no longer the tip, with
	// no bounce re-run, gets no grant.
	df.set(func(d *doorGitHub) {
		d.runs[947] = doorRun{event: "push", branch: "main", sha: old, path: ".github/workflows/y.yml", attempt: 1}
	})
	var m map[string]any
	_ = json.Unmarshal(jobEvent("queued", 9471, 947, "acme/app", true, "self-hosted", "sengi"), &m)
	m["workflow_job"].(map[string]any)["run_attempt"] = 2
	queued, _ := json.Marshal(m)
	df.deliver(t, "workflow_job", queued)
	eventuallyBox(t, "run 947's job served", func() bool { return len(df.guards()) == 8 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[7]; g == nil || g.Run != nil {
		t.Fatalf("a re-run read as attempt 1 by GitHub's API is guarded to %+v, want no run", g)
	}
}

// GitHub refuses the broker's bounce re-run (a person re-ran the run just
// before it asked): the broker's record of it is dropped, so the person's
// attempt 2 of a commit no longer main's tip gets no grant, and the broker
// does not re-run it again.
func TestJobGrantRefusedBounceRerunWidensNothing(t *testing.T) {
	df := noReguardFixture(t)
	ctx := context.Background()
	old := strings.Repeat("c", 40)
	df.served(t, 9401, 940, 1)
	df.gh.mu.Lock()
	df.gh.rerunRefused = true
	df.gh.mu.Unlock()
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	eventually(t, "the re-run refused", func() bool {
		df.gh.mu.Lock()
		defer df.gh.mu.Unlock()
		return df.gh.rerunAsks > 0
	})
	eventuallyBox(t, "A's new box", func() bool { return len(df.guards()) >= 2 })

	// The person's attempt 2, main having moved on.
	df.set(func(d *doorGitHub) {
		d.compare[old] = "behind"
		d.runs[941] = doorRun{event: "push", branch: "main", sha: old, path: ".github/workflows/y.yml", attempt: 2}
	})
	df.gh.mu.Lock()
	df.gh.attempt[941], df.gh.runStatus[941] = 2, "queued"
	df.gh.mu.Unlock()
	if g := df.served(t, 9412, 941, 3); g.Run != nil {
		t.Fatalf("a person's re-run after a refused bounce re-run is guarded to %+v, want no run", g.Run)
	}
	if r := df.svc.BounceReruns(ctx, "acme/app", 941); len(r) != 0 {
		t.Fatalf("run 941's bounce re-runs %v, want none", r)
	}
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
}

// A person's re-run lands between the broker's and the answer: the run is two
// attempts on, so the broker's re-run is not recorded (and the attempt that
// follows, one short in the chain, is not the broker's).
func TestJobGrantBounceRerunNotRecordedWhenAnotherAttemptRacedIt(t *testing.T) {
	df := noReguardFixture(t)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.gh.mu.Lock()
	df.gh.rerunExtra = 1
	df.gh.mu.Unlock()
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9411") })
	eventually(t, "the re-run closed", func() bool { return df.svc.Undone(ctx) == 0 })
	if r := df.svc.BounceReruns(ctx, "acme/app", 941); len(r) != 0 {
		t.Fatalf("run 941's bounce re-runs %v, want none: its run was two attempts on", r)
	}
}

// GitHub's run still shows the old attempt for a read or two after it took the
// broker's re-run: the broker reads again and records the re-run.
func TestJobGrantBounceRerunRecordedWhenGitHubLagsBehindItsAnswer(t *testing.T) {
	df := noReguardFixture(t)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.gh.mu.Lock()
	df.gh.staleRuns = 2
	df.gh.mu.Unlock()
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9411") })
	eventually(t, "the re-run recorded", func() bool { r := df.svc.BounceReruns(ctx, "acme/app", 941); return len(r) == 1 && r[1] })
}

// releases is how many boxes df's driver was asked to release.
func (df *doorFixture) releases() int {
	n := 0
	for _, c := range df.calls() {
		if c.Verb == "release" {
			n++
		}
	}
	return n
}

// A job no grant names (run 942, z.yml) is served an unguarded box: no row
// holds it, so its hook neither holds a job for a marker nor stops one. B,
// which a grant names (run 940, x.yml), takes its runner. B must never run
// there without its secrets and never be given any (the box's guard is not
// lowered, nothing is issued into it): the box is released (its job ends),
// B is recorded for a re-run on a box of its own once GitHub failed it, and
// the ungranted job is served again at once.
func TestJobGrantTakerOfAnUnguardedBoxIsStoppedAndRerun(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.deliver(t, "workflow_job", jobEvent("queued", 9421, 942, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the ungranted job's box", func() bool { return len(df.guards()) >= 1 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[0]; g != nil && g.Run != nil {
		t.Fatalf("the ungranted job's box is guarded %+v, want unguarded", g.Run)
	}
	df.applied(t, df.startOn(9401, 940, "acme/app", "potluck-9421"))
	eventuallyBox(t, "the taken box released and its job served again", func() bool {
		return df.releases() >= 1 && len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0
	})
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if why, bounced := df.svc.LostWhy(ctx, 9401); !bounced || !strings.Contains(why, "not guarded to any run") || strings.Contains(why, "job grants of another run") {
		t.Fatalf("B's record %q (bounced %v), want a bounce off an unguarded box", why, bounced)
	}
	// GitHub fails B (its box is gone); its run completes: B is re-run.
	df.failedOnGitHub(9401, 940)
	df.applied(t, endOn(9401, 940, "failure", 1, "potluck-9421"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9401") })
}

// The same, where B already has a box of its own when it takes the
// unguarded runner (so followTaker leaves the box on the ungranted job's
// record): the taken box is released from that record, the ungranted job is
// served again, B's own box is not released, and B is re-run.
func TestJobGrantTakerWithItsOwnBoxOfAnUnguardedBoxIsStoppedAndRerun(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	if g := df.served(t, 9401, 940, 1); g.Run == nil || g.Run.ID != 940 {
		t.Fatalf("B's guard %+v", g.Run)
	}
	if g := df.served(t, 9421, 942, 2); g.Run != nil {
		t.Fatalf("the ungranted job's box is guarded %+v, want unguarded", g.Run)
	}
	df.applied(t, df.startOn(9401, 940, "acme/app", "potluck-9421"))
	eventuallyBox(t, "the taken box released and its job served again", func() bool {
		return df.releases() >= 1 && len(df.guards()) >= 3 && df.svc.Undone(ctx) == 0
	})
	if g := df.guards()[2]; g == nil || g.Run != nil {
		t.Fatalf("the ungranted job served again with guard %+v, want an unguarded box", g)
	}
	if n := df.releases(); n != 1 {
		t.Fatalf("%d boxes released, want the taken one alone (B's own box kept)", n)
	}
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if _, bounced := df.svc.LostWhy(ctx, 9401); !bounced {
		t.Fatal("B not recorded as a bounce")
	}
	df.failedOnGitHub(9401, 940)
	df.applied(t, endOn(9401, 940, "failure", 1, "potluck-9421"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9401") })
}

// A taker that no grant names, on the same unguarded box, is left alone: its
// box is not released, it is not recorded, and its failure is not re-run as
// a bounce.
func TestJobGrantUngrantedTakerOfAnUnguardedBoxIsLeftAlone(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.deliver(t, "workflow_job", jobEvent("queued", 9421, 942, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the first job's box", func() bool { return len(df.guards()) >= 1 && df.svc.Undone(ctx) == 0 })
	df.set(func(d *doorGitHub) {
		d.runs[943] = doorRun{event: "push", branch: "main", sha: mainSHA, path: ".github/workflows/z.yml"}
	})
	df.applied(t, df.startOn(9431, 943, "acme/app", "potluck-9421"))
	// The positive tests release within a second of the start; give it longer.
	time.Sleep(2 * time.Second)
	if n := df.releases(); n != 0 {
		t.Fatalf("%d boxes released, want none", n)
	}
	if why, bounced := df.svc.LostWhy(ctx, 9431); why != "" || bounced {
		t.Fatalf("the ungranted taker recorded %q (bounced %v), want no record", why, bounced)
	}
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing", all, df.boxWrites())
	}
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
}

// GitHub's completed for B (granted, served on its own box) on the runner of
// the ungranted job's unguarded box is applied before its in_progress (or
// the in_progress never comes): B ran there without its secrets, so it is
// still recorded bounced, the ungranted job is served again, and B is
// re-run once; the late in_progress changes nothing and nothing is issued.
func TestJobGrantTakerOfAnUnguardedBoxCompletedFirst(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	if g := df.served(t, 9421, 942, 1); g.Run != nil {
		t.Fatalf("the ungranted job's box is guarded %+v, want unguarded", g.Run)
	}
	df.served(t, 9401, 940, 2)
	df.failedOnGitHub(9401, 940)
	df.applied(t, endOn(9401, 940, "failure", 1, "potluck-9421"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9401") })
	if why, bounced := df.svc.LostWhy(ctx, 9401); !bounced || !strings.Contains(why, "not guarded to any run") {
		t.Fatalf("B's record %q (bounced %v), want a bounce off an unguarded box", why, bounced)
	}
	eventuallyBox(t, "the ungranted job served again", func() bool { return len(df.guards()) >= 3 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[2]; g == nil || g.Run != nil {
		t.Fatalf("the ungranted job served again with guard %+v, want an unguarded box", g)
	}
	df.applied(t, df.startOn(9401, 940, "acme/app", "potluck-9421")) // the late in_progress
	time.Sleep(500 * time.Millisecond)
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if r := df.reruns(); len(r) != 1 {
		t.Fatalf("re-runs %v, want B's once", r)
	}
	if g := df.guards(); len(g) != 3 {
		t.Fatalf("%d serves, want 3: the ungranted job served again once", len(g))
	}
}
