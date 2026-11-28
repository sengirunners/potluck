package runners_test

import (
	"context"
	"encoding/json"
	"fmt"
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
// (y.yml) on main to df, which has GH_TOKEN for x.yml; z.yml is a third
// workflow file, which no grant names: run 942 on main, 951 on a branch,
// 950 a pull_request (its ref, refs/pull/..., is no branch). Runs 952
// (x.yml) and 953 (y.yml) are two more granted runs on main.
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
		d.runs[950] = doorRun{event: "pull_request", branch: "feature", sha: mainSHA, path: ".github/workflows/z.yml"}
		d.runs[951] = doorRun{event: "push", branch: "feature", sha: mainSHA, path: ".github/workflows/z.yml"}
		d.runs[952] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.runs[953] = doorRun{event: "push", branch: "main", sha: mainSHA, path: ".github/workflows/y.yml"}
		d.branches["feature"] = true
	})
	df.gh.mu.Lock()
	for _, j := range []int64{9401, 9411, 9421, 9501, 9511, 9521, 9531} { // queued until started (startOn)
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

// issuedFor waits until job's newest row is recorded issued: the marker's
// write is seen by the driver before the broker records it, and a
// completion applied in between finds the row still issuing.
func (df *doorFixture) issuedFor(t *testing.T, job int64) {
	t.Helper()
	eventually(t, fmt.Sprintf("job %d's row issued", job), func() bool {
		return df.svc.JobSecretsState(context.Background(), job) == "issued"
	})
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
// only the token of the workflow of the run its marker names: GH_TOKEN for
// x.yml (run 940's plain marker, or a re-guard marker), ISSUES_TOKEN for
// y.yml (run 941's), nothing for z.yml, which no grant names.
func (df *doorFixture) noGrantCrosses(t *testing.T) {
	t.Helper()
	all, _ := df.issued()
	perms := map[string]string{}
	for _, i := range all {
		perms[i.token] = i.perms
	}
	for h, s := range df.boxWrites() {
		m := s[protocol.SecretNameReady]
		wf := map[string]string{"940": "x", "941": "y"}[m]
		if f := strings.Fields(m); len(f) == 3 {
			wf, _, _ = strings.Cut(strings.TrimPrefix(f[1], "acme/app/.github/workflows/"), ".yml@")
		}
		switch wf {
		case "x":
			if s["ISSUES_TOKEN"] != "" || perms[s["GH_TOKEN"]] != permsA || len(s) != 2 {
				t.Errorf("box %s (an x.yml run's) holds %v (perms %v), want its GH_TOKEN alone", h, s, perms)
			}
		case "y":
			if s["GH_TOKEN"] != "" || perms[s["ISSUES_TOKEN"]] != permsB || len(s) != 2 {
				t.Errorf("box %s (a y.yml run's) holds %v (perms %v), want its ISSUES_TOKEN alone", h, s, perms)
			}
		case "z":
			if len(s) != 1 {
				t.Errorf("box %s (a z.yml run's) holds %v, want its marker alone", h, s)
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
// GH_TOKEN alone. B's completion revokes B's token only, and B (served),
// failed and its run completed, is not re-run.
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

	// B fails: its token alone is revoked; it was served, not bounced, so
	// it is not re-run (a bounce would be, its run completed).
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
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
	if why, _ := df.svc.LostWhy(ctx, 9411); why != "" {
		t.Fatalf("B recorded %q, want no record: it was served", why)
	}
}

// A takes B's runner and B takes A's, each box guarded to the other's run:
// each box is re-guarded to the run of the job on it and holds that job's
// own token alone (A's GH_TOKEN in B's box, B's ISSUES_TOKEN in A's box).
// Each completion (a failure, its run completed) revokes the token of the
// box that job held, and nothing is re-run or bounced.
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
	df.issuedFor(t, 9401)
	df.issuedFor(t, 9411)
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

	df.failedOnGitHub(9401, 940)
	df.applied(t, endOn(9401, 940, "failure", 1, "potluck-9411"))
	eventually(t, "A's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
	if _, rev := df.issued(); !slices.Equal(rev, []string{inB}) {
		t.Fatalf("A's completion revoked %v, want the token in B's box (A's own) alone", rev)
	}
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	eventually(t, "B's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 2 })
	time.Sleep(time.Second) // a bounce recorded by either completion would be re-run by now
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
	for _, j := range []int64{9401, 9411} {
		if why, _ := df.svc.LostWhy(context.Background(), j); why != "" {
			t.Fatalf("job %d recorded %q, want no record: it was served", j, why)
		}
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
// and whose ref is no branch (run 950, a pull_request: refs/pull/...), so its
// box cannot be re-guarded to it even with nothing issued (bareGuard);
// in_progress first, on the run's third attempt: B is refused, nothing is
// written anywhere, A is served again at once (before B ends), and B is
// re-run once GitHub failed it, though attempt 3 is maxAttempts: bounces do
// not use up host-loss attempts.
func TestJobGrantBounceWhenTheTakerIsRefused(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.applied(t, df.startOn(9501, 950, "acme/app", "potluck-9401"))
	// A served again (a second box) deletes the refused row, so either shows it.
	eventuallyBox(t, "A's box refused to B", func() bool { return df.svc.JobSecretsEver(ctx, 9401, "") || len(df.guards()) >= 2 })
	eventuallyBox(t, "A's new box before B ends", func() bool { return len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[1]; g == nil || g.Run == nil || g.Run.ID != 940 {
		t.Fatalf("A's new box's guard %+v, want run 940's", g)
	}
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if why, bounced := df.svc.LostWhy(ctx, 9501); !bounced || !strings.Contains(why, "a pull_request run's ref is not a branch") {
		t.Fatalf("B's record %q (bounced %v), want a bounce naming its ref", why, bounced)
	}
	df.failedOnGitHub(9501, 950)
	df.gh.mu.Lock()
	df.gh.attempt[950] = 3
	df.gh.mu.Unlock()
	df.applied(t, endOn(9501, 950, "failure", 3, "potluck-9401"))
	eventually(t, "B re-run", func() bool { return slices.Contains(df.reruns(), "job 9501") })
}

// A run whose jobs were bounced and re-run maxBounceReruns times already is
// not re-run again, and says so on the job's row.
func TestJobGrantBounceCap(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.svc.SeedBounceReruns(ctx, "acme/app", 950, 5)
	df.applied(t, df.startOn(9501, 950, "acme/app", "potluck-9401"))
	// A served again (a second box) deletes the refused row, so either shows it.
	eventuallyBox(t, "A's box refused to B", func() bool { return df.svc.JobSecretsEver(ctx, 9401, "") || len(df.guards()) >= 2 })
	df.failedOnGitHub(9501, 950)
	df.gh.mu.Lock()
	df.gh.attempt[950] = 6
	df.gh.mu.Unlock()
	df.applied(t, endOn(9501, 950, "failure", 6, "potluck-9401"))
	eventually(t, "B closed at the cap", func() bool {
		return strings.Contains(df.jobs(t)[9501].Why, "re-run 5 times for bounces already")
	})
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none past the cap", r)
	}
}

// A taker that no grant names but that its own serve would take (trusted
// event, own branch) gets the box it took re-guarded to its run with nothing
// issued: the re-guard marker alone, so its hook lets it run, as on a box of
// its own. Run 942 is on main, run 951 on a branch (its ref is that branch).
// Neither is bounced nor re-run (both fail, their runs completed), A is
// served again each time with a box guarded to its own run, and nothing is
// issued anywhere. A head branch a tag is named like (run 954) could be the
// tag's run, and so could one whose commit no branch of that name has (run
// 955: there is no branch v2, a tag deleted since; run 956: branch v3's
// history does not have it): their ref is not known, so those takers are
// bounced, as before.
func TestJobGrantTakeOverServesATakerNoGrantNames(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.set(func(d *doorGitHub) {
		d.runs[954] = doorRun{event: "push", branch: "v1", sha: mainSHA, path: ".github/workflows/z.yml"}
		d.runs[955] = doorRun{event: "workflow_dispatch", branch: "v2", sha: mainSHA, path: ".github/workflows/z.yml"}
		d.runs[956] = doorRun{event: "push", branch: "v3", sha: strings.Repeat("d", 40), path: ".github/workflows/z.yml"}
		d.branches["v3"], d.compare[strings.Repeat("d", 40)] = true, "diverged"
		d.tags["v1"] = true
	})
	df.served(t, 9401, 940, 1)
	takes := []struct {
		job, run int64
		runner   string
		marker   string
	}{
		{9421, 942, "potluck-9401", "942 acme/app/.github/workflows/z.yml@refs/heads/main refs/heads/main"},
		{9511, 951, "potluck-9401-2", "951 acme/app/.github/workflows/z.yml@refs/heads/feature refs/heads/feature"},
	}
	for i, tk := range takes {
		box := fmt.Sprintf("box:small/b%d", i+1)
		df.applied(t, df.startOn(tk.job, tk.run, "acme/app", tk.runner))
		eventually(t, fmt.Sprintf("job %d's re-guard marker in %s", tk.job, box), func() bool {
			return df.boxWrites()[box][protocol.SecretNameReady] == tk.marker
		})
		eventuallyBox(t, "A's next box", func() bool { return len(df.guards()) >= i+2 && df.svc.Undone(ctx) == 0 })
		if g := df.guards()[i+1]; g == nil || g.Run == nil || g.Run.ID != 940 {
			t.Fatalf("A's box %d's guard %+v, want run 940's", i+2, g)
		}
	}
	if all, _ := df.issued(); len(all) != 0 {
		t.Fatalf("issued %+v, want nothing: no grant names the takers' runs and A never started", all)
	}
	df.noGrantCrosses(t)

	df.applied(t, df.startOn(9541, 954, "acme/app", "potluck-9401-3"))
	eventuallyBox(t, "A's box after the bounce", func() bool { return len(df.guards()) >= 4 && df.svc.Undone(ctx) == 0 })
	if why, bounced := df.svc.LostWhy(ctx, 9541); !bounced || !strings.Contains(why, `a tag is named like its head branch "v1"`) {
		t.Fatalf("the taker on a tag-named branch recorded %q (bounced %v), want a bounce", why, bounced)
	}
	df.applied(t, df.startOn(9551, 955, "acme/app", "potluck-9401-4"))
	eventuallyBox(t, "A's box after the second bounce", func() bool { return len(df.guards()) >= 5 && df.svc.Undone(ctx) == 0 })
	if why, bounced := df.svc.LostWhy(ctx, 9551); !bounced || !strings.Contains(why, `branch "v2"`) {
		t.Fatalf("the taker whose head is no branch recorded %q (bounced %v), want a bounce", why, bounced)
	}
	df.applied(t, df.startOn(9561, 956, "acme/app", "potluck-9401-5"))
	// A is not served a sixth box (maxServes): the bounce's record shows it.
	eventuallyBox(t, "the third bounce", func() bool { _, b := df.svc.LostWhy(ctx, 9561); return b && df.svc.Undone(ctx) == 0 })
	if why, bounced := df.svc.LostWhy(ctx, 9561); !bounced || !strings.Contains(why, `its commit is not on a branch "v3"`) {
		t.Fatalf("the taker whose commit is not on its head branch recorded %q (bounced %v), want a bounce", why, bounced)
	}
	if w := df.boxWrites(); len(w) != 2 {
		t.Fatalf("box writes %v, want the two takers' markers alone", w)
	}

	for _, tk := range takes {
		df.failedOnGitHub(tk.job, tk.run)
		df.applied(t, endOn(tk.job, tk.run, "failure", 1, tk.runner))
	}
	df.failedOnGitHub(9541, 954)
	df.applied(t, endOn(9541, 954, "failure", 1, "potluck-9401-3"))
	eventually(t, "the bounced taker re-run", func() bool { return slices.Contains(df.reruns(), "job 9541") })
	time.Sleep(time.Second)
	if r := df.reruns(); len(r) != 1 {
		t.Fatalf("re-runs %v, want the bounced taker's alone", r)
	}
	for _, tk := range takes {
		if why, _ := df.svc.LostWhy(ctx, tk.job); why != "" {
			t.Fatalf("job %d recorded %q, want no record: it was served", tk.job, why)
		}
	}
}

// ownerCancelled has A's job cancelled before it started, as GitHub's
// completed for it is applied whole: its row ends with no holder (nothing
// was issued for it), GitHub names no runner for the job and still calls
// its runner busy, so the box is kept for whoever takes the runner. A is
// done: it is not served another box.
func (df *doorFixture) ownerCancelled(t *testing.T, job, run int64) {
	t.Helper()
	df.gh.mu.Lock()
	df.gh.busy[fmt.Sprintf("potluck-%d", job)] = true
	df.gh.jobDone[job], df.gh.conclusion[job], df.gh.jobRun[job] = true, "cancelled", run
	df.gh.mu.Unlock()
	df.applied(t, endOn(job, run, "cancelled", 1, ""))
	eventually(t, fmt.Sprintf("job %d's row ended", job), func() bool {
		return df.svc.JobSecretsState(context.Background(), job) == "ended"
	})
}

// A's own job was cancelled before it started and before the in_progress of
// B, which took A's runner, was decided: A's row is ended with no holder, and
// nothing was ever issued for it, so B takes it over as it would a planned
// row (its own token, under its own marker), and is not bounced or re-run.
func TestJobGrantTakeOverAfterTheOwnerEndedUnclaimed(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.ownerCancelled(t, 9401, 940)
	if st := df.svc.JobSecretsState(ctx, 9401); st != "ended" {
		t.Fatalf("A's row is %q, want ended", st)
	}
	df.applied(t, df.startOn(9411, 941, "acme/app", "potluck-9401"))
	eventually(t, "B's token under B's marker in A's box", func() bool {
		return df.boxWrites()["box:small/b1"][protocol.SecretNameReady] == markerB
	})
	// A is done, so no new box of A's shows the take-over finished: B's
	// completion applied before the marker's write is answered releases the
	// box under it, and B is bounced as a job its hook stopped.
	df.issuedFor(t, 9401)
	// B is served alone and A gets no new box.
	eventuallyBox(t, "B served", func() bool { return df.svc.Undone(ctx) == 0 })
	if n := len(df.guards()); n != 1 {
		t.Fatalf("%d boxes acquired, want A's first alone: A was cancelled", n)
	}
	df.noGrantCrosses(t)
	if why, bounced := df.svc.LostWhy(ctx, 9411); why != "" || bounced {
		t.Fatalf("B recorded %q (bounced %v), want no record", why, bounced)
	}
	df.failedOnGitHub(9411, 941)
	df.applied(t, endOn(9411, 941, "failure", 1, "potluck-9401"))
	eventually(t, "B's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
	time.Sleep(time.Second)
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
}

// The same for a job of A's own run (a swap within the run): A's row ended
// before any job claimed it, so C, of run 940, which took A's runner, gets
// run 940's token under its plain marker, and its completion revokes it.
func TestJobGrantSwapAfterTheOwnerEndedUnclaimed(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	df.served(t, 9401, 940, 1)
	df.ownerCancelled(t, 9401, 940)
	df.applied(t, df.startOn(9402, 940, "acme/app", "potluck-9401"))
	eventually(t, "C's token under run 940's marker in A's box", func() bool {
		return df.boxWrites()["box:small/b1"][protocol.SecretNameReady] == "940"
	})
	df.issuedFor(t, 9401)
	df.noGrantCrosses(t)
	df.applied(t, endOn(9402, 940, "success", 1, "potluck-9401"))
	eventually(t, "C's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
}

// A row another job held and ended (taken) guards nothing live. C, of A's
// run, took A's runner and was served there (A kept carrying the box, a swap
// within the run); C completed, A was served again unguarded (its run no
// longer on main), keeping C's row. D (run 942, no grant), served a box of
// its own (so A keeps carrying its box), runs on A's new unguarded box and
// fails: it ran as on any unguarded box, so it is not recorded bounced nor
// re-run.
func TestJobGrantTakenRowGuardsNothing(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.applied(t, df.startOn(9402, 940, "acme/app", "potluck-9401"))
	eventually(t, "C's token in A's box", func() bool { return df.boxWrites()["box:small/b1"][protocol.SecretNameReady] == "940" })
	df.issuedFor(t, 9401)
	df.set(func(d *doorGitHub) { d.runs[940] = doorRun{event: "push", branch: "feature", sha: mainSHA} })
	df.gh.mu.Lock()
	df.gh.jobDone[9402], df.gh.conclusion[9402] = true, "success"
	df.gh.mu.Unlock()
	df.applied(t, endOn(9402, 940, "success", 1, "potluck-9401"))
	eventuallyBox(t, "A served again, unguarded", func() bool { return len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[1]; g == nil || g.Run != nil {
		t.Fatalf("A's new box's guard %+v, want no run", g)
	}
	if n := df.svc.JobSecretsRows(ctx, 9401); n != 1 {
		t.Fatalf("A has %d job-grant rows, want C's taken one alone", n)
	}
	df.served(t, 9421, 942, 3)
	df.applied(t, df.startOn(9421, 942, "acme/app", "potluck-9401-2"))
	if j := df.jobs(t)[9401]; j.State != "serving" || j.Runner != "potluck-9401-2" {
		t.Fatalf("A's record %+v, want it carrying its box on potluck-9401-2", j)
	}
	df.failedOnGitHub(9421, 942)
	df.applied(t, endOn(9421, 942, "failure", 1, "potluck-9401-2"))
	time.Sleep(time.Second)
	if why, bounced := df.svc.LostWhy(ctx, 9421); why != "" || bounced {
		t.Fatalf("D recorded %q (bounced %v), want no record: its box is guarded to no run", why, bounced)
	}
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
}

// Four same-label granted runs, each job served its own box, take each
// other's runners in a ring (A takes B's, B takes C's, C takes D's, D takes
// A's): every box is re-guarded to the run of the job on it and holds that
// job's own token alone; no job is served again (each taker has its own box,
// whose job takes another), none is bounced, and each failure revokes the
// token of the box that job held without a re-run.
func TestJobGrantTakeOverRingOfFour(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	ring := []struct {
		job, run int64
		marker   string
	}{
		{9401, 940, markerA},
		{9411, 941, markerB},
		{9521, 952, "952 acme/app/.github/workflows/x.yml@refs/heads/main refs/heads/main"},
		{9531, 953, "953 acme/app/.github/workflows/y.yml@refs/heads/main refs/heads/main"},
	}
	for i, r := range ring {
		df.served(t, r.job, r.run, i+1)
	}
	for i, r := range ring {
		next := ring[(i+1)%len(ring)]
		df.applied(t, df.startOn(r.job, r.run, "acme/app", fmt.Sprintf("potluck-%d", next.job)))
	}
	eventually(t, "each job's own marker in the box it took", func() bool {
		w := df.boxWrites()
		for i, r := range ring {
			if w[fmt.Sprintf("box:small/b%d", (i+1)%len(ring)+1)][protocol.SecretNameReady] != r.marker {
				return false
			}
		}
		return true
	})
	for _, r := range ring {
		df.issuedFor(t, r.job) // the row of the box it was served, which the ring's previous job holds
	}
	df.noGrantCrosses(t)
	if all, _ := df.issued(); len(all) != 4 {
		t.Fatalf("issued %+v, want one token per job", all)
	}
	if n := len(df.guards()); n != 4 {
		t.Fatalf("%d acquires, want the four boxes alone: no job is served again", n)
	}
	for i, r := range ring {
		df.failedOnGitHub(r.job, r.run)
		df.applied(t, endOn(r.job, r.run, "failure", 1, fmt.Sprintf("potluck-%d", ring[(i+1)%len(ring)].job)))
	}
	eventually(t, "every token revoked", func() bool { _, rev := df.issued(); return len(rev) == 4 })
	time.Sleep(time.Second)
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
	for _, r := range ring {
		if why, _ := df.svc.LostWhy(ctx, r.job); why != "" {
			t.Fatalf("job %d recorded %q, want no record: it was served", r.job, why)
		}
	}
}

// B has an idle box of its own when it takes A's runner: A's box is
// re-guarded to B's run and holds B's token alone, A keeps carrying it (B's
// idle runner is there for A's job), so A is not served again and B's box
// is not released while B runs. B's failure revokes its token; A's box goes
// and A is served again, its new row guarded to its own run. B's
// completion, applied again after that serve, still finds the box B held
// (A's serve keeps the row B ended) and records no bounce: B is not re-run.
func TestJobGrantTakeOverByATakerWithItsOwnBox(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.served(t, 9411, 941, 2)
	df.applied(t, df.startOn(9411, 941, "acme/app", "potluck-9401"))
	eventually(t, "B's token under B's marker in A's box", func() bool {
		return df.boxWrites()["box:small/b1"][protocol.SecretNameReady] == markerB
	})
	df.issuedFor(t, 9401)
	df.noGrantCrosses(t)
	time.Sleep(time.Second)
	if n, rel := len(df.guards()), df.releases(); n != 2 || rel != 0 {
		t.Fatalf("%d acquires, %d releases while B runs; want A's and B's boxes alone, both kept", n, rel)
	}

	df.failedOnGitHub(9411, 941)
	end := endOn(9411, 941, "failure", 1, "potluck-9401")
	df.applied(t, end)
	eventually(t, "B's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
	eventuallyBox(t, "A served again", func() bool {
		return len(df.guards()) >= 3 && df.svc.JobSecretsState(ctx, 9401) == "planned" && df.svc.Undone(ctx) == 0
	})
	if g := df.guards()[2]; g == nil || g.Run == nil || g.Run.ID != 940 {
		t.Fatalf("A's new box's guard %+v, want run 940's", g)
	}
	df.applied(t, end) // GitHub's redelivery
	time.Sleep(time.Second)
	if why, bounced := df.svc.LostWhy(ctx, 9411); why != "" || bounced {
		t.Fatalf("B recorded %q (bounced %v) by its completion applied again, want no record", why, bounced)
	}
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
}

// A box with an open guard (fork serving) is never taken over, nor its
// taker bounced: B gets nothing there (its box's hook stops it) and keeps
// the box until it ends, A, still queued, is served again at once, and B is
// not re-run.
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
	eventuallyBox(t, "B keeps A's box, A served again", func() bool {
		j := df.jobs(t)
		return j[9411].State == "serving" && j[9411].Runner == "potluck-9401" && j[9401].Runner == "potluck-9401-2" &&
			len(df.guards()) == 2 && df.svc.Undone(ctx) == 0
	})
	time.Sleep(500 * time.Millisecond)
	if all, _ := df.issued(); len(all) != 0 || len(df.boxWrites()) != 0 {
		t.Fatalf("issued %+v, box writes %v; want nothing anywhere", all, df.boxWrites())
	}
	if n := len(df.guards()); n != 2 {
		t.Fatalf("%d acquires, want A's first box and its new one", n)
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

// A's newest row is of an earlier box: B (run 941) took A's runner, A's box
// was re-guarded to B's run and went to B (B had none of its own), and A was
// served again unguarded (its run no longer on main: no grant names it), so
// that serve added no row. C (run 942, no grant) takes A's new runner: the
// row of the box B holds is not the box C took, so C is a taker of an
// unguarded box no grant names, left alone: not recorded bounced, nothing
// released.
func TestJobGrantRowOfAnEarlierBoxIsNotTheNewBoxs(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.set(func(d *doorGitHub) { d.runs[940] = doorRun{event: "push", branch: "feature", sha: mainSHA} })
	df.applied(t, df.startOn(9411, 941, "acme/app", "potluck-9401"))
	eventually(t, "B's token under B's marker in A's box", func() bool {
		return df.boxWrites()["box:small/b1"][protocol.SecretNameReady] == markerB
	})
	eventuallyBox(t, "A served again", func() bool { return len(df.guards()) >= 2 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[1]; g == nil || g.Run != nil {
		t.Fatalf("A's new box's guard %+v, want no run", g)
	}
	df.applied(t, df.startOn(9421, 942, "acme/app", "potluck-9401-2"))
	time.Sleep(2 * time.Second)
	if why, bounced := df.svc.LostWhy(ctx, 9421); why != "" || bounced {
		t.Fatalf("C recorded %q (bounced %v), want no record: its box is guarded to no run", why, bounced)
	}
	if n := df.releases(); n != 0 {
		t.Fatalf("%d boxes released, want none", n)
	}
	if w := df.boxWrites(); len(w) != 1 {
		t.Fatalf("box writes %v, want B's in A's first box alone", w)
	}
	df.noGrantCrosses(t)
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
	if !df.logged("ALERT: job 9401 of acme/app, which a job grant names, took the runner potluck-9421 of job 9421") {
		t.Fatal("no ALERT logged for B on the unguarded box")
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
	if !df.logged("ALERT: job 9401 of acme/app, which a job grant names, took the runner potluck-9421 of job 9421") {
		t.Fatal("no ALERT logged for B on the unguarded box")
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
	if df.logged("ALERT") {
		t.Fatal("an ALERT logged for a taker no grant names")
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
