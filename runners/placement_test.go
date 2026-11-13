package runners_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/imagepins"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// box is a ref box binding with the driver's test knobs: its size, OS,
// CPUs, whether it serves GitHub runners (jit) and whether acquire refuses.
func box(name, size, os, cpu string, more ...string) computeapi.RunTarget {
	c := map[string]string{"size": size, "os": os, "cpu": cpu, "jit": "1"}
	for i := 0; i+1 < len(more); i += 2 {
		c[more[i]] = more[i+1]
	}
	return computeapi.RunTarget{Name: name, Config: c}
}

func (f *fixture) jobs(t *testing.T) map[int64]runners.JobView {
	t.Helper()
	var out struct {
		Jobs []runners.JobView `json:"jobs"`
	}
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/jobs", nil, &out); err != nil {
		t.Fatal(err)
	}
	m := map[int64]runners.JobView{}
	for _, j := range out.Jobs {
		m[j.ID] = j
	}
	return m
}

func (f *fixture) liveOn(t *testing.T, target string) int {
	hs, err := computeclient.RunTargetHandles(context.Background(), f.owner, target, true)
	if err != nil {
		t.Fatal(err)
	}
	return len(hs)
}

func (f *fixture) minted() (names []string) {
	m, _ := f.gh.snapshot()
	for _, x := range m {
		names = append(names, fmt.Sprint(x["name"]))
	}
	return names
}

// A pool with no sizes: a job goes to any box that serves runners and fits
// its size and OS, smallest first, the next when one refuses; the label is
// the pool's name, with or without self-hosted. The config is what zm runners
// setup leaves: no label, no sizes, no default_size.
func TestPlacementAcrossBoxes(t *testing.T) {
	f := newPoolFixture(t, func(*config.Runners) {},
		box("a-big", "c2m4", "linux", "2"),
		box("b-small", "c1m2", "linux", "1", "refuse", "1"),
		box("c-small", "c1m2", "linux", "1"),
		box("d-mac", "c1m2", "darwin", "1"),
		box("e-nojit", "c1m2", "linux", "1", "jit", "0"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	f.deliver(t, "workflow_job", jobEvent("queued", 8001, 130, "acme/app", true, "pool"))
	eventually(t, "job 8001 on c-small", func() bool { return f.liveOn(t, "c-small") == 1 && f.jobs(t)[8001].State == "serving" })
	if got := f.minted(); !slices.Equal(got, []string{"potluck-8001", "potluck-8001-2"}) {
		t.Fatalf("minted %v, want zm-8001 (refused by b-small, deleted) then zm-8001-2", got)
	}
	if _, del := f.gh.snapshot(); !slices.Equal(del, []string{"potluck-8001"}) {
		t.Errorf("deleted %v, want the refused box's runner", del)
	}
	eventually(t, "the second acquire's request id gh-8001-2", func() bool {
		b, _ := os.ReadFile(f.seen)
		return strings.Contains(string(b), `gh-8001-2"`)
	})
	f.deliver(t, "workflow_job", jobEvent("queued", 8002, 131, "acme/app", true, "pool:c2m4"))
	f.deliver(t, "workflow_job", jobEvent("queued", 8003, 132, "acme/app", true, "self-hosted", "pool", "macOS", "ARM64"))
	eventually(t, "sized and macOS jobs placed", func() bool { return f.liveOn(t, "a-big") == 1 && f.liveOn(t, "d-mac") == 1 })
	// Recorded with why: labels the pool does not serve, a size no box
	// serves (waiting, not dropped); another runner's job is not recorded.
	f.deliver(t, "workflow_job", jobEvent("queued", 8004, 133, "acme/app", true, "pool", "gpu"))
	f.deliver(t, "workflow_job", jobEvent("queued", 8005, 134, "acme/app", true, "pool:c8m32"))
	f.deliver(t, "workflow_job", jobEvent("queued", 8006, 135, "acme/app", true, "self-hosted", "sengi"))
	eventually(t, "the refusals recorded", func() bool {
		js := f.jobs(t)
		return js[8004].State == "ignored" && js[8005].State == "waiting" && strings.Contains(js[8005].Why, "no box fits")
	})
	js := f.jobs(t)
	if !strings.Contains(js[8004].Why, "gpu") || !strings.Contains(js[8005].Why, "want c8m32") || !strings.Contains(js[8005].Why, "e-nojit") ||
		!strings.HasPrefix(js[8005].Why, "no host in the pool shares size c8m32 (shared now: c1m2, c2m4): ") {
		t.Errorf("whys: %q, %q", js[8004].Why, js[8005].Why)
	}
	if _, ok := js[8006]; ok {
		t.Errorf("another runner's job was recorded: %+v", js[8006])
	}
	if j := js[8001]; j.State != "serving" || j.Target != "c-small" || j.Runner != "potluck-8001-2" {
		t.Errorf("job 8001 as listed: %+v", j)
	}
	if n := len(f.minted()); n != 4 {
		t.Errorf("minted %d runners, want 4", n)
	}
	// Another area's member sees none of the root area link's jobs.
	if _, err := f.owner.AddArea(ctx, "pool", api.AddAreaRequest{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "bo", Project: "pool", Area: "b", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	bo := client.WithHandler(f.dm.Handler)
	bo.Token, bo.Project, bo.ProjectFrom = tok.Secret, "pool", "flag"
	var other struct {
		Jobs []runners.JobView `json:"jobs"`
	}
	if err := bo.Do(ctx, "GET", "/v0/runners/jobs", nil, &other); err != nil || len(other.Jobs) != 0 {
		t.Errorf("area b's member listed %d jobs (%v), want none", len(other.Jobs), err)
	}
}

// A serve that finds no box keeps trying while GitHub has the job queued
// (no fixed number of tries), without asking GitHub on every try, and is
// served once a box is lent.
func TestWaitingServeIsServedWhenABoxComes(t *testing.T) {
	f := newPoolFixture(t, func(*config.Runners) {}, box("a", "c1m2", "linux", "1", "refuse", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	f.deliver(t, "workflow_job", jobEvent("queued", 8101, 140, "acme/app", true, "pool"))
	eventually(t, "waiting", func() bool { return strings.Contains(f.jobs(t)[8101].Why, "no box took it") })
	time.Sleep(1500 * time.Millisecond) // past maxTries retries at WaitMax apart
	j := f.jobs(t)[8101]
	if j.State != "waiting" || !strings.Contains(j.Why, "no box") || f.svc.Undone(ctx) != 1 {
		t.Fatalf("after many tries: %+v (undone %d)", j, f.svc.Undone(ctx))
	}
	// A box that says available but refuses is skipped for RefuseCool after
	// each refusal: a runner minted and deleted at most that often.
	if n := len(f.minted()); n > 1500/300+2 {
		t.Errorf("minted %d runners in 1.5 s with RefuseCool 300 ms", n)
	}
	f.gh.mu.Lock()
	jobGets := 0
	for _, c := range f.gh.calls {
		if strings.HasSuffix(c, "/actions/jobs/8101") {
			jobGets++
		}
	}
	f.gh.mu.Unlock()
	if jobGets != 1 {
		t.Errorf("GitHub was asked about the job %d times while it waited, want once (CheckEvery)", jobGets)
	}
	b := box("z", "c1m2", "linux", "1")
	b.Driver, b.Kind, b.Isolation, b.Images = "ref", "box", "ephemeral", []string{refPin}
	b.Config["remote"] = f.remote
	if _, err := computeclient.BindRunTarget(ctx, f.owner, computeapi.BindRunTargetRequest{Target: b}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "served on the new box", func() bool { return f.liveOn(t, "z") == 1 && f.svc.Undone(ctx) == 0 })
	if j := f.jobs(t)[8101]; j.State != "serving" || j.Target != "z" {
		t.Errorf("job: %+v", j)
	}
}

// A low API budget: a waiting serve makes no GitHub call until the reset.
func TestWaitingServeSparesALowAPIBudget(t *testing.T) {
	f := newPoolFixture(t, func(*config.Runners) {}, box("a", "c1m2", "linux", "1", "refuse", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.gh.mu.Lock()
	f.gh.rateLeft = 40
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 8201, 150, "acme/app", true, "pool"))
	eventually(t, "waiting on the budget", func() bool { return strings.Contains(f.jobs(t)[8201].Why, "API budget") })
	f.gh.mu.Lock()
	n := len(f.gh.calls)
	f.gh.mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	f.gh.mu.Lock()
	later := f.gh.calls[n:]
	f.gh.mu.Unlock()
	if len(later) != 0 {
		t.Errorf("calls while the budget was low: %v", later)
	}
}

// A box that ends before its job starts (here its Mac's owner reclaims it)
// is no one's: the job is served again on another box while GitHub still
// has it queued, its first runner deleted. One the job started on is left.
func TestBoxEndedBeforeStartIsServedAgain(t *testing.T) {
	now := time.Now()
	if now.Hour() == 23 && now.Minute() == 59 {
		t.Skip("the reclaim window would cross midnight")
	}
	window := now.Add(-time.Minute).Format("15:04:05") + "-" + now.Add(12*time.Second).Format("15:04:05")
	f := newPoolFixture(t, func(*config.Runners) {}, box("a", "c1m2", "linux", "1", "window", window), box("b", "c1m2", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.status[8301], f.gh.status[8302] = "queued", "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 8301, 160, "acme/app", true, "pool"))
	eventually(t, "on a", func() bool { return f.liveOn(t, "a") == 1 })
	f.deliver(t, "workflow_job", jobEvent("queued", 8302, 161, "acme/app", true, "pool"))
	eventually(t, "8302 on b (the least busy)", func() bool { return f.liveOn(t, "b") == 1 })
	f.deliver(t, "workflow_job", startEvent(8302, 161, "acme/app", "potluck-8302"))
	eventually(t, "applied", func() bool { return f.svc.Undone(ctx) == 0 })
	time.Sleep(time.Until(now.Add(13 * time.Second)))
	eventually(t, "job 8301 served again on b", func() bool {
		f.svc.Watch(ctx)
		return f.liveOn(t, "b") == 2 && f.svc.Undone(ctx) == 0
	})
	j := f.jobs(t)[8301]
	if j.State != "serving" || j.Target != "b" || j.Runner != "potluck-8301-2" {
		t.Errorf("job 8301: %+v", j)
	}
	if _, del := f.gh.snapshot(); !slices.Equal(del, []string{"potluck-8301"}) {
		t.Errorf("deleted %v, want the lost box's runner", del)
	}
	if j := f.jobs(t)[8302]; j.State != "serving" || j.Runner != "potluck-8302" {
		t.Errorf("a started job was served again: %+v", j)
	}
}

// Serves run side by side: one stuck at its run check holds no other.
func TestServesRunSideBySide(t *testing.T) {
	f := newPoolFixture(t, func(*config.Runners) {}, box("a", "c1m2", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	gate := make(chan struct{})
	f.gh.mu.Lock()
	f.gh.gate, f.gh.gateOn = gate, "/actions/runs/170"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 8401, 170, "acme/app", true, "pool"))
	eventually(t, "the first serve held", func() bool { f.gh.mu.Lock(); defer f.gh.mu.Unlock(); return f.gh.waiting == 1 })
	f.deliver(t, "workflow_job", jobEvent("queued", 8402, 171, "acme/app", true, "pool"))
	eventually(t, "the second served meanwhile", func() bool { return f.liveOn(t, "a") == 1 })
	f.gh.mu.Lock()
	f.gh.gate = nil
	f.gh.mu.Unlock()
	close(gate)
	eventually(t, "both", func() bool { return f.liveOn(t, "a") == 2 })
}

// Placement never ranks a box on a host that is not trusted, or one that
// serves no guarded runner; macos is a darwin box.
func TestFitsOnlyTrustedRunnerBoxes(t *testing.T) {
	v := func(name, class, os string, caps ...string) computeapi.RunTargetView {
		return computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: "box"}, HostClass: class,
			Describe: &computeapi.RunTargetDescribe{Available: true, Capabilities: caps, Size: &computeapi.RunTargetSize{Name: "c1m2", OS: os, CPU: 1}}}
	}
	jit := []string{"github-jit", "github-jit-guard"}
	views := []computeapi.RunTargetView{v("pub@lender", api.MachinePublic, "linux", jit...), v("unguarded", api.MachineTrusted, "linux", "github-jit"),
		v("mac@friend", api.MachineTrusted, "darwin", jit...), v("", "", "linux", jit...), v("lin", api.MachineTrusted, "linux", jit...)}
	if got, _ := runners.Fits(views, "", ""); !slices.Equal(got, []string{"lin", "mac@friend"}) {
		t.Errorf("any: %v", got)
	}
	if got, _ := runners.Fits(views, "c1m2", "macos"); !slices.Equal(got, []string{"mac@friend"}) {
		t.Errorf("macos: %v", got)
	}
	got, why := runners.Fits(views[:2], "", "")
	if len(got) != 0 || !strings.Contains(why, "pub@lender: not a trusted host") || !strings.Contains(why, "unguarded: serves no GitHub runner") {
		t.Errorf("public and unguarded: %v %q", got, why)
	}
}

// Xcode jobs must wait rather than boot the base Mac, including when a size
// alias points at the base Mac. A secondary allowed pin is insufficient.
func TestPlacementXcode(t *testing.T) {
	var xcode string
	for _, p := range imagepins.DefaultPins() {
		if p.Name == "xcode" {
			xcode = p.Image
		}
	}
	for _, mode := range []string{"normal", "mapped-base", "mapped-missing"} {
		mapped := mode != "normal"
		t.Run(mode, func(t *testing.T) {
			base := box("a-base", "c2m4", "darwin", "2")
			base.Images = []string{refPin, xcode}
			xc := box("b-xcode", "c2m4", "darwin", "2")
			xc.Images = []string{xcode}
			xc.Config["image_name"] = "xcode"
			f := newPoolFixture(t, func(c *config.Runners) {
				if mapped {
					c.Pool.Sizes = map[string]string{"mac": "a-base"}
					if mode == "mapped-missing" {
						c.Pool.Sizes["mac"] = "missing-mac"
					}
				}
			}, base, xc)
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatal(w.Body)
			}
			label := "pool"
			if mapped {
				label = "pool:mac"
			}
			f.deliver(t, "workflow_job", jobEvent("queued", 8901, 190, "acme/app", true, label, "macos", "xcode"))
			if mapped {
				eventually(t, "mapped base refused", func() bool {
					j := f.jobs(t)[8901]
					reason := "default image is not xcode"
					if mode == "mapped-missing" {
						reason = "missing-mac (mapped binding) is not listed"
					}
					return j.State == "waiting" && strings.Contains(j.Why, reason)
				})
				if len(f.minted()) != 0 {
					t.Fatal("minted for base Mac")
				}
			} else {
				eventually(t, "Xcode job placed on Xcode Mac", func() bool { return f.jobs(t)[8901].State == "serving" && f.jobs(t)[8901].Target == "b-xcode" })
				f.svc.HideTargets("b-xcode")
				f.deliver(t, "workflow_job", jobEvent("queued", 8902, 191, "acme/app", true, "pool:c2m4", "macos", "xcode"))
				eventually(t, "no Xcode Mac waits", func() bool {
					j := f.jobs(t)[8902]
					return j.State == "waiting" && strings.Contains(j.Why, "default image is not xcode")
				})
			}
			if f.liveOn(t, "a-base") != 0 {
				t.Fatal("Xcode job booted a base Mac")
			}
		})
	}
}

// Remote hosts may publish a different pin version than the broker. The name
// must survive transport; the broker never resolves the host's digest.
func TestPlacementNamedImageRemoteViews(t *testing.T) {
	views := []computeapi.RunTargetView{}
	for _, c := range []struct{ name, digest, image, class string }{
		{"z-base@alice", refPin, "", api.MachineTrusted},
		{"a-xcode@bob", "old.example/xcode@sha256:" + strings.Repeat("a", 64), "xcode", api.MachineTrusted},
		{"future@mac", "future.example/xcode@sha256:" + strings.Repeat("b", 64), "xcode", api.MachineTrusted},
		{"old@mac", "", "", api.MachineTrusted},
		{"public@eve", refPin, "xcode", api.MachinePublic},
	} {
		views = append(views, computeapi.RunTargetView{
			RunTarget: computeapi.RunTarget{Name: c.name, Kind: "box", Images: []string{c.digest}}, HostClass: c.class,
			Describe: &computeapi.RunTargetDescribe{ImageName: c.image, Available: true,
				Capabilities: []string{"github-jit", "github-jit-guard"},
				Size:         &computeapi.RunTargetSize{Name: "c2m4", OS: "darwin", CPU: 2, MemoryMB: 4096}},
		})
	}
	got, why := runners.Fits(views, "c2m4", "macos", "xcode")
	if !slices.Equal(got, []string{"a-xcode@bob", "future@mac"}) {
		t.Fatalf("placed %v: %s", got, why)
	}
	got, why = runners.Fits(views[:2], "c2m4", "macos")
	if !slices.Equal(got, []string{"z-base@alice", "a-xcode@bob"}) {
		t.Fatalf("plain Mac ordering %v: %s", got, why)
	}
	got, why = runners.Fits(append(views[:1:1], views[3:]...), "c2m4", "macos", "xcode")
	if len(got) != 0 || !strings.Contains(why, "default image is not xcode") {
		t.Fatalf("fallback %v: %s", got, why)
	}
}
