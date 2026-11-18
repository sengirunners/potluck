package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

const (
	pinA = "reg.example/base@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	pinB = "reg.example/tools@sha256:0000000000000000000000000000000000000000000000000000000000000002"
)

func bigReport() Report {
	return Report{OS: "linux", Arch: "amd64", Cores: 32, RAMMB: 128 << 10, FreeDiskGB: 500, KVM: true, Layers: []Layer{
		{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}, Isolation: "sandboxed-kernel"},
		{Name: "tart"},
	}}
}

func TestShareRefusals(t *testing.T) {
	ctx := context.Background()
	run := &fakeRunner{}
	var bound []computeapi.BindRunTargetRequest
	b := binderFunc(func(_ context.Context, r computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		bound = append(bound, r)
		return computeapi.RunTargetView{}, nil
	})
	base := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Driver: "ref"}
	for name, mut := range map[string]func(*ShareOptions){
		"tag only":             func(o *ShareOptions) { o.Images = []string{"reg.example/base:latest"} },
		"one unpinned":         func(o *ShareOptions) { o.Images = []string{pinA, "reg.example/x"} },
		"short digest":         func(o *ShareOptions) { o.Images = []string{"x@sha256:abc"} },
		"upper digest":         func(o *ShareOptions) { o.Images = []string{"x@sha256:" + strings.Repeat("A", 64)} },
		"leading dash":         func(o *ShareOptions) { o.Images = []string{"--privileged@sha256:" + strings.Repeat("a", 64)} },
		"comma in name":        func(o *ShareOptions) { o.Images = []string{"a,b@sha256:" + strings.Repeat("a", 64)} },
		"uppercase first":      func(o *ShareOptions) { o.Images = []string{"Reg/x@sha256:" + strings.Repeat("a", 64)} },
		"unregistered runtime": func(o *ShareOptions) { o.OCIRuntime = "kata" },
		"other cli":            func(o *ShareOptions) { o.OCICLI = "podman" },
		"no prefix":            func(o *ShareOptions) { o.NamePrefix = "" },
		"net target no port":   func(o *ShareOptions) { o.NetForbid = []string{"192.0.2.1"} },
		"net target bad port":  func(o *ShareOptions) { o.NetAllow = []string{"192.0.2.1:0"} },
		"mail an option":       func(o *ShareOptions) { o.NetMail = []string{"--x"} },
		"oci net, no mail":     func(o *ShareOptions) { o.OCINetwork = "zm-egress" },
		"runc runtime":         func(o *ShareOptions) { o.OCIRuntime = "runc" },
		"tart not ready":       func(o *ShareOptions) { o.Layer = "tart" },
		"unknown layer":        func(o *ShareOptions) { o.Layer = "lxc" },
		"unknown size":         func(o *ShareOptions) { o.Sizes = []string{"c9m9"} },
		"size too big":         func(o *ShareOptions) { o.Report.Cores, o.Sizes = 4, []string{"c8m32"} },
		"incus unbindable": func(o *ShareOptions) {
			o.Layer = "incus"
			o.Report.Layers = append(o.Report.Layers, Layer{Name: "incus", Available: true, Supported: Supported["incus"]})
		},
	} {
		o := base
		mut(&o)
		if _, err := shareOn(ctx, run, b, o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(run.calls) != 0 || len(bound) != 0 {
		t.Errorf("a refusal ran something: %v bound %d", run.calls, len(bound))
	}
}

func TestShareNotReadyGivesOneRootCommand(t *testing.T) {
	r := bigReport()
	r.Layers = []Layer{{Name: "oci", Supported: true, Problems: []string{"no container CLI"}, RootSteps: []string{"apt-get install -y docker", "runsc install"}}}
	run := &fakeRunner{}
	_, err := shareOn(context.Background(), run, binderFunc(nil), ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h"})
	var nr *NotReadyError
	if !errors.As(err, &nr) || nr.RootCommand != `sudo sh -c 'apt-get install -y docker && runsc install'` {
		t.Fatalf("%v", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("ran %v", run.calls)
	}
}

func TestShareDryRun(t *testing.T) {
	run := &fakeRunner{}
	res, err := shareOn(context.Background(), run, binderFunc(nil), ShareOptions{Report: bigReport(), Images: []string{pinA, pinB},
		Sizes: []string{"c2m4"}, NamePrefix: "host", DryRun: true})
	if err != nil || len(run.calls) != 0 {
		t.Fatalf("%v %v", err, run.calls)
	}
	if len(res.Pulls) != 2 || strings.Join(res.Pulls[0].Command, " ") != "docker pull -- "+pinA {
		t.Errorf("pulls: %+v", res.Pulls)
	}
	s := res.Sizes[0]
	if s.Binding != "host-c2m4" || s.Config["vm"] != "oci" || s.Config["oci_runtime"] != "runsc" || s.Config["oci_cli"] != "docker" ||
		s.Config["cpu"] != "2" || s.Config["memory"] != "4096" || s.Config["images"] != "" || s.Bound ||
		!strings.Contains(strings.Join(s.Conformance, " "), "--config images="+pinA+","+pinB) {
		t.Errorf("size: %+v", s)
	}
	if got := strings.Join(s.Conformance[:4], " "); got != "zm-run-conformance tart --kind box" && !strings.HasPrefix(got, "zm-run-conformance tart --kind box") {
		t.Errorf("conformance: %v", s.Conformance)
	}
}

// testSuite is the conformance program every test's fake runner answers to.
// Share, Check and Precheck look the suite up on PATH when
// ShareOptions.Conformance is empty, so a test that left it empty would depend
// on the machine (a real potluck-conformance in ~/go/bin changes the name);
// shareOn, checkOn and precheckOn fill it in.
const testSuite = "zm-run-conformance"

func withSuite(o ShareOptions) ShareOptions {
	if o.Conformance == "" {
		o.Conformance = testSuite
	}
	return o
}

// noBinderMsg is what a nil binderFunc answers with; shareOn turns it into an
// error, because Bind keeps a binder's error in the size's Error, not in
// Share's error.
const noBinderMsg = "test bug: Share bound a size, but the test gave it no binder (binderFunc(nil))"

// shareOn is Share with the suite named, unless the test names its own. It
// fails with noBinderMsg when a nil binderFunc was reached.
func shareOn(ctx context.Context, run Runner, b Binder, o ShareOptions) (ShareResult, error) {
	res, err := Share(ctx, run, b, withSuite(o))
	for _, s := range res.Sizes {
		if strings.Contains(s.Error, noBinderMsg) {
			return res, fmt.Errorf("%s: %s", s.Binding, noBinderMsg)
		}
	}
	return res, err
}

func checkOn(ctx context.Context, run Runner, o ShareOptions) (ShareResult, error) {
	return Check(ctx, run, withSuite(o))
}

func precheckOn(ctx context.Context, run Runner, o ShareOptions) error {
	return Precheck(ctx, run, withSuite(o))
}

type binderFunc func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error)

// A nil binderFunc stands for a Binder the test never expects Share to reach;
// reaching it is an error (shareOn fails the test with it), not a nil-pointer
// panic that stops the rest of the package.
func (f binderFunc) BindRunTarget(ctx context.Context, r computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
	if f == nil {
		return computeapi.RunTargetView{}, errors.New(noBinderMsg)
	}
	return f(ctx, r)
}

// TestShareBindsOnlyWhatPassed runs share against a real in-process daemon:
// conformance passes for one size and fails for another; only the passing one
// is bound, with the pinned images, as an ephemeral credential-free box.
func TestShareBindsOnlyWhatPassed(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Compute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}

	run := &fakeRunner{out: map[string]Result{}, on: func(line string) (Result, bool) {
		switch {
		case strings.HasPrefix(line, "docker pull "):
			return Result{}, true
		case strings.HasPrefix(line, "zm-run-conformance "):
			if strings.Contains(line, "--config size=c4m16") {
				return Result{Exit: 1, Stdout: "PASS  peers\nFAIL  the target is available: sudo -n refused\nFAIL  handles\n"}, true
			}
			return Result{Stdout: "PASS"}, true
		}
		return Result{}, false
	}}
	res, err := shareOn(ctx, run, computeclient.Owner{C: owner}, ShareOptions{Report: bigReport(), Images: []string{pinA}, Sizes: []string{"c2m4", "c4m16"},
		NamePrefix: "mini", Driver: "ref"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed() || !res.Sizes[0].Bound || res.Sizes[1].Bound || res.Sizes[1].Passed || !strings.HasPrefix(res.Sizes[1].Error, "conformance failed at the target is available: sudo -n refused (exit 1)") {
		t.Fatalf("results: %+v", res.Sizes)
	}
	l, err := computeclient.RunTargets(ctx, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Targets) != 1 {
		t.Fatalf("bound: %+v", l.Targets)
	}
	tg := l.Targets[0]
	if tg.Name != "mini-c2m4" || tg.Kind != "box" || tg.Driver != "ref" || tg.Effective.Isolation != "ephemeral" || tg.Effective.Credentials ||
		len(tg.Images) != 1 || tg.Images[0] != pinA || tg.Config["memory"] != "4096" || tg.Config["vm"] != "oci" {
		t.Errorf("binding: %+v", tg)
	}
	// pulls come before conformance, and the failed size was never bound
	if len(run.calls) != 3 || !strings.HasPrefix(run.calls[0], "docker pull ") {
		t.Errorf("calls: %v", run.calls)
	}
	// a second share replaces its own bindings
	if _, err := shareOn(ctx, run, computeclient.Owner{C: owner}, ShareOptions{Report: bigReport(), Images: []string{pinA}, Sizes: []string{"c2m4"}, NamePrefix: "mini", Driver: "ref"}); err != nil {
		t.Errorf("repeat share: %v", err)
	}
}

// A conformance run that failed because a driver call ran past its limit
// (two shares at once on one Linux host, 2026-11-17: "release over 30s", "describe:
// no answer within 30s") says the machine may have been busy and to run
// share again; any other failure does not.
func TestShareSaysATimingFailureIsABusyMachine(t *testing.T) {
	for _, c := range []struct {
		name, stdout string
		busy         bool
	}{
		{"timing check", "PASS  describe\nFAIL  every verb answers in 30 s, acquire in 120 s: release over 30s\n", true},
		{"no answer", "FAIL  describe: describe: no answer within 30s\nFAIL  handles\n", true},
		{"a fault", "PASS  describe\nFAIL  the target is available: sudo -n refused\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := &fakeRunner{on: func(line string) (Result, bool) {
				if strings.HasPrefix(line, "zm-run-conformance ") {
					return Result{Exit: 1, Stdout: c.stdout}, true
				}
				return Result{}, true
			}}
			o := ShareOptions{Report: bigReport(), Images: []string{pinA}, Sizes: []string{"c2m4"}, NamePrefix: "mini", Driver: "ref"}
			res, err := checkOn(context.Background(), run, o)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Sizes) != 1 || res.Sizes[0].Passed || !strings.HasPrefix(res.Sizes[0].Error, "conformance failed at ") {
				t.Fatalf("sizes: %+v", res.Sizes)
			}
			if got := strings.Contains(res.Sizes[0].Error, "run this again once the machine is quiet"); got != c.busy {
				t.Fatalf("busy hint %v, want %v: %s", got, c.busy, res.Sizes[0].Error)
			}
		})
	}
}

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	// Tests never probe this machine's real router or tailnet; withForward
	// gives a test its targets.
	forwardTargets = func() []string { return nil }
	os.Exit(m.Run())
}

// A named OCI network is the owner's word that it is filtered, so share binds
// a box on one only with --net-mail, which conformance checks (zm-eakh.12.5.13).
func TestShareNamedOCINetworkNeedsMailCheck(t *testing.T) {
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c1m2"}, DryRun: true, OCINetwork: "zm-egress"}
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "--net-mail") {
		t.Fatalf("a named network without --net-mail: %v", err)
	}
	o.NetMail = []string{"smtp.example"}
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil || !strings.Contains(strings.Join(res.Sizes[0].Conformance, " "), "--net-mail smtp.example") {
		t.Fatalf("with --net-mail: %v %+v", err, res)
	}
	for _, n := range []string{"", "none"} {
		o.OCINetwork, o.NetMail = n, nil
		if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil {
			t.Errorf("oci_network %q: %v", n, err)
		}
	}
}

// ... and binds one only when a mail check passed: every one a SKIP (the
// server does not open from here) proves nothing and leaves it unbound.
func TestShareNamedOCINetworkBindsOnlyAfterAMailPass(t *testing.T) {
	for out, bound := range map[string]bool{
		"PASS  describe\nSKIP  a box cannot send mail: smtp.example:25 (192.0.2.1:25) does not open: not proven\n": false,
		"PASS  describe\nPASS  a box cannot send mail: smtp.example:587 (192.0.2.1:587) does not open\n":           true,
	} {
		run := &fakeRunner{on: func(l string) (Result, bool) {
			if strings.HasPrefix(l, "zm-run-conformance ") {
				return Result{Stdout: "driver tart\n" + out}, true
			}
			return Result{}, true
		}}
		var binds int
		b := binderFunc(func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
			binds++
			return computeapi.RunTargetView{}, nil
		})
		res, err := shareOn(context.Background(), run, b, ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h",
			Sizes: []string{"c1m2"}, OCINetwork: "zm-egress", NetMail: []string{"smtp.example"}})
		if err != nil {
			t.Fatal(err)
		}
		sr := res.Sizes[0]
		if sr.Bound != bound || (binds == 1) != bound || !bound && !strings.Contains(sr.Error, "try a mail server this machine reaches") {
			t.Errorf("%q: bound %v (%d binds), error %q, want bound %v", out, sr.Bound, binds, sr.Error, bound)
		}
	}
}

func TestShareRuntimeOverrideRegistered(t *testing.T) {
	r := bigReport()
	r.Layers[0].Runtimes = []string{"runsc", "kata-qemu"}
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h",
		OCIRuntime: "kata-qemu", Sizes: []string{"c1m2"}, DryRun: true})
	if err != nil || res.Sizes[0].Config["oci_runtime"] != "kata-qemu" {
		t.Fatalf("%v %+v", err, res)
	}
}

type listBinder struct {
	binderFunc
	names []string
}

func (l listBinder) RunTargets(context.Context, bool) (computeapi.RunTargetList, error) {
	var out computeapi.RunTargetList
	for _, n := range l.names {
		out.Targets = append(out.Targets, computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: n}})
	}
	return out, nil
}

func TestShareFailedRerunSaysStillBound(t *testing.T) {
	run := &fakeRunner{on: func(line string) (Result, bool) {
		if strings.HasPrefix(line, "zm-run-conformance ") {
			return Result{Exit: 1}, true
		}
		return Result{}, true
	}}
	b := listBinder{names: []string{"h-c2m4"}}
	res, err := shareOn(context.Background(), run, b, ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c2m4", "c4m16"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Sizes[0].StillBound || !strings.Contains(res.Sizes[0].Error, "still bound from before") {
		t.Errorf("%+v", res.Sizes[0])
	}
	if res.Sizes[1].StillBound || !strings.Contains(res.Sizes[1].Error, "h-c4m16 is not bound") {
		t.Errorf("%+v", res.Sizes[1])
	}
}

// Share passes the network targets for boxes as a run's are passed by hand
// (zm-eakh.12.5.12): the owner's, --net-peers, and each of this machine's
// addresses on a port share listens on while conformance runs, so a box that
// reaches the host fails rather than passing on a closed port.
func TestShareChecksTheBoxNetwork(t *testing.T) {
	var line string
	reached := errors.New("no conformance run")
	run := &fakeRunner{on: func(l string) (Result, bool) {
		if strings.HasPrefix(l, "zm-run-conformance ") {
			line = l
			args := strings.Fields(l)
			for i, a := range args {
				if a == "--net-forbid" && strings.HasPrefix(args[i+1], "127.0.0.1:") {
					c, err := net.DialTimeout("tcp", args[i+1], 2*time.Second)
					if reached = err; err == nil {
						c.Close()
					}
				}
			}
		}
		return Result{}, true
	}}
	b := binderFunc(func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		return computeapi.RunTargetView{}, nil
	})
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c2m4"},
		HostAddrs: []string{"127.0.0.1", "fd00::5"}, NetAllow: []string{"192.0.2.1:443"}, NetForbid: []string{"10.0.0.1:80"},
		NetMail: []string{"smtp.example", "mx.example:25"}}
	if _, err := shareOn(context.Background(), run, b, o); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{" --net-allow 192.0.2.1:443", " --net-forbid 10.0.0.1:80", " --net-forbid [fd00::5]:", " --net-peers",
		" --net-mail smtp.example", " --net-mail mx.example:25"} {
		if !strings.Contains(line, want) {
			t.Errorf("conformance %q lacks %q", line, want)
		}
	}
	if reached != nil {
		t.Errorf("the host address share names was not listening during conformance: %v", reached)
	}
	o.DryRun = true
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil || !strings.Contains(strings.Join(res.Sizes[0].Conformance, " "), "--net-forbid 127.0.0.1:<port>") {
		t.Errorf("dry run: %v %v", err, res.Sizes)
	}
}
