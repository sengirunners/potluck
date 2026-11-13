package hostprobe

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

func checksJSON(t *testing.T, cs ...BlockCheck) string {
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The block test runs conformance's net group on the binding's own driver
// and config, forbidding this machine's addresses on an open port, the
// router and the tailnet; it passes only when every net check ran and passed.
func TestBlockTest(t *testing.T) {
	withForward(t, "192.168.1.1:53", tailnetDNS)
	passing := []BlockCheck{
		{Name: "describe", OK: true},
		{Name: "a box cannot reach 192.168.1.1:53", OK: true, Group: "net"},
		{Name: "two boxes cannot reach each other", OK: true, Group: "net"},
	}
	o := BlockTestOptions{Binding: "mac-c2m4", DriverName: "tart", DriverSHA256: sumA, Driver: "/drivers/potluck-driver-tart", Suite: "potluck-conformance",
		Images: []string{pinB}, HostAddrs: []string{"192.0.2.10"},
		Config: map[string]string{"vm": "tart", "size": "c2m4", "runner_dir": "actions-runner", "reclaim_idle_s": "600",
			"image_name": "macos", "macos_slots": "1", "linux_slots": "1"}}
	for _, c := range []struct {
		name   string
		out    Result
		passed bool
		want   string
	}{
		{"all passed", Result{Stdout: checksJSON(t, passing...)}, true, ""},
		{"a net check skipped", Result{Stdout: checksJSON(t, append(passing, BlockCheck{Name: "a box cannot reach 100.100.100.100:53", OK: true, Skipped: true, Group: "net", Detail: "does not open from here"})...)}, false, "proves nothing"},
		{"a net check failed", Result{Exit: 1, Stdout: checksJSON(t, append(passing, BlockCheck{Name: "a box cannot reach 192.168.1.1:53", Group: "net", Detail: "it opened"})...)}, false, "failed: a box cannot reach 192.168.1.1:53: it opened"},
		{"describe failed", Result{Exit: 1, Stdout: checksJSON(t, BlockCheck{Name: "describe", Detail: "isolation none"})}, false, "failed: describe"},
		{"a nonzero exit", Result{Exit: 1, Stdout: checksJSON(t, passing...), Stderr: "boom"}, false, "exited 1: boom"},
		{"no net check", Result{Stdout: checksJSON(t, passing[0])}, false, "no network check"},
		{"not JSON", Result{Stdout: "PASS everything"}, false, "not its JSON results"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var line string
			run := &fakeRunner{on: func(l string) (Result, bool) { line = l; return c.out, true }}
			r := BlockTest(context.Background(), run, o)
			if r.Passed != c.passed || !strings.Contains(r.Error, c.want) || c.passed && r.Error != "" {
				t.Fatalf("passed %v error %q, want %v %q", r.Passed, r.Error, c.passed, c.want)
			}
			for _, w := range []string{"potluck-conformance /drivers/potluck-driver-tart --kind box ", "--config images=" + pinB, "--config vm=tart",
				"--only net --json", "--net-forbid 192.168.1.1:53", "--net-forbid " + tailnetDNS, "--net-forbid 192.0.2.10:", "--net-peers"} {
				if !strings.Contains(line, w) {
					t.Errorf("command lacks %q: %s", w, line)
				}
			}
			for _, k := range []string{"runner_dir", "reclaim_idle_s", "image_name", "macos_slots", "linux_slots"} {
				if strings.Contains(line, k+"=") {
					t.Errorf("command carries the bind-only key %s: %s", k, line)
				}
			}
		})
	}
}

// What cannot be proven fails before conformance runs.
func TestBlockTestRefusesUnproven(t *testing.T) {
	never := &fakeRunner{on: func(l string) (Result, bool) { t.Errorf("ran %s", l); return Result{}, true }}
	base := BlockTestOptions{Binding: "b", DriverName: "tart", DriverSHA256: sumA, Driver: "d", Images: []string{pinA}, HostAddrs: []string{"192.0.2.10"}}
	withForward(t)
	if r := BlockTest(context.Background(), never, base); r.Passed || !strings.Contains(r.Error, "cannot reach the LAN or the tailnet") {
		t.Errorf("nothing past this machine opens: %+v", r)
	}
	withForward(t, "192.168.1.1:53")
	for name, mut := range map[string]func(*BlockTestOptions){
		"no address": func(o *BlockTestOptions) { o.HostAddrs = []string{} },
		"no image":   func(o *BlockTestOptions) { o.Images = nil },
		"no pin":     func(o *BlockTestOptions) { o.DriverSHA256 = "" },
		"bad forbid": func(o *BlockTestOptions) { o.NetForbid = []string{"nas"} },
	} {
		o := base
		mut(&o)
		if r := BlockTest(context.Background(), never, o); r.Passed || r.Error == "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// The owner's own target counts when nothing past this machine opens by itself.
	withForward(t)
	o := base
	o.NetForbid = []string{"192.168.1.20:445"}
	var line string
	run := &fakeRunner{on: func(l string) (Result, bool) {
		line = l
		return Result{Stdout: checksJSON(t, BlockCheck{Name: "n", OK: true, Group: "net"})}, true
	}}
	if r := BlockTest(context.Background(), run, o); !r.Passed || !strings.Contains(line, "--net-forbid 192.168.1.20:445") || len(r.Notes) != 1 || r.Notes[0] != forwardUnchecked {
		t.Errorf("an owner's target: %+v %s", r, line)
	}
}

func TestRecordBlockTest(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadBlockTest(dir, "mac-c2m4"); ok || err != nil {
		t.Fatalf("none recorded: %v %v", ok, err)
	}
	want := BlockTestResult{Binding: "mac-c2m4", Passed: true, Forbid: []string{tailnetDNS}}
	if err := RecordBlockTest(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadBlockTest(dir, "mac-c2m4")
	if !ok || err != nil || !got.Passed || got.Binding != want.Binding {
		t.Fatalf("got %+v %v %v", got, ok, err)
	}
	for _, bad := range []string{"../x", "a/b", "", ".hidden"} {
		if err := RecordBlockTest(dir, BlockTestResult{Binding: bad}); err == nil {
			t.Errorf("binding %q: recorded", bad)
		}
	}
	// A record that cannot be read, or names another binding, is never a pass.
	p := filepath.Join(dir, "isolation", "mac-c2m4.json")
	for _, body := range []string{"{", `{"binding":"other","passed":true}`} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if r, ok, err := LoadBlockTest(dir, "mac-c2m4"); err == nil || ok || r.Passed {
			t.Errorf("%s: %+v %v %v", body, r, ok, err)
		}
	}
}

func TestGatewayFromRouteGet(t *testing.T) {
	const out = "   route to: default\ndestination: default\n       mask: default\n    gateway: 192.168.1.1\n  interface: en0\n"
	if g := gatewayFromRouteGet(out); g != "192.168.1.1" {
		t.Errorf("got %q", g)
	}
	for _, s := range []string{"    gateway: utun4\n", "    gateway: fe80::1%en0\n", "interface: en0\n", ""} {
		if g := gatewayFromRouteGet(s); g != "" {
			t.Errorf("%q: got %q", s, g)
		}
	}
}

// Every Tart box gets egress through Softnet, so share forbids the forward
// path on Tart too, or says that is unchecked.
func TestShareTartForbidsForwardPath(t *testing.T) {
	ociDefaultPin(t)
	withForward(t, "192.168.1.1:53", tailnetDNS)
	var line string
	run := &fakeRunner{on: func(l string) (Result, bool) {
		if strings.HasPrefix(l, "zm-run-conformance ") {
			line = l
		}
		return Result{}, true
	}}
	bind := binderFunc(func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		return computeapi.RunTargetView{}, nil
	})
	if _, err := shareOn(context.Background(), run, bind, ShareOptions{Report: macReport(), NamePrefix: "m", Sizes: []string{"c2m4"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "--net-forbid 192.168.1.1:53") || !strings.Contains(line, "--net-forbid "+tailnetDNS) {
		t.Errorf("tart's conformance run does not forbid the forward path: %s", line)
	}
	withForward(t)
	res, err := shareOn(context.Background(), &fakeRunner{on: func(string) (Result, bool) { return Result{}, true }}, bind,
		ShareOptions{Report: macReport(), NamePrefix: "m", Sizes: []string{"c2m4"}})
	if err != nil || !strings.Contains(strings.Join(res.Notes, "\n"), forwardUnchecked) {
		t.Errorf("no forward target on tart: %v %q", err, res.Notes)
	}
}

// Check's conformance run config and suiteConfig agree: every key a size's
// binding carries beyond its run's is bind-only, so the block test runs
// exactly the config share checked.
func TestSuiteConfigMatchesShare(t *testing.T) {
	ociDefaultPin(t)
	for name, o := range map[string]ShareOptions{
		"oci runner":  {Report: withNet(bigReport(), BoxNet)},
		"mac":         {Report: macReport(), ReclaimIdleS: 600},
		"jobs only":   {Report: withNet(bigReport(), BoxNet), RunnerDir: "none"},
		"one at once": {Report: smallReport(), Images: []string{pinA}, RunnerDir: "none"},
	} {
		o.NamePrefix, o.DryRun = "h", true
		res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
		if err != nil || len(res.Sizes) == 0 {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		for _, sr := range res.Sizes {
			ran := map[string]string{}
			for i, a := range sr.Conformance {
				if a == "--config" {
					k, v, _ := strings.Cut(sr.Conformance[i+1], "=")
					ran[k] = v
				}
			}
			got := suiteConfig(sr.Config, res.Images)
			if len(got) != len(ran) {
				t.Errorf("%s %s: suiteConfig %v, conformance ran %v", name, sr.Binding, got, ran)
			}
			for k, v := range ran {
				if got[k] != v {
					t.Errorf("%s %s: %s=%q, conformance ran %q", name, sr.Binding, k, got[k], v)
				}
			}
		}
	}
}

// Two driver programs' sums, as pinned.
const (
	sumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// A record counts only for the binding it tested; link-local host
// addresses are not forbidden (the suite skips them, failing the test).
func TestBlockTestFingerprintAndLinkLocal(t *testing.T) {
	cfg := map[string]string{"vm": "tart", "size": "c2m4", "reclaim_idle_s": "600"}
	fp := BindingFingerprint("tart", sumA, cfg, []string{pinB})
	moved := map[string]string{"vm": "tart", "size": "c2m4", "reclaim_idle_s": "900"}
	if BindingFingerprint("tart", sumA, moved, []string{pinB}) != fp {
		t.Errorf("a bind-only key changed the fingerprint")
	}
	for name, other := range map[string]string{
		"driver":         BindingFingerprint("local", sumA, cfg, []string{pinB}),
		"config":         BindingFingerprint("tart", sumA, map[string]string{"vm": "oci", "size": "c2m4"}, []string{pinB}),
		"images":         BindingFingerprint("tart", sumA, cfg, []string{pinA}),
		"driver program": BindingFingerprint("tart", sumB, cfg, []string{pinB}),
	} {
		if other == fp {
			t.Errorf("another %s, same fingerprint", name)
		}
	}
	withForward(t, "192.168.1.1:53")
	var line string
	run := &fakeRunner{on: func(l string) (Result, bool) {
		line = l
		return Result{Stdout: checksJSON(t, BlockCheck{Name: "n", OK: true, Group: "net"})}, true
	}}
	r := BlockTest(context.Background(), run, BlockTestOptions{Binding: "b", DriverName: "tart", DriverSHA256: sumA, Driver: "d", Config: cfg, Images: []string{pinB},
		HostAddrs: []string{"169.254.3.4", "192.0.2.10"}})
	if !r.Passed || r.Fingerprint != fp || strings.Contains(line, "169.254.3.4") || !strings.Contains(line, "192.0.2.10:") {
		t.Errorf("%+v: %s", r, line)
	}
	if r := BlockTest(context.Background(), run, BlockTestOptions{Binding: "b", DriverName: "tart", DriverSHA256: sumA, Driver: "d", Images: []string{pinB}, HostAddrs: []string{"169.254.3.4"}}); r.Passed {
		t.Errorf("only a link-local address: passed")
	}
}
