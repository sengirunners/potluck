package hostprobe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// pinOwn is an owner's image that images.pin does not know.
const pinOwn = "reg.example/own@sha256:0000000000000000000000000000000000000000000000000000000000000003"

const dockerRunsc = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"}}`

// boxInspect is `docker network inspect --format '{{json .}}' potluck-box` for
// the network as the unit makes it (Docker 28's shape), with edits.
func boxInspect(edit ...string) string {
	s := `{"Name":"potluck-box","Id":"5f1c","Scope":"local","Driver":"bridge","EnableIPv4":true,"EnableIPv6":false,` +
		`"IPAM":{"Driver":"default","Options":{},"Config":[{"Subnet":"10.199.0.0/24","Gateway":"10.199.0.1"}]},` +
		`"Internal":false,"Attachable":false,"Ingress":false,"ConfigFrom":{"Network":""},"ConfigOnly":false,"Containers":{},` +
		`"Options":{"com.docker.network.bridge.enable_icc":"false","com.docker.network.bridge.name":"br-potluckbox"},"Labels":{}}`
	for i := 0; i+1 < len(edit); i += 2 {
		s = strings.Replace(s, edit[i], edit[i+1], 1)
	}
	return s
}

// dockerHost is a docker+runsc Linux box whose potluck-box network, unit and
// journal answer as given ("" for not there).
func dockerHost(inspect, active, journal string, bins ...string) (fakeFacts, *fakeRunner) {
	out := map[string]Result{"docker --version": ok("Docker version 28.5.1\n"), "docker info --format {{json .Runtimes}}": ok(dockerRunsc)}
	if inspect != "" {
		out["docker network inspect --format {{json .}} "+BoxNet] = ok(inspect + "\n")
	}
	if active != "" {
		out["systemctl is-active "+BoxNetUnit] = Result{Stdout: active + "\n", Exit: map[bool]int{true: 0, false: 3}[active == "active"]}
	}
	if journal != "" {
		out["journalctl -u "+BoxNetUnit+" -n 20 --no-pager -o cat"] = ok(journal)
	}
	return linux(append([]string{"docker", "runsc"}, bins...)...), &fakeRunner{out: out}
}

// Doctor finds zm's filtered network (zm-eakh.12.24.6) only as zm makes it, to
// the last option and address, and with its filter unit active; else it says
// why: a failed unit by its journal, not the install line again; a missing
// one with the steps in the one root command. It never makes the layer unready.
func TestDetectBoxNet(t *testing.T) {
	sys := []string{"systemctl"}
	for _, c := range []struct {
		name, inspect, active, journal string
		bins                           []string
		network, problem               string
		steps                          bool
	}{
		{name: "ready", inspect: boxInspect(), active: "active", bins: sys, network: BoxNet},
		{name: "ready, families recorded as options", inspect: boxInspect(`"Options":{"com`, `"Options":{"com.docker.network.enable_ipv4":"true","com`),
			active: "active", bins: sys, network: BoxNet},
		{name: "nothing yet", active: "inactive", bins: sys, problem: "is not set up", steps: true},
		{name: "network, no unit (made by hand)", inspect: boxInspect(), active: "inactive", bins: sys, problem: "is not set up", steps: true},
		{name: "unit, no network", active: "active", bins: sys, problem: "is not set up", steps: true},
		{name: "unit failed", active: "failed", journal: "x\nError response from daemon: invalid pool request: Pool overlaps with other one on this address space\n",
			bins: sys, problem: "failed: Error response from daemon: invalid pool request"},
		{name: "unit failed, journal unreadable", active: "failed", bins: sys, problem: "journalctl -u " + BoxNetUnit + " says why"},
		{name: "unit starting", active: "activating", bins: sys, problem: "is activating"},
		{name: "IPv6 on", inspect: boxInspect(`"EnableIPv6":false`, `"EnableIPv6":true`), active: "active", bins: sys, problem: "IPv6 on"},
		{name: "icc on", inspect: boxInspect(`enable_icc":"false"`, `enable_icc":"true"`), bins: sys, problem: "enable_icc=true"},
		{name: "routed gateway mode", inspect: boxInspect(`"Options":{"com`, `"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"routed","com`),
			active: "active", bins: sys, problem: "gateway_mode_ipv4=routed"},
		{name: "masquerade off", inspect: boxInspect(`"Options":{"com`, `"Options":{"com.docker.network.bridge.enable_ip_masquerade":"false","com`),
			active: "active", bins: sys, problem: "enable_ip_masquerade=false"},
		{name: "trusted host interfaces", inspect: boxInspect(`"Options":{"com`, `"Options":{"com.docker.network.bridge.trusted_host_interfaces":"eth0","com`),
			active: "active", bins: sys, problem: "trusted_host_interfaces=eth0"},
		{name: "internal", inspect: boxInspect(`"Internal":false`, `"Internal":true`), active: "active", bins: sys, problem: "internal"},
		{name: "another subnet", inspect: boxInspect("10.199.0.0/24", "10.0.0.0/8"), active: "active", bins: sys, problem: "addresses other than"},
		{name: "another bridge", inspect: boxInspect("br-potluckbox", "docker0"), active: "active", bins: sys, problem: "bridge.name=docker0"},
		{name: "no systemd", problem: "needs systemd"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, run := dockerHost(c.inspect, c.active, c.journal, c.bins...)
			r := Detect(context.Background(), run, f, "")
			l, _ := r.Layer("oci")
			if !l.Available || len(l.Problems) > 0 {
				t.Fatalf("the network made the layer unready: %+v", l)
			}
			if l.Network != c.network || !strings.Contains(l.NetworkProblem, c.problem) || (c.problem == "") != (l.NetworkProblem == "") {
				t.Errorf("network %q problem %q, want %q and %q", l.Network, l.NetworkProblem, c.network, c.problem)
			}
			if got := len(l.NetworkSteps) > 0; got != c.steps {
				t.Errorf("steps %v, want %v", l.NetworkSteps, c.steps)
			}
			cmd := RootCommand(r.RootSteps("oci"))
			if c.steps != (cmd != "") || c.steps && !strings.Contains(cmd, "systemctl restart "+BoxNetUnit) {
				t.Errorf("doctor's root command %q", cmd)
			}
		})
	}
	f, run := linux("podman", "runsc"), &fakeRunner{out: map[string]Result{"podman --version": ok("podman version 5.2.0\n")}}
	if l, _ := Detect(context.Background(), run, f, "").Layer("oci"); l.Available || l.Network != "" || len(l.NetworkSteps) > 0 || !strings.Contains(l.NetworkProblem, "verified local Docker") {
		t.Errorf("podman: %+v", l)
	}
}

// The unit: drops in the raw table (nothing ACCEPTs ahead of them there), each
// checked, then inserted only when missing (a restart leaves no gap), with the
// xtables lock waited for, one address per rule, IPv6 required where the host
// has it, the network made last, re-applied with docker, and nothing a systemd
// specifier or the root line's quoting would read.
func TestBoxNetUnit(t *testing.T) {
	u := BoxNetUnitFile()
	text := strings.Join(u, "\n")
	want := []string{
		"After=docker.service", "Requires=docker.service", "PartOf=docker.service", "WantedBy=docker.service", "Type=oneshot", "RemainAfterExit=yes",
		`ExecStart=sh -c "iptables -w -t raw -C PREROUTING -i br-potluckbox -p tcp -m multiport --dports 25,465,587,2525 -j DROP || iptables -w -t raw -I PREROUTING 1 -i br-potluckbox -p tcp -m multiport --dports 25,465,587,2525 -j DROP"`,
		`ExecStart=sh -c "iptables -w -t raw -C PREROUTING -i br-potluckbox -m addrtype --dst-type LOCAL -j DROP || iptables -w -t raw -I PREROUTING 1 -i br-potluckbox -m addrtype --dst-type LOCAL -j DROP"`,
		`ExecStart=sh -c "iptables -w -t raw -C PREROUTING -i br-potluckbox -m addrtype --dst-type BROADCAST -j DROP || iptables -w -t raw -I PREROUTING 1 -i br-potluckbox -m addrtype --dst-type BROADCAST -j DROP"`,
		`ExecStart=sh -c "test ! -d /proc/sys/net/ipv6 || ip6tables -w -t raw -C PREROUTING -i br-potluckbox -j DROP || ip6tables -w -t raw -I PREROUTING 1 -i br-potluckbox -j DROP"`,
	}
	for _, d := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "168.63.129.16/32", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4"} {
		want = append(want, `ExecStart=sh -c "iptables -w -t raw -C PREROUTING -i br-potluckbox -d `+d+` -j DROP || iptables -w -t raw -I PREROUTING 1 -i br-potluckbox -d `+d+` -j DROP"`)
	}
	for _, want := range append(want,
		`ExecStart=sh -c "docker network inspect potluck-box >/dev/null 2>&1 || docker network create --driver bridge --subnet 10.199.0.0/24 -o com.docker.network.bridge.enable_icc=false -o com.docker.network.bridge.name=br-potluckbox potluck-box"`,
		`ExecStart=sh -c "test ! -d /proc/sys/net/ipv6 || sysctl -q -w net.ipv6.conf.br-potluckbox.disable_ipv6=1"`,
	) {
		if !strings.Contains(text, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	if i, j := strings.Index(text, "docker network create"), strings.LastIndex(text, "-I PREROUTING"); i < j {
		t.Error("the network is made before the filter is in")
	}
	// No "/N," either: a rule naming several addresses is several rules, which -C would not check as one.
	for _, bad := range []string{"--ipv6", "ExecStart=-", " -D ", "DOCKER-USER", "FORWARD", "INPUT", "/8,", "/16,", "/4,"} {
		if strings.Contains(text, bad) {
			t.Errorf("unit has %q", bad)
		}
	}
	for _, l := range u {
		if strings.ContainsAny(l, "'`$\\%") {
			t.Errorf("unit line %q would break the root command's quoting or be a systemd specifier", l)
		}
		if strings.Count(l, "tables ") != strings.Count(l, "tables -w ") {
			t.Errorf("%q does not wait for the xtables lock", l)
		}
	}
}

// The root line's file-writing step, run through sh as sudo would run it,
// writes exactly BoxNetUnitFile (zm-eakh.12.24.6 review).
func TestBoxNetRootLineWritesTheUnit(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dst := filepath.Join(t.TempDir(), BoxNetUnit)
	line := RootCommand(BoxNetSteps()[:1])
	if !strings.HasPrefix(line, "sudo sh -c '") {
		t.Fatalf("root line %s", line)
	}
	script := strings.Replace(strings.TrimPrefix(line, "sudo "), "/etc/systemd/system/"+BoxNetUnit, dst, 1)
	if out, err := exec.Command(sh, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(BoxNetUnitFile(), "\n") + "\n"; string(b) != want {
		t.Errorf("the root line wrote\n%s\nwant\n%s", b, want)
	}
	full := RootCommand(BoxNetSteps())
	if !strings.HasSuffix(full, "systemctl daemon-reload && systemctl enable potluck-box-net.service && systemctl restart potluck-box-net.service'") {
		t.Errorf("root line %s", full)
	}
}

func TestGatewayFromRoutes(t *testing.T) {
	const routes = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\n"
	if g := gatewayFromRoutes(routes); g != "192.168.1.1" {
		t.Errorf("gateway %q", g)
	}
	if g := gatewayFromRoutes("Iface\tDestination\tGateway\n"); g != "" {
		t.Errorf("no default route: %q", g)
	}
}

func ociDefaultPin(t *testing.T) {
	withPins(t, DefaultPin{Layer: "oci", OS: "linux", Image: pinA, RunnerDir: "/home/runner"}, DefaultPin{Layer: "tart", MinHostMacOS: 26, Arch: "arm64", OS: "darwin", Image: pinB, RunnerDir: "actions-runner"})
}

// withForward swaps the forward-path targets share finds.
func withForward(t *testing.T, targets ...string) {
	old := forwardTargets
	forwardTargets = func() []string { return targets }
	t.Cleanup(func() { forwardTargets = old })
}

func withNet(r Report, network string, steps ...string) Report {
	r.Layers = append([]Layer(nil), r.Layers...)
	r.Layers[0].Network, r.Layers[0].NetworkSteps = network, steps
	if network == "" {
		r.Layers[0].NetworkProblem = "zm's filtered network is not set up"
	}
	return r
}

// A box serves GitHub Actions only with a runner_dir (zm-eakh.12.24.5): share
// takes it from zm's pin, or --runner-dir, never a guess; on oci the runner
// box gets zm's filtered network, else it is bound for jobs only with the
// root step named (zm-eakh.12.24.6). Conformance runs the config without
// runner_dir, so its boxes have the runner's network, and checks GitHub opens.
func TestShareRunnerDir(t *testing.T) {
	ociDefaultPin(t)
	steps := BoxNetSteps()
	for _, c := range []struct {
		name               string
		o                  ShareOptions
		runner, network    string
		mail, note, noNote string
	}{
		{name: "default image, zm's network", o: ShareOptions{Report: withNet(bigReport(), BoxNet)},
			runner: "/home/runner", network: BoxNet, mail: DefaultNetMail, noNote: "jobs only"},
		{name: "default image, no network yet", o: ShareOptions{Report: withNet(bigReport(), "", steps...)},
			note: "run as root once, then this again:\n  sudo sh -c 'printf"},
		{name: "own mail server", o: ShareOptions{Report: withNet(bigReport(), BoxNet), NetMail: []string{"smtp.example:587"}},
			runner: "/home/runner", network: BoxNet, mail: "smtp.example:587"},
		{name: "custom image, no flag", o: ShareOptions{Report: withNet(bigReport(), BoxNet), Images: []string{pinOwn}},
			note: "pass --runner-dir PATH"},
		{name: "default and custom image", o: ShareOptions{Report: withNet(bigReport(), BoxNet), Images: []string{pinA, pinOwn}},
			note: "pass --runner-dir PATH"},
		{name: "custom image, --runner-dir", o: ShareOptions{Report: withNet(bigReport(), BoxNet), Images: []string{pinOwn}, RunnerDir: "/opt/runner"},
			runner: "/opt/runner", network: BoxNet, mail: DefaultNetMail},
		{name: "--runner-dir none", o: ShareOptions{Report: withNet(bigReport(), BoxNet), RunnerDir: "none"}, noNote: "jobs only"},
		{name: "owner's network", o: ShareOptions{Report: withNet(bigReport(), "", steps...), OCINetwork: "mine", NetMail: []string{"mx.example"}},
			runner: "/home/runner", network: "mine", mail: "mx.example"},
		{name: "--oci-network none", o: ShareOptions{Report: withNet(bigReport(), BoxNet), OCINetwork: "none"}, network: "none", note: "(--oci-network none)"},
		{name: "mac", o: ShareOptions{Report: macReport()}, runner: "actions-runner"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := c.o
			o.NamePrefix, o.Sizes, o.DryRun = "h", []string{"c2m4"}, true
			res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
			if err != nil {
				t.Fatal(err)
			}
			cfg, conf := res.Sizes[0].Config, strings.Join(res.Sizes[0].Conformance, " ")
			if cfg["runner_dir"] != c.runner || res.RunnerDir != c.runner || cfg["oci_network"] != c.network {
				t.Errorf("config %v (result runner %q), want runner_dir %q oci_network %q", cfg, res.RunnerDir, c.runner, c.network)
			}
			if strings.Contains(conf, "runner_dir") || !strings.Contains(conf, "--config oci_network="+c.network) && c.network != "" {
				t.Errorf("conformance must check the runner's network without runner_dir: %s", conf)
			}
			if c.mail != "" && !strings.Contains(conf, "--net-mail "+c.mail) || c.mail == "" && strings.Contains(conf, "--net-mail") {
				t.Errorf("conformance %s, want --net-mail %q", conf, c.mail)
			}
			if allow := strings.Contains(conf, "--net-allow "+RunnerNetAllow); allow != (c.runner != "" && c.network != "") {
				t.Errorf("conformance %s: --net-allow %s %v", conf, RunnerNetAllow, allow)
			}
			notes := strings.Join(res.Notes, "\n")
			if c.note != "" && !strings.Contains(notes, c.note) || c.noNote != "" && strings.Contains(notes, c.noNote) {
				t.Errorf("notes %q, want %q, not %q", notes, c.note, c.noNote)
			}
			again, _ := shareOn(context.Background(), &fakeRunner{}, nil, o)
			if !reflect.DeepEqual(again.Sizes[0].Config, cfg) {
				t.Errorf("a re-run binds another config: %v then %v", cfg, again.Sizes[0].Config)
			}
		})
	}
	for _, bad := range []string{"$(id)", "a b", "it's", "-x", `a\b`} {
		if _, err := shareOn(context.Background(), &fakeRunner{}, nil, ShareOptions{Report: bigReport(), NamePrefix: "h", DryRun: true, RunnerDir: bad}); err == nil ||
			!strings.Contains(err.Error(), "--runner-dir") {
			t.Errorf("--runner-dir %q: %v", bad, err)
		}
	}
}

// The runner box binds with its runner_dir and network only once its mail
// check passed; on zm's network a SKIP leaves it unbound and names the way
// to lend for jobs only. Its conformance run forbids the forward path (the
// gateway, a tailnet address), or says that is unchecked.
func TestShareRunnerBoxBindsAfterTheNetChecks(t *testing.T) {
	ociDefaultPin(t)
	for out, bound := range map[string]bool{
		"PASS  a box cannot send mail: smtp.gmail.com:587 (192.0.2.9:587) does not open\n":           true,
		"SKIP  a box cannot send mail: smtp.gmail.com:25 (192.0.2.9:25) does not open: not proven\n": false,
	} {
		withForward(t, "192.168.1.1:53", tailnetDNS)
		var line string
		run := &fakeRunner{on: func(l string) (Result, bool) {
			if strings.HasPrefix(l, "zm-run-conformance ") {
				line = l
				return Result{Stdout: out}, true
			}
			return Result{}, true
		}}
		var got []computeapi.BindRunTargetRequest
		b := binderFunc(func(_ context.Context, r computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
			got = append(got, r)
			return computeapi.RunTargetView{}, nil
		})
		res, err := shareOn(context.Background(), run, b, ShareOptions{Report: withNet(bigReport(), BoxNet), NamePrefix: "h", Sizes: []string{"c2m4"}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, "--net-forbid 192.168.1.1:53") || !strings.Contains(line, "--net-forbid "+tailnetDNS) {
			t.Errorf("the forward path is not forbidden: %s", line)
		}
		if bound {
			if len(got) != 1 || got[0].Target.Config["runner_dir"] != "/home/runner" || got[0].Target.Config["oci_network"] != BoxNet {
				t.Errorf("bound %+v", got)
			}
		} else if len(got) != 0 || !strings.Contains(res.Sizes[0].Error, "--runner-dir none") {
			t.Errorf("a mail SKIP: bound %d, error %q", len(got), res.Sizes[0].Error)
		}
	}
	withForward(t)
	res, err := shareOn(context.Background(), &fakeRunner{on: func(string) (Result, bool) { return Result{}, true }}, binderFunc(func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		return computeapi.RunTargetView{}, nil
	}), ShareOptions{Report: withNet(bigReport(), BoxNet), NamePrefix: "h", Sizes: []string{"c2m4"}})
	if err != nil || !strings.Contains(strings.Join(res.Notes, "\n"), "forward path past this machine is not checked") {
		t.Errorf("no forward target: %v %q", err, res.Notes)
	}
}
