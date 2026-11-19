package computeclient

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// hosted is a box binding another machine serves, as the authority lists it:
// named <binding>@<machine>.
func hosted(binding, machine, size string) computeapi.RunTargetView {
	v := boxView(binding+"@"+machine, true, "", size, "linux", 2, 4096)
	v.Host, v.HostBinding = machine, binding
	return v
}

// TestResolveRunTarget: --on takes the exact name, a binding's own name, a
// machine (alone when it serves one box, else with a size), or
// <machine>-<size>; several matches are refused with their names; a miss
// names the closest targets and `target ls`.
func TestResolveRunTarget(t *testing.T) {
	host2 := hosted("example-host-c2m4", "example-host", "c2m4")
	host4 := hosted("example-host-c4m8", "example-host", "c4m8")
	lone := hosted("solo-box", "solo", "c1m2")
	other2 := hosted("elsewhere-c2m4", "elsewhere", "c2m4")
	odd := hosted("linux-small", "lender", "c1m2") // a binding not named for its machine and size
	local := boxView("local-box", true, "", "c1m2", "linux", 1, 2048)
	twin := hosted("example-host-c2m4", "mirror", "c2m4") // the same binding name on another machine
	run := view("example-host", "run")
	squat := hosted("example-host-c2m4", "lender", "c2m4") // a stranger's binding, named after a trusted machine and size
	squat.HostClass = api.MachinePublic
	squatSolo := hosted("solo", "lender", "c1m2")
	squatSolo.HostClass = api.MachinePublic
	all := []computeapi.RunTargetView{host2, host4, lone, other2, odd, local, run}

	for _, tc := range []struct {
		name    string
		targets []computeapi.RunTargetView
		on      string
		size    string
		want    string
		code    string
		says    []string
	}{
		{name: "the exact name", targets: all, on: "example-host-c2m4@example-host", want: "example-host-c2m4@example-host"},
		{name: "a local binding by name", targets: all, on: "local-box", want: "local-box"},
		{name: "the binding's own name", targets: all, on: "example-host-c4m8", want: "example-host-c4m8@example-host"},
		{name: "a machine that serves one box", targets: all, on: "solo", want: "solo-box@solo"},
		{name: "a machine with a size", targets: all, on: "example-host", size: "c4m8", want: "example-host-c4m8@example-host"},
		{name: "a machine, by its box of that kind only (a run binding of the same name is not it)", targets: []computeapi.RunTargetView{lone, run}, on: "solo", want: "solo-box@solo"},
		{name: "<machine>-<size> where the binding is named otherwise", targets: all, on: "lender-c1m2", want: "linux-small@lender"},
		{name: "<machine>-<size> with the same --size", targets: all, on: "lender-c1m2", size: "c1m2", want: "linux-small@lender"},
		{name: "a machine alone that serves several", targets: all, on: "example-host", code: api.CodeBadRequest,
			says: []string{"matches 2 run targets", "example-host-c2m4@example-host", "example-host-c4m8@example-host", "--size"}},
		{name: "a binding on two machines", targets: []computeapi.RunTargetView{host2, twin}, on: "example-host-c2m4", code: api.CodeBadRequest,
			says: []string{"example-host-c2m4@mirror", "example-host-c2m4@example-host"}},
		{name: "a size with an exact name", targets: all, on: "example-host-c2m4@example-host", size: "c2m4", code: api.CodeBadRequest, says: []string{"--size goes with a machine name"}},
		{name: "a size with a binding name", targets: all, on: "example-host-c2m4", size: "c2m4", code: api.CodeBadRequest, says: []string{"--size goes with a machine name"}},
		{name: "a binding on two machines says to name one in full, not to add a size", targets: []computeapi.RunTargetView{host2, twin}, on: "example-host-c2m4", code: api.CodeBadRequest,
			says: []string{"name one in full)"}},
		{name: "a machine with two boxes of one size", targets: []computeapi.RunTargetView{host2, hosted("example-host-c2m4b", "example-host", "c2m4")}, on: "example-host", size: "c2m4", code: api.CodeBadRequest,
			says: []string{"matches 2 run targets", "(name one in full)"}},
		{name: "a stranger's binding named like <machine>-<size> does not take it", targets: []computeapi.RunTargetView{hosted("box1", "example-host", "c2m4"), squat}, on: "example-host-c2m4", want: "box1@example-host"},
		{name: "a stranger's binding named like a machine does not take it", targets: []computeapi.RunTargetView{lone, squatSolo}, on: "solo", want: "solo-box@solo"},
		{name: "a public host by its exact name", targets: []computeapi.RunTargetView{hosted("box1", "example-host", "c2m4"), squat}, on: "example-host-c2m4@lender", want: "example-host-c2m4@lender"},
		{name: "a public host alone is taken when nothing else matches", targets: []computeapi.RunTargetView{squat}, on: "example-host-c2m4", want: "example-host-c2m4@lender"},
		{name: "a machine with no box of that size", targets: all, on: "example-host", size: "c8m16", code: api.CodeNotFound,
			says: []string{"no run target example-host", "example-host-c2m4@example-host (size \"c2m4\")", "example-host-c4m8@example-host", "target ls lists them all"}},
		{name: "a miss names the targets of the same machine and size", targets: all, on: "example-host-c2m8", code: api.CodeNotFound,
			says: []string{"no run target example-host-c2m8", "example-host-c2m4@example-host", "example-host-c4m8@example-host", "target ls lists them all"}},
		{name: "a miss with a size alike names the same size", targets: all, on: "zz-c2m4", code: api.CodeNotFound,
			says: []string{"elsewhere-c2m4@elsewhere", "example-host-c2m4@example-host"}},
		{name: "a miss with nothing alike lists those there are", targets: []computeapi.RunTargetView{lone, local}, on: "zz", code: api.CodeNotFound,
			says: []string{"those listed", "local-box", "solo-box@solo"}},
		{name: "a miss while the list is short reads as a miss, and says to wait", targets: nil, on: "example-host-c2m8@example-host", code: api.CodeNotFound,
			says: []string{"no run target example-host-c2m8@example-host", "target ls", "just restarted"}},
	} {
		got, err := ResolveRunTarget(context.Background(), client.WithHandler(targetsHandler(tc.targets...)), "box", tc.on, tc.size)
		switch {
		case tc.code != "" && !api.IsCode(err, tc.code):
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		case tc.code == "" && (err != nil || got.Name != tc.want):
			t.Errorf("%s: %q %v, want %q", tc.name, got.Name, err, tc.want)
		}
		for _, s := range tc.says {
			if err == nil || !strings.Contains(err.Error(), s) {
				t.Errorf("%s: %v lacks %q", tc.name, err, s)
			}
		}
	}
	// An exact name of another kind is refused, not resolved.
	if _, err := ResolveRunTarget(context.Background(), client.WithHandler(targetsHandler(view("db1", "db"))), "box", "db1", ""); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "is a db, not a box") {
		t.Errorf("a db named: %v", err)
	}
}

// TestPlaceBoxOnMachine: sandbox create --on <machine> --size <size> places
// on the one box that machine states, and --on example-host-c2m4 on its
// binding; --on with an OS is still refused.
func TestPlaceBoxOnMachine(t *testing.T) {
	c := client.WithHandler(targetsHandler(hosted("example-host-c2m4", "example-host", "c2m4"), hosted("example-host-c4m8", "example-host", "c4m8")))
	for _, w := range []BoxWant{{On: "example-host-c2m4"}, {On: "example-host", Size: "c2m4"}, {On: "example-host-c2m4@example-host"}} {
		if got, err := PlaceBox(context.Background(), c, w); err != nil || len(got) != 1 || got[0] != "example-host-c2m4@example-host" {
			t.Errorf("%+v: %q %v", w, got, err)
		}
	}
	if _, err := PlaceBox(context.Background(), c, BoxWant{On: "example-host", OS: "linux"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("an OS with a name: %v", err)
	}
	if _, err := PlaceBox(context.Background(), c, BoxWant{On: "example-host-c2m4", Template: "no-such-template"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("a template that does not exist is refused beside a name: %v", err)
	}
}

// TestFindRunTarget: a handle's target is its exact name, never a machine or
// a size; a miss names the closest and where all are listed. BoxHandle takes
// it that way.
func TestFindRunTarget(t *testing.T) {
	ctx := context.Background()
	c := client.WithHandler(targetsHandler(hosted("example-host-c2m4", "example-host", "c2m4"), view("scratch", "db")))
	if v, err := FindRunTarget(ctx, c, "box", "example-host-c2m4@example-host"); err != nil || v.Name != "example-host-c2m4@example-host" {
		t.Fatalf("%q %v", v.Name, err)
	}
	for _, name := range []string{"example-host-c2m4", "example-host"} { // a binding's own name, a machine
		if _, err := FindRunTarget(ctx, c, "box", name); !api.IsCode(err, api.CodeNotFound) || !strings.Contains(err.Error(), "example-host-c2m4@example-host") {
			t.Errorf("%s: %v", name, err)
		}
		if _, _, err := BoxHandle(ctx, c, "box:"+name+"/b1"); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("handle on %s: %v", name, err)
		}
	}
	if _, err := FindRunTarget(ctx, c, "box", "scratch"); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("a db: %v", err)
	}
	if tg, id, err := BoxHandle(ctx, c, "box:example-host-c2m4@example-host/b1"); err != nil || tg != "example-host-c2m4@example-host" || id != "b1" {
		t.Errorf("%q %q %v", tg, id, err)
	}
}
