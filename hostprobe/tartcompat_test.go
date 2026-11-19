package hostprobe

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/vmhost"
)

const tartHelp = `--no-graphics --no-audio --no-clipboard --no-usb-accessories --net-softnet --net-softnet-block --net-softnet-allow`

func softnetProbeLine(path string) string {
	return path + " --vm-fd 0 --vm-mac-address 02:00:00:00:00:01 --allow=" + vmhost.SoftnetAllowRules() + " --block=" + vmhost.SoftnetBlockRules() + " --sudo-escalation-probing"
}

func TestTartCompatibilityBeforeDownloads(t *testing.T) {
	for _, tc := range []struct{ name, version, failCommand, cause, want string }{
		{name: "stock 0.23", version: "0.23.0", failCommand: softnetProbeLine("/fake/softnet"), cause: "invalid value out proto udp @host port 53 for --allow: invalid IP address syntax", want: "required host DNS-only and SMTP-blocking"},
		{name: "new vmnet API unavailable", version: "0.25.0", failCommand: "/fake/softnet --version", cause: "dyld: Symbol not found: _vmnet_interface_start_with_network", want: "_vmnet_interface_start_with_network"},
		{name: "Tart cannot execute", version: "0.25.0", failCommand: "/fake/tart --version", cause: "Bad CPU type in executable", want: "Bad CPU type"},
		{name: "unknown macOS", version: "0.25.0", failCommand: "/usr/bin/sw_vers -productVersion", cause: "unavailable", want: "cannot determine macOS"},
		{name: "compatible backport", version: "0.23.0-potluck.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := macRunner("-rwsr-xr-x root", "")
			run.out["/usr/bin/sw_vers -productVersion"] = ok("15.6\n")
			run.out["/fake/softnet --version"] = ok(tc.version + "\n")
			if tc.failCommand != "" {
				run.out[tc.failCommand] = Result{Exit: 1, Stderr: tc.cause}
			}
			report := Detect(context.Background(), run, mac(true), "/state")
			l, _ := report.Layer("tart")
			if tc.want == "" {
				if !l.Available || l.SoftnetVersion != tc.version || report.MacOS != "15.6" {
					t.Fatalf("report %+v layer %+v", report, l)
				}
				p, err := HostDefaultImage(report, "tart")
				if err != nil || p.Name != "sequoia" {
					t.Fatalf("default %+v: %v", p, err)
				}
				return
			}
			if l.Available || !strings.Contains(strings.Join(l.Problems, ";"), tc.want) {
				t.Fatalf("layer %+v", l)
			}
			run.calls = nil
			_, err := Check(context.Background(), run, ShareOptions{Report: report, NamePrefix: "h"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check: %v", err)
			}
			if len(run.calls) != 0 {
				t.Fatalf("refusal ran commands: %v", run.calls)
			}
		})
	}
}

func TestTartImageCompatibility(t *testing.T) {
	def, _ := DefaultImage("tart")
	small, _ := namedSmallLinux()
	for _, tc := range []struct{ name, version, arch, image, want string }{
		{"Tahoe on older Mac", "15.6", "arm64", def.Image, "on arm64 macOS 26+"},
		{"Tahoe on Tahoe", "26.1", "arm64", def.Image, ""},
		{"Linux on older Mac", "15.6", "arm64", small.Image, ""},
		{"wrong architecture", "26.1", "amd64", def.Image, "Apple silicon"},
		{"unknown OS", "", "arm64", def.Image, "known macOS"},
		{"unknown image", "26.1", "arm64", pinA, "unknown host compatibility"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := macReport()
			report.MacOS, report.Arch = tc.version, tc.arch
			run := &fakeRunner{}
			_, err := Check(context.Background(), run, ShareOptions{Report: report, Images: []string{tc.image}, NamePrefix: "h", DryRun: true, Conformance: testSuite, Sizes: []string{"c2m4"}})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v want %s", err, tc.want)
			}
			if len(run.calls) != 0 {
				t.Fatalf("preflight ran commands: %v", run.calls)
			}
		})
	}
}

func TestIncompatibleSoftnetOffersNoRootRepair(t *testing.T) {
	run := macRunner("-rwxr-xr-x root", "")
	run.out[softnetProbeLine("/fake/softnet")] = Result{Exit: 2, Stderr: "invalid IP address syntax"}
	report := Detect(context.Background(), run, mac(true), "/state")
	l, _ := report.Layer("tart")
	if len(l.RootSteps) != 0 {
		t.Fatalf("incompatible helper offers root steps: %v", l.RootSteps)
	}
	err := Precheck(context.Background(), run, ShareOptions{Report: report, NamePrefix: "h", Conformance: testSuite})
	if err == nil || !strings.Contains(err.Error(), "required host DNS-only") {
		t.Fatalf("Precheck swallowed compatibility blocker: %v", err)
	}
}

func TestHostDefaultCanBeLentAgain(t *testing.T) {
	for _, version := range []string{"13.6", "15.6", "26.1"} {
		t.Run(version, func(t *testing.T) {
			report := macReport()
			report.MacOS = version
			o := ShareOptions{Report: report, NamePrefix: "mac", DryRun: true, Conformance: testSuite}
			first, err := Check(context.Background(), &fakeRunner{}, o)
			if err != nil {
				t.Fatal(err)
			}
			pin, _ := HostDefaultImage(report, "tart")
			if first.Sizes[0].Config["image_name"] != pin.Name {
				t.Fatal("default lost its named-image placement identity")
			}
			for _, s := range first.Sizes {
				o.Earlier = append(o.Earlier, computeapi.RunTarget{Name: s.Binding, Images: first.Images, Config: s.Config})
			}
			second, err := Check(context.Background(), &fakeRunner{}, o)
			if err != nil || strings.Join(first.Images, ",") != strings.Join(second.Images, ",") {
				t.Fatalf("re-lend: %+v, %v", second, err)
			}
		})
	}
}

func TestTartRequiresRulePassthrough(t *testing.T) {
	run := macRunner("-rwsr-xr-x root", "")
	run.out["/fake/tart run --help"] = ok("--net-softnet --no-graphics --no-audio --no-clipboard")
	report := Detect(context.Background(), run, mac(true), "/state")
	l, _ := report.Layer("tart")
	if l.Available || !strings.Contains(strings.Join(l.Problems, ";"), "--net-softnet-block") {
		t.Fatalf("accepted incompatible Tart: %+v", l)
	}
}

// A Tart without --no-usb-accessories still lends: its USB accessories are
// Tart's own virtual keyboard and pointer, nothing of the Mac's. Preflight
// warns, naming the Tart that has the flag and how to get it; one with the
// flag gets no warning.
func TestTartWithoutUSBFlagWarnsNotRefuses(t *testing.T) {
	run := macRunner("-rwsr-xr-x root", "")
	run.out["/fake/tart run --help"] = ok("--net-softnet --net-softnet-block --net-softnet-allow --no-graphics --no-audio --no-clipboard")
	report := Detect(context.Background(), run, mac(true), "/state")
	l, _ := report.Layer("tart")
	if !l.Available || len(l.Problems) != 0 {
		t.Fatalf("refused a Tart that only lacks --no-usb-accessories: %+v", l)
	}
	w := strings.Join(l.Warnings, ";")
	for _, want := range []string{"--no-usb-accessories", "Tart 2.38.0 or newer", "releases/latest", "none of this Mac's USB devices"} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings %q lack %q", w, want)
		}
	}

	run = macRunner("-rwsr-xr-x root", "")
	report = Detect(context.Background(), run, mac(true), "/state")
	l, _ = report.Layer("tart")
	if strings.Contains(strings.Join(l.Warnings, ";"), "--no-usb-accessories") {
		t.Fatalf("warned about a Tart that has the flag: %v", l.Warnings)
	}
}

func TestTartReadsHardwareArchitectureUnderRosetta(t *testing.T) {
	f := mac(true)
	f.arch = "amd64"
	report := Detect(context.Background(), macRunner("-rwsr-xr-x root", ""), f, "/state")
	l, _ := report.Layer("tart")
	if report.Arch != "arm64" || !l.Available {
		t.Fatalf("hardware architecture not used: %+v, %+v", report, l)
	}
}

type relativeMac struct{ *macFacts }

func (f relativeMac) LookPath(name string) (string, bool) {
	if name == "tart" || name == "softnet" {
		return "test-bin/" + name, true
	}
	return f.macFacts.LookPath(name)
}

func TestTartPreflightNormalizesRelativeExecutablePaths(t *testing.T) {
	tart, _ := filepath.Abs("test-bin/tart")
	softnet, _ := filepath.Abs("test-bin/softnet")
	run := macRunner("-rwsr-xr-x root", "")
	run.out[tart+" --version"] = ok("2.42.0")
	run.out[tart+" run --help"] = ok(tartHelp)
	run.out[softnet+" --version"] = ok("0.23.0-potluck.1")
	run.out[softnetProbeLine(softnet)] = ok("")
	run.out["stat -L -f %Sp %Su "+softnet] = ok("-rwsr-xr-x root")
	report := Detect(context.Background(), run, relativeMac{mac(true)}, "/state")
	layer, _ := report.Layer("tart")
	if !layer.Available || layer.CLI != tart || layer.SoftnetPath != softnet {
		t.Fatalf("resolved layer %+v", layer)
	}
	o := ShareOptions{Report: report, Images: []string{DefaultPins()[0].Image}, NamePrefix: "m", DryRun: true, Conformance: testSuite, Sizes: []string{"c2m4"}}
	res, err := Check(context.Background(), run, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pulls[0].Command[0] != tart || res.Sizes[0].Config["tart"] != tart || res.Sizes[0].Config["softnet_path"] != softnet {
		t.Fatalf("paths not propagated: %+v", res)
	}
}

func TestHostDefaultImagesFollowMacOS(t *testing.T) {
	for _, tc := range []struct{ version, part string }{{"13.6", "potluck-box-tart-linux"}, {"15.6", "macos-sequoia-base"}, {"26.0", "macos-tahoe-base"}} {
		report := macReport()
		report.MacOS = tc.version
		pin, err := HostDefaultImage(report, "tart")
		if err != nil || !strings.Contains(pin.Image, tc.part) {
			t.Fatalf("macOS %s: %+v %v", tc.version, pin, err)
		}
		chosen, _, ok := SelectedPin(ShareOptions{Report: report})
		if !ok || chosen.Image != pin.Image {
			t.Fatalf("plan differs from default: %+v", chosen)
		}
	}
}

func TestHostPreflightImageResourceRefusalsBeforePull(t *testing.T) {
	for _, tc := range []struct {
		cpu    int
		memory int64
		sizes  []string
	}{{1, 1024, nil}, {8, 16384, []string{"c1m2"}}} {
		report := macReport()
		report.Cores, report.RAMMB = tc.cpu, tc.memory
		run := &fakeRunner{}
		_, err := Check(context.Background(), run, ShareOptions{Report: report, NamePrefix: "m", Conformance: testSuite, Sizes: tc.sizes})
		if err == nil {
			t.Fatal("undersized image/host was accepted")
		}
		if len(run.calls) != 0 {
			t.Fatalf("resource refusal ran %v", run.calls)
		}
	}
}
