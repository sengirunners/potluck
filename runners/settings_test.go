package runners_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// set is potluck runners set on the machine's socket, as the pool's owner.
func (f *fixture) set(key, value string) (runners.Setup, error) {
	var st runners.Setup
	return st, f.owner.Do(context.Background(), "POST", "/v0/runners/settings", runners.SetRequest{Key: key, Value: value}, &st)
}

// Each setting changes with one call and the next job gets it: no restart,
// no hand edit. It is in config.json (0600, other keys kept) for the next
// start.
func TestSettingsApplyToTheNextJob(t *testing.T) {
	f := pinned(t, "a", "b", "c")
	ctx := context.Background()
	path := filepath.Join(f.home, "config.json")
	// A key this version does not know, as another version left it.
	raw, _ := os.ReadFile(path)
	doc := map[string]any{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["from_another_version"] = map[string]any{"kept": true}
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	f.deliver(t, "workflow_job", jobEvent("queued", 9902, 902, "acme/app", true, "pool:sa"))
	eventually(t, "a job served on box a", func() bool {
		return f.jobs(t)[9902].State == "serving" && f.liveOn(t, "a") == 1
	})

	// The fallback runtime plus 300s startup must fit the host. Its 3600s
	// ceiling refuses a 5400s runtime plus startup before acquiring a box.
	eventually(t, "before the change, a box asked for 3300 s", func() bool {
		b, _ := os.ReadFile(f.seen) // the tap's own write may trail the answer
		return strings.Contains(string(b), `"ttl_s":3300`)
	})
	st, err := f.set("job_ttl_s", "5400")
	if err != nil || st.JobTTLS != 5400 {
		t.Fatalf("set job_ttl_s 5400: %+v, %v", st, err)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 9903, 903, "acme/app", true, "pool:sb"))
	eventually(t, "the next job's box asked for 5400 s", func() bool {
		j := f.jobs(t)[9903]
		return j.State == "waiting" && strings.Contains(j.Why, "5700")
	})

	// label: the next job asks for the pool by its new label; the old one is
	// no longer the pool's.
	if st, err = f.set("job_ttl_s", "1800"); err != nil || st.JobTTLS != 1800 {
		t.Fatalf("set job_ttl_s 1800: %+v, %v", st, err)
	}
	if st, err = f.set("label", "ci"); err != nil || st.Label != "ci" {
		t.Fatalf("set label ci: %+v, %v", st, err)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 9904, 904, "acme/app", true, "pool:sc"))
	f.deliver(t, "workflow_job", jobEvent("queued", 9905, 905, "acme/app", true, "ci:sc"))
	eventually(t, "the job asking for ci served on box c", func() bool {
		return f.jobs(t)[9905].State == "serving" && f.liveOn(t, "c") == 1 && f.svc.Undone(ctx) == 0
	})
	if j, ok := f.jobs(t)[9904]; ok {
		t.Errorf("a job asking for the old label was taken as the pool's: %+v", j)
	}
	if b, _ := os.ReadFile(f.seen); !strings.Contains(string(b), `"ttl_s":2100`) {
		t.Errorf("job 9905's box was not asked for 2100 s: %s", b)
	}

	// Refused values change nothing.
	for _, bad := range [][2]string{{"job_ttl_s", "ninety"}, {"job_ttl_s", "899"}, {"job_ttl_s", "999999999"}, {"label", "linux"}, {"label", "a b"},
		{"label", ""}, {"allow_public_repos", "on"}, {"allow_fork_prs", "on"}, {"listen", "0.0.0.0:1"}} {
		if _, err := f.set(bad[0], bad[1]); err == nil {
			t.Errorf("set %s %q was taken", bad[0], bad[1])
		}
	}
	var now runners.Setup
	if err := f.owner.Do(ctx, "GET", "/v0/runners/setup", nil, &now); err != nil {
		t.Fatal(err)
	}
	if now.JobTTLS != 1800 || now.Label != "ci" {
		t.Errorf("the setup now: %+v", now)
	}

	// On disk for the next start: what a daemon starting now reads.
	fresh, err := config.LoadFrom(f.home)
	if err != nil {
		t.Fatal(err)
	}
	if r := fresh.File.Runners; r.JobTTLS != 1800 || r.Label != "ci" || r.Pool.Project != "pool" || r.GitHub.AppID != 1 {
		t.Errorf("config.json runners: %+v", r)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "from_another_version") {
		t.Errorf("a key of another version was dropped: %s", raw)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.json mode: %v, %v", fi.Mode(), err)
	}
}

// A setup that names no job_ttl_s says the default a job gets.
func TestSetupSaysTheDefaultJobLimit(t *testing.T) {
	f := newPoolFixture(t, func(r *config.Runners) { r.JobTTLS = 0 })
	var st runners.Setup
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/setup", nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.JobTTLS != 3600 {
		t.Errorf("setup: %+v", st)
	}
}

// A prematurely expired lease is host loss, not the job's runtime limit.
func TestExpiredLeaseBeforeRuntimeLimitIsHostLoss(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	f.serveOn(t, 9951, 951, "a")
	f.started(t, 9951, 951, "acme/app", "potluck-9951")
	eventually(t, "start applied", func() bool { return f.svc.Undone(ctx) == 0 })
	f.svc.AnswerStatus(func(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
		if err == nil {
			h.State = "expired"
		}
		return h, err
	})
	eventually(t, "early expiry is unexpected loss", func() bool { f.svc.Watch(ctx); return f.jobs(t)[9951].State == "lost" })
	if why := f.jobs(t)[9951].Why; !strings.Contains(why, "unexpected host loss") || strings.Contains(why, "stopped at its time limit") {
		t.Fatal(why)
	}
}

// renews are the ttl_s of each renew the box driver was sent, in order.
func (f *fixture) renews(t *testing.T) (ttls []int) {
	t.Helper()
	b, _ := os.ReadFile(f.seen)
	for dec := json.NewDecoder(bytes.NewReader(b)); ; {
		var q struct {
			Verb string `json:"verb"`
			Args struct {
				TTL int `json:"ttl_s"`
			} `json:"args"`
		}
		if dec.Decode(&q) != nil {
			break
		}
		if q.Verb == "renew" {
			ttls = append(ttls, q.Args.TTL)
		}
	}
	return ttls
}

// A job that runs keeps the limit it was served with: its renewals carry
// that, whether job_ttl_s is lowered or raised meanwhile. The change is the
// next job's.
func TestRunningJobKeepsItsLimit(t *testing.T) {
	f := pinned(t, "a")
	ctx := context.Background()
	if _, err := f.set("job_ttl_s", "1800"); err != nil {
		t.Fatal(err)
	}
	f.serveOn(t, 9961, 961, "a")
	// Lowered to the least there is: a lease still outlives three sweeps.
	for _, low := range []string{"60", "899"} {
		if _, err := f.set("job_ttl_s", low); err == nil || !strings.Contains(err.Error(), "from 900 to") {
			t.Errorf("set job_ttl_s %s: %v; want it refused, the least being three sweeps (900 s)", low, err)
		}
	}
	if st, err := f.set("job_ttl_s", "900"); err != nil || st.JobTTLS != 900 {
		t.Fatalf("set job_ttl_s 900: %+v, %v", st, err)
	}
	f.svc.Sweep(ctx)
	if got := f.renews(t); !slices.Equal(got, []int{2100}) {
		t.Errorf("after job_ttl_s was lowered to 900, the running job's renewals: %v; want its original 2100s lease budget", got)
	}
	// Raised past what its box may live (3600 s): its renewals go on.
	if _, err := f.set("job_ttl_s", "5400"); err != nil {
		t.Fatal(err)
	}
	f.svc.Sweep(ctx)
	if got := f.renews(t); !slices.Equal(got, []int{2100, 2100}) {
		t.Fatalf("after job_ttl_s was raised to 5400, the running job's renewals: %v; want its original 2100s lease budget again", got)
	}
}

// A lease that cannot be renewed in full under its host's lifetime cap is
// renewed up to the cap, not left one renewal short of it.
func TestRenewGoesUpToTheHostsLimit(t *testing.T) {
	f := pinned(t, "a")
	if _, err := f.set("job_ttl_s", "3300"); err != nil {
		t.Fatal(err)
	}
	f.serveOn(t, 9962, 962, "a") // 3600 s on a box that lives 3600 s at most
	f.svc.Sweep(context.Background())
	if got := f.renews(t); len(got) != 1 || got[0] <= 3500 || got[0] >= 3600 {
		t.Fatalf("renewals %v; want one, for what is left of the box's 3600 s", got)
	}
}

// After the label changes, a job recorded under the old label is not served
// any more, and never placed without its size: its row says why. label
// default puts the pool's own name back.
func TestLabelChangeEndsJobsOfTheOldLabel(t *testing.T) {
	f := pinned(t, "a", "b")
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.status[9971], f.gh.status[9972] = "queued", "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9972, 972, "acme/app", true, "pool:sz")) // no box of that size: it waits
	eventually(t, "9972 waiting for a box", func() bool { return f.jobs(t)[9972].State == "waiting" })
	f.deliver(t, "workflow_job", jobEvent("queued", 9971, 971, "acme/app", true, "pool:sa"))
	eventually(t, "9971 on box a", func() bool { return f.jobs(t)[9971].State == "serving" && f.liveOn(t, "a") == 1 })
	if st, err := f.set("label", "ci"); err != nil || st.Label != "ci" {
		t.Fatalf("set label ci: %+v, %v", st, err)
	}
	gone := func(id int64) func() bool {
		return func() bool {
			j := f.jobs(t)[id]
			return j.State == "ignored" && strings.Contains(j.Why, "no longer name the pool") && strings.Contains(j.Why, `"ci"`) && strings.Contains(j.Why, "runners set label <name>)")
		}
	}
	// 9971's box goes before the job started: it would be served again.
	f.svc.AnswerStatus(func(_ string, h computeapi.RunTargetHandle, err error) (computeapi.RunTargetHandle, error) {
		if err == nil {
			h.State = "expired"
		}
		return h, err
	})
	eventually(t, "the job to serve again ends, saying why", func() bool { f.svc.Watch(ctx); return gone(9971)() })
	f.svc.AnswerStatus(nil)
	eventually(t, "the waiting job of the old label ends, saying why", gone(9972))
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
	if got := f.minted(); slices.Contains(got, "potluck-9971-2") {
		t.Errorf("a job of the old label was served again: runners %v", got)
	}

	st, err := f.set("label", "default")
	if err != nil || st.Label != "pool" {
		t.Fatalf("set label default: %+v, %v; want the pool's own name", st, err)
	}
	if fresh, err := config.LoadFrom(f.home); err != nil || fresh.File.Runners.Label != "" {
		t.Errorf("config.json label after default: %q, %v; want none", fresh.File.Runners.Label, err)
	}
}
