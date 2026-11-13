package hostprobe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The cap (zm-eakh.12.43): two tc lines on the bridge, after the network is
// made, in the unit that keeps the filter, so Docker restarting re-applies
// both; no cap leaves the unit as it was; the installed unit says its cap.
func TestBoxNetCapUnit(t *testing.T) {
	plain := BoxNetUnitFile()
	if got := currentBoxNet.unit(0); !slices.Equal(got, plain) {
		t.Errorf("no cap changed the unit:\n%s", strings.Join(got, "\n"))
	}
	u := currentBoxNet.unit(60)
	want := []string{
		"ExecStart=tc qdisc replace dev br-potluckbox root handle 1: tbf rate 60mbit burst 256kb latency 50ms",
		"ExecStart=tc qdisc replace dev br-potluckbox parent 1:1 handle 10: fq_codel",
	}
	at := slices.Index(u, want[0])
	made := slices.IndexFunc(u, func(l string) bool { return strings.Contains(l, "docker network create") })
	if at < 0 || at < made || u[at+1] != want[1] || len(u) != len(plain)+2 || u[len(u)-2] != "[Install]" {
		t.Fatalf("the capped unit:\n%s", strings.Join(u, "\n"))
	}
	if rest := slices.Delete(slices.Clone(u), at, at+2); !slices.Equal(rest, plain) {
		t.Errorf("the cap changed other lines:\n%s", strings.Join(rest, "\n"))
	}
	if strings.ContainsAny(strings.Join(want, ""), "$`\\'%") {
		t.Errorf("a cap line holds what the root line's quoting or systemd would read")
	}
	// A fast link's burst holds a timer tick of the rate (tbf sends none faster).
	if l := currentBoxNet.unit(1000)[at]; !strings.Contains(l, "rate 1000mbit burst 1000kb ") {
		t.Errorf("1000 Mbit/s: %s", l)
	}
	for mbit, text := range map[int]string{60: strings.Join(u, "\n"), 1000: strings.Join(currentBoxNet.unit(1000), "\n"), 0: strings.Join(plain, "\n"), -1: ""} {
		if got := boxNetCapOf(text); got != max(mbit, 0) {
			t.Errorf("the unit capped at %d reads as %d", mbit, got)
		}
	}
}

// The cap's root line, run through sh as sudo would, writes exactly the
// capped unit and restarts it; removing a cap also deletes the live qdisc,
// which a unit without tc lines would leave.
func TestBoxNetCapSteps(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dst := filepath.Join(t.TempDir(), BoxNetUnit)
	script := strings.Replace(RootScript(BoxNetCapSteps(48)[:1]), "/etc/systemd/system/"+BoxNetUnit, dst, 1)
	if out, err := exec.Command(sh, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, _ := os.ReadFile(dst)
	if want := strings.Join(currentBoxNet.unit(48), "\n") + "\n"; string(b) != want {
		t.Errorf("the root line wrote\n%s\nwant\n%s", b, want)
	}
	if s := RootScript(BoxNetCapSteps(48)); !strings.HasSuffix(s, "systemctl restart "+BoxNetUnit) || strings.Contains(s, "qdisc del") {
		t.Errorf("cap steps: %s", s)
	}
	if s := RootScript(BoxNetCapSteps(0)); !strings.HasSuffix(s, "systemctl restart "+BoxNetUnit+" && (tc qdisc del dev br-potluckbox root 2>/dev/null || true)") {
		t.Errorf("uncap steps: %s", s)
	}
}

// Doctor reads the cap from the installed unit (no root needed).
func TestDetectBoxNetCap(t *testing.T) {
	for _, c := range []struct {
		name, unit string
		want       int
	}{
		{"capped", strings.Join(currentBoxNet.unit(60), "\n") + "\n", 60},
		{"not capped", strings.Join(BoxNetUnitFile(), "\n") + "\n", 0},
		{"unit unreadable", "", 0},
	} {
		if c.want > 0 { // a failed unit still says its cap
			f, run := dockerHost(boxInspect(), "failed", "", "systemctl")
			run.out["cat /etc/systemd/system/"+BoxNetUnit] = ok(c.unit)
			if l, _ := Detect(context.Background(), run, f, "").Layer("oci"); l.Network != "" || l.NetworkMbit != c.want {
				t.Errorf("failed unit: network %q capped at %d, want none and %d", l.Network, l.NetworkMbit, c.want)
			}
		}
		f, run := dockerHost(boxInspect(), "active", "", "systemctl")
		if c.unit != "" {
			run.out["cat /etc/systemd/system/"+BoxNetUnit] = ok(c.unit)
		}
		l, _ := Detect(context.Background(), run, f, "").Layer("oci")
		if l.Network != BoxNet || l.NetworkMbit != c.want {
			t.Errorf("%s: network %q capped at %d, want %d", c.name, l.Network, l.NetworkMbit, c.want)
		}
	}
}

// A re-run without --net-mbit keeps the cap when the network must be made
// again: whenever the unit file is there (the unit inactive, or active with
// the network removed), the steps that remake it carry the file's cap.
func TestDetectBoxNetKeepsTheCapWhenRemaking(t *testing.T) {
	ociDefaultPin(t)
	capped := strings.Join(currentBoxNet.unit(60), "\n") + "\n"
	for _, c := range []struct{ name, inspect, active string }{
		{"unit inactive", boxInspect(), "inactive"},
		{"unit active, network gone", "", "active"},
	} {
		f, run := dockerHost(c.inspect, c.active, "", "systemctl")
		run.out["cat /etc/systemd/system/"+BoxNetUnit] = ok(capped)
		r := Detect(context.Background(), run, f, "")
		l, _ := r.Layer("oci")
		if l.Network != "" || l.NetworkMbit != 60 || !slices.Equal(l.NetworkSteps, BoxNetCapSteps(60)) {
			t.Errorf("%s: network %q, cap %d, steps\n%s\nwant the capped unit's", c.name, l.Network, l.NetworkMbit, strings.Join(l.NetworkSteps, "\n"))
		}
		if got := (ShareOptions{Report: r, Layer: "oci"}).RootSteps(); !slices.Equal(got, BoxNetCapSteps(60)) {
			t.Errorf("%s: a share without --net-mbit would run\n%s", c.name, strings.Join(got, "\n"))
		}
	}
	// No unit file, or one without a cap: the steps as before.
	f, run := dockerHost("", "inactive", "", "systemctl")
	if l, _ := Detect(context.Background(), run, f, "").Layer("oci"); l.NetworkMbit != 0 || !slices.Equal(l.NetworkSteps, BoxNetSteps()) {
		t.Errorf("no unit file: cap %d, steps %v", l.NetworkMbit, l.NetworkSteps)
	}
}

// --net-mbit (zm-eakh.12.43): share's root steps set the cap where the
// installed unit differs, with the network's own steps when it is not made
// yet; unset keeps what is installed; it is refused where potluck does not
// keep the network.
func TestShareNetMbit(t *testing.T) {
	ociDefaultPin(t)
	ready := func(mbit int) Report {
		r := withNet(bigReport(), BoxNet)
		r.Layers[0].NetworkMbit = mbit
		return r
	}
	failed := func(mbit int) Report {
		r := withNet(bigReport(), "")
		r.Layers[0].NetworkProblem, r.Layers[0].NetworkMbit = BoxNetUnit+" failed", mbit
		return r
	}
	for _, c := range []struct {
		name  string
		o     ShareOptions
		steps []string
	}{
		{"unset keeps the installed cap", ShareOptions{Report: ready(60)}, nil},
		{"as installed", ShareOptions{Report: ready(60), NetMbit: 60, NetMbitSet: true}, nil},
		{"a new cap", ShareOptions{Report: ready(0), NetMbit: 60, NetMbitSet: true}, BoxNetCapSteps(60)},
		{"another cap", ShareOptions{Report: ready(60), NetMbit: 40, NetMbitSet: true}, BoxNetCapSteps(40)},
		{"removed", ShareOptions{Report: ready(60), NetMbitSet: true}, BoxNetCapSteps(0)},
		{"network not made yet", ShareOptions{Report: withNet(bigReport(), "", BoxNetSteps()...), NetMbit: 60, NetMbitSet: true}, BoxNetCapSteps(60)},
		{"a cap the kernel refused (the unit failed) is removed", ShareOptions{Report: failed(60), NetMbitSet: true}, BoxNetCapSteps(0)},
		{"a failed unit without a cap is not the cap's to fix", ShareOptions{Report: failed(0), NetMbit: 60, NetMbitSet: true}, nil},
		{"no runner, no network, no cap", ShareOptions{Report: ready(0), NetMbit: 60, NetMbitSet: true, RunnerDir: "none"}, nil},
	} {
		if got := c.o.RootSteps(); !slices.Equal(got, c.steps) {
			t.Errorf("%s: root steps\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.steps, "\n"))
		}
	}
	dry := func(o ShareOptions) (ShareResult, error) {
		o.NamePrefix, o.DryRun, o.Driver = "h", true, "ref"
		return checkOn(context.Background(), nil, o)
	}
	// Pending (--no-sudo, or no terminal): the share goes on and names the root line.
	res, err := dry(ShareOptions{Report: ready(0), NetMbit: 60, NetMbitSet: true})
	if note := strings.Join(res.Notes, "\n"); err != nil || !strings.Contains(note, "not capped at 60 Mbit/s yet") || !strings.Contains(note, RootCommand(BoxNetCapSteps(60))) {
		t.Errorf("pending cap: %v, notes %q", err, note)
	}
	if res, err := dry(ShareOptions{Report: ready(60), NetMbit: 60, NetMbitSet: true}); err != nil || strings.Contains(strings.Join(res.Notes, "\n"), "capped") {
		t.Errorf("installed cap: %v, notes %q", err, res.Notes)
	}
	// Not made yet: the one root line makes the network with its cap.
	res, err = dry(ShareOptions{Report: withNet(bigReport(), "", BoxNetSteps()...), NetMbit: 60, NetMbitSet: true})
	if note := strings.Join(res.Notes, "\n"); err != nil || !strings.Contains(note, RootCommand(BoxNetCapSteps(60))) {
		t.Errorf("no network yet: %v, notes %q", err, note)
	}
	// A failed unit with no cap: the refusal says the unit failed, not that the boxes are on another network.
	if _, err := dry(ShareOptions{Report: failed(0), NetMbit: 60, NetMbitSet: true}); err == nil || !strings.Contains(err.Error(), "--net-mbit") || !strings.Contains(err.Error(), BoxNetUnit+" failed") {
		t.Errorf("failed unit, no cap: %v", err)
	}
	for name, o := range map[string]ShareOptions{
		"negative":            {Report: ready(0), NetMbit: -1, NetMbitSet: true},
		"an owner's network":  {Report: ready(0), NetMbit: 60, NetMbitSet: true, OCINetwork: "mine", NetMail: []string{"smtp.example"}},
		"no network (--none)": {Report: ready(0), NetMbit: 60, NetMbitSet: true, RunnerDir: "none"},
	} {
		if _, err := dry(o); err == nil || !strings.Contains(err.Error(), "--net-mbit") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A layer other than oci has no network of zm's to cap: refused, a cap or
	// its removal alike, never accepted and ignored (zm-eakh.12.53 item 10).
	pins := []string{"ghcr.io/x/ubuntu@sha256:" + strings.Repeat("a", 64)}
	for _, layer := range []string{"tart"} { // share lends no incus layer yet (Supported)
		rep := Report{OS: "darwin", Arch: "arm64", Cores: 16, RAMMB: 48 << 10, FreeDiskGB: 500,
			Layers: []Layer{{Name: layer, Available: true, Supported: true}}}
		for _, mbit := range []int{60, 0} {
			o := ShareOptions{Report: rep, Images: pins, Sizes: []string{"c2m4"}, NetMbit: mbit, NetMbitSet: true}
			if _, err := dry(o); err == nil || !strings.Contains(err.Error(), "--net-mbit is for the oci layer") || !strings.Contains(err.Error(), layer) {
				t.Errorf("--net-mbit %d on %s: %v", mbit, layer, err)
			}
			if got := o.RootSteps(); len(got) != 0 {
				t.Errorf("--net-mbit %d on %s: root steps %v", mbit, layer, got)
			}
		}
		if _, err := dry(ShareOptions{Report: rep, Images: pins, Sizes: []string{"c2m4"}}); err != nil {
			t.Errorf("%s without --net-mbit: %v", layer, err)
		}
	}
}
