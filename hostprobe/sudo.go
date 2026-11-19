package hostprobe

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/config"
)

// Root and Docker in a box (zm-eakh.12.5.26; SPEC 20 "Boxes", Root and
// Docker; cmd/potluck-driver-tart oci_root.go). gVisor ignores setuid bits unless
// runsc runs with --allow-suid, so sudo (and rootless Docker's newuidmap)
// cannot work in a box under the "runsc" entry that `runsc install` makes.
// zm adds a second docker runtime entry, runsc-root: the same runsc binary
// with --allow-suid and no other flag. "runsc" stays exactly as it was, so
// every default box, and any container of the owner's own on runsc, runs as
// before; only a binding shared with --docker names runsc-root.
//
// What the root step changes on a lender's machine: it copies
// /etc/docker/daemon.json to daemon.json.zm-bak-<date and time> (zm's own
// backup: runsc install's daemon.json~ is rewritten by every install, so after
// zm installed gVisor it no longer holds the owner's file), then `runsc
// install --runtime=runsc-root --clobber` merges one key, runtimes.runsc-root
// {path, runtimeArgs ["--allow-suid"]}, keeping every other key (--clobber,
// already runsc install's default, replaces any entry of that name), then docker reloads
// (systemctl, else SIGHUP to docker.pid: running containers keep running). The binary is the one
// docker already runs as root for "runsc" (or, where zm installs gVisor in
// the same script, the pinned one in /usr/local/bin), so root runs nothing
// new. --allow-suid changes only what happens inside the Sentry: a setuid
// binary raises a process to the sandbox's root, whose capabilities are the
// container's bounding set, and none of them is a host capability. zm never
// adds --net-raw or --allow-packet-socket-write (which Docker's bridge
// networking inside gVisor wants): both let a box forge frames onto the
// bridge its neighbours share.

// SudoRuntime is zm's docker runtime entry for boxes with root on gVisor.
const SudoRuntime = "runsc-root"

// detectSudo sets l.Sudo, the runtime a box with root uses, else
// SudoProblem and, where root can fix it, SudoSteps. It never makes the layer
// unavailable: without it, boxes have no sudo. runtimes is docker info's
// Runtimes JSON; installing is whether zm's root steps install gVisor (then
// they register it too). runsc-root counts only as zm makes it
// (sudoRuntimeWrong): a hand-made or stale entry of that name could be runc,
// or runsc with --net-raw or --network=host.
func detectSudo(l *Layer, runtimes string, registered map[string]bool, installing bool) {
	why := "sandboxes with root and docker (" + config.PotluckCommand("host", "--docker") + ") need a runtime that honours setuid in the box: "
	fromDocker := `p=$(docker info --format "{{(index .Runtimes \"runsc\").Path}}") && case "$p" in /*) ;; *) p=$(command -v "$p") ;; esac && "$p"`
	switch {
	case l.Isolation == "vm" && l.Runtime != "": // Kata: a guest kernel
		l.Sudo = l.Runtime
	case l.CLI != "docker" && (l.Runtime != "" || installing):
		l.SudoProblem = why + config.Program + " registers runsc with --allow-suid as docker's " + SudoRuntime + " only; on " + l.CLI + " there is none (Kata may serve, unmeasured)"
	case registered[SudoRuntime] && l.Runtime == "runsc":
		if wrong := sudoRuntimeWrong(runtimes); wrong != "" {
			l.SudoProblem = why + wrong + "; the root line replaces it with " + config.Program + "'s (runsc install --clobber)"
			l.SudoSteps = []string{dockerRegisterSudo(fromDocker)}
		} else {
			l.Sudo = SudoRuntime
		}
	case l.Runtime == "runsc":
		l.SudoProblem = why + "docker has no " + SudoRuntime + " (runsc with --allow-suid); the root line adds it beside runsc, which stays as it is"
		l.SudoSteps = []string{dockerRegisterSudo(fromDocker)}
	case installing:
		l.SudoProblem = why + "the root line registers the gVisor it installs as " + SudoRuntime + " too (runsc with --allow-suid)"
		l.SudoSteps = []string{dockerRegisterSudo(gvisorBin + "/runsc")}
	case l.Runtime != "":
		l.SudoProblem = why + "register a docker runtime " + SudoRuntime + " yourself: your gVisor's runsc with --allow-suid"
	}
}

// sudoRuntimeWrong says how docker's runtimes JSON is not zm's runsc-root:
// the runsc of docker's "runsc" entry (else a binary named runsc) with
// exactly --allow-suid; "" when it is. runsc install writes runsc-root's path
// as its resolved executable, while the runsc entry may be a bare name or a
// symlink, so both are resolved (resolveBin) before they are compared.
// zm-run-tart checks the same before every box with root (vmhost
// oci_root.go: keep the two in step).
func sudoRuntimeWrong(runtimes string) string {
	var rts map[string]struct {
		Path        string   `json:"path"`
		RuntimeArgs []string `json:"runtimeArgs"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(runtimes)), &rts); err != nil {
		return "docker's runtimes do not read as JSON, so " + SudoRuntime + " cannot be checked"
	}
	e, ok := rts[SudoRuntime]
	runsc := rts["runsc"].Path
	switch {
	case !ok:
		return "docker has no runtime " + SudoRuntime
	case !slices.Equal(e.RuntimeArgs, []string{"--allow-suid"}):
		return fmt.Sprintf("docker's %s has runtimeArgs %q, not exactly [--allow-suid]", SudoRuntime, e.RuntimeArgs)
	case runsc != "" && resolveBin(e.Path) != resolveBin(runsc):
		return fmt.Sprintf("docker's %s runs %s, not the runsc docker's runsc entry runs (%s)", SudoRuntime, e.Path, runsc)
	case runsc == "" && filepath.Base(e.Path) != "runsc":
		return fmt.Sprintf("docker's %s runs %s, which is not runsc", SudoRuntime, e.Path)
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

// dockerRegisterSudo is the one root step that adds runsc-root with docker:
// zm's own backup of daemon.json, then runsc (a command that ends in its
// path) installs it, replacing an entry of that name (--clobber), docker
// reloads (systemctl, else SIGHUP to its pid file), and the wait gives dockerd
// 10 s to list it (never failing the script). One step, so its reload is its
// own: RootScript keeps each step once, and gVisor's install has a reload of
// its own before it.
func dockerRegisterSudo(runsc string) string {
	return `{ [ ! -e /etc/docker/daemon.json ] || cp -p /etc/docker/daemon.json "/etc/docker/daemon.json.zm-bak-$(date +%Y%m%d%H%M%S)"; } && ` +
		runsc + " install --runtime=" + SudoRuntime + " --clobber --download-sidecars=NEVER -- --allow-suid && " +
		`{ systemctl reload docker || kill -HUP "$(cat /var/run/docker.pid)"; } && ` +
		`for i in 1 2 3 4 5 6 7 8 9 10; do docker info --format "{{json .Runtimes}}" | grep -q ` + SudoRuntime + ` && break; sleep 1; done`
}

// root is whether a tart share lends root (zm-eakh.12.54.14): --root, or
// --root=false, when given (RootSet), else what an earlier tart share of
// these bindings bound, as docker below; from names that binding.
func (o ShareOptions) root() (on bool, from string) {
	if o.Root || o.RootSet {
		return o.Root, ""
	}
	return o.earlier("root", "tart")
}

// Lent are the box bindings an earlier share made on this machine
// (o.Earlier named <prefix>-<size> for a known size), by name: what a
// re-run replaces.
func (o ShareOptions) Lent() []computeapi.RunTarget {
	var out []computeapi.RunTarget
	for _, t := range o.Earlier {
		size, ok := strings.CutPrefix(t.Name, o.NamePrefix+"-")
		if !ok || t.Kind != "box" || t.Host != "" {
			continue
		}
		if _, err := SizeByName(size); err == nil {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b computeapi.RunTarget) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// earlier names the first of this machine's box bindings <prefix>-<size>
// with config key on, on layer vm ("" any layer).
func (o ShareOptions) earlier(key, vm string) (bool, string) {
	for _, t := range o.Lent() {
		if t.Config[key] == "on" && (vm == "" || cmp.Or(t.Config["vm"], "tart") == vm) {
			return true, t.Name
		}
	}
	return false, ""
}

// docker is whether a share lends root and docker: --docker, or
// --docker=false, when given (DockerSet), else what an earlier share of these
// bindings (<prefix>-<size>) bound, so a re-run keeps it as it keeps
// admission keys; from names that binding.
func (o ShareOptions) docker() (on bool, from string) {
	if o.Docker || o.DockerSet {
		return o.Docker, ""
	}
	return o.earlier("docker", "")
}
