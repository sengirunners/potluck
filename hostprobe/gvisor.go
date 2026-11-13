package hostprobe

import (
	"slices"
	"strings"
)

// The gVisor release doctor's root line installs when a Linux machine has a
// container CLI but no isolating runtime (SPEC 20, "Host onboarding").
// Debian and Ubuntu carry no kata-containers package, and Ubuntu's runsc
// (universe) is a 2023 or 2024 snapshot, so zm installs gVisor's own release
// instead: one fixed release, its tarball per arch, checked against the
// SHA-512 pinned here before anything is installed. Never "latest".
//
// Source: https://gvisor.dev/docs/user_guide/install/ ("Point release":
// https://storage.googleapis.com/gvisor/releases/release/<yyyymmdd>.<rc>/
// <uname -m>/gvisor.tar.zstd, plus .sha512 in sha512sum's format, then
// `runsc install` and `systemctl reload docker`). zm pins a point release,
// <yyyymmdd>.0: the bare <yyyymmdd> path is that date's latest point
// release, so its bytes could change under the pin. Releases since late
// 2026 publish only the tarball (runsc, containerd-shim-runsc-v1 and
// gvisor-bin/, the sidecars runsc needs, the Sentry among them); with
// gvisor-bin/ next to runsc, `runsc install` downloads nothing. zm takes the
// .tar.bz2 of the same release (GNU tar -j and busybox tar read it; zstd is
// often absent). Each sum is the release's gvisor.tar.bz2.sha512, and was
// checked against the downloaded tarball when pinned.
//
// To bump: pick a release at least two weeks old, read both .sha512 files,
// download both tarballs and check them, update the three values below, and
// check that its `runsc install` still takes --download-sidecars (gVisor
// says the flag will go away; without it the root script fails, safely).
const gvisorRelease = "20261019.0"

var gvisorTarballs = map[string]struct{ arch, sha512 string }{
	"amd64": {"x86_64", "7cbc2aeeee809dcccfb40e612504a1b76f2c119790cee59e482f7b35428cccc4f71705e7e322442d07d1f7d29b5d9e72e31319fc3088d6dcbec9fc7c1f0d8881"},
	"arm64": {"aarch64", "6b14b582cd3185169df54275eed8721d94c9b40e6cb1533d0d0c46573271c6d2d03db586ea1caac46a45e805797f00b115bead9fb93636a1ffa69f2430958977"},
}

// gvisorBin is where the tarball is unpacked (gVisor's documented place).
const gvisorBin = "/usr/local/bin"

// GVisorURL is the pinned tarball for a Go arch, or "" for one gVisor does
// not release.
func GVisorURL(arch string) string {
	t, ok := gvisorTarballs[arch]
	if !ok {
		return ""
	}
	return "https://storage.googleapis.com/gvisor/releases/release/" + gvisorRelease + "/" + t.arch + "/gvisor.tar.bz2"
}

// gvisorSteps are the root steps that install the pinned gVisor (and, for
// docker, register that runsc with it), with any tool they need installed
// first from the distribution (curl, bzip2), or a problem when they cannot
// be (an arch gVisor does not release, no known package manager).
//
// The install is one step, so the download is always cleaned up: download
// into a fresh mktemp directory, check the pinned SHA-512, and only then
// unpack into /usr/local/bin, runsc last (so a cut-short unpack leaves no
// runsc without its sidecars). Any failure before that leaves the machine
// as it was (the temporary directory is removed either way), and stops the
// root script.
func gvisorSteps(f Facts, pm, arch, cli string) (steps []string, problem string) {
	t, ok := gvisorTarballs[arch]
	if !ok {
		return nil, "gVisor releases only amd64 and arm64: install Kata Containers or runsc yourself"
	}
	fetch := "curl -fsSL --proto =https -o"
	if _, ok := f.LookPath("curl"); !ok {
		if _, ok := f.LookPath("wget"); ok {
			fetch = "wget -q -O" // busybox's has no --https-only; the pinned sum is the check
		} else {
			steps = append(steps, installStep(pm, "curl"))
		}
	}
	if _, ok := f.LookPath("bzip2"); !ok {
		steps = append(steps, installStep(pm, "bzip2"))
	}
	if slices.Contains(steps, "") {
		return nil, "no known package manager to install curl or bzip2 with, which installing gVisor needs"
	}
	tarball := `"$t/gvisor.tar.bz2"`
	install := strings.Join([]string{
		fetch + " " + tarball + " " + GVisorURL(arch),
		`echo "` + t.sha512 + `  $t/gvisor.tar.bz2" | sha512sum -c -`,
		"tar -xjf " + tarball + " -C " + gvisorBin + " gvisor-bin containerd-shim-runsc-v1",
		"tar -xjf " + tarball + " -C " + gvisorBin + " runsc",
	}, " && ")
	steps = append(steps, `t=$(mktemp -d) && { `+install+`; s=$?; rm -rf "$t"; [ "$s" -eq 0 ]; }`)
	if cli == "docker" {
		steps = append(steps, dockerRegisterRunsc(gvisorBin+"/runsc")...)
	}
	return steps, ""
}

// dockerRegisterRunsc registers the pinned runsc with docker: `runsc
// install` merges a runtime "runsc" pointing at it into
// /etc/docker/daemon.json (it keeps every other key, backs the old file up
// as daemon.json~, and replaces a runsc entry docker never loaded, so docker
// runs the binary just checked), downloading nothing (--download-sidecars=
// NEVER: the sidecars came in the pinned tarball). Then a reload, not a
// restart, so running containers keep running (dockerd re-reads its
// runtimes on SIGHUP). The wait gives dockerd up to 10 s to list it before
// zm reads the machine again; it never fails the script (zm's read says
// what is true).
func dockerRegisterRunsc(runsc string) []string {
	return []string{
		runsc + " install --download-sidecars=NEVER",
		"systemctl reload docker",
		`for i in 1 2 3 4 5 6 7 8 9 10; do docker info --format "{{json .Runtimes}}" | grep -q runsc && break; sleep 1; done`,
	}
}
