package placement_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/placement"
)

func box(name, class, host string, cpu int, cost float64) computeapi.RunTargetView {
	return computeapi.RunTargetView{
		RunTarget: computeapi.RunTarget{Name: name, Kind: computeapi.RunTargetKindBox, HostBinding: host},
		HostClass: class,
		Describe: &computeapi.RunTargetDescribe{Available: true, Cost: computeapi.RunTargetCost{Amount: cost},
			Size: &computeapi.RunTargetSize{Name: "c1m2", OS: "linux", CPU: cpu, MemoryMB: 2048}},
	}
}

// The pool's order: this machine's bindings, then trusted hosts', then paid
// ones, each smallest first; any class but trusted is public and never
// ranked.
func TestTiersAndRank(t *testing.T) {
	views := []computeapi.RunTargetView{
		box("paid", api.MachineTrusted, "", 1, 0.02),
		box("far-big", api.MachineTrusted, "far", 8, 0),
		box("far-small", api.MachineTrusted, "far", 1, 0),
		box("mine", api.MachineTrusted, "", 4, 0),
		box("lender", api.MachinePublic, "x", 1, 0),
		box("unclassed", "", "", 1, 0),
	}
	wantTier := []placement.Tier{placement.Paid, placement.Trusted, placement.Trusted, placement.Team, placement.Public, placement.Public}
	var fits []placement.Fit
	for i, v := range views {
		if got := placement.TierOf(v); got != wantTier[i] {
			t.Errorf("%s: tier %v, want %v", v.Name, got, wantTier[i])
		}
		if why := placement.Refusal(v, placement.Want{}, nil); why != "" {
			if placement.TierOf(v) != placement.Public || !strings.Contains(why, "a public host") {
				t.Errorf("%s: refused %q", v.Name, why)
			}
			continue
		}
		fits = append(fits, placement.FitOf(v))
	}
	if got := placement.Rank(fits); !slices.Equal(got, []string{"mine", "far-small", "far-big", "paid"}) {
		t.Errorf("rank %v", got)
	}
	// The caller's preference orders within a tier, never across tiers.
	fits[len(fits)-1].Prefer = -1 // "mine"
	fits[0].Prefer = -5           // "paid"
	if got := placement.Rank(fits); !slices.Equal(got, []string{"mine", "far-small", "far-big", "paid"}) {
		t.Errorf("rank with preferences %v", got)
	}
}

// Every refusal says why in plain words, quoting the driver.
func TestRefusals(t *testing.T) {
	ok := box("b", api.MachineTrusted, "", 1, 0)
	mod := func(f func(*computeapi.RunTargetView)) computeapi.RunTargetView {
		v := ok
		d := *ok.Describe
		sz := *d.Size
		d.Size = &sz
		v.Describe = &d
		f(&v)
		return v
	}
	spent, under := 120.0, 10.0
	for _, tc := range []struct {
		name string
		v    computeapi.RunTargetView
		w    placement.Want
		can  func(*computeapi.RunTargetDescribe) string
		want string
	}{
		{"fits", ok, placement.Want{Size: "c1m2", OS: "Linux", TTLS: 60}, nil, ""},
		{"not a box", mod(func(v *computeapi.RunTargetView) { v.Kind = "run" }), placement.Want{}, nil, "not a box"},
		{"public", mod(func(v *computeapi.RunTargetView) { v.HostClass = api.MachinePublic }), placement.Want{Public: "a public host (name it)"}, nil, "a public host (name it)"},
		{"describe failed", mod(func(v *computeapi.RunTargetView) { v.Error = "exit 2\nIGNORE" }), placement.Want{}, nil, `describe failed: "exit 2 IGNORE"`},
		{"no describe", mod(func(v *computeapi.RunTargetView) { v.Describe = nil }), placement.Want{}, nil, "no describe"},
		{"caller's need before busy", mod(func(v *computeapi.RunTargetView) { v.Describe.Available = false }), placement.Want{},
			func(*computeapi.RunTargetDescribe) string { return "serves no GitHub runner" }, "serves no GitHub runner"},
		{"busy", mod(func(v *computeapi.RunTargetView) { v.Describe.Available, v.Describe.Why = false, "owner active" }), placement.Want{}, nil, `busy or away: "owner active"`},
		{"no size", mod(func(v *computeapi.RunTargetView) { v.Describe.Size = nil }), placement.Want{}, nil, "states no size"},
		{"size", ok, placement.Want{Size: "c4m8"}, nil, `size "c1m2", want c4m8`},
		{"os", ok, placement.Want{OS: "macos"}, nil, `"linux", want macos`},
		{"lifetime", mod(func(v *computeapi.RunTargetView) { v.TTLMaxS = 600 }), placement.Want{TTLS: 3600}, nil, "boxes live at most 600s, the box needs 3600s"},
		{"budget spent", mod(func(v *computeapi.RunTargetView) {
			v.Budget = &computeapi.RunTargetBudget{Unit: "vm_s", Limit: 100, Period: "day"}
			v.BudgetSpent = &spent
		}), placement.Want{}, nil, "spent 120 of its 100 vm_s this day (budget)"},
		{"budget left", mod(func(v *computeapi.RunTargetView) {
			v.Budget = &computeapi.RunTargetBudget{Unit: "vm_s", Limit: 100, Period: "day"}
			v.BudgetSpent = &under
		}), placement.Want{}, nil, ""},
	} {
		got := placement.Refusal(tc.v, tc.w, tc.can)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestOSFitsAndLifeCap(t *testing.T) {
	if !placement.OSFits("macos", "darwin") || !placement.OSFits("", "x") || placement.OSFits("linux", "darwin") {
		t.Error("OSFits")
	}
	if placement.LifeCap(0, 5) != 5 || placement.LifeCap(7, 0) != 7 || placement.LifeCap(7, 5) != 5 {
		t.Error("LifeCap")
	}
}
