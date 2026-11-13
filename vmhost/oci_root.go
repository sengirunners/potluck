package vmhost

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Root and Docker inside an oci box (zm-eakh.12.5.26; SPEC 20 "Boxes", Root
// and Docker). Measured on runsc release-20261102.0 under docker 29 with
// zm's own image (whose user, runner, has passwordless sudo):
//
//   - sudo is setuid root. A default box runs with no-new-privileges, under
//     which no setuid binary gains anything, and gVisor ignores setuid bits
//     anyway unless runsc runs with --allow-suid ("sudo: effective uid is not
//     0"). Both must change: root=on drops no-new-privileges, and the runtime
//     must honour setuid: runsc-root (the same runsc with --allow-suid, a
//     docker runtime entry zm host doctor's root line registers beside
//     "runsc", which stays as it is; checkSudoRuntime holds it to that before
//     every create and every start of a kept box, since docker resolves the
//     name each time) or Kata (a real guest kernel). Root holds the bounding
//     set, so root also drops MKNOD and, with no network, NET_RAW (on one it
//     goes anyway). Docker's default capabilities otherwise stay unchanged,
//     with no host mount. Kata Docker alone requests a VM-local TUN node
//     (oci_docker.go); no host device or descriptor is passed through.
//     A kept box keeps what it was made with, so a
//     start under a binding whose root or docker now differ is refused
//     (keptRootDiffers). Root reaches only the
//     container's bounding set, so sudo gives no capability the box did not
//     already have; under gVisor every capability is the Sentry's, never one
//     of the host kernel (runsc's own processes run with none).
//   - Docker is rootless (docker=on, implies root): dockerd inside a user
//     namespace of the box user's (rootlesskit; newuidmap is the setuid part,
//     hence root), its network a slirp4netns user-mode stack, so its
//     containers' traffic leaves as the box user's ordinary sockets, through
//     the box's own filtered network. It needs no capability added. A rootful
//     dockerd needs NET_ADMIN, and NET_ADMIN lets root re-address the box's
//     eth0: measured, a box that added a neighbour's address and a new MAC
//     answered for it on the shared bridge (other boxes' traffic could be
//     drawn to it). Full Docker networking (a bridge with NAT inside) needs
//     runsc --net-raw and --allow-packet-socket-write, which let a box forge
//     frames onto that bridge: zm grants neither. So inside a box, docker
//     run under gVisor has no network unless given --network host (the slirp stack, the
//     box's own egress); docker build likewise. Storage is vfs under HOME:
//     overlay inside gVisor needs a tmpfs upper (box memory), and HOME is
//     what a kept box keeps.
//   - Kata uses its native guest kernel for a bridge inside RootlessKit's
//     user/network namespace, with no added outer capabilities (oci_docker.go).
//     Its guest must support tun, user namespaces and namespaced netfilter;
//     the agent creates its TUN node from the verified OCI device metadata.
//   - zm-dockerd (in zm's Linux image) starts it; PID 1 runs it when docker
//     is on, and every exec gets DOCKER_HOST naming its socket.

// ociDockerRun is rootless dockerd's XDG_RUNTIME_DIR in a box, ociDockerSock
// its socket (zm-dockerd in cmd/potluck-driver-tart/images/linux uses the same).
const (
	ociDockerRun  = "/tmp/zm-docker"
	ociDockerSock = ociDockerRun + "/docker.sock"
)

// ociSudoRuntime is the docker runtime entry for a box with root under
// gVisor: runsc with --allow-suid (hostprobe registers it).
const ociSudoRuntime = "runsc-root"

// ociHonoursSetuid reports a runtime under which a setuid binary (sudo,
// newuidmap) works in the box: runsc-root, or Kata (a guest kernel).
func ociHonoursSetuid(rt string) bool {
	return rt == ociSudoRuntime || ociKataRuntime(rt)
}

// parseRoot reads root and docker (on, off or unset) for an oci binding.
func (c *config) parseRoot(m map[string]string, bad *[]string) {
	onOff := func(k string) bool {
		switch m[k] {
		case "", "off":
			return false
		case "on":
			return true
		}
		*bad = append(*bad, k+" is on or off, not "+strconv.Quote(m[k]))
		return false
	}
	root, docker := onOff("root"), onOff("docker")
	if !root && !docker {
		return
	}
	if c.VM != "oci" {
		c.parseTartRoot(m, root, docker, bad)
		return
	}
	if m["root"] == "off" && docker {
		*bad = append(*bad, "docker=on needs root (rootless docker's newuidmap is setuid): leave root unset or on")
		return
	}
	switch {
	case !ociHonoursSetuid(c.OCIRuntime):
		*bad = append(*bad, "root needs a runtime that honours setuid in the box: gVisor ignores setuid bits unless runsc has --allow-suid, so use oci_runtime "+
			ociSudoRuntime+" (zm host doctor's root line registers it with docker) or Kata, not "+strconv.Quote(c.OCIRuntime))
	case c.OCIRuntime == ociSudoRuntime && !strings.HasPrefix(filepath.Base(c.OCICLI), "docker"):
		*bad = append(*bad, ociSudoRuntime+" is a docker runtime entry: root on gVisor needs oci_cli docker")
	}
	c.Root, c.Docker = true, docker
}

// parseTartRoot reads root on a Tart binding (zm-eakh.12.54.14). A macOS
// guest's sudo is the image's: Cirrus Labs' images give their user admin
// passwordless sudo, and the driver itself uses it to close SSH and Screen
// Sharing (vm.go closeLogin). So root=on changes nothing in the VM; it is the
// owner's word that boxes may keep that sudo, as GitHub's own macOS runners
// do: root inside a throwaway VM, deleted after its lease. Without it,
// conformance's root group keeps refusing an image whose sudo works. The VM
// is the boundary and Softnet on the host its network fence (it enforces the
// VM's MAC and filters what leaves), which no root inside the guest can
// change; conformance proves it from root's own shell (root.go rootFence).
// So root=on needs Softnet and a box binding, and describe lists it only for
// a macOS image (tartRootOS): a Linux guest's root would hold NET_ADMIN and
// NET_RAW, which the root group refuses. docker stays oci's: no Docker runs in
// a macOS guest. Incus is not Tart: root is refused there.
func (c *config) parseTartRoot(m map[string]string, root, docker bool, bad *[]string) {
	switch {
	case c.VM == "incus" || docker:
		*bad = append(*bad, "docker is an oci binding's, and root an oci or tart one's: on "+c.VM+" the image decides sudo and docker")
	case m["root"] == "off" || !root:
	case !c.Softnet:
		*bad = append(*bad, "root=on on tart needs network softnet: with nat the guest reaches this Mac and the LAN, and root in it could do anything there")
	case c.Kind != "box":
		*bad = append(*bad, "root=on on tart is a box binding's (capability root)")
	default:
		c.Root = true
	}
}

// tartRootOS is the only guest OS a Tart binding lists root for.
const tartRootOS = "darwin"

// rootArgs are root's raw config keys (rawConfig).
func (c config) rootArgs(m map[string]string) {
	if c.Root {
		m["root"] = "on"
	}
	if c.Docker {
		m["docker"] = "on"
	}
}

// checkSudoRuntime is what docker says runsc-root is, before a box with root
// is made on it: the same runsc as docker's "runsc" entry (else a binary
// named runsc), with exactly --allow-suid. The name alone proves nothing: a
// hand-made or stale entry could be runc, or runsc with --net-raw or
// --network=host. hostprobe's sudo.go holds a lender to the same rule (keep
// the two in step).
func (o ociCLI) checkSudoRuntime() error {
	out, err := o.run("info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return fmt.Errorf("root=on: cannot read docker's runtimes to check %s: %v", ociSudoRuntime, err)
	}
	if why := sudoRuntimeWrong(out); why != "" {
		return fmt.Errorf("root=on: %s; zm host share --docker replaces it", why)
	}
	return nil
}

// sudoRuntimeWrong says how docker's runtimes JSON (docker info's Runtimes)
// is not zm's runsc-root, "" when it is. runsc install writes runsc-root's
// path as its resolved executable, while the runsc entry may be a bare name or
// a symlink, so both are resolved (resolveBin) before they are compared.
func sudoRuntimeWrong(runtimes string) string {
	var rts map[string]struct {
		Path        string   `json:"path"`
		RuntimeArgs []string `json:"runtimeArgs"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(runtimes)), &rts); err != nil {
		return "docker's runtimes do not read as JSON, so " + ociSudoRuntime + " cannot be checked"
	}
	e, ok := rts[ociSudoRuntime]
	runsc := rts["runsc"].Path
	switch {
	case !ok:
		return "docker has no runtime " + ociSudoRuntime
	case !slices.Equal(e.RuntimeArgs, []string{"--allow-suid"}):
		return fmt.Sprintf("docker's %s has runtimeArgs %q, not exactly [--allow-suid]", ociSudoRuntime, e.RuntimeArgs)
	case runsc != "" && resolveBin(e.Path) != resolveBin(runsc):
		return fmt.Sprintf("docker's %s runs %s, not the runsc docker's runsc entry runs (%s)", ociSudoRuntime, e.Path, runsc)
	case runsc == "" && filepath.Base(e.Path) != "runsc":
		return fmt.Sprintf("docker's %s runs %s, which is not runsc", ociSudoRuntime, e.Path)
	}
	return ""
}

// resolveBin is a runtime's binary as a path: a bare name looked up on PATH,
// then its symlinks resolved; else the name as given, cleaned.
func resolveBin(p string) string {
	if !filepath.IsAbs(p) {
		if lp, err := exec.LookPath(p); err == nil {
			p = lp
		}
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// keptRootDiffers says how a kept box's root and docker (what it was made
// with: its record's config) differ from the binding's now, "" when they do
// not. A start reuses the container, flags and all, so such a box would not
// be what describe says: a start is refused instead (persist.go), and its
// disk stays kept until release or its time.
func keptRootDiffers(made map[string]string, now config) string {
	on := func(k string) bool { return made[k] == "on" }
	root, docker := on("root") || on("docker"), on("docker")
	word := map[bool]string{true: "on", false: "off"}
	switch {
	case docker != now.Docker:
		return "this box was made with docker " + word[docker] + " and the binding now has docker " + word[now.Docker]
	case root != now.Root:
		return "this box was made with root " + word[root] + " and the binding now has root " + word[now.Root]
	}
	return ""
}
