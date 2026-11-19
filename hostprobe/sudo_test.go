package hostprobe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
)

// Root and Docker in a box (zm-eakh.12.5.26): on gVisor, sudo needs runsc
// with --allow-suid. zm registers that as its own docker runtime entry,
// runsc-root, beside "runsc" (which default boxes keep), and only when the
// owner shares with --docker.
func detectDocker(t *testing.T, f fakeFacts, runtimes string) Layer {
	t.Helper()
	l, _ := Detect(context.Background(), &fakeRunner{out: map[string]Result{
		"docker --version":                        ok("Docker version 29.9.2\n"),
		"docker info --format {{json .Runtimes}}": ok(runtimes),
	}}, f, "").Layer("oci")
	return l
}

func TestSudoRuntimeDetected(t *testing.T) {
	l := detectDocker(t, linux("docker", "runsc"), `{"runc":{"path":"runc"},"runsc":{"path":"/usr/local/bin/runsc"},"runsc-root":{"path":"/usr/local/bin/runsc","runtimeArgs":["--allow-suid"]}}`)
	if !l.Available || l.Runtime != "runsc" || l.Sudo != SudoRuntime || len(l.SudoSteps) != 0 || l.SudoProblem != "" {
		t.Fatalf("runsc and runsc-root registered: %+v", l)
	}
	for _, r := range l.Runtimes {
		if r == SudoRuntime {
			t.Errorf("runsc-root is never a default box's runtime: %v", l.Runtimes)
		}
	}
	f := linux("docker", "kata-runtime")
	f.paths["/dev/kvm"] = true
	if l := detectDocker(t, f, `{"kata":{}}`); l.Sudo != "kata" || len(l.SudoSteps) != 0 {
		t.Errorf("Kata (a guest kernel honours setuid): %+v", l)
	}
}

func TestSudoRuntimeRootStep(t *testing.T) {
	l := detectDocker(t, linux("docker", "runsc"), `{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"}}`)
	if !l.Available || l.Sudo != "" || !strings.Contains(l.SudoProblem, "--allow-suid") || len(l.SudoSteps) == 0 {
		t.Fatalf("runsc without runsc-root: the layer stays available, and says how to add it: %+v", l)
	}
	script := RootScript(l.SudoSteps)
	at := -1
	for _, s := range []string{
		`cp -p /etc/docker/daemon.json "/etc/docker/daemon.json.zm-bak-$(date +%Y%m%d%H%M%S)"`, // zm's own backup first
		`p=$(docker info --format "{{(index .Runtimes \"runsc\").Path}}")`,                     // the runsc docker already runs as root
		`"$p" install --runtime=` + SudoRuntime + ` --clobber --download-sidecars=NEVER -- --allow-suid`,
		`systemctl reload docker || kill -HUP "$(cat /var/run/docker.pid)"`,
		`grep -q ` + SudoRuntime,
	} {
		i := strings.Index(script, s)
		if i <= at {
			t.Fatalf("%q missing or out of order in\n%s", s, script)
		}
		at = i
	}
	for _, bad := range []string{"net-raw", "packet-socket", "allow-flag-override", "--runtime=runsc ", "restart", "curl", "wget", "clobber=false"} {
		if strings.Contains(script, bad) {
			t.Errorf("script has %q:\n%s", bad, script)
		}
	}
	// Doctor's root line for a lender who does not share --docker is as before.
	r := Report{Layers: []Layer{l}}
	if strings.Contains(RootScript(r.RootSteps("oci")), SudoRuntime) {
		t.Errorf("doctor's root line registers runsc-root without --docker: %v", r.RootSteps("oci"))
	}

	// A machine zm installs gVisor on: the same step names the pinned runsc,
	// and its reload is its own (RootScript keeps each step once).
	l = detectDocker(t, linux("docker", "curl", "bzip2"), `{"runc":{"path":"runc"}}`)
	all := RootScript(append(append([]string{}, l.RootSteps...), l.SudoSteps...))
	i, j := strings.Index(all, gvisorBin+"/runsc install --runtime="+SudoRuntime), strings.LastIndex(all, "systemctl reload docker")
	if i < 0 || j < i || strings.Index(all, gvisorBin+"/runsc install --download-sidecars=NEVER") > i {
		t.Fatalf("fresh install: runsc-root after runsc, then a reload:\n%s", all)
	}

	// podman and nerdctl: no docker runtime entry to add.
	l = detectDocker(t, linux("podman", "runsc"), "")
	if l.Sudo != "" || len(l.SudoSteps) != 0 {
		t.Errorf("podman: %+v", l)
	}
}

func TestShareDocker(t *testing.T) {
	r := bigReport()
	r.Layers[0].Sudo = SudoRuntime
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h",
		Sizes: []string{"c1m2"}, DryRun: true, Docker: true})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Sizes[0].Config
	if c["oci_runtime"] != SudoRuntime || c["docker"] != "on" {
		t.Errorf("--docker binds runsc-root with docker on: %v", c)
	}
	if !strings.Contains(strings.Join(res.Sizes[0].Conformance, " "), "--only "+QuickGroups) || !strings.Contains(QuickGroups, "root") {
		t.Errorf("conformance runs the root group: %v (%s)", res.Sizes[0].Conformance, QuickGroups)
	}
	res, err = shareOn(context.Background(), &fakeRunner{}, nil, ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h",
		Sizes: []string{"c1m2"}, DryRun: true})
	if c := res.Sizes[0].Config; err != nil || c["oci_runtime"] != "runsc" || c["docker"] != "" || c["root"] != "" {
		t.Errorf("without --docker: %v %v", err, c)
	}

	// No runsc-root yet: share is not ready, and its root steps add it.
	r = bigReport()
	r.Layers[0].SudoProblem, r.Layers[0].SudoSteps = "no runsc-root", []string{"register runsc-root"}
	o := ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c1m2"}, Docker: true}
	_, err = shareOn(context.Background(), &fakeRunner{}, binderFunc(nil), o)
	var nr *NotReadyError
	if !errors.As(err, &nr) || !strings.Contains(nr.RootCommand, "register runsc-root") {
		t.Fatalf("--docker without runsc-root: %v", err)
	}
	if got := o.RootSteps(); len(got) != 1 || got[0] != "register runsc-root" {
		t.Errorf("--docker root steps: %v", got)
	}
	if err := precheckOn(context.Background(), nil, o); err != nil {
		t.Errorf("Precheck: the root step fixes it: %v", err)
	}
	o.Docker = false
	if got := o.RootSteps(); len(got) != 0 {
		t.Errorf("root steps without --docker: %v", got)
	}

	r = bigReport()
	r.Layers[0].Sudo = SudoRuntime
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h",
		Sizes: []string{"c1m2"}, DryRun: true, Docker: true, OCIRuntime: "runsc"}); err == nil || !strings.Contains(err.Error(), "setuid") {
		t.Errorf("--docker with --oci-runtime runsc: %v", err)
	}
}

// runsc-root is trusted by what docker says it is: the runsc of docker's
// runsc entry with exactly --allow-suid. Anything else is a problem whose
// root step replaces it (install --clobber).
func TestSudoRuntimeMustBeZms(t *testing.T) {
	for _, c := range []struct{ name, entry, want string }{
		{"runc", `{"path":"/usr/bin/runc","runtimeArgs":["--allow-suid"]}`, "/usr/bin/runc"},
		{"another runsc", `{"path":"/opt/old/runsc","runtimeArgs":["--allow-suid"]}`, "/opt/old/runsc"},
		{"net-raw", `{"path":"/usr/bin/runsc","runtimeArgs":["--allow-suid","--net-raw"]}`, "--net-raw"},
		{"host network", `{"path":"/usr/bin/runsc","runtimeArgs":["--network=host","--allow-suid"]}`, "--network=host"},
		{"no args", `{"path":"/usr/bin/runsc"}`, "--allow-suid"},
	} {
		l := detectDocker(t, linux("docker", "runsc"), `{"runc":{"path":"runc"},"runsc":{"path":"/usr/bin/runsc"},"runsc-root":`+c.entry+`}`)
		if l.Sudo != "" || !strings.Contains(l.SudoProblem, c.want) || !strings.Contains(RootScript(l.SudoSteps), "--clobber") {
			t.Errorf("%s: sudo runtime %q, problem %q, steps %v", c.name, l.Sudo, l.SudoProblem, l.SudoSteps)
		}
		if !l.Available || l.Runtime != "runsc" {
			t.Errorf("%s: the layer stays as it was: %+v", c.name, l)
		}
	}
}

// --docker is kept across re-runs, as admission keys are: a re-run without
// the flag lends root and docker again when an earlier share did, and
// --docker=false turns it off.
func TestShareDockerIsKept(t *testing.T) {
	r := bigReport()
	r.Layers[0].Sudo = SudoRuntime
	earlier := []computeapi.RunTarget{{Name: "h-c1m2", Kind: "box", Config: map[string]string{"vm": "oci", "oci_runtime": SudoRuntime, "docker": "on"}}}
	o := ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c1m2"}, DryRun: true, Earlier: earlier}
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if c := res.Sizes[0].Config; err != nil || c["docker"] != "on" || c["oci_runtime"] != SudoRuntime || !res.Docker || res.DockerFrom != "h-c1m2" {
		t.Fatalf("a re-run without --docker: %v %v (docker %v from %q)", err, c, res.Docker, res.DockerFrom)
	}
	o.DockerSet = true // --docker=false
	res, err = shareOn(context.Background(), &fakeRunner{}, nil, o)
	if c := res.Sizes[0].Config; err != nil || c["docker"] != "" || c["oci_runtime"] != "runsc" || res.Docker {
		t.Errorf("--docker=false: %v %v", err, c)
	}
	o.DockerSet, o.Earlier = false, []computeapi.RunTarget{{Name: "other-c1m2", Kind: "box", Config: map[string]string{"docker": "on"}}}
	if res, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.Sizes[0].Config["docker"] != "" {
		t.Errorf("another prefix's binding is not this share's: %v %v", err, res.Sizes[0].Config)
	}
	// The root steps follow the kept flag too.
	r.Layers[0].Sudo, r.Layers[0].SudoSteps = "", []string{"register runsc-root"}
	o = ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c1m2"}, Earlier: earlier}
	if got := o.RootSteps(); len(got) != 1 {
		t.Errorf("root steps of a re-run that keeps --docker: %v", got)
	}
}

// runsc install writes runsc-root's path as its resolved executable, while
// docker's runsc entry may be a symlink or a bare name: both resolve first.
func TestSudoRuntimeSymlinked(t *testing.T) {
	bin := t.TempDir()
	real := filepath.Join(bin, "runsc-20261102")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, "runsc")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, runsc := range []string{link, "runsc"} {
		l := detectDocker(t, linux("docker", "runsc"), `{"runsc":{"path":"`+runsc+`"},"runsc-root":{"path":"`+real+`","runtimeArgs":["--allow-suid"]}}`)
		if l.Sudo != SudoRuntime {
			t.Errorf("runsc entry %s, runsc-root its target: %q %s", runsc, l.Sudo, l.SudoProblem)
		}
	}
}

// An inherited --docker that stops share says where it came from.
func TestShareKeptDockerSaysSoWhenNotReady(t *testing.T) {
	r := bigReport()
	r.Layers[0].SudoProblem = "no runtime"
	earlier := []computeapi.RunTarget{{Name: "h-c1m2", Kind: "box", Config: map[string]string{"docker": "on"}}}
	_, err := shareOn(context.Background(), &fakeRunner{}, nil, ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h",
		Sizes: []string{"c1m2"}, DryRun: true, Earlier: earlier})
	if err == nil || !strings.Contains(err.Error(), "kept from h-c1m2 (--docker=false turns it off)") {
		t.Errorf("no sudo runtime, --docker kept: %v", err)
	}
	r.Layers[0].SudoSteps = []string{"register runsc-root"}
	_, err = shareOn(context.Background(), &fakeRunner{}, binderFunc(nil), ShareOptions{Report: r, Images: []string{pinA}, NamePrefix: "h",
		Sizes: []string{"c1m2"}, Earlier: earlier})
	if err == nil || !strings.Contains(err.Error(), "kept from h-c1m2 (--docker=false turns it off)") {
		t.Errorf("not ready, --docker kept: %v", err)
	}
}

// --root on tart (zm-eakh.12.54.14): each size binds root=on, conformance
// runs the root group with the network targets, the result says so in plain
// words, a re-run keeps it, --root=false turns it off, and oci refuses it.
func TestShareTartRoot(t *testing.T) {
	withTartImages(t, pinB)
	o := ShareOptions{Report: macReport(), Images: []string{pinB}, NamePrefix: "h", Sizes: []string{"c2m4"}, HostAddrs: []string{"192.0.2.9"}, DryRun: true, Root: true, RootSet: true}
	res, err := shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Sizes[0]
	conf := strings.Join(s.Conformance, " ")
	if s.Config["root"] != "on" || !res.Root || !strings.Contains(conf, "root=on") || !strings.Contains(conf, "--only "+QuickGroups) || !strings.Contains(conf, "--net-forbid 192.0.2.9:") {
		t.Errorf("--root: config %v, root %v, conformance %s", s.Config, res.Root, conf)
	}
	if n := strings.Join(res.Notes, "\n"); !strings.Contains(n, "passwordless sudo") || !strings.Contains(n, "throwaway VM") || !strings.Contains(n, "--root=false") {
		t.Errorf("notes say nothing plain of root: %q", n)
	}
	o.Root, o.RootSet = false, false
	if res, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.Sizes[0].Config["root"] != "" || res.Root {
		t.Errorf("without --root: %v %v", err, res.Sizes[0].Config)
	}
	o.Earlier = []computeapi.RunTarget{{Name: "h-c2m4", Kind: "box", Config: map[string]string{"vm": "tart", "root": "on"}}}
	res, err = shareOn(context.Background(), &fakeRunner{}, nil, o)
	if err != nil || res.Sizes[0].Config["root"] != "on" || res.RootFrom != "h-c2m4" || !strings.Contains(strings.Join(res.Notes, " "), "kept from h-c2m4") {
		t.Errorf("a re-run without --root: %v %v from %q %q", err, res.Sizes[0].Config, res.RootFrom, res.Notes)
	}
	o.RootSet = true // --root=false
	if res, err := shareOn(context.Background(), &fakeRunner{}, nil, o); err != nil || res.Sizes[0].Config["root"] != "" {
		t.Errorf("--root=false: %v %v", err, res.Sizes[0].Config)
	}
	if _, err := shareOn(context.Background(), &fakeRunner{}, nil, ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h",
		Sizes: []string{"c1m2"}, DryRun: true, Root: true}); err == nil || !strings.Contains(err.Error(), "--root is for the tart layer") {
		t.Errorf("--root on oci: %v", err)
	}
	// An earlier tart share's root is not an oci share's.
	o2 := ShareOptions{Report: bigReport(), Images: []string{pinA}, NamePrefix: "h", Sizes: []string{"c1m2"}, DryRun: true,
		Earlier: []computeapi.RunTarget{{Name: "h-c1m2", Kind: "box", Config: map[string]string{"vm": "tart", "root": "on"}}}}
	if res, err := shareOn(context.Background(), &fakeRunner{}, nil, o2); err != nil || res.Root || res.Sizes[0].Config["root"] != "" {
		t.Errorf("oci re-run after a tart root share: %v root %v %v", err, res.Root, res.Sizes[0].Config)
	}
}
