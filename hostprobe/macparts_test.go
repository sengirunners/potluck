package hostprobe

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// Each problem is in the part it is about, and only there.
func TestMacPartsHoldTheirOwnProblems(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(r *fakeRunner, f *macFacts)
		part func(p MacParts) []string // the part whose problems the case makes
	}{
		{"fine", func(*fakeRunner, *macFacts) {}, nil},
		{"Intel", func(r *fakeRunner, _ *macFacts) { r.out["/usr/sbin/sysctl -n hw.optional.arm64"] = ok("0\n") }, func(p MacParts) []string { return p.Mac.Problems }},
		{"no virtualization", func(r *fakeRunner, _ *macFacts) { r.out["sysctl -n kern.hv_support"] = ok("0\n") }, func(p MacParts) []string { return p.Mac.Problems }},
		{"no Tart", func(_ *fakeRunner, f *macFacts) { f.bins["tart"] = false }, func(p MacParts) []string { return p.Tart.Problems }},
		{"no Softnet", func(_ *fakeRunner, f *macFacts) { f.bins["softnet"] = false }, func(p MacParts) []string { return p.Softnet.Problems }},
		{"Softnet not setuid", func(r *fakeRunner, _ *macFacts) { r.out["stat -L -f %Sp %Su /fake/softnet"] = ok("-rwxr-xr-x root\n") }, func(p MacParts) []string { return p.Softnet.Problems }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, f := macRunner("-rwsr-xr-x root", ""), mac(true)
			tc.edit(run, f)
			p := CheckMacSetup(context.Background(), run, f, "")
			all := slices.Concat(p.Mac.Problems, p.Tart.Problems, p.Softnet.Problems, p.Rest.Problems)
			if tc.part == nil {
				if len(all) != 0 {
					t.Errorf("problems %v", all)
				}
				return
			}
			if mine := tc.part(p); len(mine) == 0 || len(mine) != len(all) {
				t.Errorf("the case's part %v, all %v", mine, all)
			}
		})
	}
}

// A tart.app with no program in it is told apart from no Tart.
func TestMacPartsHollowTartApp(t *testing.T) {
	run, f := macRunner("-rwsr-xr-x root", ""), mac(true)
	f.bins["tart"] = false
	f.paths = map[string]bool{"/Applications/tart.app": true}
	p := CheckMacSetup(context.Background(), run, f, "")
	if p.Tart.CLI != "" || !slices.Equal(p.Hollow, []string{"/Applications/tart.app"}) || len(p.Tart.Problems) != 1 {
		t.Errorf("tart %+v hollow %v", p.Tart, p.Hollow)
	}
}

// Combined problems give the layer exactly these: root steps and the
// hardening warning are offered only where no other part stops the Mac.
func TestTartLayerAcrossParts(t *testing.T) {
	oldHelp := "--net-softnet --net-softnet-block --net-softnet-allow --no-graphics --no-audio --no-clipboard" // no --no-usb-accessories: a warning
	for _, tc := range []struct {
		name     string
		edit     func(r *fakeRunner, f *macFacts)
		problems int
		steps    int
		warns    int
	}{
		{"Softnet not setuid alone", func(*fakeRunner, *macFacts) {}, 1, 2, 0},
		{"and no virtualization", func(r *fakeRunner, _ *macFacts) { r.out["sysctl -n kern.hv_support"] = ok("0\n") }, 2, 0, 0},
		{"and an old Tart", func(r *fakeRunner, _ *macFacts) { r.out["/fake/tart run --help"] = ok(oldHelp) }, 1, 2, 1},
		{"and an old Tart on a Mac with no virtualization", func(r *fakeRunner, _ *macFacts) {
			r.out["/fake/tart run --help"] = ok(oldHelp)
			r.out["sysctl -n kern.hv_support"] = ok("0\n")
		}, 2, 0, 0},
		{"and no Tart", func(_ *fakeRunner, f *macFacts) { f.bins["tart"] = false }, 2, 0, 0},
	} {
		run, f := macRunner("-rwxr-xr-x root", ""), mac(true)
		tc.edit(run, f)
		l, _ := Detect(context.Background(), run, f, "/state").Layer("tart")
		if len(l.Problems) != tc.problems || len(l.RootSteps) != tc.steps || len(l.Warnings) != tc.warns || l.Available {
			t.Errorf("%s: %+v", tc.name, l)
		}
	}
}

// A short disk needs nothing once Tart has the image.
func TestMacPartsDiskNeed(t *testing.T) {
	run, f := macRunner("-rwsr-xr-x root", ""), mac(true)
	f.disk["/Users/o/.tart"] = 5
	if p := CheckMacSetup(context.Background(), run, f, ""); p.NeedGB == 0 {
		t.Error("Tart's list unread: the image counts")
	}
	img, err := HostDefaultImage(Report{OS: "darwin", Arch: "arm64", MacOS: "26.0"}, "tart")
	if err != nil {
		t.Fatal(err)
	}
	run.out["/fake/tart list --format json"] = ok(`[{"Source":"oci","Name":"` + img.Image + `"}]`)
	if p := CheckMacSetup(context.Background(), run, f, ""); p.NeedGB != 0 {
		t.Errorf("needs %d GB with the image here (%v)", p.NeedGB, run.calls)
	}
}

// An app's PATH lacks Homebrew's directory; Softnet there is still found.
func TestSoftnetFoundOffPATH(t *testing.T) {
	run, f := macRunner("-rwsr-xr-x root", ""), mac(false)
	f.paths = map[string]bool{"/opt/homebrew/bin/softnet": true}
	run.out["/opt/homebrew/bin/softnet --version"] = ok("0.25.0\n")
	run.out[softnetProbeLine("/opt/homebrew/bin/softnet")] = ok("")
	run.out["stat -L -f %Sp %Su /opt/homebrew/bin/softnet"] = ok("-rwsr-xr-x root\n")
	if p := CheckMacSetup(context.Background(), run, f, ""); p.Softnet.SoftnetPath != "/opt/homebrew/bin/softnet" || len(p.Softnet.Problems) > 0 {
		t.Errorf("%+v", p.Softnet)
	}
}

// wedgedRunner never answers the lines wedged until their context ends, the
// way ExecRunner reports a command it killed: exit -1, no error.
type wedgedRunner struct {
	*fakeRunner
	wedged func(line string) bool
}

func (r wedgedRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if r.wedged(strings.Join(append([]string{name}, args...), " ")) {
		<-ctx.Done()
		return Result{Exit: -1}, nil
	}
	return r.fakeRunner.Run(ctx, name, args...)
}

// A command that does not answer in time leaves its fact unread: nothing
// is said about the Mac, Tart or Softnet itself.
func TestMacSetupUnread(t *testing.T) {
	defer func(d time.Duration) { macProbeTimeout = d }(macProbeTimeout)
	macProbeTimeout = 20 * time.Millisecond
	for name, tc := range map[string]struct {
		wedged string
		unread func(p MacParts) string
	}{
		"virtualization": {"sysctl -n kern.hv_support", func(p MacParts) string { return p.HVUnread }},
		"processor":      {"/usr/sbin/sysctl -n hw.optional.arm64", func(p MacParts) string { return p.ArchUnread }},
		"macOS":          {"/usr/bin/sw_vers -productVersion", func(p MacParts) string { return p.MacOSUnread }},
		"Tart version":   {"/fake/tart --version", func(p MacParts) string { return p.Tart.Unread }},
		"Tart options":   {"/fake/tart run --help", func(p MacParts) string { return p.Tart.Unread }},
		"Softnet":        {"/fake/softnet --version", func(p MacParts) string { return p.Softnet.Unread }},
		"Softnet rules":  {softnetProbeLine("/fake/softnet"), func(p MacParts) string { return p.Softnet.Unread }},
		"Softnet file":   {"stat -L -f %Sp %Su /fake/softnet", func(p MacParts) string { return p.Softnet.Unread }},
	} {
		t.Run(name, func(t *testing.T) {
			run := macRunner("-rwsr-xr-x root", "")
			f := mac(true)
			p := CheckMacSetup(context.Background(), wedgedRunner{run, func(l string) bool { return l == tc.wedged }}, f, "")
			if why := tc.unread(p); !strings.Contains(why, "no answer in time") {
				t.Errorf("unread %q in %+v", why, p)
			}
			for _, u := range []string{p.HVUnread, p.ArchUnread, p.MacOSUnread, p.Tart.Unread, p.Softnet.Unread} {
				if u != "" && u != tc.unread(p) {
					t.Errorf("also unread: %q", u)
				}
			}
			if len(p.Softnet.RootSteps) > 0 {
				t.Errorf("root steps %v", p.Softnet.RootSteps)
			}
			// doctor still refuses the layer.
			if l, _ := Detect(context.Background(), wedgedRunner{macRunner("-rwsr-xr-x root", ""), func(l string) bool { return l == tc.wedged }}, mac(true), "/state").Layer("tart"); l.Available {
				t.Error("the layer is available with something unread")
			}
		})
	}
}

// A Tart that answers its list with nothing leaves the image unknown, never
// a full disk.
func TestMacSetupImagesUnread(t *testing.T) {
	run, f := macRunner("-rwsr-xr-x root", ""), mac(true)
	f.disk["/Users/o/.tart"] = 5
	run.out["/fake/tart list --format json"] = Result{Exit: 1, Stderr: "locked"}
	if p := CheckMacSetup(context.Background(), run, f, ""); p.ImagesUnread == "" || p.NeedGB == 0 {
		t.Errorf("images unread %q, need %d", p.ImagesUnread, p.NeedGB)
	}
}

// An Intel Mac's kernel has no hw.optional.arm64: that is an answer.
func TestMacSetupIntelHasNoArm64Name(t *testing.T) {
	run := macRunner("-rwsr-xr-x root", "")
	run.out["/usr/sbin/sysctl -n hw.optional.arm64"] = Result{Exit: 1, Stderr: "sysctl: unknown oid 'hw.optional.arm64'\n"}
	if p := CheckMacSetup(context.Background(), run, mac(true), ""); p.Report.Arch != "amd64" || p.ArchUnread != "" {
		t.Errorf("arch %q, unread %q", p.Report.Arch, p.ArchUnread)
	}
}

// Everything wedged, the Mac part still ends at its one deadline, well
// under what the setup window waits.
func TestMacSetupHasOneDeadline(t *testing.T) {
	if macSetupTimeout > 15*time.Second {
		t.Errorf("the probe may take %v", macSetupTimeout)
	}
	defer func(d time.Duration) { macSetupTimeout = d }(macSetupTimeout)
	macSetupTimeout = 200 * time.Millisecond
	f := mac(true)
	f.disk["/Users/o/.tart"] = 5 // so Tart's images are listed too
	start := time.Now()
	p := CheckMacSetup(context.Background(), wedgedRunner{&fakeRunner{}, func(string) bool { return true }}, f, "")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v", took)
	}
	if p.HVUnread == "" || p.ArchUnread == "" || p.MacOSUnread == "" || p.Tart.Unread == "" || p.Softnet.Unread == "" {
		t.Errorf("read something: %+v", p)
	}
}

// A command whose child keeps its output open ends with its context.
func TestExecRunnerWaitDelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	ExecRunner{WaitDelay: 100 * time.Millisecond}.Run(ctx, "sh", "-c", "sleep 8 & wait")
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
}

// A program that ended well and left a child holding its output works.
func TestExecRunnerLeftChild(t *testing.T) {
	res, err := ExecRunner{WaitDelay: 100 * time.Millisecond}.Run(context.Background(), "sh", "-c", "echo 2.32.1; sleep 8 &")
	if err != nil || res.Exit != 0 || res.Stdout != "2.32.1\n" {
		t.Errorf("%+v %v", res, err)
	}
}

// With the macOS version or the processor unread, which image this Mac
// needs is not known, so a short disk is no verdict.
func TestMacSetupShortDiskWithTheMacUnread(t *testing.T) {
	for _, line := range []string{"/usr/bin/sw_vers -productVersion", "/usr/sbin/sysctl -n hw.optional.arm64"} {
		run, f := macRunner("-rwsr-xr-x root", ""), mac(true)
		run.out[line] = Result{Exit: 1}
		f.disk["/Users/o/.tart"] = 5
		if p := CheckMacSetup(context.Background(), run, f, ""); p.ImagesUnread == "" {
			t.Errorf("%s: need %d, images unread %q", line, p.NeedGB, p.ImagesUnread)
		}
	}
}

// What is known to be wrong stays known when a later read times out.
func TestUnreadNeverHidesAProblem(t *testing.T) {
	l := Layer{Problems: []string{"the managed Softnet is unsound"}}
	l.unread("no answer in time")
	if l.Unread != "" || len(l.Problems) != 2 {
		t.Errorf("%+v", l)
	}
}

// stuckFacts is a Mac whose disk read never returns.
type stuckFacts struct{ *macFacts }

func (stuckFacts) FreeDiskGB(string) (float64, error) { select {} }

func TestMacSetupStuckFileRead(t *testing.T) {
	defer func(d, g time.Duration) { macSetupTimeout, macSetupGrace = d, g }(macSetupTimeout, macSetupGrace)
	macSetupTimeout, macSetupGrace = 50*time.Millisecond, 50*time.Millisecond
	start := time.Now()
	p := CheckMacSetup(context.Background(), macRunner("-rwsr-xr-x root", ""), stuckFacts{mac(true)}, "")
	if time.Since(start) > 5*time.Second || p.MacOSUnread == "" || p.Report.TartDiskUnread == "" || p.Softnet.Unread == "" {
		t.Errorf("%+v", p)
	}
}

// SoftnetAlone is only when nothing else is wrong.
func TestSoftnetAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(r *fakeRunner, f *macFacts)
		want bool
	}{
		"missing":     {func(_ *fakeRunner, f *macFacts) { f.bins["softnet"] = false }, true},
		"fine":        {func(*fakeRunner, *macFacts) {}, false},
		"and no Tart": {func(_ *fakeRunner, f *macFacts) { f.bins["softnet"], f.bins["tart"] = false, false }, false},
		"and no virtualization": {func(r *fakeRunner, f *macFacts) {
			f.bins["softnet"] = false
			r.out["sysctl -n kern.hv_support"] = ok("0\n")
		}, false},
	} {
		run, f := macRunner("-rwsr-xr-x root", ""), mac(true)
		tc.edit(run, f)
		if l, _ := Detect(context.Background(), run, f, "/state").Layer("tart"); l.SoftnetAlone != tc.want {
			t.Errorf("%s: %v", name, l.SoftnetAlone)
		}
	}
}

// macOS inside a virtual machine has no kern.hv_support: when it says it is
// a guest that is a definite no, and anything else leaves it unread.
func TestMacPartsVirtualMachine(t *testing.T) {
	noOID := Result{Exit: 1, Stderr: "sysctl: unknown oid 'kern.hv_support'"}
	for _, tc := range []struct {
		name         string
		vmm          *Result
		unread, inVM bool
	}{
		{"a guest", &Result{Stdout: "1\n"}, false, true},
		{"not a guest", &Result{Stdout: "0\n"}, true, false},
		{"guest unread", nil, true, false},
	} {
		run, f := macRunner("-rwsr-xr-x root", ""), mac(true)
		run.out["sysctl -n kern.hv_support"] = noOID
		if tc.vmm != nil {
			run.out["sysctl -n kern.hv_vmm_present"] = *tc.vmm
		}
		p := CheckMacSetup(context.Background(), run, f, "")
		if p.HV || (p.HVUnread != "") != tc.unread || p.InVM != tc.inVM || len(p.Mac.Problems) != 1 {
			t.Errorf("%s: hv %t unread %q in vm %t problems %v", tc.name, p.HV, p.HVUnread, p.InVM, p.Mac.Problems)
		}
	}
}
