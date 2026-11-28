package runners_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/runners"
)

// Two jobs of one run with the same labels are queued; GitHub gives the
// runner minted for A to B, which has no box of its own. B keeps the box it
// took, and A, still queued, gets a new runner at once: it never waits for
// B to end. So too where the runner's guard is open (fork serving), which
// used to leave A carrying the box until B ended.
func TestSwapWithinARunServesTheOwnerAgainAtOnce(t *testing.T) {
	for _, forks := range []bool{false, true} {
		t.Run(map[bool]string{false: "trusted guard", true: "open guard"}[forks], func(t *testing.T) {
			f := newFixtureWith(t, func(c *config.Runners) { c.AllowForkPRs = forks })
			ctx := context.Background()
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			const a, b, run = 7401, 7402, 741
			f.gh.mu.Lock()
			f.gh.events[run] = "push"
			f.gh.status[a], f.gh.status[b] = "queued", "queued"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", jobEvent("queued", a, run, "acme/app", true, "self-hosted", "sengi"))
			eventuallyBox(t, "A served", func() bool { return f.jobs(t)[a].State == "serving" && f.svc.Undone(ctx) == 0 })
			if seen, _ := os.ReadFile(f.seen); forks != strings.Contains(string(seen), openGuard) {
				t.Fatalf("A's runner's guard (open %v): %s", forks, seen)
			}
			f.gh.mu.Lock()
			f.gh.runnerOf[b], f.gh.jobRun[b] = "potluck-7401", run
			delete(f.gh.status, b)
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", startEvent(b, run, "acme/app", "potluck-7401"))
			eventuallyBox(t, "B keeps A's box, A served again", func() bool {
				j := f.jobs(t)
				return j[b].State == "serving" && j[b].Runner == "potluck-7401" &&
					j[a].State == "serving" && j[a].Runner == "potluck-7401-2" && f.ready(t) == 2 && f.svc.Undone(ctx) == 0
			})
			// B ends: its box (A's first) goes, A's new one stays.
			f.gh.mu.Lock()
			f.gh.jobDone[b], f.gh.conclusion[b] = true, "success"
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", endEvent(b, run, "acme/app", "success", 1, "self-hosted", "sengi"))
			eventuallyBox(t, "B's box released", func() bool {
				j := f.jobs(t)
				return j[b].State == "done" && j[a].State == "serving" && j[a].Runner == "potluck-7401-2" && f.ready(t) == 1 && f.svc.Undone(ctx) == 0
			})
			// A starts on its own new runner: nothing more is minted.
			f.gh.mu.Lock()
			f.gh.runnerOf[a], f.gh.jobRun[a] = "potluck-7401-2", run
			delete(f.gh.status, a)
			f.gh.mu.Unlock()
			f.deliver(t, "workflow_job", startEvent(a, run, "acme/app", "potluck-7401-2"))
			eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
			time.Sleep(200 * time.Millisecond)
			if m := f.minted(); !slices.Equal(m, []string{"potluck-7401", "potluck-7401-2"}) || f.ready(t) != 1 {
				t.Fatalf("minted %v, %d boxes; want A's two runners, one box", m, f.ready(t))
			}
		})
	}
}

// The same where A's box is guarded to their run for its job grants: B, of
// that run, gets the run's token in the box it took, the box goes to B once
// it is issued, and A gets a new box guarded to the run at once. B's
// completion revokes B's token alone; A gets its own in its new box.
func TestJobGrantSwapWithinARunServesTheOwnerAgainAtOnce(t *testing.T) {
	df := newDoorFixture(t, true, grantGH)
	twoWorkflows(t, df)
	ctx := context.Background()
	df.served(t, 9401, 940, 1)
	df.applied(t, df.startOn(9402, 940, "acme/app", "potluck-9401"))
	eventually(t, "B's token under run 940's marker in A's box", func() bool {
		return df.boxWrites()["box:small/b1"][protocol.SecretNameReady] == "940"
	})
	eventuallyBox(t, "B keeps A's box, A served again", func() bool {
		j := df.jobs(t)
		g := df.guards()
		return len(g) >= 2 && g[1] != nil && g[1].Run != nil && g[1].Run.ID == 940 &&
			j[9402].State == "serving" && j[9402].Runner == "potluck-9401" &&
			j[9401].State == "serving" && j[9401].Runner == "potluck-9401-2" && df.svc.Undone(ctx) == 0
	})
	all, _ := df.issued()
	if len(all) != 1 {
		t.Fatalf("issued %+v, want B's token alone", all)
	}
	df.gh.mu.Lock()
	df.gh.jobDone[9402], df.gh.conclusion[9402] = true, "success"
	df.gh.mu.Unlock()
	df.applied(t, endOn(9402, 940, "success", 1, "potluck-9401"))
	eventually(t, "B's token revoked", func() bool { _, rev := df.issued(); return len(rev) == 1 })
	if _, rev := df.issued(); !slices.Equal(rev, []string{all[0].token}) {
		t.Fatalf("B's completion revoked %v, want B's token alone", rev)
	}
	df.applied(t, df.startOn(9401, 940, "acme/app", "potluck-9401-2"))
	eventually(t, "A's token in its new box", func() bool {
		all, _ := df.issued()
		return len(all) == 2 && df.svc.JobSecretsState(ctx, 9401) == "issued"
	})
	if w := df.boxWrites(); w["box:small/b2"][protocol.SecretNameReady] != "940" {
		t.Fatalf("box writes %v, want A's new box under run 940's marker", w)
	}
	df.noGrantCrosses(t)
	if r := df.reruns(); len(r) != 0 {
		t.Fatalf("re-runs %v, want none", r)
	}
}

// One slot, three queued jobs of one run with the same labels: A gets the
// box, C waits for room, and B takes A's runner. B keeps the box; A and C
// both wait for room, and no box is acquired while none is free. When B
// ends, exactly one of them gets the freed slot.
func TestSwapWithOneSlotAndAThirdJobWaits(t *testing.T) {
	f := newPoolFixtureStarting(t, func(r *config.Runners) { r.Pool.Sizes = map[string]string{"sa": "a"} },
		func(s *runners.Service) { s.Retry, s.WaitMax = 50*time.Millisecond, 200*time.Millisecond },
		box("a", "sa", "linux", "1", "boxes", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	const a, b, c, run = 99501, 99502, 99503, 9950
	f.gh.mu.Lock()
	f.gh.status[a], f.gh.status[b], f.gh.status[c] = "queued", "queued", "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", a, run, "acme/app", true, "pool:sa"))
	eventuallyBox(t, "A on the box", func() bool { return f.jobs(t)[a].State == "serving" && f.liveOn(t, "a") == 1 })
	f.deliver(t, "workflow_job", jobEvent("queued", c, run, "acme/app", true, "pool:sa"))
	eventuallyBox(t, "C waits for room", func() bool { return f.jobs(t)[c].State == "waiting" })
	startOn(t, f, b, run, "potluck-99501", "pool:sa")
	// The one box ready is B's (it was acquired for A); A and C are not served
	// while it is (a serve shows "serving" while its acquire is refused).
	boxes := func() []string {
		hs, err := computeclient.RunTargetHandles(context.Background(), f.owner, "a", true)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, h := range hs {
			if h.State == "ready" { // not an acquire the driver is refusing
				keys = append(keys, h.RequestID[strings.LastIndex(h.RequestID, "gh-"):])
			}
		}
		return keys
	}
	eventuallyBox(t, "B keeps A's box, A waits", func() bool {
		j := f.jobs(t)
		return j[b].State == "serving" && j[b].Runner == "potluck-99501" && j[a].Runner != "potluck-99501" && j[a].State != "done"
	})
	for range 10 { // waiting serves retry meanwhile
		if k := boxes(); !slices.Equal(k, []string{"gh-99501"}) {
			t.Fatalf("with no slot free, boxes %v; want B's alone (acquired for A)", k)
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.gh.mu.Lock()
	f.gh.jobDone[b], f.gh.conclusion[b] = true, "success"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", endEvent(b, run, "acme/app", "success", 1, "pool:sa"))
	eventuallyBox(t, "one of A and C on the freed slot", func() bool {
		k := boxes()
		return f.jobs(t)[b].State == "done" && len(k) == 1 && (strings.HasPrefix(k[0], "gh-99501-") || strings.HasPrefix(k[0], "gh-99503"))
	})
	time.Sleep(300 * time.Millisecond)
	if k := boxes(); len(k) != 1 {
		t.Fatalf("after B ended, boxes %v; want one, A's or C's", k)
	}
}
