package runners_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// labelMap is the fixture pool's runners.labels: neutral names, one a
// case-folded spelling, one pinned to macOS, one naming a size alias.
var labelMap = map[string]config.RunnersLabel{
	"Small-Box": {Size: "c1m2"},
	"big-box":   {Size: "C2M4"},
	"mac-box":   {Size: "c1m2", OS: "macOS"},
	"pinned":    {Size: "alias"},
	"broken":    {Size: "c1m2", OS: "plan9"},
	"twin":      {Size: "c1m2"},
	"TWIN":      {Size: "c2m4"},
	"linux":     {Size: "c2m4"}, // GitHub's own label: never mapped
}

// A mapped label is the pool's on its own, and ParseLabels agrees with the
// broker's own record of a delivery, as for the pool's label.
func TestParseLabelsLabelMap(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   runners.LabelAsk
	}{
		{name: "one mapped label", labels: []string{"big-box"}, want: runners.LabelAsk{Ours: true, Size: "c2m4", OS: "linux"}},
		{name: "mapped label in another case", labels: []string{"self-hosted", "SMALL-BOX"}, want: runners.LabelAsk{Ours: true, Size: "c1m2", OS: "linux"}},
		{name: "mapped os", labels: []string{"mac-box", "arm64"}, want: runners.LabelAsk{Ours: true, Size: "c1m2", OS: "macos", OSNamed: true}},
		{name: "mapped label with its own os", labels: []string{"mac-box", "macos"}, want: runners.LabelAsk{Ours: true, Size: "c1m2", OS: "macos", OSNamed: true}},
		{name: "two mapped labels that agree", labels: []string{"small-box", "mac-box"}, want: runners.LabelAsk{Ours: true, Size: "c1m2", OS: "macos", OSNamed: true}},
		{name: "unmapped label only", labels: []string{"other-box"}},
		{name: "github label mapped", labels: []string{"linux"}},
		{name: "pool size still works", labels: []string{"pool:c2m4"}, want: runners.LabelAsk{Ours: true, Size: "c2m4", OS: "linux"}},
		{name: "two mapped labels disagree", labels: []string{"small-box", "big-box"},
			want: runners.LabelAsk{Ours: true, Size: "c1m2", Why: "its labels ask for different boxes: Small-Box asks size c1m2, big-box asks size c2m4"}},
		{name: "mapped label and pool size disagree", labels: []string{"pool:c1m2", "big-box"},
			want: runners.LabelAsk{Ours: true, Size: "c1m2", Why: "its labels ask for different boxes: pool:c1m2 asks size c1m2, big-box asks size c2m4"}},
		{name: "mapped os and job os disagree", labels: []string{"mac-box", "windows"},
			want: runners.LabelAsk{Ours: true, Size: "c1m2", OS: "macos", Why: "its labels ask for different boxes: mac-box asks os macos, windows asks os windows"}},
		{name: "two keys fold together and disagree", labels: []string{"twin"},
			want: runners.LabelAsk{Ours: true, Size: "c2m4", Why: "its labels ask for different boxes: TWIN asks size c2m4, twin asks size c1m2"}},
		{name: "mapped label and an unserved one", labels: []string{"big-box", "gpu"},
			want: runners.LabelAsk{Ours: true, Size: "c2m4", Why: "it asks for labels the pool does not serve: gpu"}},
		{name: "broken entry", labels: []string{"broken"},
			want: runners.LabelAsk{Ours: true, Why: `runners.labels in config.json cannot be served: broken: os "plan9" is not one of linux, macos, windows`}},
	}
	ctx := context.Background()
	for i, tt := range tests {
		job := int64(9500 + i)
		t.Run(tt.name, func(t *testing.T) {
			f := newPoolFixture(t, func(c *config.Runners) { c.Labels = labelMap })
			if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
				t.Fatalf("link: %d %s", w.Code, w.Body)
			}
			var setup runners.Setup
			if err := f.owner.Do(ctx, "GET", "/v0/runners/pool", nil, &setup); err != nil {
				t.Fatal(err)
			}
			if len(setup.Labels) != len(labelMap) {
				t.Fatalf("GET /v0/runners/pool labels: %v", setup.Labels)
			}
			got := runners.ParseLabels(tt.labels, setup)
			if got != tt.want {
				t.Fatalf("ParseLabels(%v) = %+v, want %+v", tt.labels, got, tt.want)
			}
			if code := f.deliver(t, "workflow_job", jobEvent("queued", job, 950+int64(i), "acme/app", true, tt.labels...)); code != http.StatusAccepted {
				t.Fatalf("deliver: %d", code)
			}
			eventually(t, "delivery applied", func() bool {
				_, recorded := f.jobs(t)[job]
				return recorded || (!got.Ours && f.svc.Undone(ctx) == 0)
			})
			j, recorded := f.jobs(t)[job]
			if recorded != got.Ours {
				t.Fatalf("broker recorded=%v for %v, ParseLabels ours=%v", recorded, tt.labels, got.Ours)
			}
			if got.Ours && got.Why != "" && (j.State != "ignored" || j.Why != got.Why) {
				t.Fatalf("broker recorded %q %q, ParseLabels why %q", j.State, j.Why, got.Why)
			}
		})
	}
}

// A job with one mapped label and nothing else is placed on a box of the
// mapped size; a mapped size alias selects its binding; RunsOn answers the
// mapped label for a box it places on, and the pool's label for one no
// mapped label fits.
func TestLabelMapPlacesOnMappedSize(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) {
		// pool:c1m2 is the pool's own label: RunsOn answers it as the pool's
		// (with the OS), never as a mapped label.
		c.Labels = map[string]config.RunnersLabel{"big-box": {Size: "c2m4"}, "pinned": {Size: "alias"}, "pool:c1m2": {Size: "c1m2"}}
		c.Pool.Sizes = map[string]string{"alias": "c-other"}
	}, box("a-small", "c1m2", "linux", "1"), box("b-big", "c2m4", "linux", "2"), box("c-other", "c1m2", "linux", "1"))
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	f.deliver(t, "workflow_job", jobEvent("queued", 9601, 961, "acme/app", true, "big-box"))
	eventually(t, "job 9601 on b-big", func() bool { return f.liveOn(t, "b-big") == 1 && f.jobs(t)[9601].State == "serving" })
	f.deliver(t, "workflow_job", jobEvent("queued", 9602, 962, "acme/app", true, "pinned"))
	eventually(t, "job 9602 on c-other", func() bool { return f.liveOn(t, "c-other") == 1 && f.jobs(t)[9602].State == "serving" })
	if j := f.jobs(t)[9601]; j.Target != "b-big" {
		t.Errorf("job 9601: %+v", j)
	}

	ctx := context.Background()
	var setup runners.Setup
	if err := f.owner.Do(ctx, "GET", "/v0/runners/pool", nil, &setup); err != nil {
		t.Fatal(err)
	}
	var views computeapi.RunTargetList
	if err := f.owner.Do(ctx, "GET", "/v0/run_targets?describe=1", nil, &views); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"b-big": {"big-box"}, "c-other": {"pinned"}, "a-small": {"pool:c1m2", "linux"}}
	for _, v := range views.Targets {
		labels, ok, why := runners.RunsOn(whenFreeView(v), setup)
		if !ok || !slices.Equal(labels, want[v.Name]) {
			t.Errorf("RunsOn(%s) = %v %v %q, want %v", v.Name, labels, ok, why, want[v.Name])
		}
	}
}

// whenFreeView is v with its box free: RunsOn here asks which labels place
// on it, not whether a job already holds it.
func whenFreeView(v computeapi.RunTargetView) computeapi.RunTargetView {
	if v.Describe != nil {
		d := *v.Describe
		d.Available = true
		v.Describe = &d
	}
	return v
}

// A mapped label never sends a job to a host that is not trusted, even when
// that host is the only one with the mapped size.
func TestLabelMapNeverPicksAPublicHost(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.Labels = map[string]config.RunnersLabel{"big-box": {Size: "c2m4"}} })
	v := func(name, class, size string) computeapi.RunTargetView {
		return computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: name, Kind: "box"}, HostClass: class,
			Describe: &computeapi.RunTargetDescribe{Available: true, Capabilities: []string{"github-jit", "github-jit-guard"},
				Size: &computeapi.RunTargetSize{Name: size, OS: "linux", CPU: 2}}}
	}
	got, why := f.svc.FitLabels([]computeapi.RunTargetView{v("pub@lender", api.MachinePublic, "c2m4"), v("small", api.MachineTrusted, "c1m2")}, []string{"big-box"})
	if len(got) != 0 || !strings.Contains(why, "pub@lender: not a trusted host") {
		t.Errorf("public only: %v %q", got, why)
	}
	got, _ = f.svc.FitLabels([]computeapi.RunTargetView{v("pub@lender", api.MachinePublic, "c2m4"), v("big", api.MachineTrusted, "c2m4")}, []string{"big-box"})
	if !slices.Equal(got, []string{"big"}) {
		t.Errorf("public and trusted: %v", got)
	}
}

// The setup's notices name each runners.labels key the broker never reads
// as mapped and each entry it cannot serve.
func TestLabelMapNotices(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) {
		c.Labels = map[string]config.RunnersLabel{"Linux": {Size: "c2m4"}, "pool:big": {Size: "c2m4"}, "fine": {Size: "c1m2"},
			"bad": {Size: "c1m2", OS: "plan9"}, " sp": {Size: "c1m2"}, "Twin": {Size: "c1m2"}, "twin": {Size: "c2m4"},
			"Same": {Size: "C1M2"}, "same": {Size: "c1m2"}, "LINUX": {Size: "c1m2"}}
	})
	var got runners.Setup
	if err := f.owner.Do(context.Background(), "GET", "/v0/runners/setup", nil, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{`" sp"`, `"LINUX"`, `"Linux"`, `"Twin"`, `"bad"`, `"pool:big"`}
	if len(got.Notices) != len(want) {
		t.Fatalf("notices: %q", got.Notices)
	}
	for i, w := range want {
		if !strings.Contains(got.Notices[i], w) {
			t.Errorf("notice %d: %q, want it to name %s", i, got.Notices[i], w)
		}
	}
	if !strings.Contains(got.Notices[0], "does nothing") || !strings.Contains(got.Notices[1], "does nothing") || !strings.Contains(got.Notices[2], "does nothing") ||
		!strings.Contains(got.Notices[3], `"Twin" and "twin" ask for different boxes`) ||
		!strings.Contains(got.Notices[4], "cannot be served") || !strings.Contains(got.Notices[4], "plan9") {
		t.Errorf("notices: %q", got.Notices)
	}
	if len(got.Labels) != 10 {
		t.Errorf("setup labels: %v", got.Labels)
	}
}

// The doctor reads a mapped label as the broker does: a job naming one is
// the pool's and is judged by placement, and a mapped OS counts as named.
func TestDoctorLabelMap(t *testing.T) {
	for _, tt := range []struct {
		name, runsOn string
		check        int
		status, text string
	}{
		{name: "mapped label with an os", runsOn: "[self-hosted, lin-box]", check: 9, status: "pass"},
		{name: "mapped label placed", runsOn: "lin-box", check: 12, status: "pass"},
		{name: "mapped label without an os", runsOn: "any-box", check: 9, status: "warn", text: "places it on linux"},
		{name: "mapped labels that disagree", runsOn: "[lin-box, big-box]", check: 9, status: "fail", text: "ask for different boxes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := doctorFixture(t, &doctorTransport{}, func(c *config.Runners) {
				c.Labels = map[string]config.RunnersLabel{"lin-box": {Size: "c1m2", OS: "linux"}, "any-box": {Size: "c1m2"}, "big-box": {Size: "c4m16"}}
			})
			f.svc.DoctorRead = doctorReader(strings.ReplaceAll(goodDoctorWorkflow, "[self-hosted, sengi, linux]", tt.runsOn), false)
			got := f.doctor(t, "acme/app").Checks[tt.check-1]
			if got.Status != tt.status || !strings.Contains(got.Message, tt.text) {
				t.Fatalf("check %d: %+v", tt.check, got)
			}
		})
	}
}

// A job recorded under a mapped label whose mapping was removed since is
// ignored, and its row names both settings that could make it the pool's
// (runners.label and runners.labels), not only the pool's label.
func TestRemovedMappedLabelEndsItsJobs(t *testing.T) {
	f := newPoolFixture(t, func(c *config.Runners) { c.Labels = labelMap })
	if w := f.link(t, inst, "good"); w.Code != http.StatusOK {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	ctx := context.Background()
	f.gh.mu.Lock()
	f.gh.status[9981] = "queued"
	f.gh.mu.Unlock()
	f.deliver(t, "workflow_job", jobEvent("queued", 9981, 981, "acme/app", true, "big-box")) // no box of its size: it waits
	eventually(t, "9981 waiting under the mapped label", func() bool { return f.jobs(t)[9981].State == "waiting" })
	f.restart(t, func(c *config.Config) { c.File.Runners.Labels = nil })
	eventually(t, "9981 ignored, saying why", func() bool {
		f.svc.Watch(ctx)
		j := f.jobs(t)[9981]
		return j.State == "ignored" && strings.Contains(j.Why, "neither the pool's label") && strings.Contains(j.Why, "runners set label <name>") && strings.Contains(j.Why, "runners.labels")
	})
	if why := f.jobs(t)[9981].Why; strings.Contains(why, "label is now") {
		t.Fatalf("row says %q: the pool's label did not change", why)
	}
}
