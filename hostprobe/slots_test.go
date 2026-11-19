package hostprobe

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/roomwait"
)

func slotsPtr(n int) *int { return &n }

func sizeNames(res ShareResult) []string {
	var out []string
	for _, s := range res.Sizes {
		out = append(out, s.Size.Name)
	}
	return out
}

func conformanceLine(s SizeResult) string { return strings.Join(s.Conformance, " ") }

// zm-eakh.12.7.11: --linux-slots N binds linux_slots=N on every size, and
// without an admission controller every size lent fits N times inside the
// share (bigReport's half: 16 cores, 64 GiB). Conformance runs with the same
// slots, at least two for its peers check.
func TestShareLinuxSlotsFitNTimes(t *testing.T) {
	ctx := context.Background()
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", RunnerDir: "none", DryRun: true}

	res, err := shareOn(ctx, &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := sizeNames(res); !slices.Contains(got, "c8m32") || res.LinuxSlots != 0 || res.Sizes[0].Config["linux_slots"] != "" {
		t.Fatalf("no flag: sizes %v, slots %d, config %v", got, res.LinuxSlots, res.Sizes[0].Config)
	}

	o.LinuxSlots = slotsPtr(4)
	res, err = shareOn(ctx, &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if got := sizeNames(res); !slices.Equal(got, []string{"c1m2", "c2m4", "c2m8", "c4m16"}) {
		t.Errorf("--linux-slots 4 lends %v, want what fits 4 times inside half", got)
	}
	for _, s := range res.Sizes {
		if s.Config["linux_slots"] != "4" || s.Config["macos_slots"] != "" {
			t.Errorf("%s binds %v", s.Binding, s.Config)
		}
		if !strings.Contains(conformanceLine(s), "--config linux_slots=4") {
			t.Errorf("%s: conformance without the binding's slots: %s", s.Binding, conformanceLine(s))
		}
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "up to 4 Linux boxes at once") || !strings.Contains(n, "fits 4 times inside") {
		t.Errorf("notes: %q", n)
	}

	// One at a time by choice: bound 1, conformance still has two.
	o.LinuxSlots = slotsPtr(1)
	res, err = shareOn(ctx, &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Sizes[0]; s.Config["linux_slots"] != "1" || !strings.Contains(conformanceLine(s), "--config linux_slots=2") {
		t.Errorf("--linux-slots 1: binds %v, conformance %s", s.Config, conformanceLine(s))
	}

	for name, c := range map[string]struct {
		slots int
		sizes []string
		want  string
	}{
		"zero":            {0, nil, "want 1..256"},
		"too many":        {MaxLinuxSlots + 1, nil, "want 1..256"},
		"nothing fits":    {17, nil, "no size fits 17 times inside the share: raise --share or the caps, or lower --linux-slots"},
		"a size too big":  {4, []string{"c8m32"}, "size c8m32 does not fit 4 times inside the share"},
		"smallest fits16": {16, []string{"c1m2"}, ""},
	} {
		x := o
		x.LinuxSlots, x.Sizes = slotsPtr(c.slots), c.sizes
		_, err := shareOn(ctx, &fakeRunner{}, nil, x)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// With an admission controller bound, it decides room, so --linux-slots is
// the owner's ceiling: the sizes still fit twice, not N times.
func TestShareLinuxSlotsWithAdmission(t *testing.T) {
	o := admissionOptions()
	o.AdmissionURL, o.DryRun, o.RunnerDir = "https://ci.example", true, "none"
	o.Sizes = []string{"c8m32"} // fits twice inside half, not 16 times
	o.LinuxSlots = slotsPtr(16)
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.LinuxSlotsByAdmission || res.Sizes[0].Config["linux_slots"] != "16" || !strings.Contains(conformanceLine(res.Sizes[0]), "--config linux_slots=16") {
		t.Errorf("by admission %t, binds %v, conformance %s", res.LinuxSlotsByAdmission, res.Sizes[0].Config, conformanceLine(res.Sizes[0]))
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "the admission controller decides room") {
		t.Errorf("notes: %q", n)
	}
	o.AdmissionURL = NoAdmission // the same ask without a controller is refused
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "does not fit 16 times") {
		t.Errorf("no controller: %v", err)
	}
}

// A re-run without the flag keeps the earlier bindings' linux_slots, as it
// keeps admission keys; bindings that disagree are refused until the owner
// names one; one at a time (each run's own finding) is never kept; and a
// size the run leaves alone must carry the same number.
func TestShareLinuxSlotsKept(t *testing.T) {
	ctx := context.Background()
	box := func(name string, cfg map[string]string) computeapi.RunTarget {
		return computeapi.RunTarget{Name: name, Kind: "box", Config: cfg}
	}
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", RunnerDir: "none", DryRun: true,
		Sizes: []string{"c2m4", "c4m16"}}

	o.Earlier = []computeapi.RunTarget{box("h-c2m4", map[string]string{"linux_slots": "4"}), box("h-c4m16", map[string]string{"linux_slots": "4"}),
		box("other-c8m32", map[string]string{"linux_slots": "9"})} // another share's: not this one's
	res, err := shareOn(ctx, &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.LinuxSlots != 4 || res.LinuxSlotsFrom != "h-c4m16" || res.Sizes[0].Config["linux_slots"] != "4" {
		t.Errorf("kept %d from %q, binds %v", res.LinuxSlots, res.LinuxSlotsFrom, res.Sizes[0].Config)
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "(kept from h-c4m16)") {
		t.Errorf("notes: %q", n)
	}

	o.Earlier = []computeapi.RunTarget{box("h-c2m4", map[string]string{"linux_slots": "4"}), box("h-c4m16", map[string]string{"linux_slots": "3"})}
	if _, err := shareOn(ctx, &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "differ in linux_slots") {
		t.Errorf("disagreeing bindings: %v", err)
	}
	x := o
	x.LinuxSlots = slotsPtr(3) // the owner names one
	if res, err := shareOn(ctx, &fakeRunner{}, nil, x); err != nil || res.LinuxSlots != 3 {
		t.Errorf("--linux-slots 3 over disagreeing bindings: %v %d", err, res.LinuxSlots)
	}

	o.Earlier = []computeapi.RunTarget{box("h-c2m4", map[string]string{"linux_slots": "1", "macos_slots": "1"})}
	if res, err := shareOn(ctx, &fakeRunner{}, nil, o); err != nil || res.LinuxSlots != 0 || res.Sizes[0].Config["linux_slots"] != "" {
		t.Errorf("one at a time kept: %v %+v", err, res)
	}

	o.Earlier = []computeapi.RunTarget{box("h-c8m32", map[string]string{})} // left alone, runs the default two
	x = o
	x.LinuxSlots = slotsPtr(4)
	if _, err := shareOn(ctx, &fakeRunner{}, nil, x); err == nil || !strings.Contains(err.Error(), "binding h-c8m32 is not in this run") {
		t.Errorf("a size left alone with other slots: %v", err)
	}
	x.LinuxSlots = slotsPtr(2)
	if _, err := shareOn(ctx, &fakeRunner{}, nil, x); err != nil {
		t.Errorf("the same number as the size left alone: %v", err)
	}

	x = o
	x.DryRun, x.Earlier, x.EarlierUnread = false, nil, "daemon down"
	x.AdmissionURL = NoAdmission // past the admission keys' own refusal
	if _, err := checkOn(ctx, &fakeRunner{}, x); err == nil || !strings.Contains(err.Error(), "pass --linux-slots N") {
		t.Errorf("unread bindings: %v", err)
	}
}

// macOS guests stay at Apple's two: the flag is refused for them.
func TestShareLinuxSlotsNotForMacOS(t *testing.T) {
	o := ShareOptions{Report: macReport(), NamePrefix: "mac", DryRun: true, LinuxSlots: slotsPtr(4)}
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "these images are macOS") {
		t.Errorf("macOS images: %v", err)
	}
	o.LinuxSlots = nil
	if res, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.LinuxSlots != 0 {
		t.Errorf("no flag on a Mac: %v %d", err, res.LinuxSlots)
	}
}

// A re-run on a busy host: conformance whose first failure is the driver's
// "no free linux slot" (live boxes hold every slot) says so, and what to
// do, instead of the generic availability failure; the other sizes are not
// blamed on the security checks. Nothing binds. When another check fails
// first, the slot words later on stay a plain failure: a driver that leaks
// slots is not a busy host.
func TestShareBusyHostSaysWhy(t *testing.T) {
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", RunnerDir: "none", Sizes: []string{"c2m4", "c4m16"}}
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if strings.HasPrefix(line, testSuite+" ") {
			return Result{Exit: 1, Stdout: "FAIL  the target is available (nothing else can be checked if not): describe says available false: no free linux slot (2 in use)"}, true
		}
		return Result{}, true
	}}
	res, err := shareOn(context.Background(), run, binderFunc(nil), o)
	if err != nil {
		t.Fatal(err)
	}
	if e := res.Sizes[0].Error; !strings.Contains(e, "every Linux slot on this machine is in use by live boxes (2 at once)") || !strings.Contains(e, "raise --linux-slots") {
		t.Errorf("first size: %q", e)
	}
	if e := res.Sizes[1].Error; strings.Contains(e, "security checks failed") || !strings.Contains(e, "every slot was in use") {
		t.Errorf("second size: %q", e)
	}
	if !res.Failed() || res.Sizes[0].Bound {
		t.Error("a busy host bound a size")
	}

	o.LinuxSlots = slotsPtr(4) // the number conformance ran with is the one said
	res, err = shareOn(context.Background(), run, binderFunc(nil), o)
	if err != nil {
		t.Fatal(err)
	}
	if e := res.Sizes[0].Error; !strings.Contains(e, "(4 at once)") {
		t.Errorf("with --linux-slots 4: %q", e)
	}

	leaky := &fakeRunner{on: func(line string) (Result, bool) {
		if strings.HasPrefix(line, testSuite+" ") {
			return Result{Exit: 1, Stdout: "PASS  the target is available (nothing else can be checked if not)\nFAIL  release frees the slot: still held\nFAIL  handles: acquire: unavailable: no free linux slot (2 in use)"}, true
		}
		return Result{}, true
	}}
	o.LinuxSlots = nil
	res, err = shareOn(context.Background(), leaky, binderFunc(nil), o)
	if err != nil {
		t.Fatal(err)
	}
	if e := res.Sizes[0].Error; !strings.Contains(e, "conformance failed at release frees the slot") || strings.Contains(e, "in use by live boxes") {
		t.Errorf("a leaking driver: %q", e)
	}
	if e := res.Sizes[1].Error; !strings.Contains(e, "security checks failed") {
		t.Errorf("a leaking driver, second size: %q", e)
	}

	o.LinuxSlots, o.Earlier, o.EarlierUnread, o.AdmissionURL = slotsPtr(4), nil, "daemon down", NoAdmission
	if _, err := checkOn(context.Background(), run, o); err == nil || !strings.Contains(err.Error(), "cannot check that the sizes this run leaves alone") {
		t.Errorf("--linux-slots with unread bindings: %v", err)
	}
	o.LinuxSlots, o.Sizes, o.Report = slotsPtr(2), []string{"c1m2", "c2m4", "c2m8", "c4m16", "c8m32"}, bigReport()
	// every size, nothing left alone: the refusal's own way past
	o.ShelfGB, o.ShelfFill, o.VolumeMaxGB = slotsPtr(0), "off", slotsPtr(20) // past the storage keys' own refusal
	if _, err := checkOn(context.Background(), run, o); err != nil {
		t.Errorf("--linux-slots 2 sharing every size with unread bindings: %v", err)
	}
}

// potluck host on a host whose slots are all in use waits for one
// (zm-eakh.12.24.18.5): the size's check runs again once a box ends, and
// passes; each wait is said with the other steps. Past the bound, the size
// fails as a busy host's does; the zero Wait fails at once.
func TestShareBusyHostWaitsForASlot(t *testing.T) {
	full := Result{Exit: 1, Stdout: "FAIL  the target is available (nothing else can be checked if not): describe says available false: no free linux slot (2 in use)"}
	runs, freeAfter := 0, 2
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if strings.HasPrefix(line, testSuite+" ") {
			if runs++; runs <= freeAfter {
				return full, true
			}
		}
		return Result{}, true
	}}
	now := time.Date(2026, 11, 19, 5, 0, 0, 0, time.Local)
	var slept []time.Duration
	var said []string
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", RunnerDir: "none", Sizes: []string{"c2m4"},
		Progress: func(l string) { said = append(said, l) },
		Wait: roomwait.Wait{For: 30 * time.Minute, Now: func() time.Time { return now },
			Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); now = now.Add(d); return nil }}}
	bound := 0
	binds := binderFunc(func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		bound++
		return computeapi.RunTargetView{}, nil
	})
	res, err := shareOn(context.Background(), run, binds, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Sizes[0].Passed || runs != 3 || len(slept) != 2 || bound != 1 {
		t.Fatalf("passed %v after %d runs, waits %v: %q", res.Sizes[0].Passed, runs, slept, res.Sizes[0].Error)
	}
	if !slices.ContainsFunc(said, func(l string) bool {
		return strings.HasPrefix(l, "h-c2m4 has no room: every Linux slot is in use by live boxes; asking again at 05:00:10 (waited 0s of 30m)")
	}) {
		t.Errorf("progress %q", said)
	}

	runs, freeAfter, slept = 0, 1000, nil
	o.Wait.For = 2 * time.Minute
	if res, err = shareOn(context.Background(), run, binderFunc(nil), o); err != nil || res.Sizes[0].Passed ||
		!strings.Contains(res.Sizes[0].Error, "every Linux slot on this machine is in use by live boxes") {
		t.Errorf("past the bound: %v %+v", err, res.Sizes[0])
	}

	runs, slept = 0, nil
	o.Wait = roomwait.Wait{}
	if res, err = shareOn(context.Background(), run, binderFunc(nil), o); err != nil || runs != 1 || len(slept) != 0 || res.Sizes[0].Passed {
		t.Errorf("--wait 0: %v, %d runs, waits %v", err, runs, slept)
	}
}
