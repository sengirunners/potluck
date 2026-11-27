package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/vmhost"
)

type fakeFacts struct {
	os, arch string
	cores    int
	ramMB    int64
	disk     float64
	paths    map[string]bool
	noaccess map[string]bool // exists, but cannot be opened read/write
	bins     map[string]bool
	engine   error // what the driver's Docker engine check says
}

// Getenv keeps tartDir off the real home directory.
func (f fakeFacts) Getenv(k string) string {
	if k == "HOME" {
		return "/nonexistent-home"
	}
	return ""
}
func (f fakeFacts) OS() string                         { return f.os }
func (f fakeFacts) Arch() string                       { return f.arch }
func (f fakeFacts) Cores() int                         { return f.cores }
func (f fakeFacts) RAMMB() int64                       { return f.ramMB }
func (f fakeFacts) FreeDiskGB(string) (float64, error) { return f.disk, nil }
func (f fakeFacts) OpenRW(p string) error {
	if f.noaccess[p] {
		return fmt.Errorf("permission denied")
	}
	return nil
}
func (f fakeFacts) OCIEngine(string, string) error   { return f.engine }
func (f fakeFacts) Exists(p string) bool             { return f.paths[p] }
func (f fakeFacts) LookPath(n string) (string, bool) { return "/fake/" + n, f.bins[n] }

func linux(bins ...string) fakeFacts {
	f := fakeFacts{os: "linux", arch: "amd64", cores: 16, ramMB: 128 << 10, disk: 500, paths: map[string]bool{}, bins: map[string]bool{"apt-get": true}}
	for _, b := range bins {
		f.bins[b] = true
	}
	return f
}

// fakeRunner answers by the command line and records every call.
type fakeRunner struct {
	mu    sync.Mutex
	out   map[string]Result
	calls []string
	on    func(line string) (Result, bool)
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (Result, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	r.mu.Lock()
	r.calls = append(r.calls, line)
	r.mu.Unlock()
	if r.on != nil {
		if res, ok := r.on(line); ok {
			return res, nil
		}
	}
	if res, ok := r.out[line]; ok {
		return res, nil
	}
	return Result{}, fmt.Errorf("fake: no such command %s", name)
}

func ok(s string) Result { return Result{Stdout: s} }

func TestDetectLayers(t *testing.T) {
	ctx := context.Background()
	const dockerRT = `{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"}}`
	cases := []struct {
		name    string
		facts   fakeFacts
		out     map[string]Result
		layer   string
		avail   bool
		isoln   string
		runtime string
		cli     string
		problem string // substring of a problem
		step    string // substring of a root step
	}{
		{name: "incus with kvm", layer: "incus", avail: true, isoln: "vm",
			facts: func() fakeFacts { f := linux("incus"); f.paths["/dev/kvm"] = true; return f }(),
			out:   map[string]Result{"incus version": ok("Client version: 6.0.0\nServer version: 6.0.1\n")}},
		{name: "incus without kvm", layer: "incus", isoln: "vm", problem: "/dev/kvm",
			facts: linux("incus"), out: map[string]Result{"incus version": ok("Client version: 6.0.0\n")}},
		{name: "incus missing", layer: "incus", isoln: "vm", problem: "not installed", step: "apt-get install -y incus", facts: linux()},
		{name: "docker with runsc", layer: "oci", avail: true, isoln: "sandboxed-kernel", runtime: "runsc", cli: "docker",
			facts: linux("docker", "runsc"),
			out: map[string]Result{"docker --version": ok("Docker version 27.1.1, build abc\n"),
				"docker info --format {{json .Runtimes}}": ok(dockerRT)}},
		{name: "docker with only runc", layer: "oci", cli: "docker", problem: "installs gVisor (runsc) release", step: "gvisor.tar.bz2",
			facts: linux("docker"),
			out: map[string]Result{"docker --version": ok("Docker version 27.1.1\n"),
				"docker info --format {{json .Runtimes}}": ok(`{"runc":{"path":"runc"},"crun":{"path":"crun"}}`)}},
		{name: "docker with unregistered runsc", layer: "oci", cli: "docker", problem: "not registered", step: "runsc install",
			facts: linux("docker", "runsc"),
			out: map[string]Result{"docker --version": ok("Docker version 27.1.1\n"),
				"docker info --format {{json .Runtimes}}": ok(`{"runc":{"path":"runc"}}`)}},
		{name: "podman with kata", layer: "oci", problem: "verified local Docker", cli: "podman",
			facts: func() fakeFacts { f := linux("podman", "kata-runtime"); f.paths["/dev/kvm"] = true; return f }(),
			out:   map[string]Result{"podman --version": ok("podman version 5.2.0\n")}},
		{name: "podman with kata, no kvm", layer: "oci", cli: "podman", problem: "verified local Docker",
			facts: linux("podman", "kata-runtime"), out: map[string]Result{"podman --version": ok("podman version 5.2.0\n")}},
		{name: "macos with tart", layer: "tart", avail: true, isoln: "vm", cli: "/fake/tart",
			facts: fakeFacts{os: "darwin", arch: "arm64", cores: 10, ramMB: 32 << 10, disk: 500, bins: map[string]bool{"tart": true, "softnet": true}},
			out: map[string]Result{"sysctl -n kern.hv_support": ok("1\n"), "/fake/tart --version": ok("2.20.0\n"),
				"/fake/tart run --help":                 ok(tartHelp),
				"/usr/bin/sw_vers -productVersion":      ok("26.0\n"),
				"/usr/sbin/sysctl -n hw.optional.arm64": ok("1\n"),
				"/fake/softnet --version":               ok("0.25.0\n"),
				softnetProbeLine("/fake/softnet"):       ok(""),
				"stat -L -f %Sp %Su /fake/softnet":      ok("-rwsr-xr-x root\n")}},
		{name: "macos without tart", layer: "tart", isoln: "vm", problem: "tart is not installed: run `potluck tart install`, or download tart.tar.gz from https://github.com/openai/tart/releases/latest",
			facts: fakeFacts{os: "darwin", arch: "arm64", cores: 10, ramMB: 32 << 10, bins: map[string]bool{}},
			out:   map[string]Result{"sysctl -n kern.hv_support": ok("1\n")}},
		{name: "nothing installed (linux)", layer: "oci", problem: "no container CLI", step: "apt-get install -y docker", facts: linux()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Detect(ctx, &fakeRunner{out: c.out}, c.facts, "/state")
			l, found := r.Layer(c.layer)
			if !found {
				t.Fatalf("no layer %s", c.layer)
			}
			if l.Available != c.avail || l.Isolation != c.isoln || l.Runtime != c.runtime || l.CLI != c.cli {
				t.Errorf("layer %+v", l)
			}
			if c.problem != "" && !strings.Contains(strings.Join(l.Problems, "|"), c.problem) {
				t.Errorf("problems %q lack %q", l.Problems, c.problem)
			}
			if c.problem == "" && len(l.Problems) > 0 {
				t.Errorf("unexpected problems %q", l.Problems)
			}
			if c.step != "" && !strings.Contains(strings.Join(l.RootSteps, "|"), c.step) {
				t.Errorf("root steps %q lack %q", l.RootSteps, c.step)
			}
			if l.Supported != Supported[c.layer] {
				t.Errorf("supported = %v", l.Supported)
			}
		})
	}
}

func TestDetectFacts(t *testing.T) {
	f := linux()
	f.paths["/dev/kvm"] = true
	r := Detect(context.Background(), &fakeRunner{}, f, "/state")
	if r.OS != "linux" || r.Cores != 16 || r.RAMMB != 128<<10 || r.FreeDiskGB != 500 || !r.KVM {
		t.Errorf("%+v", r)
	}
	// a Mac has no /dev/kvm even if the path were there
	m := fakeFacts{os: "darwin", arch: "arm64", paths: map[string]bool{"/dev/kvm": true}}
	if Detect(context.Background(), &fakeRunner{}, m, "").KVM {
		t.Error("kvm on a Mac")
	}
}

func TestBestAndSupported(t *testing.T) {
	f := linux("incus", "docker", "runsc")
	f.paths["/dev/kvm"] = true
	r := Detect(context.Background(), &fakeRunner{out: map[string]Result{
		"incus version": ok("Server version: 6.0.0\n"), "docker --version": ok("Docker version 27\n"),
		"docker info --format {{json .Runtimes}}": ok(`{"runsc":{}}`)}}, f, "")
	if inc, _ := r.Layer("incus"); !inc.Available || inc.Supported {
		t.Errorf("incus: %+v", inc)
	}
	if b, found := r.Best(); !found || b.Name != "oci" {
		t.Errorf("best = %+v: incus is not bindable yet", b)
	}
}

func TestSizes(t *testing.T) {
	names := func(s []Size) string {
		var n []string
		for _, x := range s {
			n = append(n, x.Name)
		}
		return strings.Join(n, ",")
	}
	cases := []struct {
		name  string
		cores int
		ramMB int64
		lim   Limits
		want  string
	}{
		{"8 GB / 4 cores", 4, 8 << 10, Limits{Share: 0.5}, "c1m2"},
		{"128 GiB / 16 cores", 16, 128 << 10, Limits{Share: 0.5}, "c1m2,c2m4,c2m8,c4m16"},
		{"128 GiB / 32 threads", 32, 128 << 10, Limits{Share: 0.5}, "c1m2,c2m4,c2m8,c4m16,c8m32"},
		{"128 GiB / 16 cores, all of it", 16, 128 << 10, Limits{Share: 1}, "c1m2,c2m4,c2m8,c4m16,c8m32"},
		{"8 GB / 4 cores, all of it", 4, 8 << 10, Limits{Share: 1}, "c1m2,c2m4"},
		{"default share", 32, 128 << 10, Limits{}, "c1m2,c2m4,c2m8,c4m16,c8m32"},
		{"128 GiB / 32 threads, a quarter", 32, 128 << 10, Limits{Share: 0.25}, "c1m2,c2m4,c2m8,c4m16"},
		{"core cap", 16, 128 << 10, Limits{Share: 1, MaxCores: 4}, "c1m2,c2m4,c2m8"},
		{"ram cap", 16, 128 << 10, Limits{Share: 1, MaxRAMMB: 16 << 10}, "c1m2,c2m4,c2m8"},
		{"too small", 1, 2 << 10, Limits{Share: 1}, ""},
	}
	for _, c := range cases {
		got := names(SizesWithin(Report{Cores: c.cores, RAMMB: c.ramMB}, c.lim))
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if s := Sizes(Report{Cores: 4, RAMMB: 8 << 10}, 0.5); len(s) != 1 || s[0] != (Size{"c1m2", 1, 2048}) {
		t.Errorf("Sizes: %+v", s)
	}
}

func TestRootCommand(t *testing.T) {
	if RootCommand(nil) != "" {
		t.Error("no steps, a command")
	}
	got := RootCommand([]string{"apt-get install -y x", "systemctl restart docker", "apt-get install -y x"})
	if want := `sudo sh -c 'apt-get install -y x && systemctl restart docker'`; got != want {
		t.Errorf("%q", got)
	}
	got = RootCommand([]string{`echo 'hi' > /etc/x`})
	if want := `sudo sh -c 'echo '\''hi'\'' > /etc/x'`; got != want {
		t.Errorf("%q", got)
	}
}

func TestSteps(t *testing.T) {
	r := Report{Layers: []Layer{
		{Name: "incus", Supported: false, RootSteps: []string{"a"}},
		{Name: "oci", Supported: true, RootSteps: []string{"b", "c"}},
		{Name: "tart", Supported: true, Available: true},
	}}
	if got := r.RootSteps(""); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("%v", got)
	}
	if got := r.RootSteps("incus"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("%v", got)
	}
}

func TestKVMUsability(t *testing.T) {
	ctx := context.Background()
	f := linux("incus", "docker", "kata-runtime")
	f.paths["/dev/kvm"], f.noaccess = true, map[string]bool{"/dev/kvm": true}
	r := Detect(ctx, &fakeRunner{out: map[string]Result{"incus version": ok("Server version: 6.0.0\n"),
		"docker --version": ok("Docker version 29\n"), "docker info --format {{json .Runtimes}}": ok(`{"kata":{"runtimeType":"/opt/kata/shim"}}`)}}, f, "")
	if r.KVM || !strings.Contains(r.KVMProblem, "cannot open") {
		t.Fatalf("kvm: %+v", r)
	}
	for _, n := range []string{"incus", "oci"} {
		l, _ := r.Layer(n)
		if l.Available || !strings.Contains(strings.Join(l.RootSteps, "|"), `usermod -aG kvm "${SUDO_USER:-$USER}"`) {
			t.Errorf("%s: %+v", n, l)
		}
	}
	if got := RootCommand(r.RootSteps("incus")); !strings.Contains(got, "usermod -aG kvm") {
		t.Errorf("%q", got)
	}
	// with runsc beside Kata, an unusable KVM still leaves gVisor
	f2 := linux("docker", "kata-runtime", "runsc")
	f2.paths["/dev/kvm"], f2.noaccess = true, map[string]bool{"/dev/kvm": true}
	r2 := Detect(ctx, &fakeRunner{out: map[string]Result{"docker --version": ok("Docker version 29\n"), "docker info --format {{json .Runtimes}}": ok(`{"kata":{},"runsc":{"path":"/usr/local/bin/runsc"}}`)}}, f2, "")
	if l, _ := r2.Layer("oci"); !l.Available || l.Runtime != "runsc" {
		t.Errorf("oci: %+v", l)
	}
}

func TestDockerDaemonDown(t *testing.T) {
	f := linux("docker", "runsc")
	r := Detect(context.Background(), &fakeRunner{out: map[string]Result{"docker --version": ok("Docker version 27\n"),
		"docker info --format {{json .Runtimes}}": {Exit: 1, Stderr: "Cannot connect"}}}, f, "")
	l, _ := r.Layer("oci")
	if l.Available || len(l.RootSteps) != 0 || len(l.Problems) != 1 || !strings.Contains(l.Problems[0], "daemon is down") {
		t.Errorf("%+v", l)
	}
}

// Right after usermod -aG docker, before the next login, the daemon runs but
// docker info is refused at the socket: that is not "daemon is down".
func TestDockerSocketPermissionDenied(t *testing.T) {
	f := linux("docker", "runsc")
	r := Detect(context.Background(), &fakeRunner{out: map[string]Result{"docker --version": ok("Docker version 29.1.3\n"),
		"docker info --format {{json .Runtimes}}": {Exit: 1, Stderr: "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock"}}}, f, "")
	l, _ := r.Layer("oci")
	if l.Available || len(l.Problems) != 1 || strings.Contains(l.Problems[0], "daemon is down") ||
		!strings.Contains(l.Problems[0], "docker group") || !strings.Contains(l.Problems[0], "log in again") {
		t.Errorf("%+v", l)
	}
	if got := RootCommand(r.RootSteps("oci")); !strings.Contains(got, `usermod -aG docker "${SUDO_USER:-$USER}"`) || strings.Contains(got, "runsc") {
		t.Errorf("%q", got)
	}
}

func TestRuntimeChoiceDeterministic(t *testing.T) {
	f := linux("docker")
	f.paths["/dev/kvm"] = true
	out := map[string]Result{"docker --version": ok("Docker version 27\n"),
		"docker info --format {{json .Runtimes}}": ok(`{"kata-fc":{},"io.containerd.kata.v2":{},"runc":{},"kata-qemu":{},"runsc":{}}`)}
	for i := 0; i < 20; i++ {
		l, _ := Detect(context.Background(), &fakeRunner{out: out}, f, "").Layer("oci")
		if l.Runtime != "runsc" || l.Runtimes[0] != "runsc" || l.Runtimes[1] != "kata-qemu" || l.Runtimes[2] != "kata-fc" {
			t.Fatalf("run %d: %+v", i, l)
		}
	}
	delete(out, "x")
	out["docker info --format {{json .Runtimes}}"] = ok(`{"kata-fc":{},"kata-qemu":{},"runc":{}}`)
	if l, _ := Detect(context.Background(), &fakeRunner{out: out}, f, "").Layer("oci"); l.Runtime != "kata-qemu" {
		t.Errorf("%+v", l)
	}
}

// Doctor asks the driver's own engine check: when sudo -n cannot run its
// ownership read, oci is not ready and the root line installs the helper;
// any other refusal is a problem with no root step.
func TestDoctorRunsDriverEngineCheck(t *testing.T) {
	out := map[string]Result{"docker --version": ok("Docker version 29\n"),
		"docker info --format {{json .Runtimes}}": ok(`{"runsc":{"path":"/usr/local/bin/runsc"}}`)}
	needsRoot := fmt.Errorf("verify local Docker engine: %w: sudo: a password is required", vmhost.ErrOCIOwnerNeedsRoot)
	for _, c := range []struct {
		engine          error
		ready, rootStep bool
		problem         string
	}{
		{nil, true, false, ""},
		{needsRoot, false, true, "oci needs a root step"},
		{errors.New("Docker socket is owned by a forwarding process"), false, false, "fails the driver's check: Docker socket is owned by a forwarding process"},
	} {
		f := linux("docker", "runsc")
		f.engine = c.engine
		r := Detect(context.Background(), &fakeRunner{out: out}, f, "")
		l, _ := r.Layer("oci")
		step := vmhost.OCIOwnerRootSteps()[0]
		if l.Available != c.ready || slices.Contains(l.RootSteps, step) != c.rootStep || (c.problem != "" && !strings.Contains(strings.Join(l.Problems, "\n"), c.problem)) {
			t.Errorf("engine %v: %+v", c.engine, l)
		}
		if c.rootStep && !strings.Contains(RootCommand(r.RootSteps("")), vmhost.DockerOwnerHelper) {
			t.Errorf("the root line does not install the helper: %s", RootCommand(r.RootSteps("")))
		}
	}
}

// On Kata, doctor also reports what the driver's own check says of the
// native parent's root calls (vmhost.ErrOCIHostNeedsSudo): without
// passwordless sudo, oci is not ready, and the problem names its two fixes
// (passwordless sudo, which the owner gives, or the exact root line that
// lends with gVisor instead) once, however many calls sudo refused. The root
// line asks for nothing new: at most the ownership helper. Share, with no
// flags, says the same instead of pointing at doctor.
func TestDoctorKataNeedsPasswordlessSudo(t *testing.T) {
	out := map[string]Result{"docker --version": ok("Docker version 29\n"),
		"docker info --format {{json .Runtimes}}": ok(`{"kata-qemu":{},"runc":{}}`)}
	needsRoot := fmt.Errorf("verify local Docker engine: %w: sudo: a password is required", vmhost.ErrOCIOwnerNeedsRoot)
	busctl := fmt.Errorf("%w: sudo -n -k busctl: sudo: a password is required", vmhost.ErrOCIHostNeedsSudo)
	journal := fmt.Errorf("read active Docker configuration evidence: %w: sudo -n -k journalctl: sudo: a password is required", vmhost.ErrOCIHostNeedsSudo)
	for name, engine := range map[string]error{
		"helper missing too": errors.Join(needsRoot, busctl),
		"helper installed":   errors.Join(journal, busctl),
	} {
		t.Run(name, func(t *testing.T) {
			f := linux("docker", "curl", "bzip2")
			f.paths["/dev/kvm"] = true
			f.engine = engine
			r := Detect(context.Background(), &fakeRunner{out: out}, f, "")
			l, _ := r.Layer("oci")
			problems := strings.Join(l.Problems, "\n")
			if l.Runtime != "kata-qemu" || l.Available || strings.Count(problems, "passwordless sudo yourself") != 1 ||
				!strings.Contains(problems, "lend with gVisor instead") || !strings.Contains(problems, "runsc install") {
				t.Fatalf("%+v", l)
			}
			root := RootCommand(r.RootSteps("oci"))
			if strings.Contains(root, "runsc") || strings.Contains(root, "NOPASSWD: ALL") ||
				strings.Contains(root, vmhost.DockerOwnerHelper) != (name == "helper missing too") {
				t.Errorf("root line: %q", root)
			}
			_, err := shareOn(context.Background(), &fakeRunner{}, binderFunc(nil), ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h"})
			var nr *NotReadyError
			if !errors.As(err, &nr) || nr.Layer != "oci" || !strings.Contains(err.Error(), "passwordless sudo yourself") {
				t.Errorf("share: %v", err)
			}
		})
	}
}

// Detect carries the swap state beside its note, for the advert and doctor.
func TestDetectReportsSwap(t *testing.T) {
	r := Detect(context.Background(), &fakeRunner{out: map[string]Result{
		"swapon --show=NAME,TYPE --noheadings --raw": ok("/dev/sda2 partition\n"), "lsblk -nso TYPE /dev/sda2": ok("part\ndisk\n")}}, fakeFacts{os: "linux"}, "")
	if r.Swap != "plain-disk" || !strings.Contains(r.SwapNote, "swap on /dev/sda2") {
		t.Errorf("swap %q, note %q", r.Swap, r.SwapNote)
	}
}
