package runners_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

func statusView(name, os string, edit func(*computeapi.RunTargetView)) computeapi.RunTargetView {
	v := computeapi.RunTargetView{
		RunTarget: computeapi.RunTarget{Name: name, Kind: "box", TTLMaxS: 14400},
		HostClass: api.MachineTrusted,
		Describe: &computeapi.RunTargetDescribe{Available: true, TTLMaxS: 14400,
			Capabilities: []string{"github-jit", "github-jit-guard"},
			Size:         &computeapi.RunTargetSize{Name: "c4m16", OS: os}},
	}
	if edit != nil {
		edit(&v)
	}
	return v
}

var statusSetup = runners.Setup{Pool: "home", Label: "home", JobTTLS: 6000}

const awayWhy = "reclaimed: the owner is active (idle 41s, under 300s)"

func away(v *computeapi.RunTargetView) {
	v.Describe.Available = false
	v.Describe.Why = awayWhy
}

// A box whose owner is using the machine is lent and fits: it keeps its
// labels, and the reason it cannot take a job now is the driver's.
func TestEligibleKeepsLabelsOfABoxThatIsAway(t *testing.T) {
	v := statusView("mac-c4m16", "darwin", away)
	if _, ok, _ := runners.RunsOn(v, statusSetup); ok {
		t.Fatal("RunsOn must stay a placement snapshot: an away box takes no job now")
	}
	e := runners.Eligible(v, statusSetup)
	if e.Now || !slices.Equal(e.Labels, []string{"home:c4m16", "macos"}) || !strings.Contains(e.Why, awayWhy) {
		t.Fatalf("Eligible = %+v", e)
	}
	if v.Describe.Available {
		t.Fatal("Eligible changed the caller's view")
	}
}

func TestEligibleRefusalsStayRefusals(t *testing.T) {
	for name, edit := range map[string]func(*computeapi.RunTargetView){
		"away and too short":  func(v *computeapi.RunTargetView) { away(v); v.TTLMaxS = 3600 },
		"away, no github":     func(v *computeapi.RunTargetView) { away(v); v.Describe.Capabilities = nil },
		"away, public host":   func(v *computeapi.RunTargetView) { away(v); v.HostClass = api.MachinePublic },
		"public, no describe": func(v *computeapi.RunTargetView) { v.Describe = nil; v.HostClass = api.MachinePublic },
	} {
		t.Run(name, func(t *testing.T) {
			e := runners.Eligible(statusView("box", "darwin", edit), statusSetup)
			if e.Labels != nil || e.Now || e.Why == "" {
				t.Fatalf("Eligible = %+v", e)
			}
		})
	}
	if e := runners.Eligible(statusView("box", "darwin", nil), statusSetup); !e.Now || e.Why != "" || e.Labels == nil {
		t.Fatalf("a free box: %+v", e)
	}
}

// A spent budget clears with its period and a failed describe is not an
// answer: neither makes a box one that cannot serve.
func TestEligibleSpentBudgetAndFailedDescribe(t *testing.T) {
	spent := 5.0
	v := statusView("mac", "darwin", func(v *computeapi.RunTargetView) {
		v.Budget = &computeapi.RunTargetBudget{Limit: 1, Unit: "slot_min", Period: "day"}
		v.BudgetSpent = &spent
	})
	if _, ok, _ := runners.RunsOn(v, statusSetup); ok {
		t.Fatal("a spent budget must refuse a job now")
	}
	if e := runners.Eligible(v, statusSetup); e.Labels == nil || e.Now || e.Why == "" {
		t.Fatalf("spent budget: %+v", e)
	}
	for name, edit := range map[string]func(*computeapi.RunTargetView){
		"error":       func(v *computeapi.RunTargetView) { v.Error = "ssh: timeout"; v.Describe = nil },
		"no describe": func(v *computeapi.RunTargetView) { v.Describe = nil },
	} {
		e := runners.Eligible(statusView("mac", "darwin", edit), statusSetup)
		if !e.Unknown || e.Labels != nil || e.Why == "" {
			t.Fatalf("%s: %+v", name, e)
		}
	}
}

func TestStatusOrderAndReadiness(t *testing.T) {
	big, small := statusView("big", "linux", func(v *computeapi.RunTargetView) { v.Describe.Size.Name = "c16m64" }),
		statusView("small", "linux", func(v *computeapi.RunTargetView) { v.Describe.Size.Name = "c2m4" })
	big.Describe.Size.CPU, big.Describe.Size.MemoryMB = 16, 65536
	small.Describe.Size.CPU, small.Describe.Size.MemoryMB = 2, 4096
	if st := runners.Status([]computeapi.RunTargetView{big, small}, statusSetup); st.RunsOn != `runs-on: ["self-hosted","linux","home:c2m4"]` {
		t.Fatalf("placement's order is smallest first, not text order: %+v", st)
	}
	mac := statusView("mac", "darwin", away)
	st := runners.Status([]computeapi.RunTargetView{statusView("dev", "linux", nil), mac}, statusSetup)
	if !strings.Contains(st.RunsOn, "macos") {
		t.Fatalf("a Mac is named first: %+v", st)
	}
	macFree, macAway := statusView("a", "darwin", nil), statusView("b", "darwin", away)
	macFree.Describe.Size.CPU, macAway.Describe.Size.CPU = 8, 1
	macFree.Describe.Size.Name = "c8m16"
	if st := runners.Status([]computeapi.RunTargetView{macAway, macFree}, statusSetup); !strings.Contains(st.RunsOn, "home:c8m16") {
		t.Fatalf("of two Macs, the free one: %+v", st)
	}
	if st := runners.Status([]computeapi.RunTargetView{mac}, runners.Setup{Pool: "home"}); st.NotReady == "" || st.RunsOn != "" || len(st.Refused) != 0 {
		t.Fatalf("an unconfigured pool is not the machine's fault: %+v", st)
	}
	st = runners.Status([]computeapi.RunTargetView{statusView("m", "darwin", func(v *computeapi.RunTargetView) { v.Error = "timeout"; v.Describe = nil })}, statusSetup)
	if st.RunsOn != "" || len(st.Unknown) != 1 || len(st.Refused) != 0 {
		t.Fatalf("a box that did not describe itself: %+v", st)
	}
}

func TestStatus(t *testing.T) {
	m4, m2 := statusView("mac-c4m16", "darwin", away), statusView("mac-c2m4", "darwin", away)
	m4.Describe.Size.CPU, m2.Describe.Size.CPU = 4, 2
	m2.Describe.Size.Name = "c2m4"
	macs := []computeapi.RunTargetView{m4, m2}
	linux := statusView("dev-c1m2", "linux", nil)

	st := runners.Status(macs, statusSetup)
	if st.RunsOn != `runs-on: ["self-hosted","macos","home:c2m4"]` || len(st.Waiting) != 2 || len(st.Refused) != 0 ||
		!strings.HasPrefix(st.Waiting[0], "mac-c2m4: ") || !strings.Contains(st.Waiting[0], awayWhy) {
		t.Fatalf("Status(away macs) = %+v", st)
	}

	st = runners.Status(append(slices.Clone(macs), linux), statusSetup)
	if !strings.Contains(st.RunsOn, `"macos"`) || len(st.Waiting) != 2 {
		t.Fatalf("a Mac comes first even when away: %+v", st)
	}

	st = runners.Status([]computeapi.RunTargetView{linux}, statusSetup)
	if !strings.Contains(st.RunsOn, `"linux"`) || len(st.Waiting) != 0 {
		t.Fatalf("Status(free linux) = %+v", st)
	}

	st = runners.Status([]computeapi.RunTargetView{statusView("x", "darwin", func(v *computeapi.RunTargetView) { v.Describe.Capabilities = nil })}, statusSetup)
	if st.RunsOn != "" || len(st.Refused) != 1 || !strings.Contains(st.Refused[0], "x: ") {
		t.Fatalf("Status(refused) = %+v", st)
	}

	if st := runners.Status(nil, statusSetup); st.RunsOn != "" || len(st.Waiting)+len(st.Refused) != 0 {
		t.Fatalf("Status(nothing lent) = %+v", st)
	}
}

// A pool with a label map answers a mapped label for a box it places a job
// on: the runs-on line names it alone after self-hosted, and a Mac a mapped
// label reaches is still named first.
func TestStatusNamesAMappedLabel(t *testing.T) {
	setup := statusSetup
	setup.Labels = map[string]config.RunnersLabel{
		"big-linux": {Size: "c4m16"},
		"big-mac":   {Size: "c4m16", OS: "macos"},
	}
	linux := statusView("dev", "linux", nil)
	linux.Describe.Size.CPU, linux.Describe.Size.MemoryMB = 4, 16384
	if st := runners.Status([]computeapi.RunTargetView{linux}, setup); st.RunsOn != `runs-on: ["self-hosted","big-linux"]` {
		t.Fatalf("Status(mapped linux) = %+v", st)
	}
	mac := statusView("mac", "darwin", away)
	mac.Describe.Size.CPU, mac.Describe.Size.MemoryMB = 4, 16384
	if st := runners.Status([]computeapi.RunTargetView{linux, mac}, setup); st.RunsOn != `runs-on: ["self-hosted","big-linux"]` && st.RunsOn != `runs-on: ["self-hosted","big-mac"]` {
		t.Fatalf("Status(mapped linux and mac) = %+v", st)
	} else if !strings.Contains(st.RunsOn, "big-mac") {
		t.Fatalf("a Mac is named first: %+v", st)
	}
	// A box no mapped label places a job on keeps the pool label's line.
	small := statusView("small", "linux", func(v *computeapi.RunTargetView) { v.Describe.Size.Name = "c2m4" })
	small.Describe.Size.CPU, small.Describe.Size.MemoryMB = 2, 4096
	if st := runners.Status([]computeapi.RunTargetView{small, linux}, setup); st.RunsOn == "" {
		t.Fatalf("Status(mixed) = %+v", st)
	}
}
