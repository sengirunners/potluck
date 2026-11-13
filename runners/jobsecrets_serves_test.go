package runners_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/runners"
)

// revokedWhys is why each job grant token was revoked, in order.
func (df *doorFixture) revokedWhys(t *testing.T) []string {
	t.Helper()
	var whys []string
	if err := df.owner.Events(context.Background(), api.EventQuery{Types: computeapi.EventSecretRevoked, Project: "pool", Since: "0", NoLive: true},
		func(e api.Event) error {
			var v struct {
				Why string `json:"why"`
			}
			_ = json.Unmarshal(e.Data, &v)
			whys = append(whys, v.Why)
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	return whys
}

// revokedRecorded waits until at least n revocations are recorded and answers
// revokedWhys. GitHub's fake counts a revoke when it answers the DELETE; the
// issuer records the revoke and publishes its event after that.
func (df *doorFixture) revokedRecorded(t *testing.T, n int) []string {
	t.Helper()
	var whys []string
	eventually(t, "the revoke recorded", func() bool { whys = df.revokedWhys(t); return len(whys) >= n })
	return whys
}

// A job served again (a new box) while another job of its run still holds
// its first box: that job's completion still revokes the first box's token
// at once, and the job gets its own token in its new box.
func TestJobGrantServedAgainAfterASwap(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	ctx := context.Background()
	df.set(func(d *doorGitHub) { d.runs[830] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	df.served(t, 8301, 830, 1)
	df.served(t, 8302, 830, 2)
	df.applied(t, df.startOn(8302, 830, "acme/app", "potluck-8301")) // B takes A's runner
	eventually(t, "B's token in A's box", df.marked)
	all, _ := df.issued()
	if len(all) != 1 {
		t.Fatalf("B on A's runner: issued %+v", all)
	}
	inFirst := all[0].token

	// A is served again; its first box, which B holds, keeps running.
	df.gh.mu.Lock()
	df.gh.status[8301] = "queued"
	df.gh.mu.Unlock()
	if err := df.svc.ServeAgainKeepingBox(ctx, 8301); err != nil {
		t.Fatal(err)
	}
	eventuallyBox(t, "A's new box", func() bool { return len(df.guards()) == 3 && df.svc.Undone(ctx) == 0 })
	if g := df.guards()[2]; g == nil || g.Run == nil || g.Run.ID != 830 {
		t.Fatalf("A's new box's guard: %+v", g)
	}
	df.applied(t, df.startOn(8301, 830, "acme/app", "potluck-8301-2"))
	// Minted is not yet issued: the row turns issued once the token and the
	// marker are in the box, after GitHub's fake has counted the mint.
	eventually(t, "A's token in its new box", func() bool {
		all, _ := df.issued()
		return len(all) == 2 && df.svc.JobSecretsState(ctx, 8301) == "issued"
	})

	df.deliver(t, "workflow_job", jobEvent("completed", 8302, 830, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the token B held revoked", func() bool { _, rev := df.issued(); return len(rev) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if _, rev := df.issued(); !slices.Equal(rev, []string{inFirst}) {
		t.Fatalf("B's completion revoked %v, want only %s (A's first box, which B held)", rev, inFirst)
	}
	if whys := df.revokedRecorded(t, 1); !slices.Equal(whys, []string{string(issuers.EndJobCompleted)}) {
		t.Errorf("revoked for %q, want once for B's completion", whys)
	}
	if st := df.svc.JobSecretsState(ctx, 8301); st != "issued" {
		t.Errorf("A's current row is %q, want issued", st)
	}
	df.deliver(t, "workflow_job", jobEvent("completed", 8301, 830, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "A's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 2 })
}

// The sweep forgets job grant rows once they are older than the jobs'
// retention, one left issued too; a newer one stays.
func TestJobGrantRowsForgotten(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	ctx := context.Background()
	df.set(func(d *doorGitHub) {
		d.runs[790] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.runs[791] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.runs[792] = doorRun{event: "push", branch: "main", sha: mainSHA}
	})
	for i, job := range []int64{7901, 7911} {
		df.served(t, job, 790+int64(i), i+1)
		df.applied(t, jobEvent("completed", job, 790+int64(i), "acme/app", true, "self-hosted", "sengi"))
		if st := df.svc.JobSecretsState(ctx, job); st != "ended" {
			t.Fatalf("job %d's row is %q, want ended", job, st)
		}
	}
	df.served(t, 7921, 792, 3)
	df.applied(t, df.startOn(7921, 792, "acme/app", "potluck-7921"))
	eventually(t, "the box's secrets issued", func() bool { return df.svc.JobSecretsState(ctx, 7921) == "issued" })

	for _, job := range []int64{7901, 7921} {
		if err := df.svc.AgeJobSecrets(ctx, job, 31*24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	df.svc.Sweep(ctx)
	if n := df.svc.JobSecretsRows(ctx, 7901); n != 0 {
		t.Errorf("an old ended row kept: %d rows", n)
	}
	if st := df.svc.JobSecretsState(ctx, 7911); st != "ended" {
		t.Errorf("a recent ended row is %q, want kept", st)
	}
	if n := df.svc.JobSecretsRows(ctx, 7921); n != 0 {
		t.Errorf("an old issued row kept: %d rows", n)
	}
}

// gateAcquires makes the box driver hold every acquire's answer (the box
// is made) until the returned file is removed.
func (df *doorFixture) gateAcquires(t *testing.T) string {
	t.Helper()
	if err := os.WriteFile(df.gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(df.gate) }) // a failed test leaves no acquire held
	return df.gate
}

// GitHub saying the job started before the serve recorded its box's handle
// (the acquire not returned yet): the box still gets its token once the
// handle is recorded.
func TestJobGrantStartedBeforeTheHandle(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	df.set(func(d *doorGitHub) { d.runs[800] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	gate := df.gateAcquires(t)
	df.deliver(t, "workflow_job", jobEvent("queued", 8001, 800, "acme/app", true, "self-hosted", "sengi"))
	eventuallyBox(t, "the acquire asked", func() bool { return len(df.guards()) == 1 })
	df.deliver(t, "workflow_job", df.startOn(8001, 800, "acme/app", "potluck-8001"))
	eventually(t, "the start claimed the row while the handle is not recorded", func() bool {
		return df.svc.JobSecretsState(context.Background(), 8001) == "issuing"
	})
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	eventuallyBox(t, "the box's secrets issued", func() bool { return df.svc.JobSecretsState(context.Background(), 8001) == "issued" })
	if all, rev := df.issued(); len(all) != 1 || len(rev) != 0 || !df.marked() {
		t.Fatalf("issued %+v (revoked %v), marker %v; want one token and the marker", all, rev, df.marked())
	}
}

// An in_progress applied after its job completed (GitHub delivers out of
// order) gets nothing in the box whose runner it names, and leaves the box's
// row for a job that does start there.
func TestJobGrantLateStartOfACompletedJob(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	ctx := context.Background()
	df.set(func(d *doorGitHub) { d.runs[810] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	df.served(t, 8101, 810, 1)
	df.served(t, 8102, 810, 2)
	df.gh.mu.Lock()
	df.gh.jobDone[8102] = true
	df.gh.mu.Unlock()
	df.applied(t, jobEvent("completed", 8102, 810, "acme/app", true, "self-hosted", "sengi"))
	df.applied(t, df.startOn(8102, 810, "acme/app", "potluck-8101"))
	time.Sleep(500 * time.Millisecond)
	if all, _ := df.issued(); len(all) != 0 || df.marked() {
		t.Fatalf("a completed job's late start minted %+v, box secrets %v", all, df.boxSecrets())
	}
	if st := df.svc.JobSecretsState(ctx, 8101); st != "planned" {
		t.Errorf("the box's row is %q, want still planned", st)
	}
}

// The upgrade (runners/0012) keeps every job grant row of the table keyed
// by job, and a job served again then gets a second row.
func TestUpgradeKeepsJobSecretsRows(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	st, err := store.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, runners.Migrations...); err != nil {
		t.Fatal(err)
	}
	// The registry as it was before runners/0012, with rows.
	for _, q := range []string{
		`DROP TABLE runners_job_secrets`,
		`DELETE FROM schema_migrations WHERE name = 'runners/0012_job_secrets_serves'`,
		`CREATE TABLE runners_job_secrets (job INTEGER PRIMARY KEY, project TEXT NOT NULL, area TEXT NOT NULL, run INTEGER NOT NULL,
			workflow TEXT NOT NULL, ref TEXT NOT NULL, handle TEXT NOT NULL DEFAULT '', holder INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL, why TEXT NOT NULL DEFAULT '', updated INTEGER NOT NULL)`,
		`INSERT INTO runners_job_secrets (job, project, area, run, workflow, ref, handle, holder, state, why, updated) VALUES
			(1, 'pool', '', 10, 'w.yml', 'refs/heads/main', 'small/b1', 2, 'issued', '', 100),
			(3, 'pool', 'a', 30, 'w.yml', 'refs/heads/main', '', 0, 'failed', 'gone', 300)`,
	} {
		if _, err := st.Registry().ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	st.Close()
	if st, err = store.Open(ctx, home); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMigrations(ctx, runners.Migrations...); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Registry().ExecContext(ctx, `INSERT INTO runners_job_secrets (job, project, area, run, workflow, ref, state, updated)
		VALUES (1, 'pool', '', 10, 'w.yml', 'refs/heads/main', 'planned', 400)`); err != nil {
		t.Fatalf("a second row for job 1: %v", err)
	}
	rows, err := st.Registry().QueryContext(ctx, `SELECT id, job, handle, holder, state, why, updated FROM runners_job_secrets ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, job, holder, updated int64
		var handle, state, why string
		if err := rows.Scan(&id, &job, &handle, &holder, &state, &why, &updated); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d %d %s %d %s %s %d", id, job, handle, holder, state, why, updated))
	}
	want := []string{"1 1 small/b1 2 issued  100", "2 3  0 failed gone 300", "3 1  0 planned  400"}
	if !slices.Equal(got, want) {
		t.Errorf("rows after the upgrade: %q, want %q", got, want)
	}
}

// GitHub's API lagging the webhook (the job still queued there for a
// moment): the box still gets its token once GitHub shows it started.
func TestJobGrantStartWhileGitHubLags(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	df.set(func(d *doorGitHub) { d.runs[820] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	df.served(t, 8201, 820, 1)
	df.ready(t, 1)
	start := df.startOn(8201, 820, "acme/app", "potluck-8201")
	df.gh.mu.Lock()
	df.gh.status[8201], df.gh.runnerOf[8201] = "queued", ""
	df.gh.mu.Unlock()
	df.deliver(t, "workflow_job", start)
	time.Sleep(2 * time.Second)
	if st := df.svc.JobSecretsState(context.Background(), 8201); st != "planned" {
		t.Fatalf("the row is %q while GitHub shows the job queued, want planned", st)
	}
	df.gh.mu.Lock()
	delete(df.gh.status, 8201)
	df.gh.runnerOf[8201] = "potluck-8201"
	df.gh.mu.Unlock()
	eventuallyBox(t, "the box's secrets issued", func() bool { return df.svc.JobSecretsState(context.Background(), 8201) == "issued" })
}

// A stale in_progress of a job of the run, applied after that job's
// completion while GitHub's API still shows it running on another job's
// runner: the box gets nothing (no completion is left to revoke it), and
// its row stays for a job that does start there.
func TestJobGrantStartAfterItsCompletion(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	ctx := context.Background()
	df.set(func(d *doorGitHub) { d.runs[840] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	df.served(t, 8401, 840, 1)
	df.served(t, 8402, 840, 2)
	start := df.startOn(8402, 840, "acme/app", "potluck-8401") // GitHub's API: in progress on A's runner
	df.applied(t, jobEvent("completed", 8402, 840, "acme/app", true, "self-hosted", "sengi"))
	df.applied(t, start)
	time.Sleep(500 * time.Millisecond)
	if all, _ := df.issued(); len(all) != 0 || df.marked() {
		t.Fatalf("a completed job's stale start minted %+v, box secrets %v", all, df.boxSecrets())
	}
	if st := df.svc.JobSecretsState(ctx, 8401); st != "planned" {
		t.Errorf("the box's row is %q, want still planned", st)
	}
}

// A row left issuing by a process that is gone (a restart while minting):
// the completion of the job holding the box revokes what the box got at
// once, even while the box's host does not answer, so the release that
// completion asks for does not end the box yet. Here that job (B) took
// another job's (A's) runner.
func TestJobGrantIssuingLeftByARestart(t *testing.T) {
	df := newDoorFixture(t, true, grantGH, [2]string{"ISSUES_TOKEN", "issues"})
	gate, gated := make(chan struct{}), make(chan struct{})
	opened := false
	defer func() {
		if !opened {
			close(gate)
		}
	}()
	df.set(func(d *doorGitHub) {
		d.runs[850] = doorRun{event: "push", branch: "main", sha: mainSHA}
		d.gate, d.gated, d.gateFrom = gate, gated, 1 // the second mint waits
	})
	df.served(t, 8501, 850, 1)
	df.served(t, 8502, 850, 2)
	df.deliver(t, "workflow_job", df.startOn(8502, 850, "acme/app", "potluck-8501")) // B takes A's runner
	select {
	case <-gated:
	case <-time.After(60 * time.Second):
		t.Fatal("the second token was never asked for")
	}
	df.svc.LoseIssuing()        // the process minting is gone
	df.svc.HideTargets("small") // and A's box's host does not answer
	df.deliver(t, "workflow_job", jobEvent("completed", 8502, 850, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the first token revoked at B's completion", func() bool { all, rev := df.issued(); return len(all) == 1 && len(rev) == 1 })
	if whys := df.revokedRecorded(t, 1); len(whys) != 1 || whys[0] != string(issuers.EndJobCompleted) {
		t.Errorf("revoked for %q, want once for B's completion", whys)
	}
	opened = true
	close(gate)
}

// A webhook applied late: the box's hook began holding the job when it
// started, so issuing that could not finish inside the hold mints nothing.
func TestJobGrantStartTooLongAgo(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	df.set(func(d *doorGitHub) { d.runs[860] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	df.served(t, 8601, 860, 1)
	df.ready(t, 1)
	df.gh.mu.Lock()
	df.gh.jobStarts[8601] = df.clock.now().UTC().Format(time.RFC3339) // as its box was ready
	df.gh.mu.Unlock()
	df.clock.advance(250 * time.Second) // the in_progress applied 250 s later
	df.applied(t, df.startOn(8601, 860, "acme/app", "potluck-8601"))
	time.Sleep(500 * time.Millisecond)
	if all, _ := df.issued(); len(all) != 0 || df.marked() {
		t.Fatalf("a start 250 s ago minted %+v, box secrets %v", all, df.boxSecrets())
	}
}

// GitHub dating a job's start before its box was acquired (its time in the
// queue): the box's hook held it only from the acquire, so it still gets
// its token.
func TestJobGrantStartedBeforeItsBox(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	df.set(func(d *doorGitHub) { d.runs[870] = doorRun{event: "push", branch: "main", sha: mainSHA} })
	df.gh.mu.Lock()
	df.gh.jobStarts[8701] = df.clock.now().Add(-250 * time.Second).UTC().Format(time.RFC3339)
	df.gh.mu.Unlock()
	df.served(t, 8701, 870, 1)
	df.ready(t, 1)
	df.deliver(t, "workflow_job", df.startOn(8701, 870, "acme/app", "potluck-8701"))
	eventually(t, "the marker", df.marked)
}
