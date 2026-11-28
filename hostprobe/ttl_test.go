package hostprobe

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// --ttl-max-s N binds ttl_max_s=N on every size (on the binding, not in its
// config, where it could not be read back); a re-run without the flag keeps
// an earlier binding's; a value out of range is refused.
func TestShareTTLMaxS(t *testing.T) {
	ctx := context.Background()
	var bound []computeapi.BindRunTargetRequest
	b := binderFunc(func(_ context.Context, r computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		bound = append(bound, r)
		return computeapi.RunTargetView{}, nil
	})
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", RunnerDir: "none", Sizes: []string{"c1m2", "c2m4"}, TTLMaxS: slotsPtr(435600)}
	run := func() *fakeRunner { return &fakeRunner{on: func(string) (Result, bool) { return Result{}, true }} }
	res, err := shareOn(ctx, run(), b, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.TTLMaxS != 435600 || res.TTLMaxSFrom != "" || len(bound) != 2 {
		t.Fatalf("ttl %d from %q, %d binds", res.TTLMaxS, res.TTLMaxSFrom, len(bound))
	}
	for _, r := range bound {
		if r.Target.TTLMaxS != 435600 || r.Target.Config["ttl_max_s"] != "" {
			t.Errorf("%s binds ttl_max_s %d, config %v", r.Target.Name, r.Target.TTLMaxS, r.Target.Config)
		}
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "boxes live at most 435600 s (5 days 1 h) on every size") {
		t.Errorf("notes: %q", n)
	}

	// Without the flag nothing is bound.
	o.TTLMaxS, bound = nil, nil
	if res, err = shareOn(ctx, run(), b, o); err != nil || res.TTLMaxS != 0 || len(bound) != 2 || bound[0].Target.TTLMaxS != 0 {
		t.Errorf("no flag: %v, ttl %d, binds %+v", err, res.TTLMaxS, bound)
	}

	// A re-run keeps an earlier binding's; another share's does not count.
	box := func(name string, ttl int) computeapi.RunTarget {
		return computeapi.RunTarget{Name: name, Kind: "box", TTLMaxS: ttl, Config: map[string]string{}}
	}
	o.DryRun = true
	o.Earlier = []computeapi.RunTarget{box("h-c1m2", 86400), box("h-c2m4", 86400), box("other-c2m4", 3600)}
	if res, err = shareOn(ctx, &fakeRunner{}, nil, o); err != nil || res.TTLMaxS != 86400 || res.TTLMaxSFrom != "h-c2m4" || res.Sizes[0].TTLMaxS != 86400 {
		t.Errorf("kept: %v, ttl %d from %q", err, res.TTLMaxS, res.TTLMaxSFrom)
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "(24 h) on every size (kept from h-c2m4)") {
		t.Errorf("notes: %q", n)
	}
	o.Earlier = []computeapi.RunTarget{box("h-c1m2", 86400), box("h-c2m4", 7200)}
	if _, err = shareOn(ctx, &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "differ in ttl_max_s") {
		t.Errorf("disagreeing bindings: %v", err)
	}
	x := o
	x.TTLMaxS = slotsPtr(3600) // the owner names one
	if res, err = shareOn(ctx, &fakeRunner{}, nil, x); err != nil || res.TTLMaxS != 3600 {
		t.Errorf("--ttl-max-s 3600 over disagreeing bindings: %v %d", err, res.TTLMaxS)
	}

	// A size the run leaves alone with another cap is refused; with none, fine.
	x = o
	x.Sizes, x.TTLMaxS = []string{"c1m2"}, slotsPtr(432300)
	x.Earlier = []computeapi.RunTarget{box("h-c2m4", 86400)}
	if _, err = shareOn(ctx, &fakeRunner{}, nil, x); err == nil || !strings.Contains(err.Error(), "binding h-c2m4 is not in this run") {
		t.Errorf("a size left alone with another cap: %v", err)
	}
	x.Earlier = []computeapi.RunTarget{box("h-c2m4", 0), box("h-c4m16", 432300)}
	if _, err = shareOn(ctx, &fakeRunner{}, nil, x); err != nil {
		t.Errorf("sizes left alone with none or the same cap: %v", err)
	}

	for _, n := range []int{0, MinTTLMaxS - 1, MaxTTLMaxS + 1} {
		x.TTLMaxS = slotsPtr(n)
		if _, err := shareOn(ctx, &fakeRunner{}, nil, x); err == nil || !strings.Contains(err.Error(), "--ttl-max-s: want 900..435600") {
			t.Errorf("--ttl-max-s %d: %v", n, err)
		}
	}

	// Bindings it cannot read: a real run refuses rather than drop the cap.
	x = o
	x.DryRun, x.Earlier, x.EarlierUnread = false, nil, "daemon down"
	x.AdmissionURL = NoAdmission
	x.LinuxSlots, x.Sizes = slotsPtr(2), []string{"c1m2", "c2m4", "c2m8", "c4m16", "c8m32"} // past linux_slots' own refusals
	x.ShelfGB, x.ShelfFill, x.VolumeMaxGB = slotsPtr(0), "off", slotsPtr(20)                // and the storage keys'
	if _, err := checkOn(ctx, &fakeRunner{}, x); err == nil || !strings.Contains(err.Error(), "pass --ttl-max-s N") {
		t.Errorf("unread bindings: %v", err)
	}
}
