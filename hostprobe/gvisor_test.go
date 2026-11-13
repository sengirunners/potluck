package hostprobe

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// TestGVisorPins: the release is a fixed date (never "latest"), and each
// arch gVisor releases has a full SHA-512 and a URL on gVisor's release
// bucket under that date.
func TestGVisorPins(t *testing.T) {
	if !regexp.MustCompile(`^20\d{6}\.\d+$`).MatchString(gvisorRelease) {
		t.Fatalf("gvisorRelease %q is not a point release yyyymmdd.N (a bare date moves)", gvisorRelease)
	}
	hex := regexp.MustCompile(`^[0-9a-f]{128}$`)
	for arch, want := range map[string]string{"amd64": "x86_64", "arm64": "aarch64"} {
		p, ok := gvisorTarballs[arch]
		if !ok || p.arch != want || !hex.MatchString(p.sha512) {
			t.Errorf("%s: %+v", arch, p)
		}
		if u := GVisorURL(arch); u != "https://storage.googleapis.com/gvisor/releases/release/"+gvisorRelease+"/"+want+"/gvisor.tar.bz2" {
			t.Errorf("%s: %s", arch, u)
		}
	}
	if GVisorURL("riscv64") != "" {
		t.Error("a URL for an arch gVisor does not release")
	}
}

func noRuntime(f fakeFacts) Report {
	return Detect(context.Background(), &fakeRunner{out: map[string]Result{
		"docker --version":                        ok("Docker version 27.1.1\n"),
		"docker info --format {{json .Runtimes}}": ok(`{"runc":{"path":"runc"}}`),
		"podman --version":                        ok("podman version 5.2.0\n"),
	}}, f, "/state")
}

// TestGVisorRootScript: docker with only runc gets one root script that
// downloads the pinned tarball for this arch, checks the pinned SHA-512,
// unpacks it, then registers runsc with docker and reloads it, in that
// order, in a fresh temp dir removed on every path; never curl|sh, never a
// package named kata-containers.
func TestGVisorRootScript(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		f := linux("docker", "curl", "bzip2")
		f.arch = arch
		l, _ := noRuntime(f).Layer("oci")
		script := RootScript(l.RootSteps)
		url, sum := GVisorURL(arch), gvisorTarballs[arch].sha512
		order := []string{
			`t=$(mktemp -d)`,
			`curl -fsSL --proto =https -o "$t/gvisor.tar.bz2" ` + url,
			`echo "` + sum + `  $t/gvisor.tar.bz2" | sha512sum -c -`,
			`tar -xjf "$t/gvisor.tar.bz2" -C /usr/local/bin gvisor-bin containerd-shim-runsc-v1`,
			`tar -xjf "$t/gvisor.tar.bz2" -C /usr/local/bin runsc`,
			`s=$?; rm -rf "$t"; [ "$s" -eq 0 ]; }`,
			`/usr/local/bin/runsc install --download-sidecars=NEVER`,
			`systemctl reload docker`,
			`docker info --format "{{json .Runtimes}}" | grep -q runsc`,
		}
		at := -1
		for _, s := range order {
			i := strings.Index(script, s)
			if i <= at {
				t.Fatalf("%s: %q missing or out of order in\n%s", arch, s, script)
			}
			at = i
		}
		for _, bad := range []string{"latest", "kata-containers", "restart docker", "apt-get"} {
			if strings.Contains(script, bad) {
				t.Errorf("%s: script has %q:\n%s", arch, bad, script)
			}
		}
		if regexp.MustCompile(`\|\s*(sudo\s+)?(sh|bash)\b`).MatchString(script) {
			t.Errorf("%s: something piped into a shell:\n%s", arch, script)
		}
		// The download, check and unpack are one && chain inside the step
		// that cleans up: a failed check unpacks nothing.
		if !strings.Contains(script, `sha512sum -c - && tar -xjf`) {
			t.Errorf("%s: unpack does not depend on the check:\n%s", arch, script)
		}
		if p := strings.Join(l.Problems, "|"); !strings.Contains(p, "gVisor (runsc) release "+gvisorRelease) {
			t.Errorf("%s: problems %q", arch, p)
		}
	}
}

// TestGVisorPrerequisites: wget when there is no curl; the distribution's
// curl and bzip2 first when missing; unsupported adapters get no setup
// step; an arch gVisor does not release, or no
// package manager for a missing tool, gets a problem and no step.
func TestGVisorPrerequisites(t *testing.T) {
	steps := func(f fakeFacts) (string, string) {
		l, _ := noRuntime(f).Layer("oci")
		return RootScript(l.RootSteps), strings.Join(l.Problems, "|")
	}
	if s, _ := steps(linux("docker", "wget", "bzip2")); !strings.Contains(s, `wget -q -O "$t/gvisor.tar.bz2" `+GVisorURL("amd64")) || strings.Contains(s, "curl") {
		t.Errorf("wget: %s", s)
	}
	s, _ := steps(linux("docker"))
	if !strings.HasPrefix(s, "apt-get install -y curl && apt-get install -y bzip2 && t=$(mktemp -d)") {
		t.Errorf("prerequisites: %s", s)
	}
	s, _ = steps(linux("podman", "curl", "bzip2"))
	if s != "" {
		t.Errorf("podman: %s", s)
	}
	f := linux("docker", "curl", "bzip2")
	f.arch = "riscv64"
	if s, p := steps(f); s != "" || !strings.Contains(p, "only amd64 and arm64") {
		t.Errorf("riscv64: %q %q", s, p)
	}
	f = linux("docker", "curl")
	delete(f.bins, "apt-get")
	if s, p := steps(f); s != "" || !strings.Contains(p, "no known package manager") {
		t.Errorf("no package manager: %q %q", s, p)
	}
}

// TestRunscOnPathUnregistered: a runsc on PATH that docker does not list is
// never registered as it is (it may be the user's own, writable without
// root, or lack the sidecars its install would download unpinned): the
// pinned release is installed and docker gets that one, with a reload, not
// a restart (running containers keep running).
func TestRunscOnPathUnregistered(t *testing.T) {
	l, _ := noRuntime(linux("docker", "runsc", "curl", "bzip2")).Layer("oci")
	got := RootScript(l.RootSteps)
	if !strings.Contains(got, "sha512sum -c - && tar") || !strings.Contains(got, "} && /usr/local/bin/runsc install --download-sidecars=NEVER && systemctl reload docker && ") {
		t.Errorf("%s", got)
	}
	if strings.Contains(got, "/fake/runsc") || !strings.Contains(strings.Join(l.Problems, "|"), "not registered") {
		t.Errorf("%s %q", got, l.Problems)
	}
	// docker with an unregistered Kata and no runsc: as before, no step.
	f := linux("docker", "kata-runtime")
	if l, _ := noRuntime(f).Layer("oci"); len(l.RootSteps) != 0 {
		t.Errorf("kata: %q", l.RootSteps)
	}
}
