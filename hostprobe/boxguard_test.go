package hostprobe

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const kataRuntimes = `{"runsc":{"path":"/usr/bin/runsc"},"kata-qemu":{"runtimeType":"/opt/kata/runtime-rs/bin/containerd-shim-kata-v2"}}`

// kataHost is a docker host with Kata and runsc registered, zm's network
// made and its filter active, and the guard answering as given: its unit's
// state ("" for none), its script and its unit file.
func kataHost(state, script, unit string) (fakeFacts, *fakeRunner) {
	f, run := dockerHost(boxInspect(), "active", "", "systemctl")
	f.paths["/dev/kvm"] = true
	run.out["docker info --format {{json .Runtimes}}"] = ok(kataRuntimes)
	if state != "" {
		run.out["systemctl is-active "+BoxGuardUnit] = Result{Stdout: state + "\n", Exit: map[bool]int{true: 0, false: 3}[state == "active"]}
	}
	if state == "active" { // the guard sets it each round
		run.out["ip -d link show "+BoxNetBridge] = ok("5: br-potluckbox: <BROADCAST,UP> mtu 1500\n    bridge forward_delay 1500 no_linklocal_learn 1 vlan_filtering 0\n")
	}
	if script != "" {
		run.out["cat "+BoxGuardBin] = ok(script)
	}
	if unit != "" {
		run.out["cat /etc/systemd/system/"+BoxGuardUnit] = ok(unit)
	}
	return f, run
}

func guardUnitText() string { return strings.Join(BoxGuardUnitFile(), "\n") + "\n" }

// The root steps write exactly the embedded script and zm's unit, and start it.
func TestBoxGuardSteps(t *testing.T) {
	steps := BoxGuardSteps()
	var script string
	for _, s := range steps {
		if rest, ok := strings.CutPrefix(s, "printf %s "); ok {
			b64, _, _ := strings.Cut(rest, " ")
			b, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatal(err)
			}
			script = string(b)
		}
	}
	if script != boxGuardScript || !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatalf("installed script differs from the embedded one")
	}
	all := strings.Join(steps, "\n")
	for _, want := range []string{"mv -f " + BoxGuardBin + ".new " + BoxGuardBin, ">/etc/systemd/system/" + BoxGuardUnit, "systemctl enable " + BoxGuardUnit, "systemctl restart " + BoxGuardUnit,
		`"ExecStart=` + BoxGuardBin + ` potluck-box br-potluckbox potluck_box_guard"`, `"Requires=docker.service ` + BoxNetUnit + `"`, `"Restart=always"`} {
		if !strings.Contains(all, want) {
			t.Errorf("steps lack %q:\n%s", want, all)
		}
	}
	// The script fences what the guard is for, and runs no zm.
	for _, want := range []string{"meta ibrname != \"$br\" accept", "iifname . ether saddr . ip saddr @boxes accept",
		"iifname . arp saddr ether . arp saddr ip @boxes accept", "pre counter drop", "between_boxes meta ibrname \"$br\" counter drop"} {
		if !strings.Contains(strings.ReplaceAll(boxGuardScript, "from_box", "pre"), want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(boxGuardScript, "zm ") || strings.Contains(boxGuardScript, "potluck ") {
		t.Error("the guard script runs zm or potluck as root")
	}
}

// Doctor counts the guard only as zm installs it and only while it runs.
func TestDetectBoxGuard(t *testing.T) {
	for _, tc := range []struct {
		name, state, script, unit string
		guard                     bool
		problem                   string
	}{
		{"running as installed", "active", boxGuardScript, guardUnitText(), true, ""},
		{"not installed", "", "", "", false, "is not running"},
		{"stopped", "inactive", boxGuardScript, guardUnitText(), false, "is not running"},
		{"failed", "failed", boxGuardScript, guardUnitText(), false, "failed"},
		{"another script", "active", "#!/bin/sh\nexit 0\n", guardUnitText(), false, "is not this Potluck's"},
		{"another unit", "active", boxGuardScript, "[Unit]\n", false, "is not this Potluck's"},
		{"bridge still learns from link-local", "active-learning", boxGuardScript, guardUnitText(), false, "no_linklocal_learn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			if state == "active-learning" {
				state = "active"
			}
			f, run := kataHost(state, tc.script, tc.unit)
			if tc.state == "active-learning" {
				run.out["ip -d link show "+BoxNetBridge] = ok("5: br-potluckbox: <BROADCAST,UP>\n    bridge no_linklocal_learn 0\n")
				tc.state = "active"
			}
			l, _ := Detect(context.Background(), run, f, "").Layer("oci")
			if l.Network != BoxNet {
				t.Fatalf("fixture: zm's network not ready: %+v", l)
			}
			if l.Guard != tc.guard || !strings.Contains(l.GuardProblem, tc.problem) || (len(l.GuardSteps) > 0) == tc.guard {
				t.Fatalf("guard %v problem %q steps %d", l.Guard, l.GuardProblem, len(l.GuardSteps))
			}
			// An installed guard not running cuts every new box off: doctor says so.
			warned := strings.Contains(strings.Join(l.Warnings, " "), "has no network until it runs again")
			if want := tc.unit != "" && tc.state != "active"; warned != want {
				t.Errorf("warning %v, want %v: %q", warned, want, l.Warnings)
			}
		})
	}
}

// A Kata docker share whose boxes go on Potluck's network waits for the
// guard (share's root line installs it, and once it runs the share goes on);
// one with no network does not; one on an owner's network is refused, since
// no guard watches it. gVisor's never asks.
func TestShareKataDockerNeedsGuard(t *testing.T) {
	share := func(network, state, script, unit string) (ShareOptions, error) {
		f, run := kataHost(state, script, unit)
		o := ShareOptions{Report: Detect(context.Background(), run, f, ""), Images: []string{pinA}, NamePrefix: "test-kata",
			Sizes: []string{"c1m2"}, DryRun: true, Docker: true, OCIRuntime: "kata-qemu", OCINetwork: network}
		if network != "" {
			o.NetMail = []string{"smtp.example.com"}
		}
		_, err := Check(context.Background(), &fakeRunner{}, o)
		return o, err
	}
	o, err := share(BoxNet, "", "", "")
	var nr *NotReadyError
	if !errors.As(err, &nr) || !strings.Contains(nr.RootCommand, "systemctl restart "+BoxGuardUnit) || !strings.Contains(strings.Join(nr.Problems, " "), "anti-spoofing") {
		t.Fatalf("Kata docker on %s without the guard: %v", BoxNet, err)
	}
	if steps := strings.Join(o.RootSteps(), "\n"); !strings.Contains(steps, "systemctl restart "+BoxGuardUnit) {
		t.Fatalf("share's root steps lack the guard:\n%s", steps)
	}
	if err := Precheck(context.Background(), &fakeRunner{}, o); err != nil {
		t.Fatalf("precheck with the guard yet to install: %v", err)
	}
	if _, err := share(BoxNet, "active", boxGuardScript, guardUnitText()); err != nil {
		t.Fatalf("Kata docker with the guard running: %v", err)
	}
	if o, err := share("", "", "", ""); err != nil || strings.Contains(strings.Join(o.RootSteps(), "\n"), BoxGuardUnit) {
		t.Fatalf("Kata docker with no network asks for the guard: %v", err)
	}
	if _, err := share("mine", "active", boxGuardScript, guardUnitText()); err == nil || !strings.Contains(err.Error(), "needs Potluck's network") {
		t.Fatalf("Kata docker on an owner's network: %v", err)
	}
	// The other Potluck name: its guard is not the one share reads or installs.
	if _, err := share(LegacyBoxNet, "active", boxGuardScript, guardUnitText()); err == nil || !strings.Contains(err.Error(), "this machine's Potluck network is "+BoxNet) {
		t.Fatalf("Kata docker on %s on a %s machine: %v", LegacyBoxNet, BoxNet, err)
	}
	// gVisor's docker box (no unmasked /proc) never needs it.
	f, run := kataHost("", "", "")
	run.out["docker info --format {{json .Runtimes}}"] = ok(`{"runsc":{"path":"/usr/bin/runsc"},"runsc-root":{"path":"/usr/bin/runsc","runtimeArgs":["--allow-suid"]}}`)
	r := Detect(context.Background(), run, f, "")
	if l, _ := r.Layer("oci"); l.Runtime != "runsc" || l.GuardProblem != "" || len(l.GuardSteps) > 0 {
		t.Fatalf("a gVisor host is asked for the guard: %+v", l)
	}
}

// A machine set up under zm keeps zm's names for its guard too: the unit on
// br-zmbox is zm-box-guard.service, and a Kata docker share there asks for it.
func TestBoxGuardKeepsZmNames(t *testing.T) {
	ociDefaultPin(t)
	if u := legacyBoxNet.guardUnit(); u != "zm-box-guard.service" || BoxGuardUnit != "potluck-box-guard.service" {
		t.Fatalf("guard units %s, %s", u, BoxGuardUnit)
	}
	f, run := legacyHost(legacyInspect(), "active", strings.Join(legacyBoxNet.unit(0), "\n")+"\n")
	f.paths["/dev/kvm"] = true
	run.out["docker info --format {{json .Runtimes}}"] = ok(kataRuntimes)
	l, _ := Detect(context.Background(), run, f, "").Layer("oci")
	if l.Network != LegacyBoxNet || l.Guard || !strings.Contains(l.GuardProblem, "br-zmbox") {
		t.Fatalf("legacy machine without its guard: %+v", l)
	}
	steps := strings.Join(l.GuardSteps, "\n")
	if !strings.Contains(steps, "systemctl restart zm-box-guard.service") || !strings.Contains(steps, " zm-box br-zmbox zm_box_guard") || strings.Contains(steps, "potluck-box-guard.service") {
		t.Fatalf("legacy guard steps:\n%s", steps)
	}
	run.out["systemctl is-active zm-box-guard.service"] = ok("active\n")
	run.out["ip -d link show br-zmbox"] = ok("6: br-zmbox: <BROADCAST,UP>\n    bridge no_linklocal_learn 1\n")
	run.out["cat "+BoxGuardBin] = ok(boxGuardScript)
	run.out["cat /etc/systemd/system/zm-box-guard.service"] = ok(strings.Join(legacyBoxNet.guardUnitFile(), "\n") + "\n")
	if l, _ := Detect(context.Background(), run, f, "").Layer("oci"); !l.Guard {
		t.Fatalf("legacy guard running: %+v", l.GuardProblem)
	}
}
