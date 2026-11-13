package hostprobe

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// legacyInspect is the inspect of the network as zm made it, under zm's names.
func legacyInspect(edit ...string) string {
	return boxInspect(append([]string{"potluck-box", "zm-box", "br-potluckbox", "br-zmbox"}, edit...)...)
}

// legacyHost is dockerHost for a machine whose network, unit and unit file
// have zm's names (a lender set up before the rename); unit "" for no file.
func legacyHost(inspect, active, unit string) (fakeFacts, *fakeRunner) {
	f, run := dockerHost("", "", "", "systemctl")
	if inspect != "" {
		run.out["docker network inspect --format {{json .}} "+LegacyBoxNet] = ok(inspect + "\n")
	}
	if active != "" {
		run.out["systemctl is-active "+legacyBoxNet.Unit] = Result{Stdout: active + "\n", Exit: map[bool]int{true: 0, false: 3}[active == "active"]}
	}
	if unit != "" {
		run.out["cat /etc/systemd/system/"+legacyBoxNet.Unit] = ok(unit)
	}
	return f, run
}

// A machine set up under zm keeps working under zm's names, whole: the ready
// network is found, and what doctor says to do about it names the old unit
// and bridge, never the new ones beside them (one subnet, one filter).
func TestDetectBoxNetKeepsZmNames(t *testing.T) {
	ociDefaultPin(t)
	ctx := context.Background()
	oldUnit := strings.Join(legacyBoxNet.unit(60), "\n") + "\n"

	f, run := legacyHost(legacyInspect(), "active", oldUnit)
	l, _ := Detect(ctx, run, f, "").Layer("oci")
	if l.Network != LegacyBoxNet || !l.NetworkLegacy || l.NetworkProblem != "" || len(l.NetworkSteps) > 0 || l.NetworkMbit != 60 {
		t.Errorf("ready under the old names: %+v", l)
	}

	f, run = legacyHost(legacyInspect(), "inactive", oldUnit)
	r := Detect(ctx, run, f, "")
	l, _ = r.Layer("oci")
	if l.Network != "" || !l.NetworkLegacy || !slices.Equal(l.NetworkSteps, legacyBoxNet.capSteps(60)[:4]) {
		t.Errorf("old unit inactive: network %q legacy %v steps\n%s", l.Network, l.NetworkLegacy, strings.Join(l.NetworkSteps, "\n"))
	}
	if cmd := RootCommand(r.RootSteps("oci")); !strings.Contains(cmd, "systemctl restart zm-box-net.service") || strings.Contains(cmd, "potluck-box") {
		t.Errorf("doctor's root line for the old names: %s", cmd)
	}

	f, run = legacyHost("", "failed", oldUnit)
	if l, _ := Detect(ctx, run, f, "").Layer("oci"); !strings.Contains(l.NetworkProblem, "zm-box-net.service failed") || l.NetworkMbit != 60 {
		t.Errorf("old unit failed: %+v", l)
	}

	f, run = legacyHost(legacyInspect("enable_icc\":\"false", "enable_icc\":\"true"), "active", "")
	if l, _ := Detect(ctx, run, f, "").Layer("oci"); l.Network != "" || !strings.Contains(l.NetworkProblem, "docker network zm-box exists but") {
		t.Errorf("old network not as made: %+v", l)
	}

	// Only the network (made by hand, no unit file anywhere) is the old one too.
	f, run = legacyHost(legacyInspect(), "", "")
	if l, _ := Detect(ctx, run, f, "").Layer("oci"); !l.NetworkLegacy || !strings.Contains(strings.Join(l.NetworkSteps, " "), "zm-box-net.service") {
		t.Errorf("old network, no unit: %+v", l)
	}
}

// Both names present: the new ones are read and the old left alone. Neither
// present: the new ones are made.
func TestDetectBoxNetPrefersPotluckNames(t *testing.T) {
	ctx := context.Background()
	f, run := dockerHost(boxInspect(), "active", "", "systemctl")
	run.out["docker network inspect --format {{json .}} zm-box"] = ok(legacyInspect() + "\n")
	run.out["systemctl is-active zm-box-net.service"] = ok("active\n")
	if l, _ := Detect(ctx, run, f, "").Layer("oci"); l.Network != BoxNet || l.NetworkLegacy {
		t.Errorf("both present: %+v", l)
	}
	f, run = dockerHost("", "inactive", "", "systemctl")
	l, _ := Detect(ctx, run, f, "").Layer("oci")
	if l.NetworkLegacy || !slices.Equal(l.NetworkSteps, BoxNetSteps()) || !strings.Contains(strings.Join(l.NetworkSteps, " "), "potluck-box-net.service") {
		t.Errorf("a machine with neither: %+v", l)
	}
}

// --net-mbit on a machine with the old names changes the old unit; share
// binds the old network by its name, and both names count as Potluck's own.
func TestShareNetMbitOnZmNames(t *testing.T) {
	ociDefaultPin(t)
	f, run := legacyHost(legacyInspect(), "active", strings.Join(legacyBoxNet.unit(0), "\n")+"\n")
	r := Detect(context.Background(), run, f, "")
	got := (ShareOptions{Report: r, Layer: "oci", NetMbit: 40, NetMbitSet: true}).RootSteps()
	if !slices.Equal(got, legacyBoxNet.capSteps(40)) || !strings.Contains(strings.Join(got, " "), "br-zmbox") {
		t.Errorf("a new cap on the old network runs\n%s", strings.Join(got, "\n"))
	}
	ol, _ := r.Layer("oci")
	if !slices.Equal(BoxNetCapStepsFor(ol, 40), got) || slices.Equal(BoxNetCapStepsFor(Layer{}, 40), got) {
		t.Error("BoxNetCapStepsFor follows the layer's names")
	}
	if !IsBoxNet("zm-box") || !IsBoxNet("potluck-box") || IsBoxNet("mine") {
		t.Error("IsBoxNet")
	}
}

// The new unit file is on disk (a setup that stopped part way, or a run of
// the new names) while the old network is the one live: the old names stay,
// for the new unit would make a second network on the old one's subnet.
func TestDetectBoxNetNewUnitFileOldNetworkLive(t *testing.T) {
	ociDefaultPin(t)
	ctx := context.Background()
	f, run := legacyHost(legacyInspect(), "active", strings.Join(legacyBoxNet.unit(0), "\n")+"\n")
	run.out["cat /etc/systemd/system/"+BoxNetUnit] = ok(strings.Join(currentBoxNet.unit(0), "\n") + "\n")
	run.out["systemctl is-active "+BoxNetUnit] = Result{Stdout: "inactive\n", Exit: 3}
	l, _ := Detect(ctx, run, f, "").Layer("oci")
	if l.Network != LegacyBoxNet || !l.NetworkLegacy || l.NetworkProblem != "" || len(l.NetworkSteps) > 0 {
		t.Errorf("new unit file, old network live: network %q legacy %v problem %q steps %d", l.Network, l.NetworkLegacy, l.NetworkProblem, len(l.NetworkSteps))
	}
}
