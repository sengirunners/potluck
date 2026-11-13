package hostprobe

import (
	"context"
	"strings"
	"testing"
)

// smallReport is a friend's small Linux machine (host-d, zm-eakh.12.23): 4
// cores and 7.5 GiB, so half of it holds one c1m2 but not two.
func smallReport() Report {
	r := bigReport()
	r.Cores, r.RAMMB = 4, 7680
	return r
}

// With zm's default share and no size that fits twice, share lends what fits
// once inside it and binds it one box at a time (linux_slots and macos_slots
// 1, which zm-run-tart enforces), saying so; conformance still runs the
// driver's two at once (the peers check needs two, and two fit in the whole
// machine). An owner's --share keeps its meaning: nothing fits twice, refused.
func TestShareDefaultShareLendsWhatFitsOnce(t *testing.T) {
	ctx := context.Background()
	o := ShareOptions{Report: smallReport(), Images: []string{pinA}, NamePrefix: "h", RunnerDir: "none", DryRun: true}
	res, err := shareOn(ctx, &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sizes) != 1 || res.Sizes[0].Size.Name != "c1m2" || !res.OneAtATime {
		t.Fatalf("lent %+v (one at a time %t)", res.Sizes, res.OneAtATime)
	}
	cfg := res.Sizes[0].Config
	if cfg["linux_slots"] != "1" || cfg["macos_slots"] != "1" {
		t.Errorf("the binding does not run one box at a time: %v", cfg)
	}
	if c := strings.Join(res.Sizes[0].Conformance, " "); strings.Contains(c, "slots") {
		t.Errorf("conformance runs with the binding's one slot: %s", c)
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "one box at a time") {
		t.Errorf("notes say nothing of one at a time: %q", n)
	}

	o.Limits = Limits{Share: DefaultShare} // the owner's own --share 0.5
	if _, err := shareOn(ctx, &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "fits twice") {
		t.Errorf("explicit --share 0.5: %v", err)
	}
	o.Limits = Limits{Share: 0.6} // host-d's workaround still lends two at a time
	if res, err := shareOn(ctx, &fakeRunner{}, nil, o); err != nil || res.OneAtATime || res.Sizes[0].Config["linux_slots"] != "" {
		t.Errorf("--share 0.6: %v %+v", err, res)
	}

	// A machine where two fit is unchanged: no slots key, no note.
	o.Report, o.Limits = bigReport(), Limits{}
	if res, err := shareOn(ctx, &fakeRunner{}, nil, o); err != nil || res.OneAtATime || res.Sizes[0].Config["linux_slots"] != "" {
		t.Errorf("a big machine: %v %+v", err, res)
	}
	// Nothing fits even once inside half: refused, saying so.
	o.Report = smallReport()
	o.Report.Cores, o.Report.RAMMB = 1, 2048
	if _, err := shareOn(ctx, &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "even one box at a time") {
		t.Errorf("a machine too small for one: %v", err)
	}
}

// Lend is what doctor's sizes line and share both report.
func TestLend(t *testing.T) {
	if l := Lend(smallReport(), Limits{}, nil); !l.OneAtATime || len(l.Sizes) != 1 || l.Sizes[0].Name != "c1m2" {
		t.Errorf("small machine, default share: %+v", l)
	}
	if l := Lend(smallReport(), Limits{Share: 0.5}, nil); l.OneAtATime || len(l.Sizes) != 0 {
		t.Errorf("small machine, --share 0.5: %+v", l)
	}
	// Only when nothing fits twice: a 4-core 8 GiB Mac holds two c1m2, below
	// the macOS image's floor (2 vCPU, 4 GiB), and lends nothing, not one
	// c2m4 at a time (conformance's two c2m4 would be the whole machine).
	mac := macReport()
	mac.Cores, mac.RAMMB = 4, 8<<10
	p, err := DefaultImage("tart")
	if err != nil {
		t.Fatal(err)
	}
	if l := Lend(mac, Limits{}, []string{p.Image}); l.OneAtATime || len(l.Sizes) != 0 {
		t.Errorf("small Mac: %+v", l)
	}
	// Caps alone keep the default share; the note names them.
	if l := Lend(bigReport(), Limits{MaxCores: 1}, nil); !l.OneAtATime || l.Sizes[0].Name != "c1m2" ||
		!strings.Contains(Limits{MaxCores: 1}.Within(), "capped") {
		t.Errorf("one core cap: %+v", l)
	}
}

// Precheck refuses what share would refuse anyway, so zm never asks for
// root and then refuses; a layer that is not ready is the root step's to fix.
func TestPrecheck(t *testing.T) {
	r := smallReport()
	r.Layers[0].NetworkSteps = BoxNetSteps()
	o := ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h", RunnerDir: "/home/runner"}
	if err := precheckOn(context.Background(), nil, o); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*ShareOptions){
		"explicit --share 0.5": func(o *ShareOptions) { o.Limits.Share = 0.5 },
		"bad --runner-dir":     func(o *ShareOptions) { o.RunnerDir = "a b" },
		"reclaim on linux":     func(o *ShareOptions) { o.ReclaimIdleS = 5 },
	} {
		x := o
		mut(&x)
		if precheckOn(context.Background(), nil, x) == nil {
			t.Errorf("%s: no refusal", name)
		}
	}
	nr := o
	nr.Report.Layers = []Layer{{Name: "oci", Supported: true, Problems: []string{"no runtime"}, RootSteps: []string{"t=$(mktemp -d)"}}}
	if err := precheckOn(context.Background(), nil, nr); err != nil {
		t.Errorf("not ready: %v", err)
	}
	for name, mut := range map[string]func(*ShareOptions){
		"nothing fits":      func(o *ShareOptions) { o.Limits.Share = 0.5 },
		"an unpinned image": func(o *ShareOptions) { o.Images = []string{"zm-box:latest"} },
		"network, no mail":  func(o *ShareOptions) { o.OCINetwork = "mine" },
		"a runc runtime":    func(o *ShareOptions) { o.OCIRuntime = "runc" },
	} {
		x := nr
		mut(&x)
		if precheckOn(context.Background(), nil, x) == nil {
			t.Errorf("not ready, %s: no refusal", name)
		}
	}
	nr.OCIRuntime = "kata"
	if err := precheckOn(context.Background(), nil, nr); err != nil {
		t.Errorf("not ready, --oci-runtime kata: %v", err)
	}
}

// A share's root steps are doctor's for the layer it uses, less zm's
// network's when no box would serve a runner on it.
func TestShareOptionsRootSteps(t *testing.T) {
	oci := Layer{Name: "oci", Supported: true, CLI: "docker", Problems: []string{"no isolating runtime"},
		RootSteps: []string{"apt-get install -y kata-containers"}, NetworkSteps: BoxNetSteps()}
	r := Report{OS: "linux", Layers: []Layer{oci, {Name: "tart", Supported: true}}}
	all := r.RootSteps("")
	if got := (ShareOptions{Report: r, Images: []string{pinA}, RunnerDir: "/home/runner"}).RootSteps(); RootScript(got) != RootScript(all) {
		t.Errorf("runner boxes: %q, doctor %q", got, all)
	}
	for _, o := range []ShareOptions{{Report: r, Images: []string{pinA}}, {Report: r, Images: []string{pinA}, RunnerDir: "none"},
		{Report: r, RunnerDir: "/home/runner", OCINetwork: "mine"}} {
		if got := o.RootSteps(); RootScript(got) != "apt-get install -y kata-containers" {
			t.Errorf("no runner on zm's network (%+v): %q", o, got)
		}
	}
	if RootCommand(all) != "sudo sh -c "+shQuote(RootScript(all)) {
		t.Error("RootCommand is not RootScript quoted")
	}
}
