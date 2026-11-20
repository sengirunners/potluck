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
