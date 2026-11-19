package hostprobe

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/wiring"
)

// macReport is a Mac with Tart ready.
func macReport() Report {
	return Report{OS: "darwin", MacOS: "26.0", Arch: "arm64", Cores: 16, RAMMB: 48 << 10, FreeDiskGB: 500, Layers: []Layer{
		{Name: "tart", Available: true, Supported: true, Isolation: "vm"},
	}}
}

// Share's default is quick (zm-eakh.12.23.4): the security groups once, on
// the first size, with the network targets; a boot check on each other size.
// --full is the whole suite, network included, on every size.
func TestShareQuickByDefault(t *testing.T) {
	o := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c1m2", "c2m4", "c2m8"},
		HostAddrs: []string{"192.0.2.9"}, DryRun: true}
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Join(res.Sizes[0].Conformance, " ")
	if !strings.Contains(first, " --only "+QuickGroups+" ") || !strings.Contains(first, "--net-peers") || !strings.Contains(first, "--net-forbid 192.0.2.9:") {
		t.Errorf("first size: %s", first)
	}
	for _, s := range res.Sizes[1:] {
		line := strings.Join(s.Conformance, " ")
		if !strings.HasSuffix(line, " --only "+SizeGroups) || strings.Contains(line, "--net-") {
			t.Errorf("%s: %s", s.Binding, line)
		}
	}
	if res.Full {
		t.Error("full by default")
	}
	o.Full = true
	res, err = shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil || !res.Full {
		t.Fatal(err)
	}
	for _, s := range res.Sizes {
		if line := strings.Join(s.Conformance, " "); strings.Contains(line, "--only") || !strings.Contains(line, "--net-peers") {
			t.Errorf("--full %s: %s", s.Binding, line)
		}
	}
}

// Every conformance group is either in the quick run or named here as
// dropped, with why: a new group must be placed before share skips it.
func TestQuickGroupsCoverTheSuite(t *testing.T) {
	dropped := map[string]string{
		"renew":            "waits out a short lease; the TTL is the driver's code, the same on every size",
		"expiry":           "waits out a short lease, as renew",
		"reclaim":          "share passes no --reclaim-config, so a SKIP",
		"code":             "share passes no --ref, so a SKIP",
		"ended":            "run targets only",
		"procs":            "waits out a 20 s background process; spawn, output and signal are the driver's code, the same on every size",
		"pty":              "terminals ride the background-process verbs (procs): the driver's code, the same on every size",
		"persist":          "waits out a short lease and boots a box twice; kept disks are the driver's code, the same on every size",
		"volumes":          "boots about a dozen boxes and leaves its volumes on the host until the driver's keep; volumes are the driver's code, the same on every size (potluck's conformance holds it to them)",
		"warm":             "share binds no warm boxes (an owner's setting), so a SKIP",
		"box-secrets-ops":  "waits out a 5 s reader; unset, an exec's own secrets, atomic replace, refusals and conflict are the driver's code, the same on every size (box-secrets, the isolation checks, is quick)",
		"acquire-progress": "share's driver lists no acquire-progress capability, so a SKIP; progress and cancel are the driver's code, the same on every size",
		"box-authority":    "share passes no --box-credential (it needs a live authority's credential), so a SKIP",
		"agent-runner":     "waits out background jobs with secrets and a TERM drain; spawn, secrets and signal are the driver's code, the same on every size (procs and box-secrets-ops, likewise dropped)",
	}
	quick := strings.Split(QuickGroups, ",")
	for _, g := range append(slices.Clone(quick), SizeGroups) {
		if !slices.Contains(conformance.Groups, g) {
			t.Errorf("share asks for group %q, which conformance does not have", g)
		}
	}
	for _, g := range conformance.Groups {
		if _, ok := dropped[g]; !ok && !slices.Contains(quick, g) && g != SizeGroups {
			t.Errorf("conformance group %q is neither in QuickGroups nor dropped on purpose", g)
		}
	}
	for _, must := range []string{"lifecycle", "refuse", "net", "handles", "box-secrets", "ports"} { // wipe and secrets, refusals, network, foreign handles, box secrets' isolation
		if !slices.Contains(quick, must) {
			t.Errorf("QuickGroups lacks the security group %q", must)
		}
	}
}

// The security checks hold for every size or none: when they fail on the
// first size, no other size runs or binds.
func TestShareStopsWhenTheSecurityChecksFail(t *testing.T) {
	run := &fakeRunner{on: func(l string) (Result, bool) {
		if strings.Contains(l, "--only "+QuickGroups) {
			return Result{Exit: 1, Stdout: "FAIL  a new box does not see the last box's files"}, true
		}
		return Result{}, true
	}}
	binds := 0
	b := binderFunc(func(context.Context, computeapi.BindRunTargetRequest) (computeapi.RunTargetView, error) {
		binds++
		return computeapi.RunTargetView{}, nil
	})
	res, err := shareOn(context.Background(), run, b, ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c1m2", "c2m4"}})
	if err != nil {
		t.Fatal(err)
	}
	if binds != 0 || !res.Failed() || !strings.Contains(res.Sizes[1].Error, "security checks failed on h-c1m2") {
		t.Errorf("binds %d, sizes %+v", binds, res.Sizes)
	}
	for _, c := range run.calls {
		if strings.Contains(c, "--only "+SizeGroups) {
			t.Errorf("ran %s after the security checks failed", c)
		}
	}
}

// On a Mac share binds the reclaim policy (reclaim_idle_s): the owner's
// keyboard or pointer ends a box as reclaimed. Conformance runs without it
// (the owner is typing), and a non-Mac refuses it.
func TestShareReclaimsOnOwnerInput(t *testing.T) {
	withTartImages(t, pinA)
	o := ShareOptions{Report: macReport(), Images: []string{pinA}, NamePrefix: "m", Sizes: []string{"c2m4"}, DryRun: true, ReclaimIdleS: DefaultReclaimIdleS}
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Sizes[0]
	if s.Config["reclaim_idle_s"] != "300" || res.ReclaimIdleS != 300 || strings.Contains(strings.Join(s.Conformance, " "), "reclaim") {
		t.Errorf("config %v, conformance %v", s.Config, s.Conformance)
	}
	o.ReclaimIdleS = 0
	if res, err = shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.Sizes[0].Config["reclaim_idle_s"] != "" {
		t.Errorf("--reclaim-idle-s 0: %v %v", err, res.Sizes)
	}
	o.Report, o.ReclaimIdleS = bigReport(), 300
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err == nil || !strings.Contains(err.Error(), "--reclaim-idle-s 0") {
		t.Errorf("linux with reclaim on input: %v", err)
	}
}

// inProcessConformance answers share's zm-run-conformance lines by running
// the suite in this process against driver (the reference box). The network
// flags are dropped: the reference box is this machine, so it reaches
// everything a VM must not. Its boxes see this machine's /tmp too, so tmp
// (config host_tmp, which the reference driver gives each box a copy of)
// stands in for the shared roots the leftover checks mark and search.
func inProcessConformance(driver, tmp string, got *[]string) *fakeRunner {
	return &fakeRunner{on: func(line string) (Result, bool) {
		args := strings.Fields(line)
		if args[0] != "zm-run-conformance" {
			return Result{}, true
		}
		opt := conformance.Options{Config: map[string]string{"host_tmp": tmp}, LeftoverRoots: []string{tmp}}
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--kind":
				i++
				opt.Kind = args[i]
			case "--config":
				i++
				k, v, _ := strings.Cut(args[i], "=")
				opt.Config[k] = v
			case "--only":
				i++
				opt.Only = strings.Split(args[i], ",")
			case "--net-forbid", "--net-allow", "--net-mail":
				i++
			}
		}
		rs := conformance.Run(context.Background(), driver, opt)
		*got = append(*got, conformance.Format(rs))
		r := Result{Stdout: conformance.Format(rs)}
		if conformance.Failed(rs) > 0 {
			r.Exit = 1
		}
		return r, true
	}}
}

// The acceptance of zm-eakh.12.23.4 on the reference driver: share of three
// sizes, quick, against a real daemon and the real suite, finishes well
// inside two minutes, runs the security checks, and binds every size with
// the Mac's reclaim policy.
// refDaemon is an in-process daemon whose run targets run refdriver, with
// project acme: its owner's client and the driver's path.
func refDaemon(t *testing.T) (*client.Client, string) {
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
	return owner, drv
}

func TestShareQuickOnTheReferenceDriver(t *testing.T) {
	withTartImages(t, pinA)
	ctx := context.Background()
	owner, drv := refDaemon(t)
	var outs []string
	start := time.Now()
	res, err := shareOn(ctx, inProcessConformance(drv, t.TempDir(), &outs), computeclient.Owner{C: owner}, ShareOptions{Report: macReport(), Images: []string{pinA},
		Sizes: []string{"c1m2", "c2m4", "c2m8"}, NamePrefix: "mini", Driver: "ref", ReclaimIdleS: DefaultReclaimIdleS})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quick share of 3 sizes on the reference driver: %s", took.Round(10*time.Millisecond))
	if res.Failed() {
		t.Fatalf("%+v\n%s", res.Sizes, strings.Join(outs, "\n"))
	}
	if took > 2*time.Minute {
		t.Errorf("quick share took %s", took)
	}
	for _, want := range []string{"PASS  a new box does not see the last box's files", "PASS  exec secrets: a 0600 file",
		"PASS  exec refuses a shell string", "PASS  another binding's handle answers bad_request"} {
		if !strings.Contains(outs[0], want) {
			t.Errorf("the security run lacks %q", want)
		}
	}
	if len(outs) != 3 || strings.Contains(outs[1], "a new box") || !strings.Contains(outs[1], "PASS  exec runs the command") {
		t.Errorf("the boot runs:\n%s", strings.Join(outs[1:], "\n"))
	}
	l, err := computeclient.RunTargets(ctx, owner, false)
	if err != nil || len(l.Targets) != 3 {
		t.Fatalf("%v %+v", err, l.Targets)
	}
	for _, tg := range l.Targets {
		if tg.Config["reclaim_idle_s"] != "300" {
			t.Errorf("%s bound without the reclaim policy: %v", tg.Name, tg.Config)
		}
	}
}

// share --pool binds machine-level (run_targets.toml), which must read back:
// the pins go in the binding's images, never also as a config key "images",
// which the file cannot hold twice (zm-eakh.12.23.3: a pool host lent nothing).
func TestSharePoolBindingReadsBack(t *testing.T) {
	ctx := context.Background()
	owner, _ := refDaemon(t)
	run := &fakeRunner{on: func(string) (Result, bool) { return Result{Stdout: "PASS"}, true }}
	// The pool is named by its project id: this daemon joined no project,
	// and a name is refused unless it names a project the machine is in.
	res, err := shareOn(ctx, run, computeclient.Owner{C: owner}, ShareOptions{Report: bigReport(), Images: []string{pinA}, Sizes: []string{"c1m2"},
		NamePrefix: "mini", Driver: "ref", Pool: ids.New()})
	if err != nil || res.Failed() {
		t.Fatalf("%v %+v", err, res.Sizes)
	}
	l, err := computeclient.HostRunTargets(ctx, owner)
	if err != nil || len(l.Targets) != 1 {
		t.Fatalf("host bindings: %+v %v", l.Targets, err)
	}
	if tg := l.Targets[0]; len(tg.Images) != 1 || tg.Images[0] != pinA || tg.Config["images"] != "" {
		t.Errorf("binding: %+v", tg)
	}
}
